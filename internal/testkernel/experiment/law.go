// law.go — First Instruments Module D4: KC-3-LAW confirm/kill/mixture +
// the frozen tolerance tau, mirroring decision_surface_sim.py §4/§5.
package experiment

import (
	"math"
)

// SigmaJit is the frozen mean-cadence-null jitter scale (PREREG §6.2
// residual: N_valμ=1000, sigma_jit=0.05), used to compute SE_hat.
const SigmaJit = 0.05

// PHat is the code-true divisible-cell mixture's expected fraction landing
// at C+H (mean ~= C + H*p_hat, p_hat ~= 0.5, PREREG §M11 Finding A(1)).
const PHat = 0.5

// SEHat is the analytic stand-in for the frozen mean-cadence-null SE_hat:
// the 90%-CI half-width of the mean-cadence estimator mu_hat under sigma_jit
// multiplicative Gaussian jitter about mu_cell = ceil(C/H)*H seconds, at
// replicate count n. half-width ~= z_0.90 * sigma_jit * mu_cell / sqrt(n),
// z for a two-sided 90% CI ~= 1.645 (matches decision_surface_sim.py's
// se_hat()). Units: seconds.
func SEHat(c Cell, n int) float64 {
	if n <= 0 {
		n = 1
	}
	muCellSeconds := float64(c.CadenceLaw())
	const z = 1.645
	return z * SigmaJit * muCellSeconds / math.Sqrt(float64(n))
}

// Tau is the FROZEN KC-3-LAW tolerance (PREREG §4.3 F3): tau = min(0.25*H,
// 3*SE_hat), in seconds.
func Tau(c Cell, n int) float64 {
	quarter := 0.25 * float64(c.H)
	threeSE := 3.0 * SEHat(c, n)
	if quarter < threeSE {
		return quarter
	}
	return threeSE
}

// LawBranchResult is one branch evaluation (fired + human-readable detail),
// mirroring decision_surface_sim.py's BranchResult.
type LawBranchResult struct {
	Fired  bool
	Detail string
}

func within(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol+1e-12
}

// KC3LawConfirm evaluates the KC-3-LAW CONFIRM branch (PREREG §4.3): at a
// NON-DIVISIBLE cell, the law is confirmed iff the measured mean cadence
// (observedConsCadenceSeconds) is within tau of ceil(C/H)*H. Divisible
// cells never confirm (mixture-characterization only, Finding A). A
// persistent Run() error (runError=true) collects no valid cadence and
// cannot confirm.
func KC3LawConfirm(c Cell, observedConsCadenceSeconds float64, runError bool, n int) LawBranchResult {
	if c.Divisible() {
		return LawBranchResult{false, "divisible cell: law-characterization/mixture only (never confirm)"}
	}
	if runError {
		return LawBranchResult{false, "persistent Run() error: no valid M11 cadence collected"}
	}
	tau := Tau(c, n)
	law := float64(c.CadenceLaw())
	r := math.Abs(observedConsCadenceSeconds - law)
	return LawBranchResult{r <= tau, "residual vs tau check"}
}

// KC3LawKill evaluates the KC-3-LAW KILL branch (PREREG §4.3): at a
// NON-DIVISIBLE discriminating cell, KILL fires iff the measured cadence
// lands within tau of raw C (not the law), OR tracks H alone (no
// C-dependence), OR fits neither the law nor a stated alternative within
// tau. Divisible cells never fire this. Finding B: a persistent Run()
// error is pre-empted to INSTRUMENT-BROKEN by the ordering rule — this
// branch does NOT fire when runError is set (never a "tracks H alone" kill
// forged from an environment fault).
func KC3LawKill(c Cell, observedConsCadenceSeconds float64, runError bool, n int) LawBranchResult {
	if c.Divisible() {
		return LawBranchResult{false, "divisible cell: never fires law-kill (mixture-characterization)"}
	}
	if runError {
		return LawBranchResult{false, "persistent Run() error -> INSTRUMENT-BROKEN (Finding B), not a law-kill"}
	}
	tau := Tau(c, n)
	law := float64(c.CadenceLaw())
	rawC := float64(c.C)
	h := float64(c.H)

	tracksRawC := within(observedConsCadenceSeconds, rawC, tau) && !within(observedConsCadenceSeconds, law, tau)
	tracksH := within(observedConsCadenceSeconds, h, tau) && h != law
	fitsLaw := within(observedConsCadenceSeconds, law, tau)
	fitsNeither := !fitsLaw && !tracksRawC && !tracksH

	fired := tracksRawC || tracksH || fitsNeither
	return LawBranchResult{fired, "kill-branch evaluated"}
}

