// runner.go — First Instruments Module D6: the experiment-runner test
// harness that runs the sweep.
//
// This is measurement tooling, NOT a production code path (IMPL-SPEC D0).
// It boots real testkernel instances (Module A), reads Module E's cadence
// taps, applies the frozen D5 calibration gates, and computes the D4
// recordings (M11r, KC-3-LAW residual/mixture, per-cell kill-eligibility,
// H6 process-state tags) — all against the FROZEN 9-cell lattice
// (lattice.go).
//
// REPLICATE PROTOCOL (FROZEN, blind-review-4 Finding C): per cell, ONE
// boot; over that boot collect n CONSECUTIVE inter-consolidation intervals;
// DISCARD the FIRST interval (phase-contaminated — the boot-to-first-event
// gap is set by lastConsolidation init, not tick-aligned); serial
// dependence handled by the circular block-bootstrap (blockbootstrap.go).
package experiment

import (
	"context"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/testkernel"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// CellRunResult is the per-cell outcome of CollectCellReplicates: the raw
// consecutive inter-consolidation and inter-heartbeat intervals (seconds,
// first discarded), plus the H6 process-state tags observed during
// collection.
type CellRunResult struct {
	Cell Cell

	// ConsolidationIntervalsSeconds are n consecutive inter-consolidation
	// intervals (the FIRST already discarded per the frozen protocol).
	ConsolidationIntervalsSeconds []float64

	// HeartbeatIntervalsSeconds are the corresponding heartbeat intervals
	// observed over the same boot window (M12).
	HeartbeatIntervalsSeconds []float64

	// AnyProcessActiveOverlap is true if any recorded event's ProcessState
	// was StateActive during the window (H6) — such rows must be excluded
	// from confirmatory statistics by the caller.
	AnyProcessActiveOverlap bool

	// RunError is true if a persistent ConsolidationAction.Run() error was
	// detected (Finding B — INSTRUMENT-BROKEN, never a law-kill). This
	// runner detects it indirectly: if heartbeats accumulate but
	// consolidation events never do over a window comfortably exceeding
	// the cell's law-predicted cadence, a persistent Run() error is the
	// most likely explanation (attempts fail silently past the gate).
	RunError bool
}

// CollectCellReplicates boots ONE testkernel measurement kernel at cell's
// (C,H,P), waits for n+1 consolidation events (so n consecutive intervals
// remain after discarding the first), and returns the collected intervals.
// lifetime bounds the kernel's context and MUST comfortably exceed
// (n+1)*cell.CadenceLaw() seconds, or the collection will time out before
// reaching n replicates.
func CollectCellReplicates(t *testing.T, cell Cell, n int, lifetime time.Duration) CellRunResult {
	t.Helper()
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	defer cancel()

	k, err := testkernel.Boot(ctx, t,
		testkernel.WithConsolidationInterval(cell.C),
		testkernel.WithHeartbeatInterval(cell.H),
		testkernel.WithPollInterval(time.Duration(cell.P)*time.Second),
		testkernel.WithoutLocalHarness(),
	)
	if err != nil {
		t.Fatalf("CollectCellReplicates(%s): testkernel.Boot: %v", cell.ID, err)
	}
	defer func() {
		if err := k.Stop(); err != nil {
			t.Errorf("CollectCellReplicates(%s): testkernel.Stop: %v", cell.ID, err)
		}
	}()

	// Wait for n+2 consolidation events: n+1 raw intervals, of which the
	// FIRST is discarded (phase-contaminated, Finding C), leaving n.
	deadline := time.After(lifetime - 2*time.Second)
	var events []engine.ConsolidationEvent
	for {
		events = k.ConsolidationEvents()
		if len(events) >= n+2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("CollectCellReplicates(%s): only %d/%d consolidation events before deadline", cell.ID, len(events), n+2)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Discard the first interval (phase-contaminated, Finding C).
	consIntervals := make([]float64, 0, n)
	anyActive := false
	for i := 2; i < len(events); i++ { // events[0]->events[1] is the discarded first interval
		iv := events[i].At.Sub(events[i-1].At).Seconds()
		consIntervals = append(consIntervals, iv)
		if events[i].ProcessState == engine.StateActive || events[i-1].ProcessState == engine.StateActive {
			anyActive = true
		}
	}

	hbEvents := k.HeartbeatEvents()
	hbIntervals := make([]float64, 0, len(hbEvents))
	for i := 1; i < len(hbEvents); i++ {
		iv := hbEvents[i].At.Sub(hbEvents[i-1].At).Seconds()
		hbIntervals = append(hbIntervals, iv)
		if hbEvents[i].ProcessState == engine.StateActive || hbEvents[i-1].ProcessState == engine.StateActive {
			anyActive = true
		}
	}

	// RunError heuristic: heartbeats accumulated (the process is alive and
	// ticking) but far fewer consolidation events than the window should
	// have produced given the cell's law-predicted cadence — this is the
	// signature of a persistent ConsolidationAction.Run() error (Finding
	// B): the gate at process.go:880 keeps re-firing every heartbeat, but
	// the success-point tap (:891) never records because Run() keeps
	// erroring, so the M11 event count stays far below what the elapsed
	// window and law-predicted cadence would otherwise produce.
	runError := false
	if len(hbIntervals) > 0 && cell.CadenceLaw() > 0 {
		elapsedSeconds := hbEvents[len(hbEvents)-1].At.Sub(hbEvents[0].At).Seconds()
		expectedConsEvents := elapsedSeconds / float64(cell.CadenceLaw())
		if expectedConsEvents >= float64(n)+2 && len(consIntervals) == 0 {
			runError = true
		}
	}

	return CellRunResult{
		Cell:                          cell,
		ConsolidationIntervalsSeconds: consIntervals,
		HeartbeatIntervalsSeconds:     hbIntervals,
		AnyProcessActiveOverlap:       anyActive,
		RunError:                      runError,
	}
}

// M11rFromResult computes the M11r cadence ratio (observed consolidation
// cadence / observed heartbeat cadence) from a CellRunResult's collected
// intervals.
func M11rFromResult(r CellRunResult) (m11r, consCadenceSeconds, hbCadenceSeconds float64) {
	consCadenceSeconds = mean(r.ConsolidationIntervalsSeconds)
	hbCadenceSeconds = mean(r.HeartbeatIntervalsSeconds)
	if hbCadenceSeconds == 0 {
		return 0, consCadenceSeconds, hbCadenceSeconds
	}
	return consCadenceSeconds / hbCadenceSeconds, consCadenceSeconds, hbCadenceSeconds
}

// EvaluateCellObservation applies the D4 per-cell decision logic to a
// CellRunResult: KC-3-LAW confirm/kill (non-divisible discriminating
// cells) or divisible-mixture characterization, tagged with per-cell
// kill-eligibility and H6 process-state exclusion.
func EvaluateCellObservation(r CellRunResult, replicateCountForTau int) Observation {
	c := r.Cell
	m11r, consCadenceSeconds, hbCadenceSeconds := M11rFromResult(r)
	elig := KillEligibleA(c)

	obs := Observation{
		CellID:                         c.ID,
		ObservedConsolidationCadenceMs: consCadenceSeconds * 1000,
		ObservedHeartbeatCadenceMs:     hbCadenceSeconds * 1000,
		M11r:                           m11r,
		KillEligibleConsCadence:        elig.ConsCadence,
		KillEligibleHBCadence:          elig.HBCadence,
		Divisible:                      c.Divisible(),
		LawCellDiscriminating:          c.LawCellDiscriminating(),
		K12QuantizationDominated:       c.QuantizationDominated(),
		ProcessActiveOverlap:           r.AnyProcessActiveOverlap,
		RunError:                       r.RunError,
	}

	if r.RunError {
		return obs // INSTRUMENT-BROKEN signal; no confirm/kill/mixture computed
	}

	if c.NonDivisible() {
		residual := consCadenceSeconds*1000 - float64(c.CadenceLaw())*1000
		confirmed := KC3LawConfirm(c, consCadenceSeconds, false, replicateCountForTau).Fired
		obs.KC3LawResidualMs = &residual
		obs.KC3LawConfirmed = &confirmed
	} else {
		summary := SummarizeDivisibleMixture(c, r.ConsolidationIntervalsSeconds, 1.0)
		fracC := summary.FracAtC
		fracCPlusH := summary.FracAtCPlusH
		meanMs := summary.MeanSeconds * 1000
		obs.DivisibleMixtureFracAtC = &fracC
		obs.DivisibleMixtureFracAtCPlusH = &fracCPlusH
		obs.DivisibleMixtureMeanMs = &meanMs
	}

	return obs
}
