// calibration.go — First Instruments Module D5: calibration-first HARD ABORT.
//
// Ports decision_surface_sim.py's §6.2 machinery to Go: the FROZEN
// split-null calibration (threshold-setting vs held-out validation nulls),
// the Gaussian-multiplicative null family, the circular block-bootstrap,
// the 95th-percentile-of-decision-statistic threshold derivation, the
// "clears" rule (90%-CI upper bound <= threshold), and the three hard-abort
// gates (PRECISION, POWER, JITTER-MODEL-VALIDATION).
//
// BEFORE any kernel observation is collected, the runner (runner.go) MUST
// call Calibrate and refuse to proceed unless CalibrationOutcome.AllPass()
// is true — this file's job is to make that refusal possible; it produces
// no kernel-observation side effects itself (pure arithmetic over
// synthetic/frozen-seed draws).
package experiment

import (
	"math"
	"math/rand"
	"sort"
)

// Frozen §6.2 null-family + gate parameters. Values match
// decision_surface_sim.py's FROZEN-registration constants (recorded in the
// manifest per IMPL-SPEC D4/D5), NOT the sim's own downscaled internal
// resolution constants (N_THRESH/N_VAL/N_REPLICATES/BOOTSTRAP_B below ARE
// the frozen registration counts — this package runs the full counts by
// default; callers needing faster iteration for development pass a
// CalibrationConfig with reduced counts explicitly, which is recorded in
// the manifest as a deviation).
const (
	// SigmaNull is the frozen Gaussian-multiplicative null jitter scale
	// (sigma_null = sigma_jit = sigma_pc = 0.05, PREREG §6.2 residual (ii)).
	SigmaNull = 0.05

	// FrozenNThresh is the frozen threshold-setting null count.
	FrozenNThresh = 1000
	// FrozenNVal is the frozen held-out validation null count.
	FrozenNVal = 1000
	// FrozenNMax is the frozen max replicate count per cell (n-ladder cap).
	FrozenNMax = 240
	// FrozenBootstrapB is the frozen minimum bootstrap resample count.
	FrozenBootstrapB = 1000

	// CILevel is the frozen two-sided bootstrap CI level (90%, PREREG §6.2).
	CILevel = 0.90
	// HMaxS is the PRECISION gate bound: validation-null median 90%-CI
	// half-width of S must be <= this (S-units, PREREG §6.2).
	HMaxS = 0.25
	// PowerMinS is the POWER gate bound: the delta=0.25 drifting battery
	// must be rejected (does not clear) at rate >= this.
	PowerMinS = 0.80
	// DriftDeltaGated is the gated middle drift size (per octave) the POWER
	// gate battery uses.
	DriftDeltaGated = 0.25
	// JitterCVMax is the JITTER-MODEL-VALIDATION scale-criterion bound:
	// measured heartbeat-cadence CV must be <= this (within 1.5x of the
	// frozen sigma=0.05).
	JitterCVMax = 0.075
)

// CalibrationConfig lets a caller trade Monte-Carlo resolution for runtime
// (mirrors decision_surface_sim.py's documented downscale: this changes NO
// frozen registration value or decision logic, only draw counts). The
// zero value uses the frozen registration counts.
type CalibrationConfig struct {
	NThresh     int // threshold-setting null draws; 0 => FrozenNThresh
	NVal        int // held-out validation null draws; 0 => FrozenNVal
	NReplicates int // replicates per synthetic family draw; 0 => FrozenNMax
	BootstrapB  int // bootstrap resamples; 0 => FrozenBootstrapB
	Seed        int64
}

func (cfg CalibrationConfig) resolved() CalibrationConfig {
	if cfg.NThresh <= 0 {
		cfg.NThresh = FrozenNThresh
	}
	if cfg.NVal <= 0 {
		cfg.NVal = FrozenNVal
	}
	if cfg.NReplicates <= 0 {
		cfg.NReplicates = FrozenNMax
	}
	if cfg.BootstrapB <= 0 {
		cfg.BootstrapB = FrozenBootstrapB
	}
	if cfg.Seed == 0 {
		cfg.Seed = 20260707 // frozen master seed (decision_surface_sim.py STABILITY_SEED)
	}
	return cfg
}