// DivisibleMixtureCharacterization computes the {C, C+H} mixture
// characterization for a DIVISIBLE cell (PREREG §M11 Finding A(1)): the
// frozen disposition is neither confirm nor kill, just a recorded
// characterization of the knife-edge, expected mean ~= C + H*p_hat.
type DivisibleMixtureCharacterization struct {
	ExpectedMeanSeconds float64
	AtC                 float64 // = c.C
	AtCPlusH            float64 // = c.C + c.H
}

// ComputeDivisibleMixtureCharacterization returns the frozen expected
// mixture shape for a divisible cell. Callers use it as the disposition
// (never confirm/kill) documented in D4/D6.
func ComputeDivisibleMixtureCharacterization(c Cell) DivisibleMixtureCharacterization {
	return DivisibleMixtureCharacterization{
		ExpectedMeanSeconds: float64(c.C) + float64(c.H)*PHat,
		AtC:                 float64(c.C),
		AtCPlusH:            float64(c.C + c.H),
	}
}

// ObservedMixtureSummary summarizes real observed inter-consolidation
// intervals at a divisible cell against the {C, C+H} mixture expectation
// (IMPL-SPEC D4 divisible_mixture block).
type ObservedMixtureSummary struct {
	FracAtC      float64
	FracAtCPlusH float64
	MeanSeconds  float64
}

// SummarizeDivisibleMixture classifies each observed interval as "near C"
// or "near C+H" (within tol seconds) and reports the fraction landing at
// each, plus the observed mean. Intervals fitting neither are excluded
// from both fractions (a caller should treat those as anomalies).
func SummarizeDivisibleMixture(c Cell, intervalsSeconds []float64, tol float64) ObservedMixtureSummary {
	if len(intervalsSeconds) == 0 {
		return ObservedMixtureSummary{}
	}
	var nearC, nearCPlusH int
	var sum float64
	for _, iv := range intervalsSeconds {
		sum += iv
		if within(iv, float64(c.C), tol) {
			nearC++
		} else if within(iv, float64(c.C+c.H), tol) {
			nearCPlusH++
		}
	}
	n := float64(len(intervalsSeconds))
	return ObservedMixtureSummary{
		FracAtC:      float64(nearC) / n,
		FracAtCPlusH: float64(nearCPlusH) / n,
		MeanSeconds:  sum / n,
	}
}

// AsymmetryKill is the sharp-falsifier KILL predicate (PREREG §4.3 KILL
// SEMANTICS, per-cell F1): fires iff SOME per-cell-eligible (A) absolute
// reads config-stable while M11r drifts. This is the RAW predicate;
// whether it terminates in a genuine DEAD is decided by the ordering rule
// (ClassifySharpFalsifierPattern) — Finding D makes terminal-DEAD via this
// path unreachable under the code-true world.
func AsymmetryKill(c Cell, m11rDrifts, consStable, hbStable bool) LawBranchResult {
	if !m11rDrifts {
		return LawBranchResult{false, "M11r does not drift in this cell -> no inversion possible"}
	}
	elig := KillEligibleA(c)
	fired := (elig.ConsCadence && consStable) || (elig.HBCadence && hbStable)
	return LawBranchResult{fired, "asymmetry-kill evaluated"}
}

// OscillatorTickBudget is the FROZEN numeric tick budget (IMPL-SPEC D3): a
// specific number, not "a bounded budget". A 2-state oscillator under
// single-pass reconcile can never converge in 1 tick; convergence within
// this many ticks contradicts single-pass and is a KC-2 finding.
const OscillatorTickBudget = 8

// KC2OscillatorKill evaluates the KC-2 KILL predicate (IMPL-SPEC D3,
// PREREG §4.2): fires iff the oscillator converged (HasChanges()==false)
// within the frozen 8-tick budget — a contradiction of single-pass.
func KC2OscillatorKill(convergedWithinBudget bool, ticksToConverge int) LawBranchResult {
	fired := convergedWithinBudget && ticksToConverge <= OscillatorTickBudget
	return LawBranchResult{fired, "kc2-oscillator-kill evaluated"}
}

// NegativeControl4 fires (INSTRUMENT-BROKEN signal) iff the observed
// heartbeat cadence reads config-stable (== reference H) even though H
// moved in this cell relative to Ks0 — apparent stability under a moved
// clock means H was not actually varied at run time (PREREG §M5).
func NegativeControl4(c Cell, observedHeartbeatCadenceSeconds float64) LawBranchResult {
	hMoved := ClocksMoved(c)["H"]
	hbStable := within(observedHeartbeatCadenceSeconds, float64(ReferenceCell.H), 1e-9)
	fired := hMoved && hbStable
	return LawBranchResult{fired, "negative-control-4 evaluated"}
}
