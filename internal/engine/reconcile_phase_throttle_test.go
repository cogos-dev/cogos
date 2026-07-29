// reconcile_phase_throttle_test.go — regression tests for issue #494's
// "unrelated observation" that a chronically-failing provider (e.g. discord
// with no bot token configured) logs the identical
// `reconcile-daemon: FetchLive failed provider=discord err="discord: bot
// token not set"` line at Warn on every single reconcile tick forever,
// contributing to ~/.cog/var/logs/serve.log growing unbounded with no new
// information in each repeat.
//
// warnPhaseFailureThrottled (reconcile_daemon.go) fixes this generically for
// every cycle-aborting phase (LoadConfig, FetchLive, acquire state lock,
// ComputePlan, ApplyPlan): the first failure for a given (providerType,
// phase) — or any failure whose error text differs from the last one seen —
// still logs at Warn; an exact repeat logs at Debug (suppressed at the
// daemon's default log level, see log_capture.go's upgradeLoggerWithFileSink).
package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// syncBuffer wraps bytes.Buffer with a mutex so it can safely serve as an
// slog.Handler sink written from a daemon's background goroutine while the
// test goroutine concurrently reads it. A plain bytes.Buffer here would
// (correctly) trip -race: the daemon's run() goroutine logs a final
// "reconcile-daemon: stopped" line from inside its shutdown defer AFTER
// flipping State() to Shutdown, so polling State() alone does not establish
// a happens-before edge against that trailing write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestWarnPhaseFailureThrottled_LogLevels drives warnPhaseFailureThrottled
// directly (white-box, same package) and asserts the level-selection logic:
// first occurrence and any text change log at Warn; an exact repeat logs at
// Debug; clearPhaseFailureThrottle resets the streak; different
// (providerType, phase) keys are independent of each other.
func TestWarnPhaseFailureThrottled_LogLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	d := &ReconcileDaemon{lastPhaseErr: make(map[string]string)}

	err1 := errors.New("simulated fetch failure")
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines for 3 calls, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("first occurrence should log at WARN, got: %s", lines[0])
	}
	for i, line := range lines[1:] {
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("repeat occurrence #%d (identical error text) should log at DEBUG (throttled), got: %s", i+2, line)
		}
		if strings.Contains(line, "level=WARN") {
			t.Errorf("repeat occurrence #%d must NOT log at WARN — this is exactly the spam issue #494 reports", i+2)
		}
	}

	// A DIFFERENT error text for the same (providerType, phase) key is new
	// information and must re-WARN.
	buf.Reset()
	err2 := errors.New("a different, new failure")
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err2)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a changed error message must re-WARN, got: %s", buf.String())
	}

	// clearPhaseFailureThrottle (called after a phase succeeds) resets the
	// streak: a later failure with the SAME text as err2 must WARN again,
	// not be treated as a continuation of the old streak.
	d.clearPhaseFailureThrottle("throttle-test", "FetchLive")
	buf.Reset()
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err2)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("after clearPhaseFailureThrottle, a repeat of the previously-seen text must WARN again (fresh recurrence), got: %s", buf.String())
	}

	// A different providerType is an independent key.
	buf.Reset()
	d.warnPhaseFailureThrottled("other-provider", "FetchLive", err1)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a different providerType must WARN independently of throttle-test's streak, got: %s", buf.String())
	}

	// A different phase for the SAME providerType is also independent.
	buf.Reset()
	d.warnPhaseFailureThrottled("throttle-test", "ComputePlan", err1)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a different phase for the same providerType must WARN independently, got: %s", buf.String())
	}
}

