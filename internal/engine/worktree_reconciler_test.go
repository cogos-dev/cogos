// worktree_reconciler_test.go — acceptance + integration tests for ADR-096.
//
// Test cases (per ADR-096 §Acceptance criteria):
//
//   - TestWorktreeReconciler_SevenLegacyWorktrees_AllAlarmUnknownBinding:
//     all seven current worktrees (5 mod3 orphans + 2 cogos agent-locked)
//     return alarm-unknown-binding under an empty ledger.
//
//   - TestWorktreeReconciler_SpawnPlusCleanTerminal_RemovableClean:
//     SpawnWorktree + worktree.terminal{reason=merged} -> removable-clean,
//     prune runs, worktree removed from disk.
//
//   - TestWorktreeReconciler_SpawnPlusUncommitted_AlarmNoMutation:
//     SpawnWorktree + uncommitted changes + terminal dispatch event ->
//     alarm-uncommitted-on-terminal-dispatch, no filesystem mutation.
//
//   - TestWorktreeReconciler_UnknownBinding_AlarmEmittedNoMutation:
//     worktree created via direct git worktree add (bypassing SpawnWorktree)
//     -> alarm-unknown-binding, alarm event emitted, no filesystem mutation.
//
//   - TestWorktreeReconciler_Idempotency_RemovableCleanTwice:
//     run reconcile twice on the same removable-clean state -> only one
//     worktree.pruned event, no second git worktree remove attempted.

package engine

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── Test adapters ────────────────────────────────────────────────────────────

// fakeLedger is a combined in-memory LedgerReader + LedgerWriter used by tests.
// All emitted events are recorded; ReadWorktreeEvents replays the recorded set.
type fakeLedger struct {
	mu       sync.Mutex
	events   []WorktreeLedgerEvent
	emitLog  []emittedEvent
	repoRoot string
}

type emittedEvent struct {
	Kind CogBlockKind
	Data map[string]interface{}
}

func newFakeLedger(repoRoot string) *fakeLedger {
	return &fakeLedger{repoRoot: repoRoot}
}

func (f *fakeLedger) ReadWorktreeEvents(_ context.Context, _ string) ([]WorktreeLedgerEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Deep-copy slice header; events themselves are not mutated.
	out := make([]WorktreeLedgerEvent, len(f.events))
	copy(out, f.events)
	return out, nil
}

func (f *fakeLedger) EmitWorktreeEvent(_ context.Context, eventType CogBlockKind, data map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitLog = append(f.emitLog, emittedEvent{Kind: eventType, Data: data})
	switch eventType {
	case BlockWorktreeCreated:
		f.events = append(f.events, WorktreeLedgerEvent{Created: &WorktreeCreatedEvent{
			WorktreeID:   asStringWT(data["worktree_id"]),
			DispatchID:   asStringWT(data["dispatch_id"]),
			RepoRoot:     asStringWT(data["repo_root"]),
			WorktreePath: asStringWT(data["worktree_path"]),
			Branch:       asStringWT(data["branch"]),
			Base:         asStringWT(data["base"]),
		}})
	case BlockWorktreeTerminal:
		f.events = append(f.events, WorktreeLedgerEvent{Terminal: &WorktreeTerminalEvent{
			WorktreeID: asStringWT(data["worktree_id"]),
			DispatchID: asStringWT(data["dispatch_id"]),
			Reason:     TerminalReason(asStringWT(data["reason"])),
		}})
	case BlockWorktreePruned:
		f.events = append(f.events, WorktreeLedgerEvent{Pruned: &WorktreePrunedEvent{
			WorktreeID: asStringWT(data["worktree_id"]),
		}})
	}
	return nil
}

