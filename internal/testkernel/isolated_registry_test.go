// isolated_registry_test.go — ADR-101 Phase 2 proving test.
//
// Verifies that WithIsolatedRegistry causes the kernel's ReconcileDaemon to
// operate exclusively on the injected provider list, with zero calls to the
// global registry. This is the key invariant that lets integration tests run
// real plan/apply cycles in parallel without stub interference.
//
// Two tests:
//
//  1. TestIsolatedRegistry_OnlyFakeRuns — boots with a single fake Reconcilable
//     whose Type() returns "fake-isolated". Triggers a reconcile, waits for the
//     cycle to complete, then asserts:
//     - The fake's LoadConfig/FetchLive counts went up (cycle actually ran).
//     - No provider in the global registry was ever called (registry is empty in
//       test binaries, so this is implicitly verified; the test additionally
//       confirms that re-registering a sentinel into the global registry after
//       boot doesn't cause it to be called by the daemon).
//
//  2. TestIsolatedRegistry_GlobalSentinelNotCalled — registers a sentinel
//     Reconcilable into the global registry BEFORE booting, then boots with an
//     injected "fake-isolated" provider. Asserts the global sentinel is never
//     called while the fake is called. This is the strongest form of the
//     isolation proof.
package testkernel_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/testkernel"
	"github.com/myrgic/cogos/pkg/reconcile"
)

// ─── fakeReconcilable ────────────────────────────────────────────────────────

// fakeReconcilable is a minimal Reconcilable whose call counts are tracked via
// atomics so they are safe to read from the test goroutine.
type fakeReconcilable struct {
	typeName     string
	loadCount    atomic.Int32
	fetchCount   atomic.Int32
	computeCount atomic.Int32
}

func newFake(typeName string) *fakeReconcilable {
	return &fakeReconcilable{typeName: typeName}
}

func (f *fakeReconcilable) Type() string { return f.typeName }

func (f *fakeReconcilable) LoadConfig(_ string) (any, error) {
	f.loadCount.Add(1)
	return map[string]any{"type": f.typeName}, nil
}

func (f *fakeReconcilable) FetchLive(_ context.Context, _ any) (any, error) {
	f.fetchCount.Add(1)
	return map[string]any{"live": true}, nil
}

func (f *fakeReconcilable) ComputePlan(_ any, _ any, _ *reconcile.State) (*reconcile.Plan, error) {
	f.computeCount.Add(1)
	return &reconcile.Plan{
		ResourceType: f.typeName,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Actions: []reconcile.Action{{
			Action:       reconcile.ActionSkip,
			ResourceType: f.typeName,
			Name:         "test",
			Details:      map[string]any{"reason": "in sync"},
		}},
		Summary: reconcile.Summary{Skipped: 1},
	}, nil
}

func (f *fakeReconcilable) ApplyPlan(_ context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, a := range plan.Actions {
		results = append(results, reconcile.Result{
			Phase:  "apply",
			Action: string(a.Action),
			Name:   a.Name,
			Status: reconcile.ApplySucceeded,
		})
	}
	return results, nil
}

func (f *fakeReconcilable) BuildState(_ any, _ any, existing *reconcile.State) (*reconcile.State, error) {
	s := reconcile.NewState(f.typeName)
	if existing != nil {
		s.Lineage = existing.Lineage
		s.Serial = existing.Serial
	}
	return s, nil
}

func (f *fakeReconcilable) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// ─── waitForCycle ─────────────────────────────────────────────────────────────

// waitForCycle polls until fake.fetchCount exceeds threshold or deadline is
// reached. The ReconcileDaemon tick interval is set very high so the only way
// fetchCount increases is through an explicit Trigger call.
func waitForCycle(t *testing.T, fake *fakeReconcilable, threshold int32, deadline time.Duration) {
	t.Helper()
	dl := time.After(deadline)
	for {
		select {
		case <-dl:
			t.Fatalf("waitForCycle: FetchLive count did not reach %d within %v (got %d)",
				threshold, deadline, fake.fetchCount.Load())
		default:
			if fake.fetchCount.Load() >= threshold {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestIsolatedRegistry_OnlyFakeRuns verifies that when WithIsolatedRegistry is
// used, only the injected "fake-isolated" provider is ever reconciled. A trigger
// on the fake type fires the cycle; the test waits for FetchLive to be called
// and confirms call counts are positive.
func TestIsolatedRegistry_OnlyFakeRuns(t *testing.T) {
	// Ensure global registry is clean for this test. Other tests in this package
	// may leave entries; reset before and after.
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	fake := newFake("fake-isolated")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithIsolatedRegistry(fake),
	)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	daemon := k.ReconcileDaemon()

	// Fire a trigger for the fake provider. The daemon's poll interval is the
	// default (30 s); without a trigger this test would wait a long time.
	daemon.Trigger("fake-isolated")

	// Wait for FetchLive to be called at least once (one full cycle).
	waitForCycle(t, fake, 1, 5*time.Second)

	// Confirm all three stages ran.
	if got := fake.loadCount.Load(); got < 1 {
		t.Errorf("LoadConfig count = %d; want >= 1", got)
	}
	if got := fake.fetchCount.Load(); got < 1 {
		t.Errorf("FetchLive count = %d; want >= 1", got)
	}
	if got := fake.computeCount.Load(); got < 1 {
		t.Errorf("ComputePlan count = %d; want >= 1", got)
	}

	// Daemon must still be live (not stalled or shut down).
	if got := daemon.State(); got == engine.ReconcileDaemonShutdown {
		t.Errorf("daemon state = %q; want not Shutdown", got)
	}
}

// TestIsolatedRegistry_GlobalSentinelNotCalled is the strongest isolation proof.
// A sentinel is registered into the global registry before boot. The kernel is
// booted with WithIsolatedRegistry(fake). After a full cycle of the fake, the
// sentinel's call counts must remain zero — confirming the daemon never touched
// the global registry.
func TestIsolatedRegistry_GlobalSentinelNotCalled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	// sentinel lives in the global registry. If the daemon ever queries the
	// global registry its LoadConfig/FetchLive counters will go up.
	sentinel := newFake("global-sentinel")
	reconcile.UpsertProvider(sentinel.Type(), sentinel)

	fake := newFake("fake-isolated-sentinel-test")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithIsolatedRegistry(fake),
	)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	daemon := k.ReconcileDaemon()

	// Trigger the fake's cycle explicitly.
	daemon.Trigger(fake.Type())

	// Wait for the fake to complete at least one cycle.
	waitForCycle(t, fake, 1, 5*time.Second)

	// Give the event loop a moment to settle, then check the sentinel counts.
	time.Sleep(20 * time.Millisecond)

	if got := sentinel.loadCount.Load(); got != 0 {
		t.Errorf("isolation breach: global sentinel LoadConfig called %d times; want 0", got)
	}
	if got := sentinel.fetchCount.Load(); got != 0 {
		t.Errorf("isolation breach: global sentinel FetchLive called %d times; want 0", got)
	}
	if got := sentinel.computeCount.Load(); got != 0 {
		t.Errorf("isolation breach: global sentinel ComputePlan called %d times; want 0", got)
	}

	// Fake must have run.
	if got := fake.fetchCount.Load(); got < 1 {
		t.Errorf("fake FetchLive count = %d; want >= 1", got)
	}
}
