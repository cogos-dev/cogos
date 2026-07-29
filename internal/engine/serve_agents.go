package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type triggerAgentHTTPInput struct {
	Reason string `json:"reason"`
	Wait   bool   `json:"wait"`
}

// agentDispatchHTTPInput mirrors dispatchToHarnessInput's async field onto
// the HTTP surface (POST /v1/agents/{id}/dispatch) — sibling-path discipline
// with the MCP tool's Async parameter. Embeds DispatchRequest so all other
// fields decode exactly as before; Async is the only addition.
type agentDispatchHTTPInput struct {
	DispatchRequest
	Async bool `json:"async,omitempty"`
}

type agentStatusCompat struct {
	AgentSummary
	Uptime          string                `json:"uptime,omitempty"`
	Activity        *AgentActivitySummary `json:"activity,omitempty"`
	Memory          []AgentMemoryEntry    `json:"memory,omitempty"`
	Proposals       []AgentProposalEntry  `json:"proposals,omitempty"`
	Inbox           *AgentInboxSummary    `json:"inbox,omitempty"`
	LastObservation string                `json:"last_observation,omitempty"`
	IdentityRef     string                `json:"identity_ref,omitempty"`
}

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	s.route(mux, "GET /v1/agents", s.handleAgentsList)
	s.route(mux, "GET /v1/agents/{id}", s.handleAgentGet)
	s.route(mux, "POST /v1/agents/{id}/tick", s.handleAgentTick)
	s.route(mux, "POST /v1/agents/{id}/dispatch", s.handleAgentDispatch)
	s.route(mux, "GET /v1/dispatch-jobs/{id}", s.handleDispatchJobGet)

	// Legacy dashboard routes.
	s.route(mux, "GET /v1/agent/status", s.handleAgentStatusCompat)
	s.route(mux, "GET /v1/agent/traces", s.handleAgentTracesCompat)
	s.route(mux, "POST /v1/agent/trigger", s.handleAgentTriggerCompat)
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	resp, err := QueryListAgents(r.Context(), s.agentController, ListAgentsRequest{})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAgentGet(w http.ResponseWriter, r *http.Request) {
	snap, err := QueryGetAgent(r.Context(), s.agentController, GetAgentRequest{
		AgentID:      r.PathValue("id"),
		IncludeTrace: r.URL.Query().Get("include_trace") == "true",
		TraceLimit:   parseIntDefault(r.URL.Query().Get("trace_limit"), 0),
	})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, snap)
}

func (s *Server) handleAgentTick(w http.ResponseWriter, r *http.Request) {
	var input triggerAgentHTTPInput
	_ = json.NewDecoder(r.Body).Decode(&input)
	resp, err := QueryTriggerAgent(r.Context(), s.agentController, TriggerAgentRequest{
		AgentID: r.PathValue("id"),
		Reason:  input.Reason,
		Wait:    input.Wait,
	})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAgentDispatch(w http.ResponseWriter, r *http.Request) {
	var wrapped agentDispatchHTTPInput
	if err := json.NewDecoder(r.Body).Decode(&wrapped); err != nil {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid_json",
		})
		return
	}
	input := wrapped.DispatchRequest
	if input.AgentID == "" {
		input.AgentID = r.PathValue("id")
	}
	// Kernel policy, not a caller parameter: overwrite any client-supplied
	// cap with this node's configured dispatch_timeout_cap_seconds
	// (default 600). Nil-receiver-safe.
	input.TimeoutCapSeconds = s.cfg.DispatchTimeoutCap()

	if wrapped.Async {
		if s.mcpServer == nil {
			writeAgentJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "async dispatch requires the MCP server (job registry) to be wired",
				"code":  "unavailable",
			})
			return
		}
		receipt := s.mcpServer.startAsyncDispatch(r.Context(), input)
		writeAgentJSON(w, http.StatusAccepted, receipt)
		return
	}

	// Cluster-aware routing (Phase 2 S4), mirrored from toolDispatchToHarness
	// in mcp_server.go: pass the server's BEP engine as the router so a
	// target_node dispatch forwards over the authenticated BEP channel
	// instead of always failing fast with cluster_disabled. Nil-safe by
	// construction — router is only ever assigned a genuinely non-nil
	// *BEPEngine, so a dark cluster (s.bepEngine == nil) leaves router as a
	// true nil interface rather than a non-nil interface wrapping a nil
	// pointer (the classic Go footgun QueryDispatchToHarnessRouted's
	// `router == nil` check would otherwise miss).
	var router RemoteDispatchRouter
	if s.bepEngine != nil {
		router = s.bepEngine
	}
	resp, err := QueryDispatchToHarnessRouted(r.Context(), s.agentController, router, input)
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, resp)
}

