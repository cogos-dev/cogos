// gate.go — CogOS v3 attentional gate
//
// The gate receives events (perturbations) and routes them into the fovea.
// It decides:
//   - Which memory files should be elevated in salience as a result of this event
//   - Whether the event triggers a state transition in the process
//
// Stage 1: minimal routing — gate accepts events and records them.
// Stage 2+: gate will perform semantic matching against the attentional field.
package main

import (
	"sync"
	"time"
)

// GateEvent is an input to the attentional gate.
type GateEvent struct {
	// Type is the event category (e.g. "user.message", "tool.call", "heartbeat").
	Type string

	// Content is the raw content of the event (e.g. user message text).
	Content string

	// Timestamp records when the event arrived.
	Timestamp time.Time

	// SessionID is the originating session (empty for internal events).
	SessionID string

	// Data holds type-specific structured data.
	Data map[string]interface{}
}

// NewGateEventFromBlock builds a GateEvent from an ingress CogBlock while
// keeping GateEvent as the active process-routing primitive.
func NewGateEventFromBlock(block *CogBlock, eventType, content string) *GateEvent {
	if block == nil {
		return &GateEvent{
			Type:      eventType,
			Content:   content,
			Timestamp: time.Now().UTC(),
		}
	}

	return &GateEvent{
		Type:      eventType,
		Content:   content,
		Timestamp: block.Timestamp,
		SessionID: block.SessionID,
		Data: map[string]interface{}{
			"block_id": block.ID,
		},
	}
}

// GateResult is the gate's routing decision for an event.
type GateResult struct {
	// Elevated is the set of memory files to bring into the fovea for this event.
	Elevated []FileScore

	// StateTransition is the suggested next process state (empty = no change).
	StateTransition ProcessState

	// Accepted records whether the event was accepted into the fovea.
	Accepted bool
}

// Gate routes events into the attentional field.
type Gate struct {
	mu    sync.Mutex
	field *AttentionalField
	cfg   *Config
}

// NewGate constructs a Gate backed by the given attentional field.
func NewGate(field *AttentionalField, cfg *Config) *Gate {
	return &Gate{
		field: field,
		cfg:   cfg,
	}
}

// Process routes an event through the gate and returns a routing decision.
func (g *Gate) Process(evt *GateEvent) *GateResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	result := &GateResult{
		Accepted: true,
	}

	switch evt.Type {
	case "user.message", "tool.call", "tool.result":
		// External perturbation → Active state.
		result.StateTransition = StateActive
		// Stage 1: return top-10 fovea items as the elevated set.
		result.Elevated = g.field.Fovea(10)

	case "consolidation.tick":
		// Internal maintenance event → Consolidating state.
		result.StateTransition = StateConsolidating

	case "heartbeat":
		// Dormant heartbeat → stay Dormant (no transition).
		result.StateTransition = StateDormant

	default:
		result.StateTransition = StateReceptive
	}

	return result
}
