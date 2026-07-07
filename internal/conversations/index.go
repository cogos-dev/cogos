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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// metaLockTimeout bounds how long UpsertSession/DeleteSession wait to acquire
// the cross-process lock on _meta.json before giving up. Generous relative to
// the write itself (a few JSON marshals + one rename) but short enough that a
// wedged peer doesn't hang the caller indefinitely.
const metaLockTimeout = 5 * time.Second

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

	// lastMeta{Mtime,Size,Hash} record the (mtime, size, content hash) of
	// _meta.json as of the last Load or local write. LoadIfChanged uses them to
	// skip the full reload when the on-disk index is unchanged since this process
	// last touched it. (mtime, size) is a cheap pre-filter; the content hash is
	// the authoritative check, catching a same-size in-place rewrite by an
	// external writer that (mtime, size) alone would miss. Guarded by mu.
	lastMetaMtime time.Time
	lastMetaSize  int64
	lastMetaHash  string
}

// metaDelta describes the change a single writeMetaFileLocked call must apply
// to _meta.json, as opposed to a full re-assertion of this process's in-memory
// idx.sessions snapshot (which could be stale relative to a peer process's
// more recent write — see writeMetaFileLocked).
type metaDelta struct {
	upserts map[string]SessionMeta
	deletes []string
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
	// Stat before reading so a write racing this load at worst causes a
	// redundant reload next cycle (recorded mtime older than content), never a
	// stale read (recorded mtime never newer than content).
	var metaMtime time.Time
	var metaSize int64
	if fi, statErr := os.Stat(idx.metaPath()); statErr == nil {
		metaMtime = fi.ModTime()
		metaSize = fi.Size()
	}

	sessions := make(map[string]SessionMeta)
	var metaHash string
	if data, err := os.ReadFile(idx.metaPath()); err == nil {
		metaHash = sha256Hex(data)
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
	idx.lastMetaMtime = metaMtime
	idx.lastMetaSize = metaSize
	idx.lastMetaHash = metaHash
	idx.mu.Unlock()
	return nil
}

// LoadIfChanged reloads the index from disk only when _meta.json has changed
// since the last Load or local write, detected by (mtime, size). When this
// process is the sole writer — the common case for the daemon — every change is
// already reflected in memory by UpsertSession/DeleteSession, so the expensive
// full reload is skipped. An external writer (e.g. the cog CLI) changes the
// file's mtime/size and triggers a real reload. Returns whether a reload ran.
func (idx *Index) LoadIfChanged() (bool, error) {
	fi, err := os.Stat(idx.metaPath())
	if err != nil {
		// Missing/unstattable _meta.json (fresh index, or removed out from under
		// us): fall back to a full Load, which treats a missing file as empty.
		return true, idx.Load()
	}
	idx.mu.RLock()
	mtime, size, hash := idx.lastMetaMtime, idx.lastMetaSize, idx.lastMetaHash
	idx.mu.RUnlock()

	// Cheap pre-filter: any size or mtime change is unambiguously a change.
	if fi.Size() != size || !fi.ModTime().Equal(mtime) {
		return true, idx.Load()
	}
	// Same (mtime, size) can still be an in-place same-size rewrite by an
	// external process (the cog CLI editing fixed-width fields), which
	// (mtime, size) alone cannot detect. Confirm by content hash before trusting
	// the in-memory copy. Reading+hashing the single _meta.json file is far
	// cheaper than the full reload it guards (which re-reads every per-session
	// turns file). A torn/partial read hashes differently and falls through to a
	// reload, so it self-heals rather than sticking.
	data, readErr := os.ReadFile(idx.metaPath())
	if readErr != nil {
		return true, idx.Load()
	}
	if sha256Hex(data) == hash {
		return false, nil
	}
	return true, idx.Load()
}

// UpsertSession writes session meta + turns to memory and to disk.
//
// The on-disk _meta.json is a shared file: the daemon's own reconcile/ticker
// cycle and a separately-invoked "cog reconcile conversations" CLI process
// can both call UpsertSession/DeleteSession against the same projDir
// (issue #449). Without coordination the two read-modify-write cycles
// interleave and the second writer's plain marshal-of-idx.sessions silently
// drops whatever the first writer added, because each process's in-memory
// idx.sessions only reflects its own view as of its last Load.
//
// To close that window, the on-disk write is guarded by a cross-process
// filelock (metaLockPath) and, while holding it, writeMetaFileLocked re-reads
// the current _meta.json from disk and applies only *this call's* delta
// (metaDelta) on top of it — never a full re-assertion of this process's
// entire in-memory idx.sessions snapshot, which could itself be stale
// relative to a peer's more recent write and would silently resurrect
// whatever the peer already removed. See writeMetaFileLocked for why a
// snapshot-merge (rather than a delta-merge) is unsound.
func (idx *Index) UpsertSession(meta SessionMeta, turns []Turn) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.sessions[meta.SessionID] = meta
	idx.turns[meta.SessionID] = turns

	// Persist turns file.
	if err := idx.writeTurnsFile(meta.SessionID, turns); err != nil {
		return fmt.Errorf("conversations/index: write turns %s: %w", meta.SessionID, err)
	}
	// Persist meta index under the cross-process lock. Only this call's own
	// upsert is applied as a delta on top of the current on-disk state — see
	// writeMetaFileLocked.
	return idx.writeMetaFileLocked(metaDelta{upserts: map[string]SessionMeta{meta.SessionID: meta}})
}