// TestReconcileDaemon_ChronicFetchFailureIsThrottled is the end-to-end
// regression test: a provider whose FetchLive fails identically on every
// tick (mirroring discord with no bot token) must produce exactly one Warn
// "FetchLive failed" log line across many real daemon ticks, not one per
// tick.
func TestReconcileDaemon_ChronicFetchFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bad := &errorReconcilable{typeName: "test-chronic-fetch-failure"}
	reconcile.UpsertProvider(bad.Type(), bad)

	daemon := newTestDaemon(t.TempDir(), 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()

	// Wait for the daemon's goroutine to fully exit (State() only transitions
	// to Shutdown once the run loop has returned — see the doc comment on
	// ReconcileDaemonShutdown) before reading its tick counters below.
	deadline := time.After(2 * time.Second)
	for daemon.State() != ReconcileDaemonShutdown {
		select {
		case <-deadline:
			t.Fatal("daemon did not reach Shutdown state in time")
		case <-time.After(5 * time.Millisecond):
		}
	}

	ticks := bad.callCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks (LoadConfig calls), got %d — increase the test window", ticks)
	}

	warnCount, debugCount := countLevelledLines(buf.String(), "reconcile-daemon: FetchLive failed")
	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'FetchLive failed' lines across %d ticks, want exactly 1 (first occurrence only) — issue #494's log-noise regression", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'FetchLive failed' lines, want at least %d (every tick after the first, throttled)", debugCount, ticks-1)
	}
}

// countLevelledLines counts how many lines in text contain marker at WARN
// vs DEBUG level (slog's TextHandler format: "... level=WARN ... msg=\"...\"
// ..."). Shared by every chronic-failure end-to-end test below so the
// warn-once-then-debug assertion is written once.
func countLevelledLines(text, marker string) (warnCount, debugCount int) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		switch {
		case strings.Contains(line, "level=WARN"):
			warnCount++
		case strings.Contains(line, "level=DEBUG"):
			debugCount++
		}
	}
	return warnCount, debugCount
}

// waitForShutdown polls daemon.State() until it reaches Shutdown (the run()
// goroutine has fully returned — see the doc comment on
// ReconcileDaemonShutdown) or fails the test after 2s. Shared by every
// chronic-failure end-to-end test below; see syncBuffer's doc comment for
// why this matters (a fixed sleep here previously raced under -race).
func waitForShutdown(t *testing.T, daemon *ReconcileDaemon) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for daemon.State() != ReconcileDaemonShutdown {
		select {
		case <-deadline:
			t.Fatal("daemon did not reach Shutdown state in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ─── cog-review PR #496 first-pass follow-up: LoadState/BuildState/WriteState ──

// loadCountingNoop is a noopReconcilable-shaped fake that never reports
// changes (so ApplyPlan/BuildState/WriteState are never reached, and a
// pre-seeded corrupt state file is never overwritten/fixed) but tracks how
// many times LoadConfig ran, so tests can confirm several ticks elapsed.
type loadCountingNoop struct {
	typeName  string
	loadCount atomic.Int32
}

func (r *loadCountingNoop) Type() string { return r.typeName }
func (r *loadCountingNoop) LoadConfig(_ string) (any, error) {
	r.loadCount.Add(1)
	return map[string]any{}, nil
}
func (r *loadCountingNoop) FetchLive(_ context.Context, _ any) (any, error) {
	return map[string]any{}, nil
}
func (r *loadCountingNoop) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return &reconcile.Plan{
		ResourceType: r.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions: []reconcile.Action{{
			Action: reconcile.ActionSkip, ResourceType: r.typeName, Name: "test-resource",
			Details: map[string]any{"reason": "in sync"},
		}},
		Summary: reconcile.Summary{Skipped: 1},
	}, nil
}
func (r *loadCountingNoop) ApplyPlan(_ context.Context, _ *reconcile.Plan) ([]reconcile.Result, error) {
	return nil, nil
}
func (r *loadCountingNoop) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return nil, nil
}
func (r *loadCountingNoop) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// TestReconcileDaemon_ChronicLoadStateFailureIsThrottled is the cog-review
// (PR #496 first pass) follow-up for the LoadState sibling site: a
// persistently corrupt/unreadable state file fails reconcile.LoadState with
// the same error on every tick. Must produce exactly one Warn
// "LoadState failed" line across many ticks, not one per tick.
func TestReconcileDaemon_ChronicLoadStateFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	root := t.TempDir()
	provider := &loadCountingNoop{typeName: "test-chronic-loadstate-failure"}
	reconcile.UpsertProvider(provider.Type(), provider)

	// Pre-corrupt the state file so LoadState fails identically on every
	// tick (json.Unmarshal error). This provider's ComputePlan always
	// reports "no changes" (ActionSkip), so ApplyPlan/BuildState/WriteState
	// never run and never fix the file.
	statePath := reconcile.StatePath(root, provider.Type())
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt state file: %v", err)
	}

	daemon := newTestDaemon(root, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()
	waitForShutdown(t, daemon)

	ticks := provider.loadCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks, got %d — increase the test window", ticks)
	}

	warnCount, debugCount := countLevelledLines(buf.String(), "reconcile-daemon: LoadState failed")
	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'LoadState failed' lines across %d ticks, want exactly 1", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'LoadState failed' lines, want at least %d", debugCount, ticks-1)
	}
}

