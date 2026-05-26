// bep_events.go — root package shim + bus bridge.
// SyncEvent, sync event constants, and emit helpers have been extracted to
// pkg/substrate/bep as part of Phase 2 S1 (issue #330).
// This file re-exports them as aliases/wrappers for root package consumers,
// and retains SyncEventToBusData which depends on the root-only *CogBlock type.

package main

import (
	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Type alias ─────────────────────────────────────────────────────────────────

// SyncEvent is an alias for the canonical type in pkg/substrate/bep.
type SyncEvent = bep.SyncEvent

// ─── Sync event type constants ──────────────────────────────────────────────────

const (
	SyncEventPeerConnected    = bep.SyncEventPeerConnected
	SyncEventPeerDisconnected = bep.SyncEventPeerDisconnected
	SyncEventFileReceived     = bep.SyncEventFileReceived
	SyncEventFileSent         = bep.SyncEventFileSent
	SyncEventConflict         = bep.SyncEventConflict
	SyncEventIndexComplete    = bep.SyncEventIndexComplete
	SyncEventEngineStarted    = bep.SyncEventEngineStarted
	SyncEventEngineStopped    = bep.SyncEventEngineStopped
)

// ─── Emit helpers (forwarded to pkg/substrate/bep) ──────────────────────────────

var EmitSyncEvent       = bep.EmitSyncEvent
var EmitPeerConnected   = bep.EmitPeerConnected
var EmitPeerDisconnected = bep.EmitPeerDisconnected
var EmitFileReceived    = bep.EmitFileReceived
var EmitFileSent        = bep.EmitFileSent
var EmitSyncConflict    = bep.EmitSyncConflict
var EmitIndexComplete   = bep.EmitIndexComplete
var EmitEngineStarted   = bep.EmitEngineStarted
var EmitEngineStopped   = bep.EmitEngineStopped

// ─── Bus bridge (root-only; depends on *CogBlock) ───────────────────────────────

// SyncEventToBusData converts a SyncEvent to a CogBlock for bus forwarding.
// Remains in root package because CogBlock is defined here.
func SyncEventToBusData(evt SyncEvent) *CogBlock {
	return &CogBlock{
		Type:    evt.Type,
		Payload: evt.Summary,
	}
}
