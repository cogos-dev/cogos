// serve_rates.go — GET /v1/kernel/rates (First Instruments Module C, M3).
//
// Side-effect-free: pure read of config plus the daemon's resolved
// PollInterval; no mutation, no ticker interaction.
package engine

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleKernelRates serves GET /v1/kernel/rates: the deterministic-echo
// 15-ratio table over the kernel's six clock constants (KernelRates). Uses
// the live ReconcileDaemon's actual PollInterval when one is wired
// (SetReconcileDaemon), so the ratio table reflects the running daemon's
// real cadence rather than a static guess.
func (s *Server) handleKernelRates(w http.ResponseWriter, r *http.Request) {
	var pollInterval time.Duration
	if s.reconcileDaemon != nil {
		pollInterval = s.reconcileDaemon.PollInterval()
	}

	report := KernelRates(s.cfg, pollInterval)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}
