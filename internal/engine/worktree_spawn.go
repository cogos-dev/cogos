// worktree_spawn.go — substrate-canonical worktree creation primitive (ADR-096 §2).
//
// SpawnWorktree is the EXCLUSIVE entry point for substrate-managed git
// worktrees. It enforces the ledger-first rule (ADR-091 §5): the
// `worktree.created` ledger event is written BEFORE the `git worktree add`
// invocation. If the ledger write fails, no worktree is created. If the
// `git worktree add` fails after the ledger write succeeds, the ledger entry
// remains and the WorktreeReconciler will observe `liveState =
// path_does_not_exist` for a worktree with a creation record — a valid alarm
// condition, not a chain-break.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorktreeOpts is the input to SpawnWorktree per ADR-096 §2.
type WorktreeOpts struct {
	DispatchID   string // ID of the dispatch requesting this worktree
	Branch       string // Branch name to create or check out
	Base         string // Base commit or ref (HEAD of main, a specific SHA, etc.)
	RepoRoot     string // Absolute path to the repository root
	WorktreeRoot string // Directory under which the worktree is created
}

// WorktreeHandle is the result of a successful SpawnWorktree call.
type WorktreeHandle struct {
	Identity   string // Canonical identity: worktree-{dispatch_id}
	Path       string // Absolute path to the worktree on disk
	Branch     string // Branch checked out in this worktree
	Base       string // Base ref the branch was cut from
	CreatedAt  time.Time
	DispatchID string // Bound dispatch identity
}

// SpawnWorktree is the canonical entry point for substrate-managed worktree
// creation. ledgerWriter is the ledger sink that records the
// `worktree.created` event; gitAdd is the function that performs the
// underlying `git worktree add`. Both are injectable so tests can stub them.
//
// Production callers should use SpawnWorktreeDefault, which wires the
// filesystem-backed adapters.
func SpawnWorktree(
	ctx context.Context,
	opts WorktreeOpts,
	ledgerWriter LedgerWriter,
	gitAdd func(ctx context.Context, repoRoot, worktreePath, branch, base string) error,
) (*WorktreeHandle, error) {
	if opts.DispatchID == "" {
		return nil, fmt.Errorf("SpawnWorktree: DispatchID required")
	}
	if opts.Branch == "" {
		return nil, fmt.Errorf("SpawnWorktree: Branch required")
	}
	if opts.RepoRoot == "" {
		return nil, fmt.Errorf("SpawnWorktree: RepoRoot required")
	}
	if opts.WorktreeRoot == "" {
		return nil, fmt.Errorf("SpawnWorktree: WorktreeRoot required")
	}
	if ledgerWriter == nil {
		return nil, fmt.Errorf("SpawnWorktree: LedgerWriter required")
	}
	if gitAdd == nil {
		return nil, fmt.Errorf("SpawnWorktree: gitAdd required")
	}

	identity := "worktree-" + opts.DispatchID
	wtPath := filepath.Join(opts.WorktreeRoot, identity)
	createdAt := time.Now().UTC()

	// Ledger-first (ADR-091 §5): write the creation record BEFORE invoking
	// `git worktree add`. If this fails, no worktree is created and the
	// caller sees the error.
	data := map[string]interface{}{
		"worktree_id":   identity,
		"dispatch_id":   opts.DispatchID,
		"repo_root":     opts.RepoRoot,
		"worktree_path": wtPath,
		"branch":        opts.Branch,
		"base":          opts.Base,
		"created_at":    createdAt.Format(time.RFC3339),
	}
	if err := ledgerWriter.EmitWorktreeEvent(ctx, BlockWorktreeCreated, data); err != nil {
		return nil, fmt.Errorf("SpawnWorktree: ledger write failed: %w", err)
	}

	// Then perform the git worktree add. Failure leaves the ledger entry in
	// place; the reconciler will observe and alarm on next tick.
	if err := gitAdd(ctx, opts.RepoRoot, wtPath, opts.Branch, opts.Base); err != nil {
		return nil, fmt.Errorf("SpawnWorktree: git worktree add failed: %w", err)
	}

	return &WorktreeHandle{
		Identity:   identity,
		Path:       wtPath,
		Branch:     opts.Branch,
		Base:       opts.Base,
		CreatedAt:  createdAt,
		DispatchID: opts.DispatchID,
	}, nil
}