// drawNullCellReplicates draws n config-independent (or drifting, via
// meanRatio != 1) fake-ratio replicates for one cell under the FROZEN
// Gaussian-multiplicative null family (PREREG §6.2):
//
//	R_i = num_i/den_i, num_i = meanRatio*(1+eta_n), den_i = 1*(1+eta_d),
//	eta ~ Normal(0, sigmaNull^2) i.i.d.
func drawNullCellReplicates(rng *rand.Rand, meanRatio float64, n int, sigmaNull float64) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		num := meanRatio * (1.0 + rng.NormFloat64()*sigmaNull)
		den := 1.0 * (1.0 + rng.NormFloat64()*sigmaNull)
		out[i] = num / den
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// cv is the coefficient of variation of a list of cell-means (population
// std, matching decision_surface_sim.py's cv()).
func cv(values []float64) float64 {
	m := mean(values)
	if m == 0 {
		return math.Inf(1)
	}
	var sumSq float64
	for _, v := range values {
		d := v - m
		sumSq += d * d
	}
	variance := sumSq / float64(len(values))
	return math.Sqrt(variance) / math.Abs(m)
}

// cvOfReplicates is the within-cell CV of a replicate sample (sample std /
// |mean|, matching decision_surface_sim.py's _cv_of_replicates — uses n-1
// in the denominator for the sample variance).
func cvOfReplicates(replicates []float64) float64 {
	n := len(replicates)
	if n < 2 {
		return 0
	}
	m := mean(replicates)
	if m == 0 {
		return math.Inf(1)
	}
	var sumSq float64
	for _, v := range replicates {
		d := v - m
		sumSq += d * d
	}
	variance := sumSq / float64(n-1)
	return math.Sqrt(variance) / math.Abs(m)
}

// sFromFamilyReplicates computes S = CV_grid/CV_null (PREREG §6.1) given
// per-cell replicate samples over the co-scaled stability family:
//
//	CV_grid = CV of the three cell-means
//	CV_null = pooled within-cell CV over the family (replicate-count-
//	          weighted RMS of the per-cell within-cell CVs) — §6.1
//	          residual (i)
func sFromFamilyReplicates(familyReplicates map[string][]float64) (cvGrid, cvNull, s float64) {
	cellMeans := make([]float64, 0, len(StabilityFamily))
	for _, cid := range StabilityFamily {
		cellMeans = append(cellMeans, mean(familyReplicates[cid]))
	}
	cvGrid = cv(cellMeans)

	var weightedSumSq float64
	var totalWeight int
	for _, cid := range StabilityFamily {
		reps := familyReplicates[cid]
		c := cvOfReplicates(reps)
		w := len(reps)
		weightedSumSq += float64(w) * c * c
		totalWeight += w
	}
	cvNull = math.Sqrt(weightedSumSq / float64(totalWeight))

	if cvNull > 0 {
		s = cvGrid / cvNull
	} else {
		s = math.Inf(1)
	}
	return cvGrid, cvNull, s
}

// percentile returns the q-quantile (0..1) of an already-sorted slice
// (linear interpolation, matching decision_surface_sim.py's _percentile).
func percentile(sortedVals []float64, q float64) float64 {
	if len(sortedVals) == 0 {
		return math.NaN()
	}
	if len(sortedVals) == 1 {
		return sortedVals[0]
	}
	pos := q * float64(len(sortedVals)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sortedVals[lo]
	}
	frac := pos - float64(lo)
	return sortedVals[lo]*(1-frac) + sortedVals[hi]*frac
}

// familySSampler draws one {Ks0,Ks2,Ks4} family of replicates (each cell's
// mean ratio given by meanRatioOf) and returns S. This is the resampling
// unit the block-bootstrap draws from for a SYNTHETIC (i.i.d.) family — a
// real kernel-observed family instead uses block-resampling over the
// serially-dependent consecutive-interval data (see BlockBootstrapCIUpper).
func familySSampler(rng *rand.Rand, meanRatioOf func(cid string) float64, nRep int, sigmaNull float64) float64 {
	fam := map[string][]float64{}
	for _, cid := range StabilityFamily {
		fam[cid] = drawNullCellReplicates(rng, meanRatioOf(cid), nRep, sigmaNull)
	}
	_, _, s := sFromFamilyReplicates(fam)
	return s
}

// BootstrapCIUpper draws b realizations of a resampling statistic (each a
// full synthetic family draw via sampler) and returns the upper bound and
// half-width of a two-sided `level` CI. i.i.d. synthetic nulls use this
// directly (block length 1 — the block structure only bites on
// serially-dependent REAL consecutive-interval data, PREREG §6.2/Finding C).
func BootstrapCIUpper(sampler func(rng *rand.Rand) float64, seed int64, level float64, b int) (upper, half float64) {
	rng := rand.New(rand.NewSource(seed))
	draws := make([]float64, b)
	for i := 0; i < b; i++ {
		draws[i] = sampler(rng)
	}
	sort.Float64s(draws)
	alpha := (1.0 - level) / 2.0
	lo := percentile(draws, alpha)
	hi := percentile(draws, 1.0-alpha)
	return hi, (hi - lo) / 2.0
}

