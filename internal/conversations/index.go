// index.go — in-memory full-text index for conversation turns.
//
// The index is a flat in-memory structure backed by a projection directory:
//
//	.cog/state/conversations/
//	  <session_id>.json   — JSON array of Turn (one file per session)
//	  _meta.json          — JSON object: session_id → SessionMeta
//
// This is intentionally not SQLite FTS5 — the observatory aims for zero
// additional runtime dependencies. Text search uses strings.Contains on
// pre-lowercased text, which is fast enough for the expected corpus size
// (~hundreds of sessions, ~tens of thousands of turns total).
//
// Embedding-based semantic search is deferred to a follow-on PR.
package conversations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Index holds all indexed turns in memory plus a reference to the projection
// directory for persistence.
//
// All access to the sessions/turns maps is guarded by mu. The reconcile daemon
// and the autonomic ticker both drive FetchLive concurrently (which calls
// Load), and MCP tool handlers read the index concurrently with those; without
// this lock those paths raced on the maps and crashed the kernel with
// "fatal error: concurrent map writes".
type Index struct {
	// mu guards sessions and turns. Use RLock for read-only methods and Lock
	// for methods that mutate the maps. Disk I/O is performed outside the lock
	// where possible (see Load).
	mu sync.RWMutex

	projDir string

	// sessions maps session_id → SessionMeta.
	sessions map[string]SessionMeta

	// turns maps session_id → []Turn (in turn-index order).
	turns map[string][]Turn
}

// NewIndex creates an Index backed by projDir. projDir is created if absent.
func NewIndex(projDir string) (*Index, error) {
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		return nil, fmt.Errorf("conversations/index: mkdir %s: %w", projDir, err)
	}
	idx := &Index{
		projDir:  projDir,
		sessions: make(map[string]SessionMeta),
		turns:    make(map[string][]Turn),
	}
	return idx, nil
}

// Load reads all projection files from projDir into memory.
// Call once at startup (FetchLive). Subsequent operations keep state in sync.
//
// Disk reads are performed into freshly-allocated local maps with no lock held,
// then swapped into the index under a single write lock. This keeps the
// (potentially slow) file I/O off the lock while still making the visible map
// state mutation atomic with respect to concurrent readers and other Loads.
func (idx *Index) Load() error {
	sessions := make(map[string]SessionMeta)
	if data, err := os.ReadFile(idx.metaPath()); err == nil {
		var m map[string]SessionMeta
		if jsonErr := json.Unmarshal(data, &m); jsonErr == nil {
			sessions = m
		}
	}

	// Load turns for each known session into a local map.
	turns := make(map[string][]Turn, len(sessions))
	for sid := range sessions {
		t, err := idx.loadTurnsFile(sid)
		if err != nil {
			// Corrupt turn file — remove from index; will be re-projected.
			delete(sessions, sid)
			continue
		}
		turns[sid] = t
	}

	// Atomically swap in the freshly loaded state.
	idx.mu.Lock()
	idx.sessions = sessions
	idx.turns = turns
	idx.mu.Unlock()
	return nil
}

// UpsertSession writes session meta + turns to memory and to disk.
func (idx *Index) UpsertSession(meta SessionMeta, turns []Turn) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.sessions[meta.SessionID] = meta
	idx.turns[meta.SessionID] = turns

	// Persist turns file.
	if err := idx.writeTurnsFile(meta.SessionID, turns); err != nil {
		return fmt.Errorf("conversations/index: write turns %s: %w", meta.SessionID, err)
	}
	// Persist meta index (marshals idx.sessions; must hold the lock).
	return idx.writeMetaFileLocked()
}

// DeleteSession removes a session from memory and disk.
func (idx *Index) DeleteSession(sessionID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.sessions, sessionID)
	delete(idx.turns, sessionID)

	turnsPath := idx.turnsPath(sessionID)
	if err := os.Remove(turnsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("conversations/index: remove turns file: %w", err)
	}
	return idx.writeMetaFileLocked()
}

