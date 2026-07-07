// cadence_events.go — First Instruments Module E: behavioral-cadence
// instrumentation (M11/M12/M13).
//
// Side-effect-free observation surface over the process run-loop's cadence
// events (IMPL-SPEC E1/E2). Exposes, without altering control flow, the
// timestamps of:
//
//   - Dormant-consolidation completions (M11) — tapped at the SUCCESS point
//     (process.go, co-located with p.lastConsolidation = now), never on a
//     gate-pass or attempt (Finding B: a persistent Run() error records no
//     event, so a broken instrument never masquerades as a law-kill).
//   - Heartbeat events (M12) — tapped PAST the StateActive early-return
//     (Finding F9: tapping at the run-loop invocation site would record an
//     event even when the body never ran, making the H6 StateActive
//     exclusion a no-op).
//   - Active-window-expiry observations (M13) — recorded by whatever reader
//     polls IsActive; this package exposes the recording surface, the
//     experiment runner (Module D) is the reader in practice.
//
// K3/K8 one-way-readout discipline: recording an event here NEVER mutates
// process/kernel state and NEVER changes when consolidation/heartbeat
// timing fires — it is a pure append to an in-memory slice, guarded by a
// mutex. Per IMPL-SPEC §0, these event slices are telemetry, not kernel
// state, and are excluded from the no-mutation state-hash test.
package engine

import (
	"sync"
	"time"
)

// ConsolidationTrigger distinguishes which select-case fired the
// consolidation (IMPL-SPEC E1). Only "heartbeat_gated" events feed M11 —
// see cadence_events.go's package doc comment for why the direct_ticker
// path (runConsolidation, a distinct field/index/coherence maintenance
// loop that does not call ConsolidationAction.Run() or update
// lastConsolidation) is not tapped here.
type ConsolidationTrigger string

const (
	// TriggerHeartbeatGated is the K12-gated dormant-consolidation path
	// inside emitHeartbeat — the ONLY trigger that feeds M11.
	TriggerHeartbeatGated ConsolidationTrigger = "heartbeat_gated"
)

// ConsolidationEvent is one observed dormant-consolidation completion (M11).
type ConsolidationEvent struct {
	// At is the wall-clock time of the tap (the same `now` captured at the
	// top of emitHeartbeat's body, co-located with p.lastConsolidation).
	At time.Time
	// Trigger is always TriggerHeartbeatGated for events recorded via
	// recordConsolidation (the only call site in this package).
	Trigger ConsolidationTrigger
	// ProcessState is the process's State() at the moment of the tap (H6 —
	// mirrors PREREG §3.5-H6). A dormant measurement boot is expected
	// non-Active; StateActive-overlap rows are excluded from confirmatory
	// M11r/KC-3-LAW statistics by the runner (Module D).
	ProcessState ProcessState
}

// HeartbeatEvent is one observed heartbeat that did real work (M12) — i.e.
// one that passed the StateActive gate.
type HeartbeatEvent struct {
	At           time.Time
	ProcessState ProcessState
}

// ActiveExpiryObservation is one observation of a session's active-window
// expiry (M13): the wall-clock time at which IsActive(window, now) was
// observed to first return false after the window elapsed, plus the
// read_cadence_ms of the poller that observed it (E1 — since expiry is
// read-gated with no reaper, the observed cadence is a function of both the
// window and the reader; the reader's own cadence must be logged, not a new
// hidden reader introduced by the tap itself, E2).
type ActiveExpiryObservation struct {
	At            time.Time
	SessionID     string
	Window        time.Duration
	ReadCadenceMs int64
}

// cadenceRecorder is the append-only, mutex-guarded store backing the
// Module E taps. Always non-nil on a constructed *Process (see NewProcess).
type cadenceRecorder struct {
	mu             sync.Mutex
	consolidations []ConsolidationEvent
	heartbeats     []HeartbeatEvent
	activeExpiries []ActiveExpiryObservation
}

func newCadenceRecorder() *cadenceRecorder {
	return &cadenceRecorder{}
}

// recordConsolidation appends a dormant-consolidation completion event.
// Called ONLY from the success point in emitHeartbeat (never on gate-pass
// or attempt — Finding B).
func (c *cadenceRecorder) recordConsolidation(at time.Time, trigger ConsolidationTrigger, state ProcessState) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consolidations = append(c.consolidations, ConsolidationEvent{At: at, Trigger: trigger, ProcessState: state})
}

// recordHeartbeat appends a heartbeat event. Called ONLY past the
// StateActive gate in emitHeartbeat (Finding F9).
func (c *cadenceRecorder) recordHeartbeat(at time.Time, state ProcessState) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeats = append(c.heartbeats, HeartbeatEvent{At: at, ProcessState: state})
}

// recordActiveExpiry appends an active-window-expiry observation.
func (c *cadenceRecorder) recordActiveExpiry(obs ActiveExpiryObservation) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeExpiries = append(c.activeExpiries, obs)
}

func (c *cadenceRecorder) snapshotConsolidations() []ConsolidationEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ConsolidationEvent, len(c.consolidations))
	copy(out, c.consolidations)
	return out
}

func (c *cadenceRecorder) snapshotHeartbeats() []HeartbeatEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HeartbeatEvent, len(c.heartbeats))
	copy(out, c.heartbeats)
	return out
}

func (c *cadenceRecorder) snapshotActiveExpiries() []ActiveExpiryObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ActiveExpiryObservation, len(c.activeExpiries))
	copy(out, c.activeExpiries)
	return out
}

// ConsolidationEvents returns a read-only snapshot of observed dormant-
// consolidation completions (M11). Side-effect-free (a copy is returned;
// the caller cannot mutate the recorder's internal slice).
func (p *Process) ConsolidationEvents() []ConsolidationEvent {
	if p == nil || p.cadence == nil {
		return nil
	}
	return p.cadence.snapshotConsolidations()
}

// HeartbeatEvents returns a read-only snapshot of observed heartbeat events
// that passed the StateActive gate (M12).
func (p *Process) HeartbeatEvents() []HeartbeatEvent {
	if p == nil || p.cadence == nil {
		return nil
	}
	return p.cadence.snapshotHeartbeats()
}

// ActiveExpiryObservations returns a read-only snapshot of observed
// active-window-expiry events (M13).
func (p *Process) ActiveExpiryObservations() []ActiveExpiryObservation {
	if p == nil || p.cadence == nil {
		return nil
	}
	return p.cadence.snapshotActiveExpiries()
}

// RecordActiveExpiryObservation lets a read-gated poller of IsActive log an
// observation (M13, E1/E2). The poller — not this method — decides when to
// call IsActive; this is purely a recording surface, so it introduces no
// new hidden reader (E2).
func (p *Process) RecordActiveExpiryObservation(obs ActiveExpiryObservation) {
	if p == nil || p.cadence == nil {
		return
	}
	p.cadence.recordActiveExpiry(obs)
}
