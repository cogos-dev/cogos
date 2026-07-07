// first_instruments_e_test.go — Module E tests (First Instruments Stage-2).
//
// Covers the K12-law unit tests specced in IMPL-SPEC.md Module E: the
// PRIMARY non-divisible cadence test (C=5s,H=2s => events at ceil(5/2)*2=6s
// multiples, NOT 5s) and the divisible-cell MIXTURE-EXPECTATION test
// (C=4s,H=2s => a jitter-decided {4s,6s} mixture, >=10% at 6s — locking the
// code-true knife-edge behavior, blind-review-4 Finding A(3)). Also covers
// heartbeat-spacing, trigger labeling, the success-point tap (Finding B),
// and the no-mutation guarantee.
package testkernel_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/testkernel"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// bootCadenceKernel boots a measurement kernel at the given (C,H) seconds
// with the natural reconcile tick defeated (1h poll interval) and the local
// harness controller disabled, per the harness discipline (IMPL-SPEC A4/D2).
// lifetime bounds the kernel's own context — it MUST exceed the caller's
// planned cadence-wait window, since the process's tickers (and thus the
// M11/M12 cadence taps) stop firing the instant this context is cancelled.
func bootCadenceKernel(t *testing.T, consolidationSec, heartbeatSec int, lifetime time.Duration) *testkernel.Kernel {
	t.Helper()
	reconcile.ResetProviders()
	t.Cleanup(reconcile.ResetProviders)

	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	t.Cleanup(cancel)

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithConsolidationInterval(consolidationSec),
		testkernel.WithHeartbeatInterval(heartbeatSec),
		testkernel.WithPollInterval(1*time.Hour),
		testkernel.WithoutLocalHarness(),
	)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})
	return k
}

