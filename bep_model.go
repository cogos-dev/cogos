// bep_model.go — root package shim.
// AgentSyncModel has been extracted to internal/engine as part of Phase 2 S1 (issue #330).
// This file re-exports it as a type alias so that existing callers in root
// package main and its tests compile without modification.

package main

import (
	"github.com/myrgic/cogos/internal/engine"
)

// AgentSyncModel is an alias for the canonical type in internal/engine.
type AgentSyncModel = engine.AgentSyncModel

// NewAgentSyncModel forwards to the canonical constructor in internal/engine.
func NewAgentSyncModel(eng *BEPEngine, watchDir, stateDir string, shortID uint64) *AgentSyncModel {
	return engine.NewAgentSyncModel(eng, watchDir, stateDir, shortID)
}
