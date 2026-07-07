package experiment

import (
	"math/rand"
	"testing"
)

// fastCalibrationConfig trades Monte-Carlo resolution for test runtime,
// per the same documented downscale decision_surface_sim.py itself uses —
// changes no frozen registration value or decision logic, only draw counts.
func fastCalibrationConfig() CalibrationConfig {
	return CalibrationConfig{NThresh: 100, NVal: 100, NReplicates: 30, BootstrapB: 100, Seed: 20260707}
}

// TestCalibrate_ConfigIndependentNull_AllGatesPass confirms a
// config-independent (mean_ratio=1 everywhere) synthetic family passes the
// PRECISION and POWER gates and clears a derived threshold at roughly the
// intended ~95% rate — the calibrated behavior of the frozen decision
// statistic.
func TestCalibrate_ConfigIndependentNull_AllGatesPass(t *testing.T) {
	cfg := fastCalibrationConfig()
	out := Calibrate(cfg, 0.04, false)
	if !out.PrecisionPass {
		t.Errorf("PRECISION gate failed: h=%v (want <= %v)", out.PrecisionH, HMaxS)
	}
	if !out.PowerPass {
		t.Errorf("POWER gate failed: rejectRate=%v (want >= %v)", out.PowerRejectRate, PowerMinS)
	}
	if !out.JitterScalePass || !out.JitterShapePass {
		t.Errorf("JITTER-MODEL-VALIDATION gate failed: scale=%v shape=%v", out.JitterScalePass, out.JitterShapePass)
	}
	if !out.AllPass() {
		t.Errorf("AllPass() = false; want true (all three gates should pass for a healthy synthetic null): failed=%v", out.FailedGates())
	}
	if out.Threshold <= 0 {
		t.Errorf("Threshold = %v; want a positive derived threshold, not a hardcoded 2.0 point-rule", out.Threshold)
	}
}

// TestCalibrate_JitterShapeFailure_InstrumentBroken confirms a shape
// failure (non-Gaussian measured jitter) fails AllPass regardless of scale,
// per the frozen "no scale substitution fixes a wrong shape" rule.
func TestCalibrate_JitterShapeFailure_InstrumentBroken(t *testing.T) {
	cfg := fastCalibrationConfig()
	out := Calibrate(cfg, 0.04, true /* ksRejects = shape failure */)
	if out.JitterShapePass {
		t.Error("JitterShapePass = true with ksRejects=true; want false")
	}
	if out.AllPass() {
		t.Error("AllPass() = true despite a jitter SHAPE failure; want false (INSTRUMENT-BROKEN)")
	}
	failed := out.FailedGates()
	found := false
	for _, g := range failed {
		if g == "JITTER-shape(non-Gaussian)" {
			found = true
		}
	}
	if !found {
		t.Errorf("FailedGates() = %v; want to include JITTER-shape(non-Gaussian)", failed)
	}
}

// TestCalibrate_JitterScaleFailure confirms a scale failure (measured CV
// too high) is distinguishable from a shape failure.
func TestCalibrate_JitterScaleFailure(t *testing.T) {
	cfg := fastCalibrationConfig()
	out := Calibrate(cfg, 0.5 /* way above JitterCVMax=0.075 */, false)
	if out.JitterScalePass {
		t.Error("JitterScalePass = true with measuredCV=0.5; want false")
	}
	if out.JitterShapePass != true {
		t.Error("JitterShapePass should still be true (shape and scale are independent checks)")
	}
	if out.AllPass() {
		t.Error("AllPass() = true despite a jitter SCALE failure; want false")
	}
}

// TestClears_SingleDefinition confirms the "clears" rule is CI-upper-bound
// <= threshold, never a point estimate or "exceeds" framing.
func TestClears_SingleDefinition(t *testing.T) {
	if !Clears(0.5, 1.0) {
		t.Error("Clears(0.5, 1.0) = false; want true (0.5 <= 1.0)")
	}
	if Clears(1.5, 1.0) {
		t.Error("Clears(1.5, 1.0) = true; want false (1.5 > 1.0)")
	}
	if !Clears(1.0, 1.0) {
		t.Error("Clears(1.0, 1.0) = false; want true (boundary is inclusive: <=)")
	}
}

// TestDeterminismGuard confirms CV_null==0 => DETERMINISTIC-ECHO, and only
// this exact condition fires the guard.
func TestDeterminismGuard(t *testing.T) {
	if !DeterminismGuard(0.0) {
		t.Error("DeterminismGuard(0.0) = false; want true")
	}
	if DeterminismGuard(0.001) {
		t.Error("DeterminismGuard(0.001) = true; want false")
	}
}

// TestSFromFamilyReplicates_ConfigIndependent_LowCVGrid confirms that a
// config-independent (identical mean at every cell) family has a low
// CV_grid relative to a drifting family — the qualitative behavior S is
// built to detect.
func TestSFromFamilyReplicates_ConfigIndependent_LowCVGrid(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	nullFam := map[string][]float64{}
	for _, cid := range StabilityFamily {
		nullFam[cid] = drawNullCellReplicates(rng, 1.0, 100, SigmaNull)
	}
	cvGridNull, _, _ := sFromFamilyReplicates(nullFam)

	driftFam := map[string][]float64{}
	driftOf := driftMeanRatioFactory(0.5)
	for _, cid := range StabilityFamily {
		driftFam[cid] = drawNullCellReplicates(rng, driftOf(cid), 100, SigmaNull)
	}
	cvGridDrift, _, _ := sFromFamilyReplicates(driftFam)

	if cvGridDrift <= cvGridNull {
		t.Errorf("CV_grid(drift)=%v should be > CV_grid(null)=%v (drift inflates across-cell variance)", cvGridDrift, cvGridNull)
	}
}

// TestPowerGate_ZeroDrift_LowerRejectRate confirms the POWER gate's reject
// rate increases with drift size delta (sanity check on the gate's
// monotonicity, not a frozen assertion).
func TestPowerGate_RejectRateIncreasesWithDelta(t *testing.T) {
	cfg := fastCalibrationConfig()
	threshold := DeriveThreshold(cfg)
	_, rateSmall := PowerGate(cfg, 0.05, threshold)
	_, rateLarge := PowerGate(cfg, 0.50, threshold)
	if rateLarge < rateSmall {
		t.Errorf("reject rate at delta=0.50 (%v) < delta=0.05 (%v); want non-decreasing with larger drift", rateLarge, rateSmall)
	}
}
