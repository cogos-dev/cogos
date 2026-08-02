// reconcile_daemon_backoff_test.go — retry backoff, quarantine, and the
// cycle-summary log throttle.
//
// The fixture deliberately reproduces the production shape that motivated this
// work rather than a generic failing provider:
//
//   - ApplyPlan returns a Result{Status: ApplyFailed} with a NIL top-level
//     error (the provider treats a failed action as non-fatal and continues),
//     so the only place the failure surfaces is the per-action loop and the
//     cycle summary.
//   - Health() reports Suspended, NOT Degraded. This matters: the daemon's
//     convergence tracker only counts Degraded on its health axis, so this
//     provider contributes nothing to the anomaly counter through that route.
//     A test that used a Degraded fixture would pass while production stayed
//     dark, which is the exact failure mode the anti-hiding test below exists
//     to catch.
package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

type flakyReconcilable struct {
	typeName string

	mu            sync.Mutex
	inSync        bool
	failCount     int // number of ApplyFailed results ApplyPlan returns
	configVersion string
	health        reconcile.HealthStatus

	applyCount   atomic.Int32
	computeCount atomic.Int32
	fetchCount   atomic.Int32
}

func newFlakyReconcilable(name string) *flakyReconcilable {
	return &flakyReconcilable{
		typeName:      name,
		failCount:     1,
		configVersion: "v1",
		health:        reconcile.HealthSuspended,
	}
}

func (r *flakyReconcilable) Type() string { return r.typeName }

func (r *flakyReconcilable) LoadConfig(_ string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{"version": r.configVersion}, nil
}

func (r *flakyReconcilable) FetchLive(_ context.Context, _ any) (any, error) {
	r.fetchCount.Add(1)
	return map[string]any{"live": true}, nil
}

func (r *flakyReconcilable) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	r.computeCount.Add(1)
	r.mu.Lock()
	inSync := r.inSync
	r.mu.Unlock()

	plan := &reconcile.Plan{
		ResourceType: r.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if inSync {
		plan.Actions = []reconcile.Action{{
			Action: reconcile.ActionSkip, ResourceType: r.typeName, Name: "target",
		}}
		plan.Summary.Skipped = 1
		return plan, nil
	}
	plan.Actions = []reconcile.Action{{
		Action: reconcile.ActionUpdate, ResourceType: r.typeName, Name: "target",
	}}
	plan.Summary.Updates = 1
	return plan, nil
}

// ApplyPlan mirrors LMSModelStateProvider: a failed action is surfaced as an
// ApplyFailed result and the call returns (results, nil).
func (r *flakyReconcilable) ApplyPlan(_ context.Context, _ *reconcile.Plan) ([]reconcile.Result, error) {
	r.applyCount.Add(1)
	r.mu.Lock()
	n := r.failCount
	r.mu.Unlock()

	var results []reconcile.Result
	for i := 0; i < n; i++ {
		results = append(results, reconcile.Result{
			Phase:  "apply",
			Action: string(reconcile.ActionUpdate),
			Name:   "target",
			Status: reconcile.ApplyFailed,
			Error:  "SDK actuator not installed",
		})
	}
	return results, nil
}

func (r *flakyReconcilable) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return reconcile.NewState(r.typeName), nil
}

func (r *flakyReconcilable) Health() reconcile.ResourceStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusOutOfSync,
		Health:    r.health,
		Operation: reconcile.OperationIdle,
	}
}

func (r *flakyReconcilable) setInSync(v bool) {
	r.mu.Lock()
	r.inSync = v
	r.mu.Unlock()
}

func (r *flakyReconcilable) setFailCount(n int) {
	r.mu.Lock()
	r.failCount = n
	r.mu.Unlock()
}

func (r *flakyReconcilable) setConfigVersion(v string) {
	r.mu.Lock()
	r.configVersion = v
	r.mu.Unlock()
}

// newBackoffTestDaemon builds a daemon over an injected provider list with
// deterministic (jitter-free) backoff.
func newBackoffTestDaemon(t *testing.T, p reconcile.Reconcilable, quarantineAfter int) *ReconcileDaemon {
	t.Helper()
	return NewReconcileDaemon(ReconcileDaemonConfig{
		WorkspaceRoot:   t.TempDir(),
		PollInterval:    time.Hour, // never auto-ticks; tests drive runTick directly
		MaxConcurrent:   1,
		Providers:       []reconcile.Reconcilable{p},
		RetryJitter:     -1, // disable jitter for exact skip windows
		QuarantineAfter: quarantineAfter,
		Convergence:     ConvergenceConfig{DegradedCycles: 3, ReWarnEvery: 20},
	})
}

// ─── D2: the cycle-summary throttle ───────────────────────────────────────────

