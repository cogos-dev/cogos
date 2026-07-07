package experiment

import (
	"math"
	"testing"
)

// TestLattice_SixNonDivisible_ThreeDivisible confirms the frozen lattice's
// divisibility partition matches PREREG §4.3 exactly.
func TestLattice_SixNonDivisible_ThreeDivisible(t *testing.T) {
	var nonDiv, div []string
	for _, c := range Lattice {
		if c.NonDivisible() {
			nonDiv = append(nonDiv, c.ID)
		} else {
			div = append(div, c.ID)
		}
	}
	if len(nonDiv) != 6 {
		t.Errorf("non-divisible cells = %v (%d); want 6", nonDiv, len(nonDiv))
	}
	if len(div) != 3 {
		t.Errorf("divisible cells = %v (%d); want 3", div, len(div))
	}
	for _, id := range SixNonDivisible {
		found := false
		for _, n := range nonDiv {
			if n == id {
				found = true
			}
		}
		if !found {
			t.Errorf("expected non-divisible cell %s not found in computed set %v", id, nonDiv)
		}
	}
}

// TestLattice_EveryCellCGEH_CeilAtLeast2 confirms H1: every cell has C>=H
// and ceil(C/H)>=2, so no cell is quantization-dominated.
func TestLattice_EveryCellCGEH_CeilAtLeast2(t *testing.T) {
	for _, c := range Lattice {
		if c.C < c.H {
			t.Errorf("cell %s: C=%d < H=%d (quantization-dominated, violates H1)", c.ID, c.C, c.H)
		}
		if c.CeilCH() < 2 {
			t.Errorf("cell %s: ceil(C/H)=%d < 2 (violates H1)", c.ID, c.CeilCH())
		}
		if c.QuantizationDominated() {
			t.Errorf("cell %s: QuantizationDominated()=true; want false for every frozen cell", c.ID)
		}
	}
}

// TestLattice_TauStrictlyLessThanSeparation_AtNonDivisibleCells confirms
// the frozen decidability claim: 0.25*H is strictly < the law-vs-rawC
// separation at every non-divisible cell (PREREG §4.3).
func TestLattice_TauStrictlyLessThanSeparation_AtNonDivisibleCells(t *testing.T) {
	const n = 120 // representative settled n-ladder rung
	for _, id := range SixNonDivisible {
		c, ok := CellByID(id)
		if !ok {
			t.Fatalf("cell %s not found", id)
		}
		quarterH := 0.25 * float64(c.H)
		sep := float64(c.LawVsRawCSepSeconds())
		if quarterH >= sep {
			t.Errorf("cell %s: 0.25*H=%v not strictly < separation=%v (decidability violated)", c.ID, quarterH, sep)
		}
		tau := Tau(c, n)
		if tau >= sep {
			t.Errorf("cell %s: tau=%v not strictly < separation=%v (law confirm/kill would straddle both hypotheses)", c.ID, tau, sep)
		}
	}
}

// TestKillEligibleA_ReferenceCell_BothIneligible confirms Ks0 (the
// reference) has no clock moved relative to itself, so both (A) absolutes
// are ineligible there.
func TestKillEligibleA_ReferenceCell_BothIneligible(t *testing.T) {
	elig := KillEligibleA(ReferenceCell)
	if elig.ConsCadence || elig.HBCadence {
		t.Errorf("KillEligibleA(Ks0) = %+v; want both false (Ks0 is the reference, nothing moved)", elig)
	}
}

// TestKillEligibleA_OneFactorCCell_HBIneligible confirms the F1 fix: in a
// C-only cell (KsC2), the heartbeat absolute is INELIGIBLE (its clock H did
// not move) — this is the fix for the by-construction false DEAD a
// set-level tag would have produced.
func TestKillEligibleA_OneFactorCCell_HBIneligible(t *testing.T) {
	c, ok := CellByID("KsC2")
	if !ok {
		t.Fatal("KsC2 not found")
	}
	elig := KillEligibleA(c)
	if !elig.ConsCadence {
		t.Error("KsC2: ConsCadence eligibility = false; want true (C moved)")
	}
	if elig.HBCadence {
		t.Error("KsC2: HBCadence eligibility = true; want false (H did not move)")
	}
}

// TestKillEligibleA_OneFactorHCell_BothEligible confirms an H-cell (KsH2)
// has both absolutes eligible (H is in both generators).
func TestKillEligibleA_OneFactorHCell_BothEligible(t *testing.T) {
	c, ok := CellByID("KsH2")
	if !ok {
		t.Fatal("KsH2 not found")
	}
	elig := KillEligibleA(c)
	if !elig.ConsCadence || !elig.HBCadence {
		t.Errorf("KsH2: eligibility = %+v; want both true (H moved, in both generators)", elig)
	}
}

// TestKC3LawConfirm_CodeTrueNonDivisible_Confirms confirms the CODE-TRUE
// law generator (observed cadence == ceil(C/H)*H) CONFIRMS at every
// non-divisible cell.
func TestKC3LawConfirm_CodeTrueNonDivisible_Confirms(t *testing.T) {
	const n = 120
	for _, id := range SixNonDivisible {
		c, _ := CellByID(id)
		observed := float64(c.CadenceLaw()) // exact law value (no jitter)
		r := KC3LawConfirm(c, observed, false, n)
		if !r.Fired {
			t.Errorf("cell %s: KC3LawConfirm did not fire for the exact law-predicted cadence %v", c.ID, observed)
		}
	}
}

