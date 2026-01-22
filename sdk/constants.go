// Package sdk provides the CogOS SDK for workspace integration.
//
// The SDK provides holographic projection of workspace state through URI resolution.
// URIs are the source of truth. The kernel projects. Widgets consume projections.
package sdk

import "math"

// SRCConstants encodes the crystal's mathematical structure.
// These are not configuration - they are derived truths from Self-Reference Coherence theory.
//
// The constants derive from the primordial distinction (0 ≠ 1) and the cost of oscillation.
// See: .cog/ontology/crystal.cog.md for full derivations.
type SRCConstants struct {
	// Ln2 is the cost of one distinction (Landauer/Shannon/Eigen).
	// This is the fundamental unit: ln(2) ≈ 0.693147.
	// Every bit flip, every observation, every distinction costs exactly ln(2) nats.
	Ln2 float64

	// Tau1 is the coherence threshold - the correlation half-life.
	// Equal to Ln2. Signals below this threshold are half-decayed.
	Tau1 float64

	// Tau2 is the stability boundary - escape velocity of self-reference.
	// Equal to 2*Ln2 ≈ 1.386. This is where patterns start degrading.
	// One ln(2) per aspect (Gravity dimension, Time dimension).
	Tau2 float64

	// GEff is the self-reference coupling constant: g_eff = 1/(pinch_depth + 1) = 1/3.
	// This determines how much of context budget goes to identity (stable core)
	// versus temporal (changing context). The 1/3 split is derived, not chosen.
	GEff float64

	// VarianceRatio is the thought/action efficiency: Var(X)/Var(X̂) = 6 (exact).
	// When γ = κ = 1: (γ+κ)(2γ+κ)/γ = (1+1)(2+1)/1 = 6.
	// This is why minds exist: thinking is 6x cheaper than doing.
	VarianceRatio int

	// CorrelationThreshold is the geometric coherence limit: ρ_max = sqrt(2/3).
	// This is the maximum correlation achievable in the viable manifold.
	// Signals start at this value and decay from here.
	CorrelationThreshold float64
}

// constants is the singleton instance.
// These values are immutable by design - they derive from mathematics, not configuration.
var constants = SRCConstants{
	Ln2:                  math.Ln2,            // 0.6931471805599453
	Tau1:                 math.Ln2,            // Same as Ln2
	Tau2:                 2 * math.Ln2,        // 1.3862943611198906
	GEff:                 1.0 / 3.0,           // 0.333...
	VarianceRatio:        6,                   // Exact integer
	CorrelationThreshold: math.Sqrt(2.0 / 3.0), // 0.816496580927726
}

// Constants returns the immutable SRC constants.
// This is a function, not a variable, to prevent mutation.
//
// These constants are compiled into the SDK, not read from files.
// They are available without a workspace connection.
//
// Example:
//
//	src := sdk.Constants()
//	decayRate := src.Tau2       // Use for signal decay timescales
//	chunkSize := src.VarianceRatio // Use for summarization threshold
//	idBudget := src.GEff       // 1/3 of context for identity
func Constants() SRCConstants {
	return constants
}

// Ln2 returns the cost of one distinction (≈ 0.693).
func (s SRCConstants) Ln2Val() float64 { return s.Ln2 }

// Tau1Val returns the coherence threshold (= Ln2).
func (s SRCConstants) Tau1Val() float64 { return s.Tau1 }

// Tau2Val returns the stability boundary (= 2*Ln2).
// This is the Coherence Horizon - escape velocity of self-reference.
func (s SRCConstants) Tau2Val() float64 { return s.Tau2 }

// GEffVal returns the coupling constant (1/3).
func (s SRCConstants) GEffVal() float64 { return s.GEff }

// VarianceRatioVal returns the thought/action efficiency ratio (6).
func (s SRCConstants) VarianceRatioVal() int { return s.VarianceRatio }

// CorrelationThresholdVal returns sqrt(2/3) (≈ 0.816).
func (s SRCConstants) CorrelationThresholdVal() float64 { return s.CorrelationThreshold }

// MessageWeight computes recency weight for thread messages using ln(2) decay.
// At depth 0: weight = 1.0
// At depth 1: weight = 0.5
// At depth 2: weight = 0.25
// ... halving each step
//
// This is not arbitrary exponential decay - it's the natural half-life
// determined by the cost of distinction.
func MessageWeight(depth int) float64 {
	return math.Exp(-constants.Ln2 * float64(depth))
}
