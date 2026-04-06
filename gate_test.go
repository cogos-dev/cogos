package main

import (
	"testing"
	"time"
)

func newTestGate(t *testing.T) *Gate {
	t.Helper()
	cfg := makeConfig(t, t.TempDir())
	field := NewAttentionalField(cfg)
	return NewGate(field, cfg)
}

func TestGateUserMessageTransitionsToActive(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "user.message",
		Content:   "hello",
		Timestamp: time.Now(),
	})

	if !result.Accepted {
		t.Error("expected event to be accepted")
	}
	if result.StateTransition != StateActive {
		t.Errorf("StateTransition = %s; want active", result.StateTransition)
	}
}

func TestGateToolCallTransitionsToActive(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "tool.call",
		Timestamp: time.Now(),
	})

	if result.StateTransition != StateActive {
		t.Errorf("tool.call StateTransition = %s; want active", result.StateTransition)
	}
}

func TestGateToolResultTransitionsToActive(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "tool.result",
		Timestamp: time.Now(),
	})

	if result.StateTransition != StateActive {
		t.Errorf("tool.result StateTransition = %s; want active", result.StateTransition)
	}
}

func TestGateConsolidationTickTransitionsToConsolidating(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "consolidation.tick",
		Timestamp: time.Now(),
	})

	if result.StateTransition != StateConsolidating {
		t.Errorf("StateTransition = %s; want consolidating", result.StateTransition)
	}
}

func TestGateHeartbeatTransitionsToDormant(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "heartbeat",
		Timestamp: time.Now(),
	})

	if result.StateTransition != StateDormant {
		t.Errorf("StateTransition = %s; want dormant", result.StateTransition)
	}
}

func TestGateUnknownEventTransitionsToReceptive(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	result := g.Process(&GateEvent{
		Type:      "something.unknown",
		Timestamp: time.Now(),
	})

	if result.StateTransition != StateReceptive {
		t.Errorf("unknown event StateTransition = %s; want receptive", result.StateTransition)
	}
}

func TestGateAlwaysAccepts(t *testing.T) {
	t.Parallel()
	g := newTestGate(t)

	types := []string{"user.message", "tool.call", "heartbeat", "consolidation.tick", "unknown"}
	for _, typ := range types {
		result := g.Process(&GateEvent{Type: typ, Timestamp: time.Now()})
		if !result.Accepted {
			t.Errorf("event type %q was not accepted", typ)
		}
	}
}