// GetMeta returns the SessionMeta for a session, or false if not indexed.
func (idx *Index) GetMeta(sessionID string) (SessionMeta, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	m, ok := idx.sessions[sessionID]
	return m, ok
}

// ListSessions returns all indexed SessionMetas sorted by LastTurnAt descending.
func (idx *Index) ListSessions(since, until time.Time, identity string) []SessionMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var out []SessionMeta
	for _, m := range idx.sessions {
		if !since.IsZero() && m.LastTurnAt.Before(since) {
			continue
		}
		if !until.IsZero() && m.FirstTurnAt.After(until) {
			continue
		}
		if identity != "" && !strings.EqualFold(m.Identity, identity) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastTurnAt.After(out[j].LastTurnAt)
	})
	return out
}

// GetTurns returns a copy of the indexed turn slice for a session (in
// turn-index order) plus its meta and a presence flag. The returned slice is a
// fresh copy safe to mutate/append without holding the index lock — the
// incremental parse path appends new turns onto it before re-upserting.
func (idx *Index) GetTurns(sessionID string) (SessionMeta, []Turn, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	meta, ok := idx.sessions[sessionID]
	if !ok {
		return SessionMeta{}, nil, false
	}
	src := idx.turns[sessionID]
	cp := make([]Turn, len(src))
	copy(cp, src)
	return meta, cp, true
}

// GetTurn returns the Turn at turnIndex within session, or false.
func (idx *Index) GetTurn(sessionID string, turnIndex int) (Turn, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	turns, ok := idx.turns[sessionID]
	if !ok || turnIndex < 0 || turnIndex >= len(turns) {
		return Turn{}, false
	}
	return turns[turnIndex], true
}

// Search performs a case-insensitive multi-term search over all indexed turns.
//
// Query parsing rules:
//   - Double-quoted substrings are matched as exact (case-insensitive) phrases.
//   - Unquoted tokens separated by whitespace form an AND conjunction: a turn
//     matches only when ALL tokens are present as substrings of its text.
//   - A single unquoted token behaves identically to the original single-term
//     substring match (backward-compatible).
//
// Filters: since/until bound timestamps; sessionID restricts to one session;
// identity filters by session identity; limit caps results (0 = no limit).
func (idx *Index) Search(query string, since, until time.Time, sessionID, identity string, limit int) []SearchHit {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	terms := parseSearchQuery(query)
	var hits []SearchHit

	// Collect sessions to search.
	var sids []string
	if sessionID != "" {
		sids = []string{sessionID}
	} else {
		for sid := range idx.turns {
			sids = append(sids, sid)
		}
		sort.Strings(sids)
	}

	for _, sid := range sids {
		meta, hasMeta := idx.sessions[sid]
		if identity != "" && hasMeta && !strings.EqualFold(meta.Identity, identity) {
			continue
		}

		turns := idx.turns[sid]
		for _, t := range turns {
			if !since.IsZero() && t.Timestamp.Before(since) {
				continue
			}
			if !until.IsZero() && t.Timestamp.After(until) {
				continue
			}
			if !matchesAllTerms(t.Text, terms) {
				continue
			}
			// Use the first term as the excerpt anchor for multi-term queries.
			anchor := query
			if len(terms) > 0 {
				anchor = terms[0]
			}
			excerpt := makeExcerpt(t.Text, anchor, 300)
			hit := SearchHit{
				SessionID: sid,
				TurnIndex: t.TurnIndex,
				UUID:      t.UUID,
				Timestamp: t.Timestamp,
				Role:      t.Role,
				Excerpt:   excerpt,
				Identity:  meta.Identity,
			}
			if hasMeta {
				hit.SessionTitle = meta.Title
				hit.Source = meta.Source
			}
			hits = append(hits, hit)
			if limit > 0 && len(hits) >= limit {
				return hits
			}
		}
	}
	return hits
}

