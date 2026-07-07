// serve_coherence.go — GET /v1/reconcile/coherence (First Instruments Module B, M1-B).
//
// Exposes the graded reconcile-drift score C_B computed from the
// ReconcileDaemon's cached per-provider Summary. Read-only: LastCoherence
// reads a cache populated as a side effect of the ordinary reconcile cycle
// (telemetry, IMPL-SPEC §0), runs no reconcile cycle itself, and mutates no
// kernel state.
package engine

import (
	"encoding/json"
	"net/http"
	"time"
)

// reconcileCoherenceResponse is the GET /v1/reconcile/coherence response
// body shape (IMPL-SPEC Module B "Surface").
type reconcileCoherenceResponse struct {
	CB               float64             `json:"c_b"`
	PerProviderDrift []ProviderCoherence `json:"per_provider_drift_fraction"`
	Timestamp        string              `json:"timestamp"`
}

// handleReconcileCoherence serves GET /v1/reconcile/coherence: the M1-B
// graded reconcile-drift score C_B plus per-provider detail. If no
// ReconcileDaemon has been wired (SetReconcileDaemon never called), reports
// C_B=1.0 with an empty detail slice — the same "nothing observed to have
// drifted" default ReconcileDaemon.LastCoherence itself returns when no
// cycle has completed yet.
func (s *Server) handleReconcileCoherence(w http.ResponseWriter, r *http.Request) {
	var cB float64 = 1.0
	var perProvider []ProviderCoherence
	if s.reconcileDaemon != nil {
		cB, perProvider = s.reconcileDaemon.LastCoherence()
	}

	resp := reconcileCoherenceResponse{
		CB:               cB,
		PerProviderDrift: perProvider,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