// buildStateErrorReconcilable always has a change to apply (so the daemon
// reaches BuildState every tick) but BuildState always fails identically —
// exercises warnPhaseFailureThrottled's wiring at the BuildState call site
// (cog-review, PR #496 first pass).
type buildStateErrorReconcilable struct {
	typeName  string
	loadCount atomic.Int32
}

func (r *buildStateErrorReconcilable) Type() string { return r.typeName }
func (r *buildStateErrorReconcilable) LoadConfig(_ string) (any, error) {
	r.loadCount.Add(1)
	return map[string]any{}, nil
}
func (r *buildStateErrorReconcilable) FetchLive(_ context.Context, _ any) (any, error) {
	return map[string]any{}, nil
}
func (r *buildStateErrorReconcilable) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return &reconcile.Plan{
		ResourceType: r.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions: []reconcile.Action{{
			Action: reconcile.ActionCreate, ResourceType: r.typeName, Name: "test-resource",
			Details: map[string]any{},
		}},
		Summary: reconcile.Summary{Creates: 1},
	}, nil
}
func (r *buildStateErrorReconcilable) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{Phase: "apply", Action: string(a.Action), Name: a.Name, Status: reconcile.ApplySucceeded})
	}
	return results, nil
}
func (r *buildStateErrorReconcilable) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return nil, errors.New("simulated build-state failure")
}
func (r *buildStateErrorReconcilable) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// TestReconcileDaemon_ChronicBuildStateFailureIsThrottled is the cog-review
// (PR #496 first pass) follow-up for the BuildState sibling site.
func TestReconcileDaemon_ChronicBuildStateFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bad := &buildStateErrorReconcilable{typeName: "test-chronic-buildstate-failure"}
	reconcile.UpsertProvider(bad.Type(), bad)

	daemon := newTestDaemon(t.TempDir(), 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()
	waitForShutdown(t, daemon)

	ticks := bad.loadCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks, got %d — increase the test window", ticks)
	}

	warnCount, debugCount := countLevelledLines(buf.String(), "reconcile-daemon: BuildState failed")
	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'BuildState failed' lines across %d ticks, want exactly 1", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'BuildState failed' lines, want at least %d", debugCount, ticks-1)
	}
}

// writeStateOKBuildReconcilable is buildStateErrorReconcilable's mirror:
// BuildState always SUCCEEDS (returning a valid, non-nil *reconcile.State),
// so the daemon always reaches WriteState — which the test makes fail
// identically every tick by blocking its target directory with a file (see
// TestReconcileDaemon_ChronicWriteStateFailureIsThrottled). Exercises
// warnPhaseFailureThrottled's wiring at the WriteState call site (cog-review,
// PR #496 first pass).
type writeStateOKBuildReconcilable struct {
	typeName  string
	loadCount atomic.Int32
}

