// serve_convergence.go — GET /v1/reconcile/convergence.
//
// The pull surface for per-provider reconcile anomalies: which providers are
// flagged, since when, for what reason, how many distinct episodes they have
// had, and whether the daemon has stopped actuating them.
//
// Why this endpoint exists as part of the log-noise change rather than after
// it: ReconcileDaemon.ProviderConvergence() already computed all of this, but
// had no HTTP route and no other production caller — it was reachable only
// from tests. So the ONLY live expression of "this provider is broken" was the
// repeated WARN in the log. Suppressing that repetition without shipping a
// readout would have removed the noise and the signal together, which is the
// failure this whole change is meant to avoid: a standing condition should
// live in a surface you can query, not in a line you have to keep re-reading.
//
// Read-only: reads the tracker snapshot and the daemon's quarantine map,
// mutates nothing, runs no reconcile cycle.
package engine

import (
	"encoding/json"
	"net/http"
	"time"
)

// reconcileConvergenceResponse is the GET /v1/reconcile/convergence body.
type reconcileConvergenceResponse struct {
	// Providers is every provider that has completed at least one cycle,
	// sorted by name.
	Providers []ProviderConvergence `json:"providers"`
	// FlaggedCount is how many providers currently have an OPEN anomaly
	// episode. This is the number that corresponds to "how many things are
	// wrong" — one persistent condition contributes exactly 1 for as long as
	// it lasts, however many log lines it has produced.
	FlaggedCount int `json:"flagged_count"`
	// QuarantinedCount is how many providers the daemon has stopped
	// actuating (they are still observed).
	QuarantinedCount int    `json:"quarantined_count"`
	Timestamp        string `json:"timestamp"`
}

// handleReconcileConvergence serves GET /v1/reconcile/convergence. If no
// ReconcileDaemon has been wired (SetReconcileDaemon never called), reports an
// empty provider list — the same "nothing observed" default the tracker itself
// returns before any cycle has completed.
func (s *Server) handleReconcileConvergence(w http.ResponseWriter, r *http.Request) {
	resp := reconcileConvergenceResponse{
		Providers: []ProviderConvergence{},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	if s.reconcileDaemon != nil {
		providers := s.reconcileDaemon.ProviderConvergence()
		if providers != nil {
			resp.Providers = providers
		}
		for _, p := range resp.Providers {
			if p.Flagged {
				resp.FlaggedCount++
			}
			if p.Quarantined {
				resp.QuarantinedCount++
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