// TestKC3LawKill_RawCAlternative_Kills confirms the raw-C alternative
// (observed cadence == raw C, not the law) KILLS the law at every
// non-divisible cell (where raw C != law by construction).
func TestKC3LawKill_RawCAlternative_Kills(t *testing.T) {
	const n = 120
	for _, id := range SixNonDivisible {
		c, _ := CellByID(id)
		observed := float64(c.C) // raw C, not ceil(C/H)*H
		r := KC3LawKill(c, observed, false, n)
		if !r.Fired {
			t.Errorf("cell %s: KC3LawKill did not fire for the raw-C alternative %v (law=%v)", c.ID, observed, c.CadenceLaw())
		}
		// The confirm branch must NOT also fire for the same observation.
		confirm := KC3LawConfirm(c, observed, false, n)
		if confirm.Fired {
			t.Errorf("cell %s: KC3LawConfirm fired for the raw-C alternative — confirm/kill should be mutually exclusive here", c.ID)
		}
	}
}

// TestKC3LawKill_HAloneAlternative_Kills confirms the H-alone alternative
// (cadence collapses to H, no C-dependence) KILLS the law wherever H !=
// the law-predicted cadence.
func TestKC3LawKill_HAloneAlternative_Kills(t *testing.T) {
	const n = 120
	for _, id := range SixNonDivisible {
		c, _ := CellByID(id)
		if float64(c.H) == float64(c.CadenceLaw()) {
			continue // H-alone coincides with the law at this cell; not a discriminating case
		}
		observed := float64(c.H)
		r := KC3LawKill(c, observed, false, n)
		if !r.Fired {
			t.Errorf("cell %s: KC3LawKill did not fire for the H-alone alternative %v (law=%v)", c.ID, observed, c.CadenceLaw())
		}
	}
}

// TestKC3Law_DivisibleCells_NeverConfirmOrKill confirms divisible cells
// {K0, KsHhalf, KsC2} never confirm or kill regardless of observation —
// they are law-CHARACTERIZATION/mixture only (Finding A).
func TestKC3Law_DivisibleCells_NeverConfirmOrKill(t *testing.T) {
	const n = 120
	for _, id := range ThreeDivisible {
		c, _ := CellByID(id)
		for _, observed := range []float64{float64(c.C), float64(c.CadenceLaw()), float64(c.H), float64(c.C + c.H)} {
			if r := KC3LawConfirm(c, observed, false, n); r.Fired {
				t.Errorf("cell %s (divisible): KC3LawConfirm fired for observed=%v; want never-fires", c.ID, observed)
			}
			if r := KC3LawKill(c, observed, false, n); r.Fired {
				t.Errorf("cell %s (divisible): KC3LawKill fired for observed=%v; want never-fires", c.ID, observed)
			}
		}
	}
}

// TestKC3Law_RunError_NeitherConfirmsNorKills_Finding_B confirms Finding B:
// a persistent Run() error never confirms and never kills — it must be
// routed to INSTRUMENT-BROKEN by the caller (terminal.go), never
// masquerading as a "tracks H alone" law-kill.
func TestKC3Law_RunError_NeitherConfirmsNorKills_FindingB(t *testing.T) {
	const n = 120
	c, _ := CellByID("Ks0")
	observed := float64(c.H) // the exact signature a run-error would forge
	if r := KC3LawConfirm(c, observed, true /* runError */, n); r.Fired {
		t.Error("KC3LawConfirm fired despite runError=true; want never-fires on a broken instrument")
	}
	if r := KC3LawKill(c, observed, true /* runError */, n); r.Fired {
		t.Error("KC3LawKill fired despite runError=true; want never-fires (Finding B: no tracks-H kill from an environment fault)")
	}
}

// TestDivisibleMixtureCharacterization_ExpectedMean confirms the frozen
// mixture-mean formula C + H*p_hat.
func TestDivisibleMixtureCharacterization_ExpectedMean(t *testing.T) {
	c, _ := CellByID("KsC2") // (20,4,2)
	m := ComputeDivisibleMixtureCharacterization(c)
	want := 20.0 + 4.0*0.5
	if math.Abs(m.ExpectedMeanSeconds-want) > 1e-9 {
		t.Errorf("ExpectedMeanSeconds = %v; want %v", m.ExpectedMeanSeconds, want)
	}
	if m.AtC != 20 || m.AtCPlusH != 24 {
		t.Errorf("AtC=%v AtCPlusH=%v; want 20, 24", m.AtC, m.AtCPlusH)
	}
}

// TestSummarizeDivisibleMixture_ClassifiesIntervals confirms the observed
// mixture summary correctly classifies a synthetic {4s,6s} mixture sample.
func TestSummarizeDivisibleMixture_ClassifiesIntervals(t *testing.T) {
	c, _ := CellByID("K0") // C=3600 not useful here; use a synthetic 4/2 cell instead
	c = Cell{ID: "synthetic", C: 4, H: 2, P: 2}
	intervals := []float64{4.0, 4.1, 6.0, 4.0, 6.1, 4.0, 6.0, 4.0, 4.0, 6.0}
	summary := SummarizeDivisibleMixture(c, intervals, 0.5)
	if summary.FracAtC <= 0 || summary.FracAtCPlusH <= 0 {
		t.Errorf("summary = %+v; want both fractions > 0 (a genuine mixture)", summary)
	}
	if summary.FracAtCPlusH < 0.10 {
		t.Errorf("FracAtCPlusH = %v; want >= 0.10 in this synthetic sample", summary.FracAtCPlusH)
	}
}

// TestOscillatorTickBudget_IsFrozenEight confirms the frozen numeric budget.
func TestOscillatorTickBudget_IsFrozenEight(t *testing.T) {
	if OscillatorTickBudget != 8 {
		t.Errorf("OscillatorTickBudget = %d; want 8 (frozen)", OscillatorTickBudget)
	}
}