// nullMeanRatio is the config-independent null: mean ratio 1 at every cell.
func nullMeanRatio(_ string) float64 { return 1.0 }

// driftMeanRatioFactory returns a mean-ratio function that config-tracks by
// delta per octave along the co-scale family (PREREG §6.2 POWER battery).
func driftMeanRatioFactory(delta float64) func(cid string) float64 {
	return func(cid string) float64 {
		cell, ok := CellByID(cid)
		if !ok {
			return 1.0
		}
		s := float64(cell.C) / float64(ReferenceCell.C)
		if s <= 0 {
			return 1.0
		}
		return 1.0 * (1.0 + delta*math.Log2(s))
	}
}

// DeriveThreshold computes the frozen threshold: the 95th percentile, over
// NThresh threshold-setting nulls, of each null's block-bootstrap 90%-CI
// upper bound of S (i.i.d. nulls use block length 1). This is deliberately
// NOT the 95th percentile of the point-S distribution — see the rationale
// in IMPL-SPEC D5 / PREREG §6.2 (a CI-upper-bound-derived threshold keeps a
// config-independent null clearing at the intended ~95%, not ~50%).
func DeriveThreshold(cfg CalibrationConfig) float64 {
	cfg = cfg.resolved()
	rng := rand.New(rand.NewSource(cfg.Seed + 1)) // SEED_THRESHOLD
	uppers := make([]float64, cfg.NThresh)
	for i := 0; i < cfg.NThresh; i++ {
		subSeed := rng.Int63n(1<<31-1) + 1
		upper, _ := BootstrapCIUpper(func(r *rand.Rand) float64 {
			return familySSampler(r, nullMeanRatio, cfg.NReplicates, SigmaNull)
		}, subSeed, CILevel, cfg.BootstrapB)
		uppers[i] = upper
	}
	sort.Float64s(uppers)
	return percentile(uppers, 0.95)
}

// Clears reports whether a candidate statistic's 90%-CI upper bound
// clears (<=) the frozen threshold — the SINGLE "clears" definition used
// everywhere (PREREG §6.2): config-stable = confidently small S.
func Clears(ciUpper, threshold float64) bool {
	return ciUpper <= threshold
}

// PrecisionGate computes the PRECISION gate (§6.2): on the held-out
// validation-null set (disjoint seed from threshold-setting), per null
// compute the bootstrapped 90%-CI half-width of S; the gate statistic h is
// the MEDIAN half-width across NVal validation nulls. Passes iff h <= HMaxS.
func PrecisionGate(cfg CalibrationConfig) (pass bool, h float64) {
	cfg = cfg.resolved()
	rng := rand.New(rand.NewSource(cfg.Seed + 2)) // SEED_VALIDATION
	halves := make([]float64, cfg.NVal)
	for i := 0; i < cfg.NVal; i++ {
		subSeed := rng.Int63n(1<<31-1) + 1
		_, half := BootstrapCIUpper(func(r *rand.Rand) float64 {
			return familySSampler(r, nullMeanRatio, cfg.NReplicates, SigmaNull)
		}, subSeed, CILevel, cfg.BootstrapB)
		halves[i] = half
	}
	sort.Float64s(halves)
	h = percentile(halves, 0.5)
	return h <= HMaxS, h
}

// PowerGate computes the POWER gate (§6.2): a battery of synthetic
// drifting-ratio families at drift size delta must be rejected (its 90%-CI
// upper bound does NOT clear threshold) at rate >= PowerMinS.
func PowerGate(cfg CalibrationConfig, delta float64, threshold float64) (pass bool, rejectRate float64) {
	cfg = cfg.resolved()
	rng := rand.New(rand.NewSource(cfg.Seed + 3)) // SEED_POWER
	driftRatioOf := driftMeanRatioFactory(delta)
	nBattery := cfg.NVal // same battery size as validation, per sim's power_gate_from_machinery
	rejected := 0
	for i := 0; i < nBattery; i++ {
		subSeed := rng.Int63n(1<<31-1) + 1
		upper, _ := BootstrapCIUpper(func(r *rand.Rand) float64 {
			return familySSampler(r, driftRatioOf, cfg.NReplicates, SigmaNull)
		}, subSeed, CILevel, cfg.BootstrapB)
		if upper > threshold {
			rejected++
		}
	}
	rejectRate = float64(rejected) / float64(nBattery)
	return rejectRate >= PowerMinS, rejectRate
}