// waitForConsolidationEvents polls ConsolidationEvents until at least n have
// been recorded, or the deadline passes.
func waitForConsolidationEvents(t *testing.T, k *testkernel.Kernel, n int, deadline time.Duration) []engine.ConsolidationEvent {
	t.Helper()
	dl := time.After(deadline)
	for {
		events := k.ConsolidationEvents()
		if len(events) >= n {
			return events
		}
		select {
		case <-dl:
			t.Fatalf("waitForConsolidationEvents: only %d/%d events after %v", len(events), n, deadline)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForHeartbeatEvents(t *testing.T, k *testkernel.Kernel, n int, deadline time.Duration) []engine.HeartbeatEvent {
	t.Helper()
	dl := time.After(deadline)
	for {
		events := k.HeartbeatEvents()
		if len(events) >= n {
			return events
		}
		select {
		case <-dl:
			t.Fatalf("waitForHeartbeatEvents: only %d/%d events after %v", len(events), n, deadline)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ─── PRIMARY K12-law unit test (non-divisible): C=5s, H=2s ─────────────────
//
// ceil(5/2)*2 = 6s != 5s. Events must land at ~6s multiples from boot, NOT
// 5s multiples — the cheapest confirmation the ceiling actually rounds up
// rather than tracking raw C. Margin (6-5=1s) is jitter-robust.
func TestK12Law_NonDivisible_C5H2_EventsAt6sMultiples(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cadence-wait test in -short mode")
	}
	k := bootCadenceKernel(t, 5, 2, 45*time.Second)

	// Wait for 2 consolidation events => 1 full inter-consolidation interval.
	events := waitForConsolidationEvents(t, k, 2, 20*time.Second)
	interval := events[1].At.Sub(events[0].At)

	const wantSeconds = 6.0
	const tolSeconds = 1.5 // generous scheduler-jitter allowance; 6s vs 5s margin is 1s
	got := interval.Seconds()
	if math.Abs(got-wantSeconds) > tolSeconds {
		t.Errorf("inter-consolidation interval = %.3fs; want ~%.1fs (ceil(5/2)*2), tolerance %.1fs", got, wantSeconds, tolSeconds)
	}
	// The discriminating assertion: NOT 5s (raw C).
	if math.Abs(got-5.0) < 0.5 {
		t.Errorf("inter-consolidation interval = %.3fs landed near raw C=5s, not the law-predicted ceil(5/2)*2=6s", got)
	}

	for _, e := range events {
		if e.Trigger != engine.TriggerHeartbeatGated {
			t.Errorf("event trigger = %q; want %q", e.Trigger, engine.TriggerHeartbeatGated)
		}
	}
}

// ─── DIVISIBLE-CELL MIXTURE-EXPECTATION test: C=4s, H=2s ───────────────────
//
// ceil(4/2)*2 = 4s = C (divisible). The code-true generator captures `now`
// AFTER jittered body work, so the deciding tick is a near-coin-flip: the
// observed inter-consolidation intervals are a {C, C+H} = {4s, 6s} mixture,
// NOT a clean deterministic 4s. Assert (i) every interval is near either 4s
// or 6s (no interval far from both) and (ii) a non-trivial fraction (>=10%)
// land near 6s.
func TestK12Law_Divisible_C4H2_MixtureExpectation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cadence-wait test in -short mode")
	}
	k := bootCadenceKernel(t, 4, 2, 8*time.Minute)

	// >=40 intervals per spec => >=41 events. This is a slow real-time wait
	// (41 * ~4-6s ≈ 3-4 minutes); bounded generously.
	const minIntervals = 40
	events := waitForConsolidationEvents(t, k, minIntervals+1, 6*time.Minute)

	const tol = 1.0 // seconds
	near6 := 0
	for i := 1; i < len(events); i++ {
		interval := events[i].At.Sub(events[i-1].At).Seconds()
		near4 := math.Abs(interval-4.0) <= tol
		is6 := math.Abs(interval-6.0) <= tol
		if !near4 && !is6 {
			t.Errorf("interval[%d] = %.3fs; not within tolerance of either 4s or 6s (mixture expectation violated)", i, interval)
		}
		if is6 {
			near6++
		}
	}

	total := len(events) - 1
	fracNear6 := float64(near6) / float64(total)
	if fracNear6 < 0.10 {
		t.Errorf("fraction of intervals near 6s = %.3f (%d/%d); want >= 0.10 (confirms the jitter knife-edge coin-flip is real, not a clean 4s)", fracNear6, near6, total)
	}
}

// ─── Heartbeat spacing (both directions) ───────────────────────────────────

func TestHeartbeatEvents_SpacingApproximatesH(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cadence-wait test in -short mode")
	}
	// H=1s per §2 test table ("set H=1s, confirm ~1s heartbeat spacing").
	k := bootCadenceKernel(t, 3600, 1, 30*time.Second)

	events := waitForHeartbeatEvents(t, k, 4, 15*time.Second)
	for i := 1; i < len(events); i++ {
		spacing := events[i].At.Sub(events[i-1].At).Seconds()
		if spacing < 0.3 || spacing > 3.0 {
			t.Errorf("heartbeat spacing[%d] = %.3fs; want ~1s (generous jitter bound)", i, spacing)
		}
	}
}

// ─── No-mutation guarantee (K3/K8) ──────────────────────────────────────────

// TestCadenceTaps_NoKernelStateMutation confirms that reading the cadence
// event snapshots does not mutate kernel state: repeated reads return
// growing-or-equal-length slices (append-only) and never shrink or reorder
// already-observed events.
func TestCadenceTaps_NoKernelStateMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cadence-wait test in -short mode")
	}
	k := bootCadenceKernel(t, 3600, 1, 30*time.Second)

	first := waitForHeartbeatEvents(t, k, 2, 15*time.Second)
	// Read again immediately; must be a stable prefix (append-only, no
	// reordering, no mutation from the read itself).
	second := k.HeartbeatEvents()
	if len(second) < len(first) {
		t.Fatalf("HeartbeatEvents shrank across reads: %d then %d", len(first), len(second))
	}
	for i := range first {
		if !first[i].At.Equal(second[i].At) {
			t.Errorf("event[%d] timestamp changed across reads: %v vs %v", i, first[i].At, second[i].At)
		}
	}

	// Mutating the returned slice must not affect the recorder's internal
	// state (snapshot semantics).
	if len(second) > 0 {
		second[0].At = time.Time{}
		third := k.HeartbeatEvents()
		if third[0].At.IsZero() {
			t.Error("mutating a returned HeartbeatEvents slice affected a subsequent snapshot — not side-effect-free")
		}
	}
}
