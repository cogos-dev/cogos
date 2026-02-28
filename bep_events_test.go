package main

import (
	"testing"
)

func TestEmitSyncEvent(t *testing.T) {
	evt := EmitSyncEvent(SyncEventEngineStarted, map[string]any{
		"device_id":   "TEST-ID",
		"listen_addr": ":22000",
		"peer_count":  0,
	})

	if evt.Type != SyncEventEngineStarted {
		t.Errorf("Type = %q, want %q", evt.Type, SyncEventEngineStarted)
	}
	if evt.Timestamp == "" {
		t.Error("Timestamp should not be empty")
	}
	if evt.Summary["device_id"] != "TEST-ID" {
		t.Errorf("Summary[device_id] = %v", evt.Summary["device_id"])
	}
}

func TestSyncEventToBusData(t *testing.T) {
	evt := SyncEvent{
		Type:    SyncEventPeerConnected,
		Summary: map[string]any{"peer_id": "ABC", "name": "node-a"},
	}

	bus := SyncEventToBusData(evt)
	if bus.Type != SyncEventPeerConnected {
		t.Errorf("bus Type = %q", bus.Type)
	}
	if bus.Payload["peer_id"] != "ABC" {
		t.Errorf("bus Payload[peer_id] = %v", bus.Payload["peer_id"])
	}
}

func TestEmitPeerConnected(t *testing.T) {
	evt := EmitPeerConnected("ABCDEFG", "node-a")
	if evt.Type != SyncEventPeerConnected {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitPeerDisconnected(t *testing.T) {
	evt := EmitPeerDisconnected("ABCDEFG", "timeout")
	if evt.Type != SyncEventPeerDisconnected {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitFileReceived(t *testing.T) {
	evt := EmitFileReceived("test.agent.yaml", "ABCDEFG", 1024)
	if evt.Type != SyncEventFileReceived {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitFileSent(t *testing.T) {
	evt := EmitFileSent("test.agent.yaml", 2)
	if evt.Type != SyncEventFileSent {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitSyncConflict(t *testing.T) {
	evt := EmitSyncConflict("test.agent.yaml", "ABCDEFG")
	if evt.Type != SyncEventConflict {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitIndexComplete(t *testing.T) {
	evt := EmitIndexComplete("ABCDEFG", 10, 3, 1)
	if evt.Type != SyncEventIndexComplete {
		t.Errorf("Type = %q", evt.Type)
	}
	if evt.Summary["files"] != 10 {
		t.Errorf("files = %v", evt.Summary["files"])
	}
}

func TestEmitEngineStarted(t *testing.T) {
	evt := EmitEngineStarted("TEST-ID", ":22000", 0)
	if evt.Type != SyncEventEngineStarted {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitEngineStopped(t *testing.T) {
	evt := EmitEngineStopped("shutdown")
	if evt.Type != SyncEventEngineStopped {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestSyncEventConstants(t *testing.T) {
	// Verify event type naming convention.
	events := []string{
		SyncEventPeerConnected,
		SyncEventPeerDisconnected,
		SyncEventFileReceived,
		SyncEventFileSent,
		SyncEventConflict,
		SyncEventIndexComplete,
		SyncEventEngineStarted,
		SyncEventEngineStopped,
	}
	for _, e := range events {
		if len(e) < 10 {
			t.Errorf("event type too short: %q", e)
		}
		if e[:9] != "cog.sync." {
			t.Errorf("event type should start with 'cog.sync.': %q", e)
		}
	}
}