func (r *writeStateOKBuildReconcilable) Type() string { return r.typeName }
func (r *writeStateOKBuildReconcilable) LoadConfig(_ string) (any, error) {
	r.loadCount.Add(1)
	return map[string]any{}, nil
}
func (r *writeStateOKBuildReconcilable) FetchLive(_ context.Context, _ any) (any, error) {
	return map[string]any{}, nil
}
func (r *writeStateOKBuildReconcilable) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return &reconcile.Plan{
		ResourceType: r.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions: []reconcile.Action{{
			Action: reconcile.ActionCreate, ResourceType: r.typeName, Name: "test-resource",
			Details: map[string]any{},
		}},
		Summary: reconcile.Summary{Creates: 1},
	}, nil
}
func (r *writeStateOKBuildReconcilable) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{Phase: "apply", Action: string(a.Action), Name: a.Name, Status: reconcile.ApplySucceeded})
	}
	return results, nil
}
func (r *writeStateOKBuildReconcilable) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return reconcile.NewState(r.typeName), nil
}
func (r *writeStateOKBuildReconcilable) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthHealthy)
}

// TestReconcileDaemon_ChronicWriteStateFailureIsThrottled is the cog-review
// (PR #496 first pass) follow-up for the WriteState sibling site: the
// state directory is pre-blocked by a regular file at the exact path
// WriteState needs to os.MkdirAll, so every WriteState call fails
// identically ("not a directory").
func TestReconcileDaemon_ChronicWriteStateFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	root := t.TempDir()
	bad := &writeStateOKBuildReconcilable{typeName: "test-chronic-writestate-failure"}
	reconcile.UpsertProvider(bad.Type(), bad)

	// reconcile.StatePath(root, type) = root/.cog/config/<type>/.state.json.
	// AcquireStateLock (which runs BEFORE LoadState in every cycle) also
	// needs root/.cog/config/<type>/ to exist for its sibling .lock file, so
	// blocking THAT directory would make every cycle fail at the
	// acquire-state-lock phase instead of ever reaching WriteState. Instead,
	// make the parent directory a normal directory (lock acquisition
	// succeeds) but make .state.json ITSELF a directory rather than a file:
	// LoadState's os.ReadFile then fails (a separate, independently-
	// throttled "LoadState failed" — expected and harmless here, since
	// LoadState failures don't abort the cycle), and WriteState's final
	// os.Rename(tmp, sp) fails every time because you cannot rename a file
	// onto an existing directory ("file exists" on macOS/Linux).
	statePath := reconcile.StatePath(root, bad.Type())
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatalf("mkdir state-path-as-directory: %v", err)
	}

	daemon := newTestDaemon(root, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()
	waitForShutdown(t, daemon)

	ticks := bad.loadCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks, got %d — increase the test window", ticks)
	}

	warnCount, debugCount := countLevelledLines(buf.String(), "reconcile-daemon: WriteState failed")
	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'WriteState failed' lines across %d ticks, want exactly 1", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'WriteState failed' lines, want at least %d", debugCount, ticks-1)
	}
}

// ─── cog-review PR #496 second-pass follow-up: per-action ApplyFailed ─────────

