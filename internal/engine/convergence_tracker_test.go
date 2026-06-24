package engine

import (
	"testing"
	"time"
)

func findConv(snap []ProviderConvergence, name string) (ProviderConvergence, bool) {
	for _, s := range snap {
		if s.Provider == name {
			return s, true
		}
	}
	return ProviderConvergence{}, false
}

func hasReason(s ProviderConvergence, reason string) bool {
	for _, r := range s.Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func TestConvergenceTracker_FlagsOverBudgetAfterThreshold(t *testing.T) {
	tr := newConvergenceTracker(ConvergenceConfig{
		CycleBudget:      100 * time.Millisecond,
		OverBudgetCycles: 3,
	})
	// Two over-budget cycles: not yet flagged (threshold is 3).
	tr.Observe("slow", 500, 480, false)
	tr.Observe("slow", 500, 480, false)
	if s, _ := findConv(tr.Snapshot(), "slow"); s.Flagged {
		t.Fatalf("flagged after 2 cycles, want not flagged: %+v", s)
	}
	// Third over-budget cycle: flagged with the over_budget reason.
	tr.Observe("slow", 500, 480, false)
	s, ok := findConv(tr.Snapshot(), "slow")
	if !ok || !s.Flagged || !hasReason(s, "over_budget") {
		t.Fatalf("want flagged over_budget after 3 cycles, got %+v", s)
	}
	if s.LastFetchMs != 480 {
		t.Errorf("LastFetchMs = %d, want 480 (dominant phase surfaced)", s.LastFetchMs)
	}
	// A cycle back under budget clears the flag.
	tr.Observe("slow", 50, 40, false)
	if s, _ := findConv(tr.Snapshot(), "slow"); s.Flagged {
		t.Errorf("flag should clear when back under budget, got %+v", s)
	}
}

func TestConvergenceTracker_FlagsPersistentDegraded(t *testing.T) {
	tr := newConvergenceTracker(ConvergenceConfig{DegradedCycles: 3})
	for i := 0; i < 3; i++ {
		tr.Observe("component", 5, 0, true) // fast, but degraded health
	}
	s, ok := findConv(tr.Snapshot(), "component")
	if !ok || !s.Flagged || !hasReason(s, "degraded") {
		t.Fatalf("want flagged degraded after 3 cycles, got %+v", s)
	}
	// A healthy cycle clears it.
	tr.Observe("component", 5, 0, false)
	if s, _ := findConv(tr.Snapshot(), "component"); s.Flagged {
		t.Errorf("flag should clear when healthy, got %+v", s)
	}
}

func TestConvergenceTracker_HealthyFastProviderNeverFlags(t *testing.T) {
	tr := newConvergenceTracker(ConvergenceConfig{
		CycleBudget:      100 * time.Millisecond,
		OverBudgetCycles: 3,
		DegradedCycles:   3,
	})
	// An actively-changing provider that stays fast and healthy must never flag,
	// even over many cycles — "has changes" is intentionally not a signal.
	for i := 0; i < 25; i++ {
		tr.Observe("conversations", 40, 20, false)
	}
	if s, _ := findConv(tr.Snapshot(), "conversations"); s.Flagged {
		t.Errorf("fast healthy provider flagged, want never: %+v", s)
	}
}

// TestReconcileDaemon_ConvergenceSurface exercises the public snapshot the way an
// operator/agent (or a CI assertion) would: drive the daemon's tracker with
// numbers matching the real conversations regression (~950ms FetchLive) and
// assert it is surfaced as a flagged over_budget provider.
func TestReconcileDaemon_ConvergenceSurface(t *testing.T) {
	d := NewReconcileDaemon(ReconcileDaemonConfig{
		Convergence: ConvergenceConfig{CycleBudget: 100 * time.Millisecond, OverBudgetCycles: 2},
	})
	d.health.Observe("conversations", 950, 920, false)
	d.health.Observe("conversations", 1000, 960, false)

	s, ok := findConv(d.ProviderConvergence(), "conversations")
	if !ok || !s.Flagged || !hasReason(s, "over_budget") {
		t.Fatalf("daemon should surface the slow provider via ProviderConvergence(), got %+v", d.ProviderConvergence())
	}
}