func (f *fakeLedger) emitCountByKind(k CogBlockKind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.emitLog {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// fakeGit is an in-memory GitAdapter for hermetic tests.
type fakeGit struct {
	mu               sync.Mutex
	worktrees        map[string]LiveWorktree // keyed by path; absent = "not on disk"
	mainPath         string                  // path of the main worktree (always present in lists)
	removeCalls      []string                // paths passed to RemoveWorktree, in order
	removeShouldFail bool                    // if true, RemoveWorktree returns an error
}

func newFakeGit(mainPath string) *fakeGit {
	return &fakeGit{
		worktrees: make(map[string]LiveWorktree),
		mainPath:  mainPath,
	}
}

func (g *fakeGit) addWorktree(w LiveWorktree) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.worktrees[w.Path] = w
}

func (g *fakeGit) ListWorktrees(_ context.Context, _ string) ([]LiveWorktree, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Main worktree always appears first.
	out := []LiveWorktree{{Path: g.mainPath, Branch: "main"}}
	for _, w := range g.worktrees {
		out = append(out, w)
	}
	return out, nil
}

func (g *fakeGit) RemoveWorktree(_ context.Context, _, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removeCalls = append(g.removeCalls, path)
	if g.removeShouldFail {
		return errFakeGitRemove
	}
	delete(g.worktrees, path)
	return nil
}

var errFakeGitRemove = fakeErr("fake git remove failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// ─── Acceptance: seven legacy worktrees, empty ledger ─────────────────────────

// TestWorktreeReconciler_SevenLegacyWorktrees_AllAlarmUnknownBinding asserts
// that the seven worktrees described in ADR-096 (5 mod3 orphans + 2 cogos
// agent-locked) all classify as alarm-unknown-binding when given an empty
// ledger. This is the operator-attention state observed on 2026-05-17 and the
// canonical acceptance-criterion-table from the ADR.
func TestWorktreeReconciler_SevenLegacyWorktrees_AllAlarmUnknownBinding(t *testing.T) {
	t.Parallel()

	type wt struct {
		path     string
		branch   string
		detached bool
		locked   bool
	}
	// Modeled after ADR-096 §Context: paths from the live mod3 + cogos repos.
	cases := []wt{
		// 5 mod3 orphans.
		{path: "/test/mod3/.claude/worktrees/mod3-modality-rfc", branch: "wave/2026-05-13-mod3/modality-rfc"},
		{path: "/test/mod3/.claude/worktrees/mod3-modality-schemas", branch: "worktree-mod3-modality-schemas"},
		{path: "/test/mod3/.claude/worktrees/mod3-pipecat", branch: "wave/2026-05-13-mod3/pipecat"},
		{path: "/test/mod3/.claude/worktrees/mod3-sidecar-doc", branch: "wave/2026-05-13-mod3/sidecar-doc"},
		{path: "/test/mod3/.claude/worktrees/mod3-worker-cli", branch: "wave/2026-05-13-mod3/worker-cli"},
		// 2 cogos agent-locked.
		{path: "/test/cogos/.claude/worktrees/agent-a5647032c84cb9e61", branch: "worktree-agent-a5647032c84cb9e61", locked: true},
		{path: "/test/cogos/.claude/worktrees/agent-a8c054cc717aa7cb4", detached: true, locked: true},
	}

	repoRoot := "/test/cogos"
	ledger := newFakeLedger(repoRoot)
	git := newFakeGit(repoRoot)
	for _, c := range cases {
		git.addWorktree(LiveWorktree{
			Path:     c.path,
			Branch:   c.branch,
			Detached: c.detached,
			Locked:   c.locked,
		})
	}

	r := NewWorktreeReconciler(repoRoot, ledger, ledger, git)

	ctx := context.Background()
	cfg, err := r.LoadConfig("/test/workspace")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := r.FetchLive(ctx, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := r.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if len(plan.Actions) != len(cases) {
		t.Fatalf("expected %d actions, got %d", len(cases), len(plan.Actions))
	}
	for _, a := range plan.Actions {
		cls, _ := a.Details["classification"].(string)
		if cls != string(ClassAlarmUnknownBinding) {
			t.Errorf("worktree %s: classification=%s want alarm-unknown-binding", a.Name, cls)
		}
		if a.Action != reconcile.ActionUpdate {
			t.Errorf("worktree %s: action=%s want Update (alarm)", a.Name, a.Action)
		}
	}

	// ApplyPlan must NOT remove anything; it should only emit alarm events.
	if _, err := r.ApplyPlan(ctx, plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(git.removeCalls) != 0 {
		t.Fatalf("expected zero RemoveWorktree calls for alarm-unknown-binding, got %d: %v", len(git.removeCalls), git.removeCalls)
	}
	if got, want := ledger.emitCountByKind(BlockWorktreeAlarm), len(cases); got != want {
		t.Errorf("expected %d alarm events emitted, got %d", want, got)
	}
}

// ─── Acceptance: spawn + clean terminal -> removable-clean -> prune ───────────

func TestWorktreeReconciler_SpawnPlusCleanTerminal_RemovableClean(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo-a"
	wtRoot := repoRoot + "/.claude/worktrees"
	dispatchID := "dispatch-abc123"
	wtPath := wtRoot + "/worktree-dispatch-abc123"

	ledger := newFakeLedger(repoRoot)
	git := newFakeGit(repoRoot)

	ctx := context.Background()

	// Spawn the worktree via SpawnWorktree (writes worktree.created BEFORE git add).
	gitAdd := func(_ context.Context, _, path, _, _ string) error {
		git.addWorktree(LiveWorktree{
			Path:   path,
			Branch: "feat/test",
		})
		return nil
	}
	handle, err := SpawnWorktree(ctx, WorktreeOpts{
		DispatchID:   dispatchID,
		Branch:       "feat/test",
		Base:         "main",
		RepoRoot:     repoRoot,
		WorktreeRoot: wtRoot,
	}, ledger, gitAdd)
	if err != nil {
		t.Fatalf("SpawnWorktree: %v", err)
	}
	if handle.Path != wtPath {
		t.Fatalf("handle.Path=%q want %q", handle.Path, wtPath)
	}

	// Emit worktree.terminal{reason=merged}.
	if err := ledger.EmitWorktreeEvent(ctx, BlockWorktreeTerminal, map[string]interface{}{
		"worktree_id": handle.Identity,
		"dispatch_id": dispatchID,
		"reason":      string(TerminalReasonMerged),
	}); err != nil {
		t.Fatalf("emit terminal: %v", err)
	}

	// Reconcile.
	r := NewWorktreeReconciler(repoRoot, ledger, ledger, git)
	cfg, err := r.LoadConfig("/test/workspace")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := r.FetchLive(ctx, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := r.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	got := plan.Actions[0]
	if cls, _ := got.Details["classification"].(string); cls != string(ClassRemovableClean) {
		t.Errorf("classification=%s want removable-clean", cls)
	}
	if got.Action != reconcile.ActionDelete {
		t.Errorf("action=%s want Delete", got.Action)
	}

	// ApplyPlan should call RemoveWorktree and emit worktree.pruned.
	// Pre-create the path on disk so the os.Stat existence check passes.
	mustMkdir(t, wtPath)
	results, err := r.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("expected one succeeded result, got %+v", results)
	}
	if len(git.removeCalls) != 1 || git.removeCalls[0] != wtPath {
		t.Errorf("expected RemoveWorktree(%q), got %v", wtPath, git.removeCalls)
	}
	if n := ledger.emitCountByKind(BlockWorktreePruned); n != 1 {
		t.Errorf("expected 1 worktree.pruned event, got %d", n)
	}
}

// ─── Acceptance: spawn + uncommitted on terminal -> alarm, no mutation ────────

func TestWorktreeReconciler_SpawnPlusUncommitted_AlarmNoMutation(t *testing.T) {
	t.Parallel()

	repoRoot := "/test/repo-b"
	wtRoot := repoRoot + "/.claude/worktrees"
	dispatchID := "dispatch-uncommitted"

	ledger := newFakeLedger(repoRoot)
	git := newFakeGit(repoRoot)
	ctx := context.Background()

	gitAdd := func(_ context.Context, _, path, _, _ string) error {
		git.addWorktree(LiveWorktree{
			Path:                  path,
			Branch:                "feat/uncommitted",
			HasUncommittedChanges: true, // simulate dirty worktree
		})
		return nil
	}
	handle, err := SpawnWorktree(ctx, WorktreeOpts{
		DispatchID:   dispatchID,
		Branch:       "feat/uncommitted",
		Base:         "main",
		RepoRoot:     repoRoot,
		WorktreeRoot: wtRoot,
	}, ledger, gitAdd)
	if err != nil {
		t.Fatalf("SpawnWorktree: %v", err)
	}

	// Terminal dispatch event.
	if err := ledger.EmitWorktreeEvent(ctx, BlockWorktreeTerminal, map[string]interface{}{
		"worktree_id": handle.Identity,
		"dispatch_id": dispatchID,
		"reason":      string(TerminalReasonExited),
	}); err != nil {
		t.Fatalf("emit terminal: %v", err)
	}

	r := NewWorktreeReconciler(repoRoot, ledger, ledger, git)
	cfg, err := r.LoadConfig("/test/workspace")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := r.FetchLive(ctx, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := r.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	got := plan.Actions[0]
	if cls, _ := got.Details["classification"].(string); cls != string(ClassAlarmUncommittedOnTerminalDispatch) {
		t.Errorf("classification=%s want alarm-uncommitted-on-terminal-dispatch", cls)
	}
	if got.Action != reconcile.ActionUpdate {
		t.Errorf("action=%s want Update (alarm)", got.Action)
	}

	if _, err := r.ApplyPlan(ctx, plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(git.removeCalls) != 0 {
		t.Errorf("expected zero RemoveWorktree calls, got %d: %v", len(git.removeCalls), git.removeCalls)
	}
	if n := ledger.emitCountByKind(BlockWorktreeAlarm); n != 1 {
		t.Errorf("expected 1 alarm event, got %d", n)
	}
	if n := ledger.emitCountByKind(BlockWorktreePruned); n != 0 {
		t.Errorf("expected zero pruned events, got %d", n)
	}
}

// ─── Acceptance: unknown-binding -> alarm, no mutation ────────────────────────

func TestWorktreeReconciler_UnknownBinding_AlarmEmittedNoMutation(t *testing.T) {
	t.Parallel()

	repoRoot := "/test/repo-c"
	wtPath := "/test/repo-c/.claude/worktrees/hand-created-by-operator"

	ledger := newFakeLedger(repoRoot)
	git := newFakeGit(repoRoot)
	ctx := context.Background()

	// Worktree created OUTSIDE SpawnWorktree (no ledger entry).
	git.addWorktree(LiveWorktree{
		Path:   wtPath,
		Branch: "operator-branch",
	})

	r := NewWorktreeReconciler(repoRoot, ledger, ledger, git)
	cfg, err := r.LoadConfig("/test/workspace")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := r.FetchLive(ctx, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := r.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	got := plan.Actions[0]
	if cls, _ := got.Details["classification"].(string); cls != string(ClassAlarmUnknownBinding) {
		t.Errorf("classification=%s want alarm-unknown-binding", cls)
	}
	if got.Action != reconcile.ActionUpdate {
		t.Errorf("action=%s want Update (alarm), NEVER Delete", got.Action)
	}

	if _, err := r.ApplyPlan(ctx, plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(git.removeCalls) != 0 {
		t.Fatalf("expected zero RemoveWorktree calls for alarm-unknown-binding (hard rule), got %d: %v",
			len(git.removeCalls), git.removeCalls)
	}
	if n := ledger.emitCountByKind(BlockWorktreeAlarm); n != 1 {
		t.Errorf("expected 1 alarm event, got %d", n)
	}
}

// ─── Acceptance: idempotency on removable-clean ───────────────────────────────

func TestWorktreeReconciler_Idempotency_RemovableCleanTwice(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	repoRoot := tmp + "/repo-d"
	wtRoot := repoRoot + "/.claude/worktrees"
	dispatchID := "dispatch-idempotent"
	wtPath := wtRoot + "/worktree-dispatch-idempotent"

	ledger := newFakeLedger(repoRoot)
	git := newFakeGit(repoRoot)
	ctx := context.Background()

	gitAdd := func(_ context.Context, _, path, _, _ string) error {
		git.addWorktree(LiveWorktree{Path: path, Branch: "feat/idem"})
		return nil
	}
	handle, err := SpawnWorktree(ctx, WorktreeOpts{
		DispatchID:   dispatchID,
		Branch:       "feat/idem",
		Base:         "main",
		RepoRoot:     repoRoot,
		WorktreeRoot: wtRoot,
	}, ledger, gitAdd)
	if err != nil {
		t.Fatalf("SpawnWorktree: %v", err)
	}
	if err := ledger.EmitWorktreeEvent(ctx, BlockWorktreeTerminal, map[string]interface{}{
		"worktree_id": handle.Identity,
		"dispatch_id": dispatchID,
		"reason":      string(TerminalReasonMerged),
	}); err != nil {
		t.Fatalf("emit terminal: %v", err)
	}
	mustMkdir(t, wtPath)

	r := NewWorktreeReconciler(repoRoot, ledger, ledger, git)
	runOnce := func() {
		cfg, err := r.LoadConfig("/test/workspace")
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		live, err := r.FetchLive(ctx, cfg)
		if err != nil {
			t.Fatalf("FetchLive: %v", err)
		}
		plan, err := r.ComputePlan(cfg, live, nil)
		if err != nil {
			t.Fatalf("ComputePlan: %v", err)
		}
		if _, err := r.ApplyPlan(ctx, plan); err != nil {
			t.Fatalf("ApplyPlan: %v", err)
		}
	}

	runOnce()
	// Second tick: ledger now contains worktree.pruned for this ID. The worktree
	// has also been removed from the fakeGit registry, so it should not appear
	// in FetchLive. Either way, NO second prune / second worktree.pruned.
	runOnce()

	if got := len(git.removeCalls); got != 1 {
		t.Errorf("expected exactly 1 RemoveWorktree call across two ticks, got %d", got)
	}
	if got := ledger.emitCountByKind(BlockWorktreePruned); got != 1 {
		t.Errorf("expected exactly 1 worktree.pruned event across two ticks, got %d", got)
	}
}

// ─── Spawn ledger-first ordering ──────────────────────────────────────────────

// TestSpawnWorktree_LedgerFirst asserts that the worktree.created ledger event
// is written BEFORE the git worktree add invocation (ADR-091 §5 ledger-first
// rule, ADR-096 §2 precondition).
func TestSpawnWorktree_LedgerFirst(t *testing.T) {
	t.Parallel()

	repoRoot := "/test/ledger-first"
	wtRoot := repoRoot + "/.claude/worktrees"

	var order []string
	var mu sync.Mutex
	record := func(s string) { mu.Lock(); defer mu.Unlock(); order = append(order, s) }

	ledger := &recordingLedger{onEmit: func(_ context.Context, k CogBlockKind, _ map[string]interface{}) error {
		record("ledger:" + string(k))
		return nil
	}}
	gitAdd := func(_ context.Context, _, _, _, _ string) error {
		record("git:add")
		return nil
	}

	_, err := SpawnWorktree(context.Background(), WorktreeOpts{
		DispatchID:   "dispatch-order",
		Branch:       "feat/order",
		Base:         "main",
		RepoRoot:     repoRoot,
		WorktreeRoot: wtRoot,
	}, ledger, gitAdd)
	if err != nil {
		t.Fatalf("SpawnWorktree: %v", err)
	}

	if len(order) != 2 || order[0] != "ledger:worktree.created" || order[1] != "git:add" {
		t.Errorf("expected order [ledger:worktree.created, git:add], got %v", order)
	}
}

// TestSpawnWorktree_LedgerWriteFailure_NoGitInvocation asserts that if the
// ledger write fails, the underlying git worktree add is NEVER called
// (ADR-091 §5 and ADR-096 §2 precondition).
func TestSpawnWorktree_LedgerWriteFailure_NoGitInvocation(t *testing.T) {
	t.Parallel()

	called := false
	ledger := &recordingLedger{onEmit: func(_ context.Context, _ CogBlockKind, _ map[string]interface{}) error {
		return fakeErr("simulated ledger outage")
	}}
	gitAdd := func(_ context.Context, _, _, _, _ string) error {
		called = true
		return nil
	}

	_, err := SpawnWorktree(context.Background(), WorktreeOpts{
		DispatchID:   "dispatch-x",
		Branch:       "feat/x",
		Base:         "main",
		RepoRoot:     "/test/repo",
		WorktreeRoot: "/test/repo/.claude/worktrees",
	}, ledger, gitAdd)
	if err == nil {
		t.Fatal("expected error when ledger write fails, got nil")
	}
	if called {
		t.Errorf("git worktree add was called despite ledger write failure (violates ADR-091 §5)")
	}
}

// recordingLedger implements LedgerWriter for tests that only care about
// emit ordering / failure injection.
type recordingLedger struct {
	onEmit func(context.Context, CogBlockKind, map[string]interface{}) error
}

func (r *recordingLedger) EmitWorktreeEvent(ctx context.Context, k CogBlockKind, d map[string]interface{}) error {
	if r.onEmit != nil {
		return r.onEmit(ctx, k, d)
	}
	return nil
}

// ─── Parser ───────────────────────────────────────────────────────────────────

func TestParseWorktreeListPorcelain(t *testing.T) {
	t.Parallel()
	in := "worktree /repo\nHEAD abcd1234\nbranch refs/heads/main\n\n" +
		"worktree /repo/.claude/worktrees/agent-abc\nHEAD ef567890\nbranch refs/heads/feat/agent-abc\nlocked\n\n" +
		"worktree /repo/.claude/worktrees/detached-xyz\nHEAD 0011223344\ndetached\nlocked\n\n"
	got := parseWorktreeListPorcelain(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 worktrees, got %d", len(got))
	}
	if got[0].Branch != "main" {
		t.Errorf("got[0].Branch=%q want main", got[0].Branch)
	}
	if got[1].Branch != "feat/agent-abc" || !got[1].Locked {
		t.Errorf("got[1]=%+v", got[1])
	}
	if !got[2].Detached || !got[2].Locked {
		t.Errorf("got[2]=%+v", got[2])
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
