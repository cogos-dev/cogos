// bep_engine.go — root package shim.
// BEPEngine, PeerConnection, and EngineStatus have been extracted to
// internal/engine as part of Phase 2 S1 (issue #330).
// This file re-exports them as type aliases so that existing callers
// in root package main and its tests compile without modification.

package main

import (
	"github.com/myrgic/cogos/internal/engine"
)

// Type aliases — identical types at the Go language level.

type BEPEngine = engine.BEPEngine
type PeerConnection = engine.PeerConnection

// EngineStatus and PeerStatusSummary are already exported from
// pkg/substrate/bep (via the bep.EngineStatus / bep.PeerStatusSummary types
// that BEPEngine.Status() returns); no separate aliases needed here.

// NewBEPEngine forwards to the canonical constructor in internal/engine.
// Signature is identical to the former root implementation.
func NewBEPEngine(root string, config *BEPConfig, provider *BEPProvider) (*BEPEngine, error) {
	return engine.NewBEPEngine(root, config, provider)
}