// SpawnWorktreeDefault is the production-wired SpawnWorktree using
// FilesystemLedgerWriter and the real `git` CLI. workspaceRoot is the
// CogOS workspace whose `.cog/ledger/` is the substrate-of-record.
func SpawnWorktreeDefault(ctx context.Context, workspaceRoot string, opts WorktreeOpts) (*WorktreeHandle, error) {
	writer := NewFilesystemLedgerWriter(workspaceRoot)
	return SpawnWorktree(ctx, opts, writer, realGitAdd)
}

// realGitAdd shells out to `git worktree add -b <branch> <path> <base>`.
// Creates a NEW branch named branch at base. If branch already exists, omit
// the -b flag (use `git worktree add <path> <branch>`). v0 always uses -b;
// callers must supply a unique branch name.
func realGitAdd(ctx context.Context, repoRoot, worktreePath, branch, base string) error {
	args := []string{"worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, worktreePath)
	if base != "" {
		args = append(args, base)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ─── Ledger adapters (filesystem-backed) ──────────────────────────────────────

// FilesystemLedgerReader scans `<workspace>/.cog/ledger/*/events.jsonl` for
// `worktree.*` events. Production implementation.
type FilesystemLedgerReader struct {
	WorkspaceRoot string

	// scanCache memoizes the parsed worktree events of each per-session
	// events.jsonl, keyed by file path. The WorktreeReconciler runs LoadConfig
	// (→ ReadWorktreeEvents) on every ~30s reconcile cycle; without this cache
	// that re-reads and JSON-parses the entire ledger (hundreds of MB / hundreds
	// of thousands of lines across thousands of session dirs) every tick just to
	// extract a handful of worktree events. Ledger files are append-only, so a
	// changed size+mtime reliably signals new content; unchanged files reuse
	// their parsed events. Guarded by mu (LoadConfig can be called concurrently
	// by the reconcile daemon and the autonomic ticker).
	mu        sync.Mutex
	scanCache map[string]worktreeScanEntry
}

// worktreeScanEntry is one cached file scan. repoRoot is part of the validity
// key because scanWorktreeEventsFile filters by it.
type worktreeScanEntry struct {
	repoRoot string
	size     int64
	modTime  time.Time
	events   []WorktreeLedgerEvent
}

func NewFilesystemLedgerReader(workspaceRoot string) *FilesystemLedgerReader {
	return &FilesystemLedgerReader{WorkspaceRoot: workspaceRoot}
}

// ReadWorktreeEvents reads all worktree.* events from the workspace ledger
// and filters by repo_root.
func (r *FilesystemLedgerReader) ReadWorktreeEvents(ctx context.Context, repoRoot string) ([]WorktreeLedgerEvent, error) {
	ledgerDir := filepath.Join(r.WorkspaceRoot, ".cog", "ledger")

	// We don't depend on which session-ID directory the events live in;
	// scan all of them. Tests may bypass this entirely via an in-memory
	// LedgerReader.
	sessionDirs, err := readDirIfExists(ledgerDir)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scanCache == nil {
		r.scanCache = make(map[string]worktreeScanEntry)
	}

	var events []WorktreeLedgerEvent
	seen := make(map[string]struct{}, len(sessionDirs))
	for _, d := range sessionDirs {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		default:
		}
		eventsFile := filepath.Join(ledgerDir, d, "events.jsonl")
		fi, statErr := os.Stat(eventsFile)
		if statErr != nil {
			// Missing/unreadable per-session ledger; skip (and let the eviction
			// pass below drop any stale cache entry for it).
			continue
		}
		seen[eventsFile] = struct{}{}

		// Cache hit: same repoRoot filter and unchanged size+mtime → reuse the
		// previously parsed events instead of re-reading and re-parsing.
		if c, ok := r.scanCache[eventsFile]; ok && c.repoRoot == repoRoot && c.size == fi.Size() && c.modTime.Equal(fi.ModTime()) {
			events = append(events, c.events...)
			continue
		}

		evs, err := scanWorktreeEventsFile(eventsFile, repoRoot)
		if err != nil {
			// Skip unreadable per-session ledgers; do not fail the whole tick.
			// Drop any stale cache entry so a transient error doesn't pin it.
			delete(r.scanCache, eventsFile)
			continue
		}
		r.scanCache[eventsFile] = worktreeScanEntry{
			repoRoot: repoRoot,
			size:     fi.Size(),
			modTime:  fi.ModTime(),
			events:   evs,
		}
		events = append(events, evs...)
	}

	// Evict cache entries for session ledgers that disappeared this tick.
	for k := range r.scanCache {
		if _, ok := seen[k]; !ok {
			delete(r.scanCache, k)
		}
	}

	return events, nil
}

// FilesystemLedgerWriter appends worktree.* events to the workspace ledger.
// Uses AppendEvent so events are hash-chained per existing ledger contract.
type FilesystemLedgerWriter struct {
	WorkspaceRoot string
	SessionID     string // session bucket; defaults to "worktree-reconciler"
}

func NewFilesystemLedgerWriter(workspaceRoot string) *FilesystemLedgerWriter {
	return &FilesystemLedgerWriter{
		WorkspaceRoot: workspaceRoot,
		SessionID:     "worktree-reconciler",
	}
}

// EmitWorktreeEvent appends a `worktree.*` event to the ledger.
func (w *FilesystemLedgerWriter) EmitWorktreeEvent(
	_ context.Context,
	eventType CogBlockKind,
	data map[string]interface{},
) error {
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      string(eventType),
			Timestamp: nowISO(),
			SessionID: w.SessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "worktree-reconciler"},
	}
	return AppendEvent(w.WorkspaceRoot, w.SessionID, env)
}

