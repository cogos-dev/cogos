// bep_events.go — cog.sync.* bus event types and emission helpers for BEP transport.
// Follows the pattern established in reconcile_events.go.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ─── Sync event type constants ──────────────────────────────────────────────────

const (
	SyncEventPeerConnected    = "cog.sync.peer.connected"
	SyncEventPeerDisconnected = "cog.sync.peer.disconnected"
	SyncEventFileReceived     = "cog.sync.file.received"
	SyncEventFileSent         = "cog.sync.file.sent"
	SyncEventConflict         = "cog.sync.conflict"
	SyncEventIndexComplete    = "cog.sync.index.complete"
	SyncEventEngineStarted    = "cog.sync.engine.started"
	SyncEventEngineStopped    = "cog.sync.engine.stopped"
)

// ─── SyncEvent struct ───────────────────────────────────────────────────────────

// SyncEvent is the structured payload for BEP sync lifecycle events.
type SyncEvent struct {
	Type      string         `json:"event"`
	Timestamp string         `json:"timestamp,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// ─── Emission ───────────────────────────────────────────────────────────────────

// EmitSyncEvent creates a SyncEvent, logs it to stderr, and returns it.
func EmitSyncEvent(eventType string, summary map[string]any) SyncEvent {
	evt := SyncEvent{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary:   summary,
	}

	data, err := json.Marshal(evt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[bep-sync] event=%s error=marshal_failed\n", eventType)
		return evt
	}
	fmt.Fprintf(os.Stderr, "[bep-sync] %s\n", string(data))
	return evt
}

// ─── Typed helpers ──────────────────────────────────────────────────────────────

func EmitPeerConnected(peerID, peerName string) SyncEvent {
	return EmitSyncEvent(SyncEventPeerConnected, map[string]any{
		"peer_id": peerID,
		"name":    peerName,
	})
}

func EmitPeerDisconnected(peerID, reason string) SyncEvent {
	return EmitSyncEvent(SyncEventPeerDisconnected, map[string]any{
		"peer_id": peerID,
		"reason":  reason,
	})
}

func EmitFileReceived(filename, peerID string, size int) SyncEvent {
	return EmitSyncEvent(SyncEventFileReceived, map[string]any{
		"file":    filename,
		"peer_id": peerID,
		"size":    size,
	})
}

func EmitFileSent(filename string, peerCount int) SyncEvent {
	return EmitSyncEvent(SyncEventFileSent, map[string]any{
		"file":       filename,
		"peer_count": peerCount,
	})
}

func EmitSyncConflict(filename, peerID string) SyncEvent {
	return EmitSyncEvent(SyncEventConflict, map[string]any{
		"file":    filename,
		"peer_id": peerID,
	})
}

func EmitIndexComplete(peerID string, fileCount, toRequest, conflicts int) SyncEvent {
	return EmitSyncEvent(SyncEventIndexComplete, map[string]any{
		"peer_id":    peerID,
		"files":      fileCount,
		"to_request": toRequest,
		"conflicts":  conflicts,
	})
}

func EmitEngineStarted(deviceID string, listenAddr string, peerCount int) SyncEvent {
	return EmitSyncEvent(SyncEventEngineStarted, map[string]any{
		"device_id":   deviceID,
		"listen_addr": listenAddr,
		"peer_count":  peerCount,
	})
}

func EmitEngineStopped(reason string) SyncEvent {
	return EmitSyncEvent(SyncEventEngineStopped, map[string]any{
		"reason": reason,
	})
}

// ─── Bus forwarding ─────────────────────────────────────────────────────────────

// SyncEventToBusData converts a SyncEvent to a CogBlock for bus forwarding.
func SyncEventToBusData(evt SyncEvent) *CogBlock {
	return &CogBlock{
		Type:    evt.Type,
		Payload: evt.Summary,
	}
}