// TestWarnActionFailureThrottled_LogLevels drives warnActionFailureThrottled/
// clearActionFailureThrottle directly, mirroring
// TestWarnPhaseFailureThrottled_LogLevels: first occurrence and any text
// change WARN, exact repeats DEBUG, clearing resets the streak, and a
// different action or name is an independent key even for the same
// providerType (the whole point of keying finer than the phase-level
// helper).
func TestWarnActionFailureThrottled_LogLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	d := &ReconcileDaemon{lastPhaseErr: make(map[string]string)}

	d.warnActionFailureThrottled("site", "create", "app-a", "strategy lookup: unsupported deploy strategy \"\"")
	d.warnActionFailureThrottled("site", "create", "app-a", "strategy lookup: unsupported deploy strategy \"\"")
	d.warnActionFailureThrottled("site", "create", "app-a", "strategy lookup: unsupported deploy strategy \"\"")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines for 3 calls, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("first occurrence should log at WARN, got: %s", lines[0])
	}
	for i, line := range lines[1:] {
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("repeat occurrence #%d should log at DEBUG (throttled), got: %s", i+2, line)
		}
	}

	// A different action's failure on the SAME provider is an independent
	// key and must WARN, even while app-a's streak is still active.
	buf.Reset()
	d.warnActionFailureThrottled("site", "create", "app-b", "strategy lookup: unsupported deploy strategy \"\"")
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a different action name must WARN independently of app-a's streak, got: %s", buf.String())
	}

	// clearActionFailureThrottle resets app-a's streak: a later repeat of
	// the same text must WARN again.
	d.clearActionFailureThrottle("site", "create", "app-a")
	buf.Reset()
	d.warnActionFailureThrottled("site", "create", "app-a", "strategy lookup: unsupported deploy strategy \"\"")
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("after clearActionFailureThrottle, a repeat must WARN again (fresh recurrence), got: %s", buf.String())
	}
}

// actionFailingReconcilable always has one action to apply, and that
// action's ApplyPlan result is ApplyFailed with a fixed error text every
// cycle (ApplyPlan itself returns a nil top-level error, matching
// site.applyAction's shape: a bad strategy fails one action, not the whole
// ApplyPlan call) — exercises warnActionFailureThrottled's wiring into
// runOneCycle's per-action results loop (cog-review, PR #496 second pass).
type actionFailingReconcilable struct {
	typeName  string
	loadCount atomic.Int32
}

func (r *actionFailingReconcilable) Type() string { return r.typeName }
func (r *actionFailingReconcilable) LoadConfig(_ string) (any, error) {
	r.loadCount.Add(1)
	return map[string]any{}, nil
}
func (r *actionFailingReconcilable) FetchLive(_ context.Context, _ any) (any, error) {
	return map[string]any{}, nil
}
func (r *actionFailingReconcilable) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	return &reconcile.Plan{
		ResourceType: r.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions: []reconcile.Action{{
			Action: reconcile.ActionCreate, ResourceType: r.typeName, Name: "test-resource",
			Details: map[string]any{},
		}},
		Summary: reconcile.Summary{Creates: 1},
	}, nil
}
func (r *actionFailingReconcilable) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{
			Phase: "apply", Action: string(a.Action), Name: a.Name,
			Status: reconcile.ApplyFailed, Error: "simulated action failure",
		})
	}
	return results, nil
}
func (r *actionFailingReconcilable) BuildState(_ any, _ any, _ *reconcile.State) (*reconcile.State, error) {
	return reconcile.NewState(r.typeName), nil
}
func (r *actionFailingReconcilable) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthDegraded)
}

// TestReconcileDaemon_ChronicActionFailureIsThrottled is the end-to-end
// regression test: a provider whose ApplyPlan succeeds overall but always
// reports the SAME action as ApplyFailed (mirroring a site CRD with an
// invalid strategy — ApplyPlan itself returns no top-level error, so the
// per-action results loop is the only place this failure is ever logged)
// must produce exactly one Warn "action failed" line across many ticks.
func TestReconcileDaemon_ChronicActionFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bad := &actionFailingReconcilable{typeName: "test-chronic-action-failure"}
	reconcile.UpsertProvider(bad.Type(), bad)

	daemon := newTestDaemon(t.TempDir(), 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()
	waitForShutdown(t, daemon)

	ticks := bad.loadCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks, got %d — increase the test window", ticks)
	}

	warnCount, debugCount := countLevelledLines(buf.String(), "reconcile-daemon: action failed")
	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'action failed' lines across %d ticks, want exactly 1", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'action failed' lines, want at least %d", debugCount, ticks-1)
	}
}
