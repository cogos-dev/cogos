package cogfield

import (
	"math"
	"testing"
	"time"
)

func TestComputeRelevance(t *testing.T) {
	now := time.Unix(1000000, 0)
	rhoSq := 0.816496580927726

	// No half-life: relevance = strength * sqrt(2/3)
	ps := &PersistedSignal{
		Strength:    1.0,
		DepositedAt: float64(now.Unix()),
		HalfLife:    0,
	}
	got := ComputeRelevance(ps, now)
	if math.Abs(got-rhoSq) > 1e-10 {
		t.Errorf("no half-life: got %f, want %f", got, rhoSq)
	}

	// Fresh signal (age=0): relevance = strength * sqrt(2/3)
	ps = &PersistedSignal{
		Strength:    2.0,
		DepositedAt: float64(now.Unix()),
		HalfLife:    1.0,
	}
	got = ComputeRelevance(ps, now)
	if math.Abs(got-2.0*rhoSq) > 1e-10 {
		t.Errorf("fresh signal: got %f, want %f", got, 2.0*rhoSq)
	}

	// Aged signal should decay
	past := now.Add(-24 * time.Hour)
	ps = &PersistedSignal{
		Strength:    5.0,
		DepositedAt: float64(past.Unix()),
		HalfLife:    1.0, // 1 hour half-life
	}
	got = ComputeRelevance(ps, now)
	if got >= 5.0*rhoSq {
		t.Errorf("aged signal should decay: got %f, max %f", got, 5.0*rhoSq)
	}
	if got <= 0 {
		t.Errorf("aged signal should be positive: got %f", got)
	}
}

func TestSignalIsActive(t *testing.T) {
	now := time.Unix(1000000, 0)

	// Fresh strong signal should be active
	ps := &PersistedSignal{
		Strength:    5.0,
		DepositedAt: float64(now.Unix()),
		HalfLife:    1.0,
	}
	if !SignalIsActive(ps, now) {
		t.Error("fresh strong signal should be active")
	}

	// Very old signal with short half-life should be inactive
	ancient := now.Add(-1000 * time.Hour)
	ps = &PersistedSignal{
		Strength:    1.0,
		DepositedAt: float64(ancient.Unix()),
		HalfLife:    0.1,
	}
	if SignalIsActive(ps, now) {
		t.Error("ancient signal should be inactive")
	}
}