// parseSearchQuery splits a query string into individual match terms.
// Double-quoted substrings are extracted as single exact-match terms.
// Remaining text is split on whitespace.
//
// Examples:
//
//	`foo bar`            → ["foo", "bar"]
//	`"foo bar" baz`      → ["foo bar", "baz"]
//	`"exact phrase"`     → ["exact phrase"]
//	`hello`              → ["hello"]
func parseSearchQuery(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	var terms []string
	rest := query
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if rest[0] == '"' {
			// Find closing quote.
			end := strings.Index(rest[1:], "\"")
			if end < 0 {
				// Unclosed quote: treat remainder as literal term.
				terms = append(terms, rest[1:])
				break
			}
			phrase := rest[1 : end+1]
			if phrase != "" {
				terms = append(terms, phrase)
			}
			rest = rest[end+2:]
		} else {
			// Take until next whitespace or quote.
			i := strings.IndexAny(rest, " \t\r\n\"")
			if i < 0 {
				terms = append(terms, rest)
				break
			}
			terms = append(terms, rest[:i])
			rest = rest[i:]
		}
	}
	return terms
}

// matchesAllTerms returns true when text contains ALL terms as
// case-insensitive substrings.
func matchesAllTerms(text string, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	ltext := strings.ToLower(text)
	for _, term := range terms {
		if !strings.Contains(ltext, strings.ToLower(term)) {
			return false
		}
	}
	return true
}

// ─── persistence helpers ─────────────────────────────────────────────────────

func (idx *Index) metaPath() string {
	return filepath.Join(idx.projDir, "_meta.json")
}

// turnsFilename derives a safe flat filename from sessionID. Composite keys
// for normalized ingest sessions take the form "<source>/<session_id>"; the
// "/" is replaced with "__" so the file stays in projDir without creating
// subdirectories.
func turnsFilename(sessionID string) string {
	return strings.ReplaceAll(sessionID, "/", "__") + ".json"
}

func (idx *Index) turnsPath(sessionID string) string {
	return filepath.Join(idx.projDir, turnsFilename(sessionID))
}

// writeMetaFileLocked persists the session meta index. Callers must hold
// idx.mu (write lock) because it marshals idx.sessions.
func (idx *Index) writeMetaFileLocked() error {
	b, err := json.MarshalIndent(idx.sessions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(idx.metaPath(), b, 0o644)
}

func (idx *Index) writeTurnsFile(sessionID string, turns []Turn) error {
	b, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(idx.turnsPath(sessionID), b, 0o644)
}

func (idx *Index) loadTurnsFile(sessionID string) ([]Turn, error) {
	data, err := os.ReadFile(idx.turnsPath(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var turns []Turn
	if err := json.Unmarshal(data, &turns); err != nil {
		return nil, fmt.Errorf("parse turns file: %w", err)
	}
	return turns, nil
}

// ─── text helpers ─────────────────────────────────────────────────────────────

// makeExcerpt returns a ~maxLen-char snippet from text that contains query.
// If the query is not found (shouldn't happen after Search filter) returns
// the beginning of the text.
func makeExcerpt(text, query string, maxLen int) string {
	ltext := strings.ToLower(text)
	lq := strings.ToLower(query)

	pos := strings.Index(ltext, lq)
	if pos < 0 {
		if len(text) <= maxLen {
			return text
		}
		return text[:maxLen] + "…"
	}

	// Center the excerpt on the match.
	half := maxLen / 2
	start := pos - half
	if start < 0 {
		start = 0
	}
	end := start + maxLen
	if end > len(text) {
		end = len(text)
		start = end - maxLen
		if start < 0 {
			start = 0
		}
	}

	excerpt := text[start:end]
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if end < len(text) {
		excerpt = excerpt + "…"
	}
	return excerpt
}
