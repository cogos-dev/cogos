// Package trace defines the cycle-trace event schema emitted by the cogos
// kernel's homeostatic metabolic cycle onto the kernel bus
// (http://localhost:6931/v1/bus) for external consumers such as the Mod³
// dashboard.
//
// This package only declares the wire types and constructor helpers. Wiring
// these events into the state-transition path (internal/engine/process.go)
// and the tool-dispatch/assessment paths (agent_harness.go, agent_tools*.go)
// is performed in a later wave — see
// .cog/mem/semantic/plans/audio-loop-and-trace-wiring.cog.md (task B5).
//
// Design notes:
//   - The package deliberately has no dependency on cogos-internal packages
//     (engine, harness, bus) so that both the engine and the root cogos
//     package can import it without creating an import cycle.
//   - State values are accepted as fmt.Stringer so callers can pass
//     engine.ProcessState (an int enum with a String() method) directly.
//   - The envelope is transport-agnostic. The actual bus-publish call is
//     owned by task D2 / B5 and is out of scope here.
package trace

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind identifies the payload variant carried by a CycleEvent.
type Kind string

const (
	KindStateTransition Kind = "state_transition"
	KindToolDispatch    Kind = "tool_dispatch"
	KindAssessment      Kind = "assessment"
)

// CycleEvent is the common envelope for cycle-trace events emitted onto the
// kernel bus. All events within a single metabolic-cycle iteration share a
// CycleID so consumers can correlate them.
type CycleEvent struct {
	ID        string          `json:"id"`
	Timestamp time.Time       `json:"ts"`
	Source    string          `json:"source"`   // identity name (e.g. "cog")
	CycleID   string          `json:"cycle_id"` // correlates events within one metabolic-cycle iteration
	Kind      Kind            `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// StateTransitionEvent describes a move between process states
// (engine.StateActive / StateReceptive / StateConsolidating / StateDormant).
type StateTransitionEvent struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// ToolDispatchEvent describes a single tool invocation issued by the agent
// harness (see agent_tools*.go).
type ToolDispatchEvent struct {
	Tool       string          `json:"tool"`
	Args       json.RawMessage `json:"args,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`
	// Attribution carries the dispatch caller's subject (RFC-identity-embedding
	// I1/I2): "anonymous" when no identity was presented, otherwise the Sub
	// field from the DispatchIdentity that triggered this harness invocation.
	// Empty on events emitted by the legacy metabolic-cycle path (root
	// agent_harness.go) which does not carry a DispatchIdentity.
	Attribution string `json:"attribution,omitempty"`
}

// AssessmentEvent mirrors the Assessment struct in agent_harness.go. The
// Action field uses the vocabulary the harness already emits:
// "observe" | "propose" | "execute" | "sleep" | "wait" (and synonyms such as
// "consolidate" | "repair" | "escalate").
type AssessmentEvent struct {
	Action     string  `json:"action"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale,omitempty"`
}

// NewStateTransition builds a CycleEvent wrapping a StateTransitionEvent.
// from and to accept any fmt.Stringer so that engine.ProcessState values can
// be passed directly without introducing an import dependency.
func NewStateTransition(source, cycleID string, from, to fmt.Stringer, reason string) (CycleEvent, error) {
	fromStr, toStr := "", ""
	if from != nil {
		fromStr = from.String()
	}
	if to != nil {
		toStr = to.String()
	}
	return newEvent(source, cycleID, KindStateTransition, StateTransitionEvent{
		From:   fromStr,
		To:     toStr,
		Reason: reason,
	})
}

// NewToolDispatch builds a CycleEvent wrapping a ToolDispatchEvent. args may
// be nil; if non-nil it is expected to already be valid JSON.
func NewToolDispatch(source, cycleID, tool string, args json.RawMessage, duration time.Duration, err error) (CycleEvent, error) {
	return NewToolDispatchWithAttribution(source, cycleID, tool, "", args, duration, err)
}

// NewToolDispatchWithAttribution is like NewToolDispatch but also stamps an
// attribution subject onto the event (RFC-identity-embedding I1/I2). Use
// "anonymous" when no identity was presented; empty string is equivalent and
// omitted from the JSON wire form.
func NewToolDispatchWithAttribution(source, cycleID, tool, attribution string, args json.RawMessage, duration time.Duration, err error) (CycleEvent, error) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	return newEvent(source, cycleID, KindToolDispatch, ToolDispatchEvent{
		Tool:        tool,
		Args:        args,
		DurationMS:  duration.Milliseconds(),
		Error:       errStr,
		Attribution: attribution,
	})
}

// NewAssessment builds a CycleEvent wrapping an AssessmentEvent.
func NewAssessment(source, cycleID, action string, confidence float64, rationale string) (CycleEvent, error) {
	return newEvent(source, cycleID, KindAssessment, AssessmentEvent{
		Action:     action,
		Confidence: confidence,
		Rationale:  rationale,
	})
}

// newEvent marshals the given payload and wraps it in a CycleEvent with a
// freshly minted UUID v4 and the current wall-clock time.
func newEvent(source, cycleID string, kind Kind, payload any) (CycleEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return CycleEvent{}, fmt.Errorf("trace: marshal %s payload: %w", kind, err)
	}
	return CycleEvent{
		ID:        uuid.NewString(),
		Timestamp: time.Now().UTC(),
		Source:    source,
		CycleID:   cycleID,
		Kind:      kind,
		Payload:   raw,
	}, nil
}
