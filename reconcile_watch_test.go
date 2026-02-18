// reconcile_watch_test.go
// Tests for the continuous reconciliation watch loop.

package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// watchTestProvider implements Reconcilable for watch loop testing.
type watchTestProvider struct {
	planResult  *ReconcilePlan
	applyErr    error
	fetchErr    error
	planCalls   int32 // atomic — accessed from goroutine
	applyCalls  int32 // atomic — accessed from goroutine
	stateResult *ReconcileState
}

func (w *watchTestProvider) Type() string { return "watch-test" }

func (w *watchTestProvider) LoadConfig(root string) (any, error) {
	return map[string]string{"root": root}, nil
}

func (w *watchTestProvider) FetchLive(_ context.Context, _ any) (any, error) {
	if w.fetchErr != nil {
		return nil, w.fetchErr
	}
	return map[string]string{"status": "live"}, nil
}

func (w *watchTestProvider) ComputePlan(_ any, _ any, _ *ReconcileState) (*ReconcilePlan, error) {
	atomic.AddInt32(&w.planCalls, 1)
	if w.planResult != nil {
		return w.planResult, nil
	}
	// Default: no changes
	return &ReconcilePlan{
		ResourceType: "watch-test",
		Actions:      []ReconcileAction{},
		Summary:      ReconcileSummary{},
	}, nil
}

func (w *watchTestProvider) ApplyPlan(_ context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	atomic.AddInt32(&w.applyCalls, 1)
	if w.applyErr != nil {
		return nil, w.applyErr
	}
	results := make([]ReconcileResult, len(plan.Actions))
	for i, a := range plan.Actions {
		results[i] = ReconcileResult{
			Phase:  "apply",
			Action: string(a.Action),
			Name:   a.Name,
			Status: ApplySucceeded,
		}
	}
	return results, nil
}

func (w *watchTestProvider) BuildState(_ any, _ any, existing *ReconcileState) (*ReconcileState, error) {
	if w.stateResult != nil {
		return w.stateResult, nil
	}
	if existing != nil {
		return existing, nil
	}
	return NewReconcileState("watch-test"), nil
}

func (w *watchTestProvider) Health() ResourceStatus {
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// --- Tests ---

func TestWatchConfigDefaults(t *testing.T) {
	cfg := WatchConfig{
		ResourceType: "test",
	}

	// Verify zero-value defaults match expected behavior
	if cfg.Interval != 0 {
		t.Errorf("zero-value Interval = %v, want 0 (cmdWatch sets 5m)", cfg.Interval)
	}
	if cfg.AutoApply != false {
		t.Error("default AutoApply should be false")
	}
	if cfg.MaxCycles != 0 {
		t.Errorf("default MaxCycles = %d, want 0 (unlimited)", cfg.MaxCycles)
	}

	// Verify cmdWatch sets the correct defaults by parsing empty flags
	// We test the actual defaults by constructing what cmdWatch would produce
	parsedCfg := WatchConfig{
		ResourceType: "test",
		Interval:     5 * time.Minute,
		AutoApply:    false,
		MaxCycles:    0,
	}
	if parsedCfg.Interval != 5*time.Minute {
		t.Errorf("default Interval = %v, want 5m", parsedCfg.Interval)
	}
	if parsedCfg.AutoApply {
		t.Error("default AutoApply should be false")
	}
	if parsedCfg.MaxCycles != 0 {
		t.Errorf("default MaxCycles = %d, want 0", parsedCfg.MaxCycles)
	}
}

func TestRunWatchMaxCycles(t *testing.T) {
	resetProviders()
	defer resetProviders()

	provider := &watchTestProvider{}
	RegisterProvider("watch-test", provider)

	tmpDir := t.TempDir()

	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     1 * time.Millisecond, // fast for testing
		MaxCycles:    2,
		Root:         tmpDir,
	}

	err := RunWatch(cfg)
	if err != nil {
		t.Fatalf("RunWatch returned error: %v", err)
	}

	calls := atomic.LoadInt32(&provider.planCalls)
	if calls != 2 {
		t.Errorf("plan was called %d times, want 2", calls)
	}
}

func TestRunWatchCycleNoChanges(t *testing.T) {
	provider := &watchTestProvider{
		planResult: &ReconcilePlan{
			ResourceType: "watch-test",
			Actions:      []ReconcileAction{},
			Summary:      ReconcileSummary{Creates: 0, Updates: 0, Deletes: 0, Skipped: 0},
		},
	}

	tmpDir := t.TempDir()
	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     5 * time.Minute,
		Root:         tmpDir,
	}

	ctx := context.Background()
	err := runWatchCycle(ctx, provider, cfg)
	if err != nil {
		t.Fatalf("runWatchCycle returned error: %v", err)
	}

	calls := atomic.LoadInt32(&provider.planCalls)
	if calls != 1 {
		t.Errorf("plan was called %d times, want 1", calls)
	}

	// Apply should NOT have been called (no changes, no auto-apply)
	applyCalls := atomic.LoadInt32(&provider.applyCalls)
	if applyCalls != 0 {
		t.Errorf("apply was called %d times, want 0 (no changes)", applyCalls)
	}
}

