package experiment

import "testing"

// TestFindingD_SharpFalsifierNeverTerminalDead is the executable proof of
// Finding D (PREREG §7): for every cell and every plausible data pattern
// that RAW-fires the (A)-vs-(B) asymmetry KILL, ClassifySharpFalsifierPattern
// must NEVER return a terminal DEAD. The pattern is pre-empted either to
// INSTRUMENT-BROKEN (neg-control-4 or a persistent Run() error) or to
// DATA-INSUFFICIENT (a genuine KC-3-LAW kill demotes M11r, or a divisible
// cell is mixture-characterization-only).
func TestFindingD_SharpFalsifierNeverTerminalDead(t *testing.T) {
	for _, c := range Lattice {
		for _, runError := range []bool{false, true} {
			for _, m11rDrifts := range []bool{false, true} {
				for _, consStable := range []bool{false, true} {
					for _, hbStable := range []bool{false, true} {
						decision := ClassifySharpFalsifierPattern(c, float64(ReferenceCell.H), runError, m11rDrifts, consStable, hbStable)
						if decision.State == TerminalDead {
							t.Errorf("cell %s (runError=%v, m11rDrifts=%v, consStable=%v, hbStable=%v): sharp-falsifier returned terminal DEAD (%s) — Finding D violated",
								c.ID, runError, m11rDrifts, consStable, hbStable, decision.Route)
						}
						if decision.DeadViaSharpFalsifier {
							t.Errorf("cell %s: DeadViaSharpFalsifier=true; want always false", c.ID)
						}
					}
				}
			}
		}
	}
}

// TestClassifySharpFalsifierPattern_RunError_InstrumentBroken confirms a
// persistent Run() error is always routed to INSTRUMENT-BROKEN, never a
// tracks-H-alone kill (Finding B, mirrored into the terminal classifier).
func TestClassifySharpFalsifierPattern_RunError_InstrumentBroken(t *testing.T) {
	c, _ := CellByID("Ks0")
	decision := ClassifySharpFalsifierPattern(c, float64(ReferenceCell.H), true /* runError */, true, false, false)
	if decision.State != TerminalInstrumentBroken {
		t.Errorf("State = %q; want %q for a persistent Run() error", decision.State, TerminalInstrumentBroken)
	}
}

// TestClassifyStabilityTerminal_CalibrationFailure_InstrumentBroken
// confirms a failed calibration hard-aborts to INSTRUMENT-BROKEN before any
// DEAD/SURVIVED adjudication.
func TestClassifyStabilityTerminal_CalibrationFailure_InstrumentBroken(t *testing.T) {
	badCalibration := CalibrationOutcome{PrecisionPass: false, PowerPass: true, JitterScalePass: true, JitterShapePass: true}
	decision := ClassifyStabilityTerminal(true /* m11rClears */, badCalibration, true /* pcSynthClears */, false /* lawKilled */)
	if decision.State != TerminalInstrumentBroken {
		t.Errorf("State = %q; want %q when calibration fails, regardless of m11rClears", decision.State, TerminalInstrumentBroken)
	}
}

// TestClassifyStabilityTerminal_PCSynthFailure_InstrumentBroken confirms a
// failed PC-SYNTH positive control also hard-aborts to INSTRUMENT-BROKEN.
func TestClassifyStabilityTerminal_PCSynthFailure_InstrumentBroken(t *testing.T) {
	goodCalibration := CalibrationOutcome{PrecisionPass: true, PowerPass: true, JitterScalePass: true, JitterShapePass: true}
	decision := ClassifyStabilityTerminal(true, goodCalibration, false /* pcSynthClears=false */, false)
	if decision.State != TerminalInstrumentBroken {
		t.Errorf("State = %q; want %q when PC-SYNTH fails to clear", decision.State, TerminalInstrumentBroken)
	}
}

// TestClassifyStabilityTerminal_LawKilled_DataInsufficient confirms a
// KC-3-LAW kill demotes M11r to DATA-INSUFFICIENT for the invariant search,
// even when calibration and PC-SYNTH both pass.
func TestClassifyStabilityTerminal_LawKilled_DataInsufficient(t *testing.T) {
	goodCalibration := CalibrationOutcome{PrecisionPass: true, PowerPass: true, JitterScalePass: true, JitterShapePass: true}
	decision := ClassifyStabilityTerminal(true, goodCalibration, true, true /* lawKilled */)
	if decision.State != TerminalDataInsufficient {
		t.Errorf("State = %q; want %q when the KC-3-LAW kill fires", decision.State, TerminalDataInsufficient)
	}
}

// TestClassifyStabilityTerminal_ClearsAndSurvives_SurvivedGeneric and
// TestClassifyStabilityTerminal_DriftsAndDoesNotClear_Dead cover the only
// two reachable terminal outcomes once INSTRUMENT-BROKEN/DATA-INSUFFICIENT
// are ruled out — SURVIVED-GENERIC and DEAD (the sole reachable DEAD route).
func TestClassifyStabilityTerminal_ClearsAndSurvives_SurvivedGeneric(t *testing.T) {
	goodCalibration := CalibrationOutcome{PrecisionPass: true, PowerPass: true, JitterScalePass: true, JitterShapePass: true}
	decision := ClassifyStabilityTerminal(true /* m11rClears */, goodCalibration, true, false)
	if decision.State != TerminalSurvivedGeneric {
		t.Errorf("State = %q; want %q", decision.State, TerminalSurvivedGeneric)
	}
}

func TestClassifyStabilityTerminal_DriftsAndDoesNotClear_Dead(t *testing.T) {
	goodCalibration := CalibrationOutcome{PrecisionPass: true, PowerPass: true, JitterScalePass: true, JitterShapePass: true}
	decision := ClassifyStabilityTerminal(false /* m11rClears=false, i.e. drifts */, goodCalibration, true, false)
	if decision.State != TerminalDead {
		t.Errorf("State = %q; want %q — this IS the sole reachable DEAD route (stability disjunct)", decision.State, TerminalDead)
	}
}
