// health_convergence_test.go — regression tests for #433.
//
// The conversations reconciler never converged: self-heal fired every ~60s
// forever, replanning ~2374 actions each cycle, with sync pinned at OutOfSync.
//
// Root cause: Health() equated "the last plan contained changes" with
// OutOfSync. The observatory indexes conversation transcripts that are appended
// to while the kernel is running, so on a live node there is nearly always a
// create or update in flight. That made OutOfSync the permanent steady state,
// which (a) fired the autonomic ticker's self-heal on every tick forever, and
// (b) destroyed the signal — a status that is always OutOfSync cannot report
// actual divergence.
//
// The invariant these tests pin: progress is not divergence. Only UNRESOLVED
// work is divergence — a plan with changes that has not been applied, or an
// applied plan whose actions failed.
package conversations

import (
	"context"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// The core #433 case: a plan that contained changes and applied cleanly must
// leave the provider Synced. Before the fix this returned OutOfSync forever,
// which is what made self-heal a metronome.
func TestHealth_AppliedPlanWithChangesIsSynced(t *testing.T) {
	p := NewProvider()

	// A plan was computed and it had real work in it.
	p.mu.Lock()
	p.lastPlanSummary.Creates = 1
	p.lastPlanSummary.Updates = 2
	p.planApplied = false
	p.mu.Unlock()

	// Not yet applied: genuine, momentary divergence.
	if got := p.Health().Sync; got != reconcile.SyncStatusOutOfSync {
		t.Fatalf("pending plan: want OutOfSync, got %s", got)
	}

	// The apply landed cleanly, no failures.
	p.mu.Lock()
	p.planApplied = true
	p.applyFailures = 0
	p.mu.Unlock()

	if got := p.Health().Sync; got != reconcile.SyncStatusSynced {
		t.Fatalf("applied plan with changes: want Synced, got %s — "+
			"this is #433: progress reported as divergence", got)
	}
}

// An applied plan whose actions failed is still divergent: the planned work did
// not land. This is the case self-heal SHOULD fire on.
func TestHealth_FailedApplyStaysOutOfSync(t *testing.T) {
	p := NewProvider()

	p.mu.Lock()
	p.lastPlanSummary.Updates = 2
	p.planApplied = true
	p.applyFailures = 1
	p.mu.Unlock()

	if got := p.Health().Sync; got != reconcile.SyncStatusOutOfSync {
		t.Fatalf("applied plan with failures: want OutOfSync, got %s", got)
	}
}

// Computing a new plan must clear the applied flag, so a fresh round of pending
// work is visible as divergence rather than inheriting the previous cycle's
// "already applied" state.
func TestHealth_ComputePlanClearsAppliedFlag(t *testing.T) {
	p := NewProvider()

	p.mu.Lock()
	p.planApplied = true
	p.mu.Unlock()

	cfg := &providerConfig{Root: t.TempDir()}
	live := &liveState{Entries: map[string]IndexEntry{}}
	if _, err := p.ComputePlan(cfg, live, nil); err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	p.mu.Lock()
	applied := p.planApplied
	p.mu.Unlock()
	if applied {
		t.Fatal("ComputePlan must reset planApplied; a newly computed plan has not been applied")
	}
}

// End-to-end convergence: the same unchanged corpus reconciled twice must not
// report OutOfSync on the second pass. This is the property whose absence made
// the reconciler run forever.
func TestHealth_ConvergesAcrossRepeatedCycles(t *testing.T) {
	p, root := newTestProvider(t)

	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ctx := context.Background()
	for cycle := 1; cycle <= 3; cycle++ {
		live, err := p.FetchLive(ctx, cfgAny)
		if err != nil {
			t.Fatalf("cycle %d FetchLive: %v", cycle, err)
		}
		plan, err := p.ComputePlan(cfgAny, live, nil)
		if err != nil {
			t.Fatalf("cycle %d ComputePlan: %v", cycle, err)
		}
		if _, err := p.ApplyPlan(ctx, plan); err != nil {
			t.Fatalf("cycle %d ApplyPlan: %v", cycle, err)
		}

		if got := p.Health().Sync; got != reconcile.SyncStatusSynced {
			t.Fatalf("cycle %d: want Synced after a clean apply, got %s", cycle, got)
		}
	}
}