// JitterModelValidation is the frozen accept criterion for the
// JITTER-MODEL-VALIDATION gate (IMPL-SPEC D5, blind-review-4 G6 residual
// (iii)): measured heartbeat-cadence CV <= JitterCVMax (scale) AND a
// one-sample KS test of standardized residuals vs Normal(0, measuredCV^2)
// does not reject at alpha=0.05 (shape). ksRejects is supplied by the
// caller's KS-test implementation (kept as an injected bool rather than
// this package owning a KS-test implementation, since the real gate runs
// against MEASURED kernel jitter residuals, not synthetic draws — see
// runner.go's MeasureJitterModel).
func JitterModelValidation(measuredCV float64, ksRejects bool) (scalePass, shapePass bool) {
	scalePass = measuredCV <= JitterCVMax
	shapePass = !ksRejects
	return scalePass, shapePass
}

// CalibrationOutcome is the result of the three frozen hard-abort gates
// (IMPL-SPEC D5 §2 test table). AllPass gates whether the runner may
// proceed to collect any kernel observation.
type CalibrationOutcome struct {
	Threshold float64

	PrecisionPass bool
	PrecisionH    float64

	PowerPass       bool
	PowerRejectRate float64

	JitterScalePass  bool
	JitterShapePass  bool
	MeasuredJitterCV float64

	NAtHalt int // the n-ladder rung reached (FrozenNMax if all pass at the cap)
}

// AllPass reports whether every gate passed — the ONLY condition under
// which the runner may collect kernel data (IMPL-SPEC D5: never rescued by
// relaxing h_max, power_min, or the threshold).
func (o CalibrationOutcome) AllPass() bool {
	return o.PrecisionPass && o.PowerPass && o.JitterScalePass && o.JitterShapePass
}

// FailedGates lists which gates failed, for INSTRUMENT-BROKEN reporting.
func (o CalibrationOutcome) FailedGates() []string {
	var failed []string
	if !o.PrecisionPass {
		failed = append(failed, "PRECISION")
	}
	if !o.PowerPass {
		failed = append(failed, "POWER")
	}
	if !o.JitterScalePass {
		failed = append(failed, "JITTER-scale")
	}
	if !o.JitterShapePass {
		failed = append(failed, "JITTER-shape(non-Gaussian)")
	}
	return failed
}

// Calibrate runs the FROZEN §6.2 calibration-first HARD ABORT: derives the
// threshold, runs PRECISION and POWER over the synthetic null machinery,
// and folds in the caller-supplied measured-jitter validation (which must
// come from a real, labeled, non-confirmatory measurement run against the
// live kernel at the scaled anchor Ks0 — see runner.go). n_max=FrozenNMax;
// if either PRECISION or POWER is unmet at n_max, AllPass() is false and
// the runner must halt INSTRUMENT-BROKEN, never relax the gates.
func Calibrate(cfg CalibrationConfig, measuredJitterCV float64, jitterKSRejects bool) CalibrationOutcome {
	cfg = cfg.resolved()
	threshold := DeriveThreshold(cfg)
	precisionPass, h := PrecisionGate(cfg)
	powerPass, rejectRate := PowerGate(cfg, DriftDeltaGated, threshold)
	scalePass, shapePass := JitterModelValidation(measuredJitterCV, jitterKSRejects)

	return CalibrationOutcome{
		Threshold:        threshold,
		PrecisionPass:    precisionPass,
		PrecisionH:       h,
		PowerPass:        powerPass,
		PowerRejectRate:  rejectRate,
		JitterScalePass:  scalePass,
		JitterShapePass:  shapePass,
		MeasuredJitterCV: measuredJitterCV,
		NAtHalt:          cfg.NReplicates,
	}
}

// DeterminismGuard is the §6.3 determinism guard (WINS all disagreements):
// if CV_null==0, S is undefined and the quantity is DETERMINISTIC-ECHO, not
// a stochastic invariant. Only M11r (the sole Shape-(I) existence
// candidate) enters S; DETERMINISTIC-ECHO measurables never do.
func DeterminismGuard(cvNull float64) bool {
	return cvNull == 0.0
}
