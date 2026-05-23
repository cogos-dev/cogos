package cogfield

import (
	"math"
	"time"
)

// SignalFieldState mirrors the persisted format from sdk/signals.go.
type SignalFieldState struct {
	Signals map[string]map[string]*PersistedSignal `json:"signals"` // location -> type -> signal
	SavedAt float64                                `json:"saved_at"`
}

// PersistedSignal mirrors the kernel's signal format from sdk/signals.go.
type PersistedSignal struct {
	SignalType  string         `json:"signal_type"`
	Strength    float64        `json:"strength"`
	DepositedBy string         `json:"deposited_by"`
	DepositedAt float64        `json:"deposited_at"`
	HalfLife    float64        `json:"half_life"`
	DecayType   string         `json:"decay_type"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ComputeRelevance computes the current signal relevance using the SRC decay formula.
// relevance = strength * exp(-age/tau2) * sqrt(2/3)
func ComputeRelevance(ps *PersistedSignal, now time.Time) float64 {
	tau2 := 1.3862943611198906  // 2*ln(2)
	rhoSq := 0.816496580927726 // sqrt(2/3)

	depositedAt := time.Unix(int64(ps.DepositedAt), 0)
	ageHours := now.Sub(depositedAt).Hours()
	if ps.HalfLife <= 0 {
		return ps.Strength * rhoSq
	}
	ageTurns := ageHours / ps.HalfLife
	return ps.Strength * math.Exp(-ageTurns/tau2) * rhoSq
}

// SignalIsActive returns true if the signal relevance exceeds the activity threshold.
// Threshold is e^(-1) * sqrt(2/3) ~ 0.30.
func SignalIsActive(ps *PersistedSignal, now time.Time) bool {
	threshold := math.Exp(-1) * 0.816496580927726
	return ComputeRelevance(ps, now) > threshold
}
