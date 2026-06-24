// worktree_ledger_cache_test.go — covers the per-file scan cache in
// FilesystemLedgerReader.ReadWorktreeEvents. The reconcile cycle calls this
// every ~30s; the cache must re-parse only files whose size/mtime changed.
package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeLedgerEvent appends one worktree.* EventEnvelope line to
// <ws>/.cog/ledger/<session>/events.jsonl, creating the dir on demand.
func writeLedgerEvent(t *testing.T, ws, session string, kind CogBlockKind, data map[string]any) {
	t.Helper()
	dir := filepath.Join(ws, ".cog", "ledger", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ledger session: %v", err)
	}
	env := EventEnvelope{HashedPayload: EventPayload{
		Type:      string(kind),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SessionID: session,
		Data:      data,
	}}
	line, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("write event: %v", err)
	}
}

func TestReadWorktreeEvents_CachesUnchangedFiles(t *testing.T) {
	// Wrap the scan function so we can count actual (cache-miss) parses.
	var parses int32
	orig := scanWorktreeEventsFile
	scanWorktreeEventsFile = func(path, repoRoot string) ([]WorktreeLedgerEvent, error) {
		atomic.AddInt32(&parses, 1)
		return orig(path, repoRoot)
	}
	defer func() { scanWorktreeEventsFile = orig }()

	ws := t.TempDir()
	repoRoot := "/repo/root"
	writeLedgerEvent(t, ws, "sessA", BlockWorktreeCreated, map[string]any{
		"worktree_id":   "wt-1",
		"repo_root":     repoRoot,
		"worktree_path": "/repo/root/.wt/wt-1",
		"branch":        "feature",
	})

	r := NewFilesystemLedgerReader(ws)
	ctx := context.Background()

	// First read parses sessA once and returns the created event.
	ev1, err := r.ReadWorktreeEvents(ctx, repoRoot)
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if len(ev1) != 1 {
		t.Fatalf("read 1: got %d events, want 1", len(ev1))
	}
	if n := atomic.LoadInt32(&parses); n != 1 {
		t.Fatalf("read 1: parses=%d, want 1", n)
	}

	// Second read: file unchanged → cache hit, no new parse.
	ev2, err := r.ReadWorktreeEvents(ctx, repoRoot)
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if len(ev2) != 1 {
		t.Fatalf("read 2: got %d events, want 1", len(ev2))
	}
	if n := atomic.LoadInt32(&parses); n != 1 {
		t.Fatalf("read 2 should be a cache hit: parses=%d, want still 1", n)
	}

	// Append a second event: size grows → cache invalidated → re-parse, 2 events.
	writeLedgerEvent(t, ws, "sessA", BlockWorktreeTerminal, map[string]any{
		"worktree_id": "wt-1",
		"reason":      "merged",
	})
	ev3, err := r.ReadWorktreeEvents(ctx, repoRoot)
	if err != nil {
		t.Fatalf("read 3: %v", err)
	}
	if len(ev3) != 2 {
		t.Fatalf("read 3: got %d events, want 2", len(ev3))
	}
	if n := atomic.LoadInt32(&parses); n != 2 {
		t.Fatalf("read 3 should re-parse the grown file: parses=%d, want 2", n)
	}

	// Remove the session dir: the cache entry is evicted, zero events returned.
	if err := os.RemoveAll(filepath.Join(ws, ".cog", "ledger", "sessA")); err != nil {
		t.Fatalf("remove session: %v", err)
	}
	ev4, err := r.ReadWorktreeEvents(ctx, repoRoot)
	if err != nil {
		t.Fatalf("read 4: %v", err)
	}
	if len(ev4) != 0 {
		t.Fatalf("read 4: got %d events, want 0 after deletion", len(ev4))
	}

	r.mu.Lock()
	_, stillCached := r.scanCache[filepath.Join(ws, ".cog", "ledger", "sessA", "events.jsonl")]
	r.mu.Unlock()
	if stillCached {
		t.Error("deleted session ledger was not evicted from the scan cache")
	}
}

// TestReadWorktreeEvents_RepoRootKeyedCache ensures a different repoRoot filter
// does not serve another root's cached events.
func TestReadWorktreeEvents_RepoRootKeyedCache(t *testing.T) {
	ws := t.TempDir()
	writeLedgerEvent(t, ws, "sessA", BlockWorktreeCreated, map[string]any{
		"worktree_id":   "wt-1",
		"repo_root":     "/repo/alpha",
		"worktree_path": "/repo/alpha/.wt/wt-1",
	})
	r := NewFilesystemLedgerReader(ws)
	ctx := context.Background()

	alpha, err := r.ReadWorktreeEvents(ctx, "/repo/alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if len(alpha) != 1 {
		t.Fatalf("alpha: got %d, want 1", len(alpha))
	}
	// Different repoRoot must not reuse alpha's cached events (created events are
	// repo_root-filtered).
	beta, err := r.ReadWorktreeEvents(ctx, "/repo/beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if len(beta) != 0 {
		t.Fatalf("beta: got %d events, want 0 (alpha's event must not leak across repoRoot)", len(beta))
	}
}