// handleDispatchJobGet implements GET /v1/dispatch-jobs/{id} — the primary
// poll surface for an async cog_dispatch_to_harness job (mirrored by the
// cog_poll_dispatch MCP tool for callers without HTTP access).
func (s *Server) handleDispatchJobGet(w http.ResponseWriter, r *http.Request) {
	if s.mcpServer == nil {
		writeAgentJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "dispatch job registry is not wired (MCP server not configured)",
			"code":  "unavailable",
		})
		return
	}
	jobID := r.PathValue("id")
	rec, ok := s.mcpServer.dispatchJobs.Get(jobID)
	if !ok {
		writeAgentJSON(w, http.StatusNotFound, map[string]any{
			"error": fmt.Sprintf("no such dispatch job %q", jobID),
			"code":  "not_found",
		})
		return
	}
	writeAgentJSON(w, http.StatusOK, dispatchJobStatusFromRecord(rec))
}

func (s *Server) handleAgentStatusCompat(w http.ResponseWriter, r *http.Request) {
	snap, err := QueryGetAgent(r.Context(), s.agentController, GetAgentRequest{
		AgentID:      DefaultAgentID,
		IncludeTrace: true,
		TraceLimit:   20,
	})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	body := agentStatusCompat{
		AgentSummary:    snap.Summary,
		Uptime:          secondsToDuration(snap.Summary.UptimeSec),
		Activity:        snap.Activity,
		Memory:          snap.Memory,
		Proposals:       snap.Proposals,
		Inbox:           snap.Inbox,
		LastObservation: snap.LastObservation,
		IdentityRef:     snap.IdentityRef,
	}
	writeAgentJSON(w, http.StatusOK, body)
}

func (s *Server) handleAgentTracesCompat(w http.ResponseWriter, r *http.Request) {
	snap, err := QueryGetAgent(r.Context(), s.agentController, GetAgentRequest{
		AgentID:      DefaultAgentID,
		IncludeTrace: true,
		TraceLimit:   20,
	})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, snap.Traces)
}

func (s *Server) handleAgentTriggerCompat(w http.ResponseWriter, r *http.Request) {
	var input triggerAgentHTTPInput
	_ = json.NewDecoder(r.Body).Decode(&input)
	resp, err := QueryTriggerAgent(r.Context(), s.agentController, TriggerAgentRequest{
		AgentID: DefaultAgentID,
		Reason:  input.Reason,
		Wait:    input.Wait,
	})
	if err != nil {
		writeAgentHTTPError(w, err)
		return
	}
	writeAgentJSON(w, http.StatusOK, resp)
}

func writeAgentHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]any{
		"error": err.Error(),
	}
	var aerr *AgentControllerError
	switch {
	case errors.As(err, &aerr):
		switch aerr.Code {
		case "invalid_input":
			status = http.StatusBadRequest
		case "not_found":
			status = http.StatusNotFound
		case "unavailable":
			status = http.StatusServiceUnavailable
		}
		body["code"] = aerr.Code
	case errors.Is(err, ErrAgentUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, ErrAgentNotFound):
		status = http.StatusNotFound
	}
	writeAgentJSON(w, status, body)
}

func writeAgentJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func parseIntDefault(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return fallback
	}
	return v
}

func secondsToDuration(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return (time.Duration(sec) * time.Second).String()
}