// TestReconcileDaemon_CycleCompleteWarnsOnceForIdenticalFailure is the
// log-volume fix. Today's code emits 30 WARNs for 30 identical failing cycles;
// in production that was 1,185 lines in one day for a single provider, 90.6%
// of all daemon WARNs.
func TestReconcileDaemon_CycleCompleteWarnsOnceForIdenticalFailure(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("chronic")
	d := newBackoffTestDaemon(t, p, -1) // quarantine off: isolate the log behaviour

	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_ = d.runOneCycle(ctx, p.Type())
	}

	lines := linesWithMsg(read(), "reconcile-daemon: cycle complete")
	if len(lines) != 30 {
		t.Fatalf("cycle-complete lines = %d, want 30 (the line itself must not disappear)", len(lines))
	}

	warns, debugs := 0, 0
	for _, l := range lines {
		switch l.Level {
		case "WARN":
			warns++
		case "DEBUG":
			debugs++
		}
	}
	if warns != 1 {
		t.Errorf("cycle-complete WARNs = %d, want exactly 1", warns)
	}
	if debugs != 29 {
		t.Errorf("cycle-complete DEBUGs = %d, want 29", debugs)
	}
}

// TestReconcileDaemon_CycleCompleteReWarnsWhenOutcomeShapeChanges proves the
// throttle reports CHANGE, not merely first occurrence — a louder failure must
// not be swallowed because a quieter one was already reported.
func TestReconcileDaemon_CycleCompleteReWarnsWhenOutcomeShapeChanges(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("shifting")
	d := newBackoffTestDaemon(t, p, -1)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = d.runOneCycle(ctx, p.Type())
	}
	p.setFailCount(2) // same provider, worse outcome
	for i := 0; i < 5; i++ {
		_ = d.runOneCycle(ctx, p.Type())
	}

	warns := 0
	for _, l := range linesWithMsg(read(), "reconcile-daemon: cycle complete") {
		if l.Level == "WARN" {
			warns++
		}
	}
	if warns != 2 {
		t.Fatalf("cycle-complete WARNs = %d, want 2 (one per distinct outcome shape)", warns)
	}
}

// TestReconcileDaemon_CycleThrottleClearsOnRecovery covers the early-return
// path. A provider that fails, recovers, then fails again with the identical
// outcome must WARN again — the recovery has to reset the throttle. The
// recovered provider exits runOneCycle through the "provider in sync" early
// return, which is a different exit from the one that records the fingerprint.
func TestReconcileDaemon_CycleThrottleClearsOnRecovery(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("recovering")
	d := newBackoffTestDaemon(t, p, -1)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = d.runOneCycle(ctx, p.Type())
	}
	p.setInSync(true)
	_ = d.runOneCycle(ctx, p.Type()) // in-sync early return
	p.setInSync(false)
	for i := 0; i < 3; i++ {
		_ = d.runOneCycle(ctx, p.Type())
	}

	warns := 0
	for _, l := range linesWithMsg(read(), "reconcile-daemon: cycle complete") {
		if l.Level == "WARN" {
			warns++
		}
	}
	if warns != 2 {
		t.Fatalf("cycle-complete WARNs = %d, want 2 — a recurrence after recovery is fresh news", warns)
	}
}

// ─── D3: backoff and the terminal state ───────────────────────────────────────

// TestReconcileDaemon_BacksOffRetriesForAFailingProvider asserts the retry
// count actually falls: with the skip window doubling, 40 ticks produce far
// fewer than 40 attempts.
func TestReconcileDaemon_BacksOffRetriesForAFailingProvider(t *testing.T) {
	p := newFlakyReconcilable("backing-off")
	d := newBackoffTestDaemon(t, p, -1) // quarantine off: measure backoff alone

	ctx := context.Background()
	for i := 0; i < 40; i++ {
		d.runTick(ctx)
	}

	attempts := int(p.applyCount.Load())
	if attempts >= 40 {
		t.Fatalf("apply attempts = %d over 40 ticks, want strictly fewer (no backoff applied)", attempts)
	}
	if attempts > 10 {
		t.Errorf("apply attempts = %d over 40 ticks, want <= 10 with exponential backoff", attempts)
	}
	if attempts == 0 {
		t.Fatal("apply attempts = 0, want the provider still retried occasionally")
	}
}