// ─── ledger file scanning helpers ─────────────────────────────────────────────

func readDirIfExists(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// scanWorktreeEventsFile reads one per-session events.jsonl and projects the
// worktree.* events matching repoRoot. It is a var so the scan cache test can
// wrap it to count actual (cache-miss) parses; production keeps the default.
var scanWorktreeEventsFile = func(path, repoRoot string) ([]WorktreeLedgerEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []WorktreeLedgerEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue // skip malformed rows; do not fail the whole scan
		}
		ev, ok := projectWorktreeEvent(&env, repoRoot)
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// projectWorktreeEvent decodes one EventEnvelope into a typed WorktreeLedgerEvent
// if its Type is one of the worktree.* kinds AND its repo_root matches.
// Returns (event, true) on match, (zero, false) otherwise.
func projectWorktreeEvent(env *EventEnvelope, repoRoot string) (WorktreeLedgerEvent, bool) {
	t := CogBlockKind(env.HashedPayload.Type)
	data := env.HashedPayload.Data

	switch t {
	case BlockWorktreeCreated:
		// Filter by repo_root if specified.
		if rr, _ := data["repo_root"].(string); repoRoot != "" && rr != "" && !isSamePath(rr, repoRoot) {
			return WorktreeLedgerEvent{}, false
		}
		c := &WorktreeCreatedEvent{
			WorktreeID:   asStringWT(data["worktree_id"]),
			DispatchID:   asStringWT(data["dispatch_id"]),
			RepoRoot:     asStringWT(data["repo_root"]),
			WorktreePath: asStringWT(data["worktree_path"]),
			Branch:       asStringWT(data["branch"]),
			Base:         asStringWT(data["base"]),
		}
		if ts, _ := time.Parse(time.RFC3339, asStringWT(data["created_at"])); !ts.IsZero() {
			c.CreatedAt = ts
		}
		return WorktreeLedgerEvent{Created: c}, true

	case BlockWorktreeTerminal:
		t := &WorktreeTerminalEvent{
			WorktreeID: asStringWT(data["worktree_id"]),
			DispatchID: asStringWT(data["dispatch_id"]),
			Reason:     TerminalReason(asStringWT(data["reason"])),
		}
		return WorktreeLedgerEvent{Terminal: t}, true

	case BlockWorktreePruned:
		p := &WorktreePrunedEvent{
			WorktreeID: asStringWT(data["worktree_id"]),
		}
		return WorktreeLedgerEvent{Pruned: p}, true

	case BlockWorktreeAlarm:
		// Filter by repo_root if specified (alarm events carry repo_root).
		if rr, _ := data["repo_root"].(string); repoRoot != "" && rr != "" && !isSamePath(rr, repoRoot) {
			return WorktreeLedgerEvent{}, false
		}
		a := &WorktreeAlarmedEvent{
			WorktreePath: asStringWT(data["worktree_path"]),
		}
		return WorktreeLedgerEvent{Alarmed: a}, true
	}
	return WorktreeLedgerEvent{}, false
}

func asStringWT(v interface{}) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
