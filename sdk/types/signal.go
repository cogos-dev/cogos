package types

import (
	"math"
	"time"
)

// Signal represents a single signal in the stigmergic field.
type Signal struct {
	// Location is where the signal is deposited (e.g., "inference", "mem/semantic").
	Location string `json:"location"`

	// Type is the signal classification (e.g., "ACTIVE", "PENDING", "ERROR").
	Type string `json:"type"`

	// DepositedAt is when the signal was created.
	DepositedAt time.Time `json:"deposited_at"`

	// HalfLife is the characteristic decay time in hours.
	HalfLife float64 `json:"half_life"`

	// Strength is the initial signal strength [0, 1].
	Strength float64 `json:"strength"`

	// Metadata contains signal-specific data.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SRCConstantsForDecay contains the constants needed for decay calculation.
// This avoids a circular import with the main sdk package.
var SRCConstantsForDecay = struct {
	Tau2                 float64
	CorrelationThreshold float64
}{
	Tau2:                 1.3862943611198906, // 2*ln(2)
	CorrelationThreshold: 0.816496580927726,  // sqrt(2/3)
}

// Relevance computes the current relevance of the signal.
// Uses the SRC decay formula: relevance = strength * exp(-age/τ₂) * √(2/3)
func (s *Signal) Relevance(now time.Time) float64 {
	if s.DepositedAt.IsZero() {
		return SRCConstantsForDecay.CorrelationThreshold * 0.5 // Unknown age
	}

	ageHours := now.Sub(s.DepositedAt).Hours()
	ageTurns := ageHours / s.HalfLife

	// Apply exponential decay with correlation threshold ceiling
	return s.Strength * math.Exp(-ageTurns/SRCConstantsForDecay.Tau2) * SRCConstantsForDecay.CorrelationThreshold
}

// IsActive returns true if the signal relevance is above threshold.
// Threshold is e^(-1) * √(2/3) ≈ 0.30.
func (s *Signal) IsActive(now time.Time) bool {
	threshold := math.Exp(-1) * SRCConstantsForDecay.CorrelationThreshold
	return s.Relevance(now) > threshold
}

// SignalSet is a collection of signals at a location.
type SignalSet struct {
	// Location is the location these signals are from.
	Location string `json:"location"`

	// Signals is the list of signals.
	Signals []*Signal `json:"signals"`

	// ActiveCount is the number of signals above relevance threshold.
	ActiveCount int `json:"active_count"`

	// Timestamp is when this set was queried.
	Timestamp time.Time `json:"timestamp"`
}

// Active returns only active signals from the set.
func (ss *SignalSet) Active() []*Signal {
	now := time.Now()
	active := make([]*Signal, 0)
	for _, s := range ss.Signals {
		if s.IsActive(now) {
			active = append(active, s)
		}
	}
	return active
}

// ByType returns signals matching the given type.
func (ss *SignalSet) ByType(sigType string) []*Signal {
	matched := make([]*Signal, 0)
	for _, s := range ss.Signals {
		if s.Type == sigType {
			matched = append(matched, s)
		}
	}
	return matched
}

// HasActive returns true if any active signals exist.
func (ss *SignalSet) HasActive() bool {
	return ss.ActiveCount > 0
}