func TestReconcileDaemon_QuarantinesAfterNConsecutiveFailures(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("quarantine-me")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d.runTick(ctx)
	}

	quarantineLines := linesWithMsg(read(), "reconcile-daemon: provider quarantined, actuation stopped")
	if len(quarantineLines) != 1 {
		t.Fatalf("quarantine WARNs = %d, want exactly 1", len(quarantineLines))
	}
	if l := quarantineLines[0]; l.Level != "WARN" {
		t.Errorf("quarantine line level = %s, want WARN", l.Level)
	}
	recovery, _ := quarantineLines[0].Attrs["recovery"].(string)
	if recovery == "" {
		t.Error("quarantine line carries no recovery hint")
	}
	if !strings.Contains(recovery, p.Type()) {
		t.Errorf("recovery hint %q does not name the provider", recovery)
	}

	if !d.isQuarantined(p.Type()) {
		t.Fatal("provider not quarantined after repeated failures")
	}

	// Actuation must have stopped.
	frozen := p.applyCount.Load()
	for i := 0; i < 20; i++ {
		d.runTick(ctx)
	}
	if got := p.applyCount.Load(); got != frozen {
		t.Errorf("apply attempts rose from %d to %d after quarantine, want frozen", frozen, got)
	}
}

// TestReconcileDaemon_QuarantineDoesNotAlterHealthOrClearTheEpisode is the
// anti-hiding test: the change must remove noise, not signal.
func TestReconcileDaemon_QuarantineDoesNotAlterHealthOrClearTheEpisode(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("still-broken")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 80; i++ {
		d.runTick(ctx)
	}

	if !d.isQuarantined(p.Type()) {
		t.Fatal("precondition failed: provider not quarantined")
	}

	// 1. Health is untouched. Quarantine changes retry cadence and log volume
	//    only; the kernel must keep failing green.
	if h := p.Health().Health; h != reconcile.HealthSuspended {
		t.Errorf("Health = %s, want Suspended (quarantine must not launder health)", h)
	}

	// 2. The anomaly episode is OPEN and names the provider in the pull surface.
	s, ok := findConv(d.ProviderConvergence(), p.Type())
	if !ok {
		t.Fatalf("provider absent from ProviderConvergence(), got %+v", d.ProviderConvergence())
	}
	if !s.Flagged {
		t.Errorf("convergence snapshot not flagged: %+v", s)
	}
	if !s.Quarantined {
		t.Errorf("convergence snapshot does not report quarantine: %+v", s)
	}
	if !hasReason(s, "quarantined") {
		t.Errorf("reasons = %v, want to include quarantined", s.Reasons)
	}
	if s.Episode != 1 {
		t.Errorf("Episode = %d, want exactly 1 for one continuous condition", s.Episode)
	}
	if s.QuarantinedSince == "" || s.Recovery == "" {
		t.Errorf("snapshot missing QuarantinedSince/Recovery: %+v", s)
	}

	// 3. Nothing was silently marked resolved.
	lines := read()
	if got := countMsg(lines, msgAnomalyCleared); got != 0 {
		t.Errorf("anomaly-cleared lines = %d while still broken, want 0", got)
	}
	if got := countMsg(lines, msgAnomalyRaised); got != 1 {
		t.Errorf("anomaly-raised lines = %d, want exactly 1", got)
	}

	// 4. The daemon is still LOOKING: drift detection continues while
	//    quarantined, which is what keeps the episode clearable.
	before := p.computeCount.Load()
	for i := 0; i < 80; i++ {
		d.runTick(ctx)
	}
	if got := p.computeCount.Load(); got <= before {
		t.Errorf("ComputePlan calls did not advance (%d → %d): a quarantined provider must still be observed",
			before, got)
	}
}

// TestReconcileDaemon_QuarantineLiftsWhenConditionResolvesOnItsOwn covers the
// case a pure terminal state would strand: nobody intervenes, but the world
// changes and the provider comes back into sync. Because quarantine stops
// actuation rather than observation, the daemon notices.
func TestReconcileDaemon_QuarantineLiftsWhenConditionResolvesOnItsOwn(t *testing.T) {
	read := captureLogs(t)
	p := newFlakyReconcilable("self-healing")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d.runTick(ctx)
	}
	if !d.isQuarantined(p.Type()) {
		t.Fatal("precondition failed: provider not quarantined")
	}

	// The world fixes itself (the model gets loaded by hand).
	p.setInSync(true)
	p.mu.Lock()
	p.health = reconcile.HealthHealthy
	p.mu.Unlock()

	for i := 0; i < 80; i++ {
		d.runTick(ctx)
	}

	if d.isQuarantined(p.Type()) {
		t.Fatal("provider still quarantined after the condition resolved")
	}
	if got := countMsg(read(), msgAnomalyCleared); got == 0 {
		t.Error("no anomaly-cleared line: a resolved condition must close its episode, " +
			"or the counter pins forever on something that is no longer true")
	}
	if s, _ := findConv(d.ProviderConvergence(), p.Type()); s.Flagged || s.Quarantined {
		t.Errorf("snapshot still flagged/quarantined after recovery: %+v", s)
	}
}