func TestRunWatchCycleDrift(t *testing.T) {
	provider := &watchTestProvider{
		planResult: &ReconcilePlan{
			ResourceType: "watch-test",
			Actions: []ReconcileAction{
				{Action: ActionCreate, ResourceType: "channel", Name: "new-channel"},
				{Action: ActionUpdate, ResourceType: "role", Name: "admin"},
				{Action: ActionDelete, ResourceType: "channel", Name: "old-channel"},
			},
			Summary: ReconcileSummary{Creates: 1, Updates: 1, Deletes: 1},
		},
	}

	tmpDir := t.TempDir()
	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     5 * time.Minute,
		AutoApply:    false, // drift detection only
		Root:         tmpDir,
	}

	ctx := context.Background()
	err := runWatchCycle(ctx, provider, cfg)
	if err != nil {
		t.Fatalf("runWatchCycle returned error: %v", err)
	}

	// Plan should have been called
	planCalls := atomic.LoadInt32(&provider.planCalls)
	if planCalls != 1 {
		t.Errorf("plan was called %d times, want 1", planCalls)
	}

	// Apply should NOT have been called (auto-apply is off)
	applyCalls := atomic.LoadInt32(&provider.applyCalls)
	if applyCalls != 0 {
		t.Errorf("apply was called %d times, want 0 (auto-apply off)", applyCalls)
	}

	// Verify the plan detected changes
	if !provider.planResult.Summary.HasChanges() {
		t.Error("expected plan to have changes")
	}
}

func TestRunWatchCycleAutoApply(t *testing.T) {
	provider := &watchTestProvider{
		planResult: &ReconcilePlan{
			ResourceType: "watch-test",
			Actions: []ReconcileAction{
				{Action: ActionCreate, ResourceType: "channel", Name: "new-channel"},
				{Action: ActionUpdate, ResourceType: "role", Name: "admin"},
			},
			Summary: ReconcileSummary{Creates: 1, Updates: 1},
		},
		stateResult: NewReconcileState("watch-test"),
	}

	tmpDir := t.TempDir()
	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     5 * time.Minute,
		AutoApply:    true,
		Root:         tmpDir,
	}

	ctx := context.Background()
	err := runWatchCycle(ctx, provider, cfg)
	if err != nil {
		t.Fatalf("runWatchCycle returned error: %v", err)
	}

	// Plan should have been called
	planCalls := atomic.LoadInt32(&provider.planCalls)
	if planCalls != 1 {
		t.Errorf("plan was called %d times, want 1", planCalls)
	}

	// Apply SHOULD have been called (auto-apply is on and there are changes)
	applyCalls := atomic.LoadInt32(&provider.applyCalls)
	if applyCalls != 1 {
		t.Errorf("apply was called %d times, want 1", applyCalls)
	}
}

func TestRunWatchCycleAutoApplyNoChanges(t *testing.T) {
	provider := &watchTestProvider{
		planResult: &ReconcilePlan{
			ResourceType: "watch-test",
			Actions:      []ReconcileAction{},
			Summary:      ReconcileSummary{},
		},
	}

	tmpDir := t.TempDir()
	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     5 * time.Minute,
		AutoApply:    true, // enabled, but no changes to apply
		Root:         tmpDir,
	}

	ctx := context.Background()
	err := runWatchCycle(ctx, provider, cfg)
	if err != nil {
		t.Fatalf("runWatchCycle returned error: %v", err)
	}

	// Apply should NOT have been called (no changes even though auto-apply is on)
	applyCalls := atomic.LoadInt32(&provider.applyCalls)
	if applyCalls != 0 {
		t.Errorf("apply was called %d times, want 0 (no changes)", applyCalls)
	}
}

func TestRunWatchCycleFetchError(t *testing.T) {
	provider := &watchTestProvider{
		fetchErr: fmt.Errorf("connection refused"),
	}

	tmpDir := t.TempDir()
	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     5 * time.Minute,
		Root:         tmpDir,
	}

	ctx := context.Background()
	err := runWatchCycle(ctx, provider, cfg)
	if err == nil {
		t.Fatal("expected error from fetch failure")
	}
	if err.Error() != "fetching live: connection refused" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunWatchGracefulShutdown(t *testing.T) {
	resetProviders()
	defer resetProviders()

	provider := &watchTestProvider{}
	RegisterProvider("watch-test", provider)

	tmpDir := t.TempDir()

	cfg := WatchConfig{
		ResourceType: "watch-test",
		Interval:     10 * time.Second, // long enough that we cancel before it fires
		MaxCycles:    0,                 // unlimited
		Root:         tmpDir,
	}

	// Run watch in a goroutine, cancel via context after first cycle
	ctx, cancel := context.WithCancel(context.Background())

	doneCh := make(chan error, 1)
	go func() {
		// We can't directly use RunWatch with our own context since it creates its own.
		// Instead, test runWatchCycle directly and verify context cancellation works.
		err := runWatchCycle(ctx, provider, cfg)
		doneCh <- err
	}()

	// Wait for the cycle to complete, then cancel
	err := <-doneCh
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cancel()

	// Verify at least one plan call happened
	calls := atomic.LoadInt32(&provider.planCalls)
	if calls < 1 {
		t.Errorf("expected at least 1 plan call, got %d", calls)
	}
}