// DeleteSession removes a session from memory and disk.
func (idx *Index) DeleteSession(sessionID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	delete(idx.sessions, sessionID)
	delete(idx.turns, sessionID)

	// Same per-session lock writeTurnsFile takes, so a concurrent peer
	// UpsertSession for this exact sessionID can't recreate the turns file
	// in between this Remove and the caller's write, and vice versa.
	if err := idx.removeTurnsFile(sessionID); err != nil {
		return fmt.Errorf("conversations/index: remove turns file: %w", err)
	}
	return idx.writeMetaFileLocked(metaDelta{deletes: []string{sessionID}})
}

// removeTurnsFile deletes sessionID's turns file under the same per-session
// cross-process lock writeTurnsFile uses, so a delete can't race a
// concurrent peer write of the same sessionID's turns file.
func (idx *Index) removeTurnsFile(sessionID string) error {
	lock, err := filelock.Acquire(idx.turnsLockPath(sessionID), metaLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire turns lock for %s: %w", sessionID, err)
	}
	defer lock.Release()

	if err := os.Remove(idx.turnsPath(sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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

// metaLockPath is the advisory cross-process lock file guarding the
// read-modify-write cycle on _meta.json. A sibling file, not metaPath itself,
// so the lock's own lifecycle (create/lock/unlock) never touches the JSON
// content file that Load/LoadIfChanged read.
func (idx *Index) metaLockPath() string {
	return filepath.Join(idx.projDir, "_meta.json.lock")
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// writeMetaFileLocked persists delta (this call's own upserts/deletes) into
// _meta.json. Callers must hold idx.mu (write lock).
//
// The write is atomic (tmp + rename, same pattern as
// BusSessionManager.saveRegistry in internal/engine/bus_session.go) — a plain
// truncate-before-write left _meta.json empty (and every subsequent Load
// silently falling back to an empty index) if the process was killed
// mid-write.
//
// It also takes the cross-process filelock (pkg/filelock) around the full
// read-modify-write cycle: re-read whatever is currently on disk, apply only
// delta on top of it, then atomically write the result. This is what closes
// issue #449 — a CLI reconcile and the daemon's own reconcile cycle can each
// read-modify-write _meta.json independently; taking the lock serializes the
// two cycles, and applying a per-call delta (rather than a full marshal of
// this process's in-memory idx.sessions) means a writer never re-asserts a
// stale snapshot that could silently resurrect a session a peer has since
// deleted, or drop one a peer has since added. A snapshot-merge was tried
// first and found unsound by TestCrossProcessDeleteSurvivesConcurrentUpsert:
// re-adding every key from idx.sessions on top of the freshly-read on-disk
// map reintroduces this process's own stale copy of a session a peer already
// deleted, because idx.sessions only reflects this process's last Load, not
// the peer's more recent delete.
//
// Deliberately does NOT unconditionally adopt the disk-merged map (which may
// contain a peer process's session keys) into idx.sessions, and does NOT
// unconditionally update lastMeta{Mtime,Size,Hash} to describe the merged
// file's on-disk stats. An earlier version of this fix did both
// unconditionally, and cog-review (PR #458) correctly flagged the resulting
// metadata/turns split-brain: idx.sessions would gain a peer's session key
// while idx.turns — populated only by this process's own Load/UpsertSession —
// never got that session's turns, and because lastMetaHash was set to match
// the bytes just written, the next LoadIfChanged saw "no external change" and
// skipped the full reload that would otherwise have backfilled idx.turns.
// cog_list_conversations would then list a session that
// cog_get_turn/cog_search could never actually serve.
//
// The fix: after computing merged, compare it against idx.sessions as the
// caller already left it (delta pre-applied by UpsertSession/DeleteSession
// before calling here). If they're equal, no peer wrote anything this
// process doesn't already fully know about (turns included) — the common
// sole-writer case — so it's safe to keep the LoadIfChanged fast-path
// bookkeeping (lastMeta{Mtime,Size,Hash}) in sync with what was just written,
// same as before this fix. If they differ, a peer's content is present in
// merged that idx.sessions doesn't have turns for; leave idx.sessions and the
// lastMeta bookkeeping untouched by this call, so the next LoadIfChanged
// detects the file changed out from under its recorded baseline and performs
// a full reload — sessions map AND turns map together — keeping the two maps
// consistent with each other at the cost of that one reload.
func (idx *Index) writeMetaFileLocked(delta metaDelta) error {
	lock, err := filelock.Acquire(idx.metaLockPath(), metaLockTimeout)
	if err != nil {
		return fmt.Errorf("conversations/index: acquire meta lock: %w", err)
	}
	defer lock.Release()

	merged := make(map[string]SessionMeta)
	if data, readErr := os.ReadFile(idx.metaPath()); readErr == nil {
		var onDisk map[string]SessionMeta
		if jsonErr := json.Unmarshal(data, &onDisk); jsonErr == nil {
			for k, v := range onDisk {
				merged[k] = v
			}
		}
		// A corrupt/unparseable on-disk file is treated as empty rather than
		// aborting the write — delta is still applied and this call becomes
		// the one restoring a valid file.
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("conversations/index: read meta for merge: %w", readErr)
	}

	for k, v := range delta.upserts {
		merged[k] = v
	}
	for _, k := range delta.deletes {
		delete(merged, k)
	}

	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	tmp := idx.metaPath() + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, idx.metaPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// Only adopt merged (and the fast-path bookkeeping) when it exactly
	// matches idx.sessions as the caller left it — i.e. no peer content is
	// present that this process's idx.turns doesn't already cover. See the
	// func comment above for why an unconditional adopt reintroduces the
	// sessions/turns split-brain cog-review flagged on PR #458.
	if sessionMapsEqual(merged, idx.sessions) {
		idx.lastMetaHash = sha256Hex(b)
		if fi, statErr := os.Stat(idx.metaPath()); statErr == nil {
			idx.lastMetaMtime = fi.ModTime()
			idx.lastMetaSize = fi.Size()
		}
	}
	// else: leave idx.sessions/lastMeta* exactly as they were. idx.sessions
	// still holds this call's own delta (applied by the caller), which is
	// consistent with idx.turns; the on-disk file now also carries the
	// peer's content, which the next LoadIfChanged's content-hash check will
	// notice (it no longer matches lastMetaHash) and reload properly.
	return nil
}

// sessionMapsEqual reports whether a and b contain exactly the same set of
// session IDs mapped to byte-for-byte-equal SessionMeta values, using each
// value's JSON encoding for the comparison (SessionMeta has no slice/map
// fields, so this is equivalent to a field-by-field equality check but
// avoids having to keep a manual comparator in sync with the struct).
func sessionMapsEqual(a, b map[string]SessionMeta) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		aj, aerr := json.Marshal(av)
		bj, berr := json.Marshal(bv)
		if aerr != nil || berr != nil || string(aj) != string(bj) {
			return false
		}
	}
	return true
}

// turnsLockPath is the advisory cross-process lock file guarding a single
// session's turns file. Keyed per-session (not a single shared lock across
// all sessions) so two different sessions' writers never contend on the same
// lock — only two writers of the *same* sessionID, which is the actual race.
func (idx *Index) turnsLockPath(sessionID string) string {
	return idx.turnsPath(sessionID) + ".lock"
}

// writeTurnsFile persists the full turns list for sessionID.
//
// Same shape as writeMetaFileLocked and fixed for the same reason
// (cog-review, PR #458): the daemon's reconcile ticker and a
// separately-invoked "cog reconcile conversations" CLI process can both
// detect drift on the same session's source JSONL in the same window and
// each call UpsertSession(meta, turns) for the identical sessionID
// concurrently, racing on this exact file. Unlike _meta.json (a shared map
// needing a read-merge-write), each turns file is owned wholly by one
// sessionID and every writer replaces the full content, so no on-disk merge
// is needed here — only atomicity (tmp + rename, preventing a torn read on
// loadTurnsFile) and a cross-process lock (preventing the interleaving
// itself, so the two writes fully serialize rather than merely not tearing
// each other's bytes). Without either, loadTurnsFile's json.Unmarshal can
// fail on a corrupted file and Load drops the session's meta and turns
// entirely (index.go's Load: on a per-session parse error it deletes the
// session from the in-memory map rather than surfacing a partial result) —
// a stronger data-loss outcome than #449's "last writer wins on one field".
func (idx *Index) writeTurnsFile(sessionID string, turns []Turn) error {
	lock, err := filelock.Acquire(idx.turnsLockPath(sessionID), metaLockTimeout)
	if err != nil {
		return fmt.Errorf("conversations/index: acquire turns lock for %s: %w", sessionID, err)
	}
	defer lock.Release()

	b, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return err
	}
	path := idx.turnsPath(sessionID)
	tmp := path + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
