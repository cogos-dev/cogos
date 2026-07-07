// terminal.go — First Instruments Module D: terminal-state classifier +
// ordering rule (PREREG §7), mirroring decision_surface_sim.py §6.
//
// Four terminal states: DEAD, SURVIVED-GENERIC, DATA-INSUFFICIENT,
// INSTRUMENT-BROKEN. Ordering rule: DEAD/SURVIVED may be declared only
// after DATA-INSUFFICIENT and INSTRUMENT-BROKEN are actively ruled out
// FIRST. Finding D: the sharp-falsifier DEAD disjunct is STRUCTURALLY
// UNREACHABLE — DEAD is reachable ONLY via the stability disjunct (M11r
// measured, drifts, does not clear).
package experiment

const (
	TerminalDead             = "DEAD"
	TerminalSurvivedGeneric  = "SURVIVED-GENERIC"
	TerminalDataInsufficient = "DATA-INSUFFICIENT"
	TerminalInstrumentBroken = "INSTRUMENT-BROKEN"
)

// TerminalDecision is the outcome of applying the §7 ordering rule.
type TerminalDecision struct {
	State                 string
	Route                 string // which disjunct/rule produced it
	DeadViaSharpFalsifier bool   // did a terminal DEAD arrive via the sharp falsifier? (should always be false)
}

// ClassifySharpFalsifierPattern applies the §7 ordering rule to a data
// pattern that RAW-fires the (A)-vs-(B) asymmetry KILL. Finding D asserts
// this NEVER returns a terminal DEAD via the sharp falsifier.
func ClassifySharpFalsifierPattern(c Cell, observedHeartbeatCadenceSeconds float64, runError, m11rDrifts, consStable, hbStable bool) TerminalDecision {
	elig := KillEligibleA(c)

	// Step 1: rule out INSTRUMENT-BROKEN first.
	// (i) stable HEARTBEAT-cadence absolute while H moved -> neg-control-4.
	if elig.HBCadence && hbStable {
		nc4 := NegativeControl4(c, observedHeartbeatCadenceSeconds)
		if nc4.Fired {
			return TerminalDecision{TerminalInstrumentBroken, "neg-control-4 (hb-cadence stable while H moved)", false}
		}
	}

	// Step 1b: a persistent Run() error is INSTRUMENT-BROKEN (Finding B).
	if runError {
		return TerminalDecision{TerminalInstrumentBroken, "persistent Run() error (Finding B)", false}
	}

	// (ii) stable CONSOLIDATION-cadence absolute while M11r drifts,
	// adjudicated only at non-divisible cells.
	if elig.ConsCadence && consStable && m11rDrifts {
		if c.Divisible() {
			return TerminalDecision{TerminalDataInsufficient,
				"divisible cell: mixture-characterization, sharp-falsifier not adjudicated -> no DEAD", false}
		}
		return TerminalDecision{TerminalDataInsufficient,
			"KC-3-LAW kill (cons-cadence stable => cadence != ceil(C/H)*H) => M11r DATA-INSUFFICIENT", false}
	}

	return TerminalDecision{TerminalDataInsufficient, "no eligible stable absolute -> sharp falsifier does not terminate DEAD", false}
}

// ClassifyStabilityTerminal is the stability-disjunct terminal classifier
// (the ONLY reachable route to DEAD). Ordering rule: rule out
// INSTRUMENT-BROKEN and DATA-INSUFFICIENT first.
func ClassifyStabilityTerminal(m11rClears bool, calibration CalibrationOutcome, pcSynthClears, lawKilled bool) TerminalDecision {
	if !calibration.AllPass() {
		return TerminalDecision{TerminalInstrumentBroken,
			"calibration hard-abort: " + joinGates(calibration.FailedGates()), false}
	}
	if !pcSynthClears {
		return TerminalDecision{TerminalInstrumentBroken, "PC-SYNTH positive control failed to clear", false}
	}
	if lawKilled {
		return TerminalDecision{TerminalDataInsufficient, "KC-3-LAW kill -> M11r DATA-INSUFFICIENT for invariant search", false}
	}
	if m11rClears {
		return TerminalDecision{TerminalSurvivedGeneric, "M11r clears stability threshold (config-stable) while absolute counterpart not", false}
	}
	return TerminalDecision{TerminalDead, "M11r did not clear (drift distinguishable from null) -> DEAD via stability disjunct", false}
}

func joinGates(gates []string) string {
	out := ""
	for i, g := range gates {
		if i > 0 {
			out += "/"
		}
		out += g
	}
	return out
}
