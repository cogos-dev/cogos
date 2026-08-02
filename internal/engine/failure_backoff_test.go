package engine

import (
	"fmt"
	"testing"
)

// TestFailureBackoff_SkipWindowDoublesAndCaps is the unambiguous "backoff
// grows" assertion: jitter off, so the oracle has no randomness in it.
func TestFailureBackoff_SkipWindowDoublesAndCaps(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0, 1, 2)

	want := []int{1, 2, 4, 8, 16, 32, 32, 32}
	for i, wantSkip := range want {
		fails, skip := b.RecordFailure("p", 0)
		if fails != i+1 {
			t.Fatalf("failure %d: fails = %d, want %d", i+1, fails, i+1)
		}
		if skip != wantSkip {
			t.Fatalf("failure %d: skip = %d, want %d", i+1, skip, wantSkip)
		}
	}
}

// TestFailureBackoff_GrowthSurvivesJitter asserts the property that actually
// matters: jitter perturbs the window but never flattens the curve.
func TestFailureBackoff_GrowthSurvivesJitter(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0.25, 1, 2)

	var skips []int
	for n := 1; n <= 8; n++ {
		_, skip := b.RecordFailure("p", 0)
		skips = append(skips, skip)

		nominal := b.nominalSkip(n)
		lo, hi := int(0.75*float64(nominal)), int(1.25*float64(nominal))+1
		if lo < 1 {
			lo = 1
		}
		if hi > 32 {
			hi = 32
		}
		if skip < lo || skip > hi {
			t.Fatalf("failure %d: skip = %d, want within [%d,%d] of nominal %d",
				n, skip, lo, hi, nominal)
		}
	}

	if skips[5] <= skips[1] {
		t.Fatalf("skip did not grow through jitter: n=6 gave %d, n=2 gave %d", skips[5], skips[1])
	}
}

// TestFailureBackoff_JitterDecorrelatesProviders is the only test that fails if
// jitter is dropped, which is precisely what jitter is for: providers that
// begin failing on the same tick (a shared upstream outage) must not retry in
// permanent lockstep and re-herd onto the resource that just failed.
func TestFailureBackoff_JitterDecorrelatesProviders(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0.25, 7, 11)

	// Drive 20 providers to the same failure depth on the same tick.
	const providers = 20
	for n := 0; n < 5; n++ {
		for i := 0; i < providers; i++ {
			b.RecordFailure(fmt.Sprintf("p%02d", i), 0)
		}
	}

	seen := map[int]bool{}
	b.mu.Lock()
	for _, retryAt := range b.retryAt {
		seen[retryAt] = true
	}
	b.mu.Unlock()

	if len(seen) < 2 {
		t.Fatalf("all %d providers scheduled to retry on the same tick (%v) — jitter is not decorrelating them",
			providers, seen)
	}
}

func TestFailureBackoff_ReadyGatesUntilRetryTick(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0, 1, 2)

	if !b.Ready("fresh", 0) {
		t.Fatal("a key that has never failed must be ready")
	}

	// Third failure at tick 100 ⇒ skip 4 ⇒ next eligible tick is 104.
	b.RecordFailure("p", 100)
	b.RecordFailure("p", 100)
	_, skip := b.RecordFailure("p", 100)
	if skip != 4 {
		t.Fatalf("skip = %d, want 4", skip)
	}
	for tick := 100; tick < 104; tick++ {
		if b.Ready("p", tick) {
			t.Fatalf("ready at tick %d, want gated until 104", tick)
		}
	}
	if !b.Ready("p", 104) {
		t.Fatal("not ready at tick 104, want ready on the boundary tick")
	}
}

// TestFailureBackoff_SuccessResetsToImmediate covers the reset requirement: a
// recovered provider must return to the full tick cadence, not stay throttled
// at whatever depth it had reached.
func TestFailureBackoff_SuccessResetsToImmediate(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0, 1, 2)

	for i := 0; i < 6; i++ {
		b.RecordFailure("p", 0)
	}
	if b.Ready("p", 1) {
		t.Fatal("deep-backoff key ready one tick later, want gated")
	}
	if got := b.Failures("p"); got != 6 {
		t.Fatalf("Failures = %d, want 6", got)
	}

	n, recovered := b.RecordSuccess("p")
	if !recovered || n != 6 {
		t.Fatalf("RecordSuccess = (%d, %v), want (6, true)", n, recovered)
	}
	if !b.Ready("p", 1) {
		t.Fatal("key not ready after success, want immediately ready")
	}
	if got := b.Failures("p"); got != 0 {
		t.Fatalf("Failures = %d after success, want 0", got)
	}

	// The next failure starts the curve over at skip 1, not where it left off.
	fails, skip := b.RecordFailure("p", 0)
	if fails != 1 || skip != 1 {
		t.Fatalf("post-recovery failure = (fails %d, skip %d), want (1, 1)", fails, skip)
	}

	// A success on a never-failing key reports no recovery, so callers do not
	// log a spurious "recovered" line every healthy tick.
	if _, recovered := b.RecordSuccess("never-failed"); recovered {
		t.Fatal("RecordSuccess on a healthy key reported recovery")
	}
}

// TestFailureBackoff_HoldWidensWithoutCountingAFailure covers the quarantine
// path: the daemon still observes the provider, but at the widened cadence,
// and holding must not inflate the failure streak.
func TestFailureBackoff_HoldWidensWithoutCountingAFailure(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0, 1, 2)

	b.RecordFailure("p", 0)
	before := b.Failures("p")

	skip := b.Hold("p", 10)
	if skip != 32 {
		t.Fatalf("Hold skip = %d, want the max window 32", skip)
	}
	if got := b.Failures("p"); got != before {
		t.Fatalf("Failures = %d after Hold, want unchanged at %d", got, before)
	}
	if b.Ready("p", 41) {
		t.Fatal("ready at tick 41, want gated until 42")
	}
	if !b.Ready("p", 42) {
		t.Fatal("not ready at tick 42, want ready")
	}
}

func TestFailureBackoff_ForgetDropsState(t *testing.T) {
	b := newFailureBackoffSeeded(32, 0, 1, 2)
	for i := 0; i < 4; i++ {
		b.RecordFailure("gone", 0)
	}
	b.Forget("gone")

	if got := b.Failures("gone"); got != 0 {
		t.Fatalf("Failures = %d after Forget, want 0", got)
	}
	if !b.Ready("gone", 0) {
		t.Fatal("key still gated after Forget")
	}
	b.mu.Lock()
	n := len(b.fails) + len(b.retryAt)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("backoff retained %d map entries after Forget, want 0", n)
	}
}