// TestReconcileDaemon_QuarantineLiftsWhenConfigChanges is what makes the
// terminal state safe. The dominant chronic-failure shape here is "not
// configured yet"; without this, the operator would fix the config and nothing
// would ever retry, which is strictly worse than the unbounded-retry behaviour
// being replaced.
func TestReconcileDaemon_QuarantineLiftsWhenConfigChanges(t *testing.T) {
	p := newFlakyReconcilable("misconfigured")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d.runTick(ctx)
	}
	if !d.isQuarantined(p.Type()) {
		t.Fatal("precondition failed: provider not quarantined")
	}
	frozen := p.applyCount.Load()

	// Operator edits the config that was failing.
	p.setConfigVersion("v2")
	p.setFailCount(0)
	p.setInSync(false)

	for i := 0; i < 40; i++ {
		d.runTick(ctx)
	}

	if d.isQuarantined(p.Type()) {
		t.Fatal("quarantine survived a config change — the operator's fix would never take effect")
	}
	if got := p.applyCount.Load(); got <= frozen {
		t.Errorf("apply attempts %d → %d: actuation did not resume after the config changed", frozen, got)
	}
}

// TestReconcileDaemon_ResumeClearsQuarantineAndBackoff documents the operator
// recovery path as executable behaviour rather than only a log string.
func TestReconcileDaemon_ResumeClearsQuarantineAndBackoff(t *testing.T) {
	p := newFlakyReconcilable("resume-me")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d.runTick(ctx)
	}
	if !d.isQuarantined(p.Type()) {
		t.Fatal("precondition failed: provider not quarantined")
	}

	d.Resume(p.Type())

	if d.isQuarantined(p.Type()) {
		t.Fatal("Resume did not lift quarantine")
	}
	if got := d.backoff.Failures(p.Type()); got != 0 {
		t.Errorf("failure streak = %d after Resume, want 0", got)
	}
	if !d.backoff.Ready(p.Type(), int(d.tickSeq.Load())) {
		t.Error("provider still inside a skip window after Resume")
	}

	frozen := p.applyCount.Load()
	d.runTick(ctx)
	if got := p.applyCount.Load(); got <= frozen {
		t.Errorf("apply attempts %d → %d: Resume did not restore actuation", frozen, got)
	}
}

// TestReconcileDaemon_TriggerDoesNotDefeatBackoff covers the watcher path.
// Trigger is wired to fsnotify projection watchers that fire on file events the
// reconcilers themselves can cause, so if a machine trigger reset the backoff
// or lifted quarantine, every watcher would become a backoff-defeat lever and
// the busy-loop would return.
func TestReconcileDaemon_TriggerDoesNotDefeatBackoff(t *testing.T) {
	p := newFlakyReconcilable("watched")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 60; i++ {
		d.runTick(ctx)
	}
	if !d.isQuarantined(p.Type()) {
		t.Fatal("precondition failed: provider not quarantined")
	}
	frozen := p.applyCount.Load()

	// A burst of watcher events, as a corpus write would produce.
	for i := 0; i < 50; i++ {
		d.Trigger(p.Type())
		d.runTriggered(ctx)
	}

	if !d.isQuarantined(p.Type()) {
		t.Fatal("a machine Trigger lifted quarantine — only operator Resume may do that")
	}
	if got := d.backoff.Failures(p.Type()); got == 0 {
		t.Error("a machine Trigger reset the failure streak")
	}
	if got := p.applyCount.Load(); got != frozen {
		t.Errorf("apply attempts rose from %d to %d under watcher triggers, want frozen", frozen, got)
	}
}

// TestReconcileDaemon_HealthyProviderNeverBacksOff is the regression guard: the
// happy path must be completely untouched by any of this.
func TestReconcileDaemon_HealthyProviderNeverBacksOff(t *testing.T) {
	read := captureLogs(t)
	p := &noopReconcilable{typeName: "healthy", hasChanges: false}
	d := NewReconcileDaemon(ReconcileDaemonConfig{
		WorkspaceRoot: t.TempDir(),
		PollInterval:  time.Hour,
		MaxConcurrent: 1,
		Providers:     []reconcile.Reconcilable{p},
	})

	ctx := context.Background()
	const ticks = 25
	for i := 0; i < ticks; i++ {
		d.runTick(ctx)
	}

	if got := int(p.fetchCount.Load()); got != ticks {
		t.Fatalf("FetchLive calls = %d over %d ticks, want every tick (backoff must not gate a healthy provider)",
			got, ticks)
	}
	if d.isQuarantined(p.Type()) {
		t.Error("healthy provider quarantined")
	}
	if got := d.backoff.Failures(p.Type()); got != 0 {
		t.Errorf("healthy provider failure streak = %d, want 0", got)
	}

	lines := read()
	if got := countMsg(lines, msgAnomalyRaised); got != 0 {
		t.Errorf("healthy provider raised %d anomalies, want 0", got)
	}
	if got := countMsg(lines, "reconcile-daemon: provider quarantined, actuation stopped"); got != 0 {
		t.Errorf("healthy provider produced %d quarantine lines, want 0", got)
	}
}
