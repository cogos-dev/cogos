// index.go — in-memory full-text index for conversation turns.
//
// The index is a flat in-memory structure backed by a projection directory:
//   .cog/state/conversations/
//     <session_id>.json   — JSON array of Turn (one file per session)
//     _meta.json          — JSON object: session_id → SessionMeta
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
	"time"
)

// Index holds all indexed turns in memory plus a reference to the projection
// directory for persistence.
type Index struct {
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
func (idx *Index) Load() error {
	metaPath := idx.metaPath()
	if data, err := os.ReadFile(metaPath); err == nil {
		var m map[string]SessionMeta
		if jsonErr := json.Unmarshal(data, &m); jsonErr == nil {
			idx.sessions = m
		}
	}

	// Load turns for each known session.
	for sid := range idx.sessions {
		turns, err := idx.loadTurnsFile(sid)
		if err != nil {
			// Corrupt turn file — remove from index; will be re-projected.
			delete(idx.sessions, sid)
			continue
		}
		idx.turns[sid] = turns
	}
	return nil
}

// UpsertSession writes session meta + turns to memory and to disk.
func (idx *Index) UpsertSession(meta SessionMeta, turns []Turn) error {
	idx.sessions[meta.SessionID] = meta
	idx.turns[meta.SessionID] = turns

	// Persist turns file.
	if err := idx.writeTurnsFile(meta.SessionID, turns); err != nil {
		return fmt.Errorf("conversations/index: write turns %s: %w", meta.SessionID, err)
	}
	// Persist meta index.
	return idx.writeMetaFile()
}

// DeleteSession removes a session from memory and disk.
func (idx *Index) DeleteSession(sessionID string) error {
	delete(idx.sessions, sessionID)
	delete(idx.turns, sessionID)

	turnsPath := idx.turnsPath(sessionID)
	if err := os.Remove(turnsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("conversations/index: remove turns file: %w", err)
	}
	return idx.writeMetaFile()
}

// GetMeta returns the SessionMeta for a session, or false if not indexed.
func (idx *Index) GetMeta(sessionID string) (SessionMeta, bool) {
	m, ok := idx.sessions[sessionID]
	return m, ok
}

// ListSessions returns all indexed SessionMetas sorted by LastTurnAt descending.
func (idx *Index) ListSessions(since, until time.Time, identity string) []SessionMeta {
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

// GetTurn returns the Turn at turnIndex within session, or false.
func (idx *Index) GetTurn(sessionID string, turnIndex int) (Turn, bool) {
	turns, ok := idx.turns[sessionID]
	if !ok || turnIndex < 0 || turnIndex >= len(turns) {
		return Turn{}, false
	}
	return turns[turnIndex], true
}

// Search performs a case-insensitive substring search over all indexed turns.
// Filters: since/until bound timestamps; sessionID restricts to one session;
// identity filters by session identity; limit caps results (0 = no limit).
func (idx *Index) Search(query string, since, until time.Time, sessionID, identity string, limit int) []SearchHit {
	lq := strings.ToLower(query)
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
			if !strings.Contains(strings.ToLower(t.Text), lq) {
				continue
			}
			excerpt := makeExcerpt(t.Text, query, 300)
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
			}
			hits = append(hits, hit)
			if limit > 0 && len(hits) >= limit {
				return hits
			}
		}
	}
	return hits
}

// ─── persistence helpers ─────────────────────────────────────────────────────

func (idx *Index) metaPath() string {
	return filepath.Join(idx.projDir, "_meta.json")
}

func (idx *Index) turnsPath(sessionID string) string {
	return filepath.Join(idx.projDir, sessionID+".json")
}

func (idx *Index) writeMetaFile() error {
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
