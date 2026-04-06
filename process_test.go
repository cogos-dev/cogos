package main

import (
	"context"
	"testing"
	"time"
)

// ── State string tests ─────────────────────────────────────────────────────

func TestProcessStateString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state ProcessState
		want  string
	}{
		{StateActive, "active"},
		{StateReceptive, "receptive"},
		{StateConsolidating, "consolidating"},
		{StateDormant, "dormant"},
		{ProcessState(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("ProcessState(%d).String() = %q; want %q", tc.state, got, tc.want)
		}
	}
}

// ── Construction and initial state ────────────────────────────────────────

func TestNewProcessInitialState(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	nucleus := makeNucleus("Test", "tester")
	p := NewProcess(cfg, nucleus)

	if p.State() != StateReceptive {
		t.Errorf("initial state = %s; want receptive", p.State())
	}
	if p.SessionID() == "" {
		t.Error("SessionID is empty")
	}
	if p.Field() == nil {
		t.Error("Field() returned nil")
	}
	if p.Gate() == nil {
		t.Error("Gate() returned nil")
	}
}

// ── Send ──────────────────────────────────────────────────────────────────

func TestProcessSendAccepted(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	p := NewProcess(cfg, makeNucleus("T", "r"))

	evt := &GateEvent{Type: "user.message", Timestamp: time.Now()}
	if !p.Send(evt) {
		t.Error("Send returned false on non-full channel")
	}
}

func TestProcessSendFull(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	p := NewProcess(cfg, makeNucleus("T", "r"))

	// Fill the channel (capacity = 64).
	evt := &GateEvent{Type: "flood", Timestamp: time.Now()}
	for i := 0; i < 64; i++ {
		p.Send(evt)
	}
	// One more must be rejected.
	if p.Send(evt) {
		t.Error("Send returned true on full channel")
	}
}

// ── Run / cancel ──────────────────────────────────────────────────────────

func TestProcessRunCancels(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	p := NewProcess(cfg, makeNucleus("T", "r"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

// ── State transitions via external events ────────────────────────────────

// State transitions are tested synchronously via white-box calls to handleExternal.
// This avoids goroutine scheduling races without sacrificing coverage.

func TestProcessExternalEventTransitionsToActive(t *testing.T) {
	t.Parallel()
	p := NewProcess(makeConfig(t, t.TempDir()), makeNucleus("T", "r"))

	p.handleExternal(&GateEvent{Type: "user.message", Timestamp: time.Now()})

	if p.State() != StateActive {
		t.Errorf("after user.message: state = %s; want active", p.State())
	}
}

func TestProcessHeartbeatTransitionsToDormant(t *testing.T) {
	t.Parallel()
	p := NewProcess(makeConfig(t, t.TempDir()), makeNucleus("T", "r"))

	p.handleExternal(&GateEvent{Type: "heartbeat", Timestamp: time.Now()})

	if p.State() != StateDormant {
		t.Errorf("after heartbeat: state = %s; want dormant", p.State())
	}
}

func TestProcessConsolidationTransition(t *testing.T) {
	t.Parallel()
	p := NewProcess(makeConfig(t, t.TempDir()), makeNucleus("T", "r"))

	p.handleExternal(&GateEvent{Type: "consolidation.tick", Timestamp: time.Now()})

	if p.State() != StateConsolidating {
		t.Errorf("after consolidation.tick via gate: state = %s; want consolidating", p.State())
	}
}

func TestProcessAllFourStates(t *testing.T) {
	t.Parallel()
	p := NewProcess(makeConfig(t, t.TempDir()), makeNucleus("T", "r"))

	transitions := []struct {
		eventType string
		want      ProcessState
	}{
		{"user.message", StateActive},
		{"heartbeat", StateDormant},
		{"consolidation.tick", StateConsolidating},
		{"unknown.event", StateReceptive},
	}

	for _, tc := range transitions {
		p.handleExternal(&GateEvent{Type: tc.eventType, Timestamp: time.Now()})
		if p.State() != tc.want {
			t.Errorf("after %q: state = %s; want %s", tc.eventType, p.State(), tc.want)
		}
	}
}

// ── Ledger events recorded ────────────────────────────────────────────────

func TestProcessEmitsStartEvent(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "r"))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)

	// Ledger file must exist with at least one event.
	events := mustReadAllEvents(t, root, p.SessionID())
	if len(events) == 0 {
		t.Error("no events recorded in ledger")
	}
	if events[0].HashedPayload.Type != "process.start" {
		t.Errorf("first event type = %q; want process.start", events[0].HashedPayload.Type)
	}
}
