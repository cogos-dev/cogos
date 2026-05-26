// bep_provider.go — root package shim.
// BEPProvider, BEPConfig, BEPPeer, and BEPSyncStatus have been extracted to
// internal/engine as part of Phase 2 S1 (issue #330).
// This file re-exports them as type aliases so that existing callers
// in root package main and its tests compile without modification.

package main

import (
	"github.com/myrgic/cogos/internal/engine"
	bepPkg "github.com/myrgic/cogos/pkg/substrate/bep"
)

// Type aliases — identical types at the Go language level.

type BEPProvider = engine.BEPProvider

// BEPConfig, BEPPeer, BEPSyncStatus are aliases defined in internal/engine
// as aliases to pkg/substrate/bep types (bep.Config, bep.Peer, bep.SyncStatus).
// Re-export them here for root package use.
type BEPConfig = engine.BEPConfig
type BEPPeer = engine.BEPPeer
type BEPSyncStatus = engine.BEPSyncStatus

// NewBEPProvider forwards to the canonical constructor in internal/engine.
func NewBEPProvider(root string) *BEPProvider {
	return engine.NewBEPProvider(root)
}

// isAgentCRDFile returns true if the filename matches the agent CRD naming convention.
// Forwarded to pkg/substrate/bep.IsAgentCRDFile for root package consumers.
func isAgentCRDFile(name string) bool {
	return bepPkg.IsAgentCRDFile(name)
}
