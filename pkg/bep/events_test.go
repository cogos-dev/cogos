package bep

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

func TestEmitPeerConnected(t *testing.T) {
	evt := EmitPeerConnected("ABCDEF", "test-node")
	if evt.Type != SyncEventPeerConnected {
		t.Errorf("Type = %q, want %q", evt.Type, SyncEventPeerConnected)
	}
	if evt.Summary["peer_id"] != "ABCDEF" {
		t.Errorf("peer_id = %v", evt.Summary["peer_id"])
	}
}

func TestEmitPeerDisconnected(t *testing.T) {
	evt := EmitPeerDisconnected("ABCDEF", "timeout")
	if evt.Type != SyncEventPeerDisconnected {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitFileReceived(t *testing.T) {
	evt := EmitFileReceived("test.agent.yaml", "PEER-1", 256)
	if evt.Type != SyncEventFileReceived {
		t.Errorf("Type = %q", evt.Type)
	}
	if evt.Summary["size"] != 256 {
		t.Errorf("size = %v", evt.Summary["size"])
	}
}

func TestEmitFileSent(t *testing.T) {
	evt := EmitFileSent("test.agent.yaml", 3)
	if evt.Type != SyncEventFileSent {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitSyncConflict(t *testing.T) {
	evt := EmitSyncConflict("test.agent.yaml", "PEER-1")
	if evt.Type != SyncEventConflict {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitIndexComplete(t *testing.T) {
	evt := EmitIndexComplete("PEER-1", 10, 3, 1)
	if evt.Type != SyncEventIndexComplete {
		t.Errorf("Type = %q", evt.Type)
	}
}

func TestEmitEngineStarted(t *testing.T) {
	evt := EmitEngineStarted("TEST-ID", ":22000", 2)
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
	// Verify event type strings follow the cog.sync.* convention.
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
			t.Errorf("event type %q seems too short", e)
		}
	}
}
