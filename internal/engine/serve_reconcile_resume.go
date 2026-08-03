// serve_reconcile_resume.go — POST /v1/reconcile/{type}/resume.
//
// The operator entrance to ReconcileDaemon.Resume(providerType) that the
// quarantine recovery hint (reconcile_daemon.go's quarantineRecoveryHint)
// tells operators about. Before this route, Resume was reachable only from
// tests: `cogos reconcile <type>` (cli_reconcile.go) runs its own
// out-of-process LoadConfig→FetchLive→ComputePlan→ApplyPlan cycle and never
// touches the live daemon's d.quarantined/d.backoff state, so a live
// daemon's quarantine could not actually be lifted from the outside — the
// hint's "resume immediately" claim had no real lever behind it. This route
// is that lever.
//
// Gated by Config.EnableReconcileControl (default false), the same
// opt-in-default-off pattern as EnableSkillExec (serve_skills.go) /
// EnableServiceControl (serve_services.go) / EnableConfigMutation
// (serve_config.go) — see boot_bindaddr_warning.go: any local process on the
// same host could otherwise lift quarantine and force an immediate reconcile
// cycle over loopback.
package engine

import (
	"encoding/json"
	"net/http"
)

// reconcileResumeResponse is the POST /v1/reconcile/{type}/resume response
// body. Resume() itself returns nothing (it is a fire-and-forget operator
// lever — see its doc comment), so this reports the provider's convergence
// state immediately after the call: quarantine is lifted synchronously
// inside Resume, so Provider.Quarantined already reads false even though the
// triggered cycle itself runs asynchronously.
type reconcileResumeResponse struct {
	Success      bool                 `json:"success"`
	ProviderType string               `json:"provider_type"`
	Error        string               `json:"error,omitempty"`
	Provider     *ProviderConvergence `json:"provider,omitempty"`
}

// requireReconcileControl checks the gate and writes a 403 if disabled.
// Returns true if the handler should continue, false if it has already
// written an error response. Mirrors requireServiceControl (serve_services.go)
// / requireConfigMutation (serve_config.go).
func (s *Server) requireReconcileControl(w http.ResponseWriter) bool {
	if s.cfg == nil || !s.cfg.EnableReconcileControl {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":  "disabled",
			"detail": "reconcile resume via HTTP is disabled; set enable_reconcile_control: true in kernel.yaml",
		})
		return false
	}
	return true
}

// handleReconcileResume serves POST /v1/reconcile/{type}/resume: lifts
// providerType's quarantine and queues an immediate cycle
// (ReconcileDaemon.Resume), then reports the provider's post-resume
// convergence state.
//
//	200 → { success: true, provider_type: "...", provider: {...} }
//	403 → reconcile control is disabled (EnableReconcileControl=false)
//	404 → no ReconcileDaemon wired, or providerType is not a registered provider
func (s *Server) handleReconcileResume(w http.ResponseWriter, r *http.Request) {
	if !s.requireReconcileControl(w) {
		return
	}

	providerType := r.PathValue("type")
	w.Header().Set("Content-Type", "application/json")

	if s.reconcileDaemon == nil || !s.reconcileDaemon.hasProvider(providerType) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(reconcileResumeResponse{
			Success:      false,
			ProviderType: providerType,
			Error:        "provider not found: " + providerType,
		})
		return
	}

	s.reconcileDaemon.Resume(providerType)

	var providerView *ProviderConvergence
	for _, p := range s.reconcileDaemon.ProviderConvergence() {
		if p.Provider == providerType {
			pCopy := p
			providerView = &pCopy
			break
		}
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(reconcileResumeResponse{
		Success:      true,
		ProviderType: providerType,
		Provider:     providerView,
	})
}
