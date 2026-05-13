// serve_sessions_mgmt.go — kernel-native HTTP surface for session & handoff
// management. The 8 routes below are the "invariance layer" of the hybrid
// design: they validate inputs, enforce the state machine via
// SessionRegistry / HandoffRegistry, and only then append to the bus.
//
// Routes registered here:
//
//	POST /v1/sessions/register                — register (or idempotently
//	                                            update) a session
//	POST /v1/sessions/{id}/heartbeat          — bump last_seen + optional
//	                                            status fields
//	POST /v1/sessions/{id}/end                — mark ended; optional
//	                                            handoff_id hand-off-ref
//	GET  /v1/sessions/presence                — roster of tracked sessions
//	                                            (optional active_within_seconds)
//
//	POST /v1/handoffs/offer                   — post an offer (kernel mints
//	                                            handoff_id if missing)
//	GET  /v1/handoffs                         — list handoffs, optional
//	                                            state / for_session filters
//	POST /v1/handoffs/{id}/claim              — atomic first-wins claim;
//	                                            emits handoff.claim_rejected
//	                                            on failure (amendment #4)
//	POST /v1/handoffs/{id}/complete           — mark claimed offer completed
//
// Note: the existing routes `/v1/sessions` and `/v1/sessions/{id}[/context]`
// registered by serve_bus.go are preserved untouched (they serve per-turn
// TAA inference context, a different concern). Go 1.22 pattern precedence
// resolves `POST /v1/sessions/register` before `GET /v1/sessions/` because
// the method+pattern is more specific.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// registerSessionMgmtRoutes attaches the 8 new routes onto mux. Called from
// NewServer after registerBusRoutes so specific patterns land cleanly
// alongside the pre-existing `/v1/sessions` GET surface.
func (s *Server) registerSessionMgmtRoutes(mux *http.ServeMux) {
	// Sessions (specific-method first).
	s.route(mux, "POST /v1/sessions/register", s.handleSessionRegister)
	s.route(mux, "GET /v1/sessions/presence", s.handleSessionPresence)
	s.route(mux, "POST /v1/sessions/{id}/heartbeat", s.handleSessionHeartbeat)
	s.route(mux, "POST /v1/sessions/{id}/end", s.handleSessionEnd)
	// RFC-0005: session fork.
	s.route(mux, "POST /v1/sessions/{id}/fork", s.handleSessionFork)

	// Handoffs.
	s.route(mux, "POST /v1/handoffs/offer", s.handleHandoffOffer)
	s.route(mux, "GET /v1/handoffs", s.handleHandoffList)
	s.route(mux, "POST /v1/handoffs/{id}/claim", s.handleHandoffClaim)
	s.route(mux, "POST /v1/handoffs/{id}/complete", s.handleHandoffComplete)
}

// ─── error helpers ───────────────────────────────────────────────────────────

// writeJSONError mirrors the existing /v1/sessions/{id}[/context] error shape
// so clients see a uniform envelope across the whole /v1/sessions family.
func writeJSONError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    kind,
			"message": msg,
		},
	})
}

// writeJSONResp writes a JSON body with the given status. Kept distinct from
// writeJSON in tests to avoid collision with test-file helpers.
func writeJSONResp(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ─── POST /v1/sessions/register ──────────────────────────────────────────────

type sessionRegisterRequest struct {
	SessionID    string                 `json:"session_id"`
	Workspace    string                 `json:"workspace"`
	Role         string                 `json:"role"`
	Task         string                 `json:"task,omitempty"`
	Model        string                 `json:"model,omitempty"`
	Hostname     string                 `json:"hostname,omitempty"`
	ContextUsage float64                `json:"context_usage,omitempty"`
	Status       string                 `json:"status,omitempty"`
	CurrentTask  string                 `json:"current_task,omitempty"`
	Extras       map[string]interface{} `json:"extras,omitempty"`
}

type sessionWriteResponse struct {
	OK        bool          `json:"ok"`
	SessionID string        `json:"session_id,omitempty"`
	Seq       int           `json:"seq,omitempty"`
	Hash      string        `json:"hash,omitempty"`
	Created   bool          `json:"created,omitempty"`
	Session   *SessionState `json:"session,omitempty"`
}

func (s *Server) handleSessionRegister(w http.ResponseWriter, r *http.Request) {
	var req sessionRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	if err := ValidateSessionID(req.SessionID); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Workspace == "" || req.Role == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"workspace and role are required")
		return
	}
	now := time.Now().UTC()
	state := SessionState{
		SessionID:    req.SessionID,
		Workspace:    req.Workspace,
		Role:         req.Role,
		Task:         req.Task,
		Model:        req.Model,
		Hostname:     req.Hostname,
		ContextUsage: req.ContextUsage,
		Status:       req.Status,
		CurrentTask:  req.CurrentTask,
		Extras:       req.Extras,
		RegisteredAt: now,
		LastSeen:     now,
	}
	// Build the base payload. registered_at is intentionally omitted here;
	// it is written inside the appendFn once ApplyRegister has determined
	// the canonical value (which for a re-register of an active session is
	// the original T1, not `now`). Fixes #44: bus event payload must agree
	// with the in-memory SessionState.RegisteredAt.
	payload := map[string]interface{}{
		"session_id":    req.SessionID,
		"workspace":     req.Workspace,
		"role":          req.Role,
		"task":          req.Task,
		"model":         req.Model,
		"hostname":      req.Hostname,
		"status":        req.Status,
		"current_task":  req.CurrentTask,
		"context_usage": req.ContextUsage,
	}
	if req.Extras != nil {
		for k, v := range req.Extras {
			if _, exists := payload[k]; !exists {
				payload[k] = v
			}
		}
	}
	// Validation has already happened above (ValidateSessionID + required
	// fields), so the only error ApplyRegister can return now is from
	// appendFn — i.e. bus-append failure, which is always a 500.
	var evt *BusBlock
	appendFn := func(canonicalRegisteredAt time.Time) error {
		payload["registered_at"] = canonicalRegisteredAt.Format(time.RFC3339Nano)
		var err error
		evt, err = s.busSessions.AppendEvent(BusSessions, EvtSessionRegister, req.SessionID, payload)
		return err
	}
	stored, created, err := s.sessionRegistry.ApplyRegister(
		state,
		time.Duration(defaultActiveWithinSeconds)*time.Second,
		now,
		appendFn,
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", err.Error())
		return
	}
	writeJSONResp(w, http.StatusOK, sessionWriteResponse{
		OK: true, SessionID: req.SessionID,
		Seq: evt.Seq, Hash: evt.Hash,
		Created: created, Session: stored,
	})
}

// ─── POST /v1/sessions/{id}/heartbeat ────────────────────────────────────────

type sessionHeartbeatRequest struct {
	Status       string  `json:"status,omitempty"`
	ContextUsage float64 `json:"context_usage,omitempty"`
	CurrentTask  string  `json:"current_task,omitempty"`
}

func (s *Server) handleSessionHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := ValidateSessionID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req sessionHeartbeatRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
			return
		}
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"session_id":    id,
		"status":        req.Status,
		"context_usage": req.ContextUsage,
		"current_task":  req.CurrentTask,
		"at":            now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = s.busSessions.AppendEvent(BusSessions, EvtSessionHeartbeat, id, payload)
		return err
	}
	stored, ok, err := s.sessionRegistry.ApplyHeartbeat(
		id, req.ContextUsage, req.Status, req.CurrentTask, now, appendFn,
	)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("session %q is not registered", id))
		return
	}
	if err != nil {
		// Two shapes of err land here:
		//   1. "session already ended" — the registry was NOT mutated
		//      (LastSeen unchanged) and no bus event was appended.
		//      Return 409.
		//   2. bus append failure — also no mutation. Return 500.
		if stored != nil && stored.Ended && evt == nil {
			writeJSONError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("session %q is already ended", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", err.Error())
		return
	}
	writeJSONResp(w, http.StatusOK, sessionWriteResponse{
		OK: true, SessionID: id,
		Seq: evt.Seq, Hash: evt.Hash, Session: stored,
	})
}

// ─── POST /v1/sessions/{id}/end ──────────────────────────────────────────────

type sessionEndRequest struct {
	Reason    string `json:"reason,omitempty"`
	HandoffID string `json:"handoff_id,omitempty"`
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := ValidateSessionID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req sessionEndRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
			return
		}
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"session_id": id,
		"reason":     req.Reason,
		"handoff_id": req.HandoffID,
		"ended_at":   now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = s.busSessions.AppendEvent(BusSessions, EvtSessionEnd, id, payload)
		return err
	}
	stored, known, err := s.sessionRegistry.ApplyEnd(id, req.Reason, req.HandoffID, now, appendFn)
	if !known {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("session %q is not registered", id))
		return
	}
	if err != nil {
		// "already ended" sentinel → 409. Bus-append failure → 500.
		// Disambiguate by whether evt was populated.
		if evt == nil && stored != nil && stored.Ended {
			writeJSONError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", err.Error())
		return
	}
	writeJSONResp(w, http.StatusOK, sessionWriteResponse{
		OK: true, SessionID: id, Seq: evt.Seq, Hash: evt.Hash, Session: stored,
	})
}

// ─── GET /v1/sessions/presence ───────────────────────────────────────────────

type sessionPresenceEntry struct {
	*SessionState
	Active bool `json:"active"`
}

func (s *Server) handleSessionPresence(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	window := time.Duration(defaultActiveWithinSeconds) * time.Second
	if raw := r.URL.Query().Get("active_within_seconds"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	includeEnded := r.URL.Query().Get("include_ended") == "true"

	snap := s.sessionRegistry.Snapshot()
	out := make([]sessionPresenceEntry, 0, len(snap))
	for _, row := range snap {
		if !includeEnded && row.Ended {
			continue
		}
		entry := sessionPresenceEntry{
			SessionState: row,
			Active:       row.IsActive(window, now),
		}
		out = append(out, entry)
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"sessions": out,
		"count":    len(out),
	})
}

// ─── POST /v1/handoffs/offer ─────────────────────────────────────────────────

type handoffOfferRequest struct {
	HandoffID       string                   `json:"handoff_id,omitempty"`
	FromSession     string                   `json:"from_session"`
	ToSession       string                   `json:"to_session,omitempty"`
	Reason          string                   `json:"reason,omitempty"`
	TTLSeconds      int                      `json:"ttl_seconds,omitempty"`
	Task            map[string]interface{}   `json:"task"`
	BootstrapPrompt string                   `json:"bootstrap_prompt"`
	BusContextRefs  []map[string]interface{} `json:"bus_context_refs,omitempty"`
	MemoryRefs      []string                 `json:"memory_refs,omitempty"`
}

type handoffWriteResponse struct {
	OK        bool          `json:"ok"`
	HandoffID string        `json:"handoff_id,omitempty"`
	Seq       int           `json:"seq,omitempty"`
	Hash      string        `json:"hash,omitempty"`
	Handoff   *HandoffState `json:"handoff,omitempty"`
}

func (s *Server) handleHandoffOffer(w http.ResponseWriter, r *http.Request) {
	var req handoffOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	// Per design spec, the kernel ALWAYS mints handoff_id. A caller-
	// supplied value is rejected so path-based claim/complete routes can
	// rely on the canonical ho-<ms>-<hex> shape.
	if req.HandoffID != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"handoff_id must not be provided; kernel mints it server-side")
		return
	}
	if err := ValidateSessionID(req.FromSession); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"from_session: "+err.Error())
		return
	}
	if req.ToSession != "" {
		if err := ValidateSessionID(req.ToSession); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request",
				"to_session: "+err.Error())
			return
		}
	}
	if req.BootstrapPrompt == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"bootstrap_prompt is required")
		return
	}
	if req.Task == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"task is required")
		return
	}
	if _, ok := req.Task["title"].(string); !ok || req.Task["title"].(string) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"task.title is required")
		return
	}
	if _, ok := req.Task["goal"].(string); !ok || req.Task["goal"].(string) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"task.goal is required")
		return
	}
	nextSteps, ok := req.Task["next_steps"].([]interface{})
	if !ok || len(nextSteps) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"task.next_steps must be a non-empty list")
		return
	}
	if req.TTLSeconds < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"ttl_seconds must be >= 0")
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = 3600
	}

	// Single `now` for the whole operation — no skew between the minted
	// handoff_id's embedded timestamp and the stored created_at.
	now := time.Now().UTC()
	handoffID := mintHandoffID(now)
	state := HandoffState{
		HandoffID:   handoffID,
		FromSession: req.FromSession,
		ToSession:   req.ToSession,
		Reason:      req.Reason,
		TTLSeconds:  req.TTLSeconds,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Duration(req.TTLSeconds) * time.Second),
	}
	// The full payload we put on the bus is the canonical offer — mirror it
	// to OfferPayload so claimants can read it verbatim.
	payload := map[string]interface{}{
		"handoff_id":       handoffID,
		"from_session":     req.FromSession,
		"to_session":       req.ToSession,
		"reason":           req.Reason,
		"ttl_seconds":      req.TTLSeconds,
		"created_at":       now.Format(time.RFC3339Nano),
		"task":             req.Task,
		"bootstrap_prompt": req.BootstrapPrompt,
		"bus_context_refs": req.BusContextRefs,
		"memory_refs":      req.MemoryRefs,
	}
	state.OfferPayload = payload

	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = s.busSessions.AppendEvent(BusHandoffs, EvtHandoffOffer, req.FromSession, payload)
		return err
	}
	stored, err := s.handoffRegistry.ApplyOffer(state, now, appendFn)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", err.Error())
		return
	}
	writeJSONResp(w, http.StatusOK, handoffWriteResponse{
		OK: true, HandoffID: handoffID,
		Seq: evt.Seq, Hash: evt.Hash, Handoff: stored,
	})
}

// mintHandoffID mirrors the bridge's format: ho-<unix-ms>-<hex suffix>.
func mintHandoffID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("ho-%d-%s", now.UnixMilli(), hex.EncodeToString(b[:]))
}

// ─── GET /v1/handoffs ────────────────────────────────────────────────────────

func (s *Server) handleHandoffList(w http.ResponseWriter, r *http.Request) {
	filterState := strings.ToLower(r.URL.Query().Get("state"))
	forSession := r.URL.Query().Get("for_session")
	now := time.Now().UTC()

	snap := s.handoffRegistry.Snapshot()
	out := make([]*HandoffState, 0, len(snap))
	for _, h := range snap {
		// Treat TTL-expired open handoffs as expired in the filter view: a
		// caller looking for "open" offers shouldn't see stale rows the
		// kernel would reject anyway.
		effective := h.State
		if effective == HandoffStateOpen && h.IsExpired(now) {
			effective = "expired"
		}
		if filterState != "" && effective != filterState {
			continue
		}
		if forSession != "" {
			if h.ToSession != "" && h.ToSession != forSession {
				continue
			}
		}
		out = append(out, h)
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"handoffs": out,
		"count":    len(out),
	})
}

// ─── POST /v1/handoffs/{id}/claim ────────────────────────────────────────────

type handoffClaimRequest struct {
	ClaimingSession string `json:"claiming_session"`
}

func (s *Server) handleHandoffClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "handoff_id required in path")
		return
	}
	var req handoffClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	if err := ValidateSessionID(req.ClaimingSession); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"claiming_session: "+err.Error())
		return
	}
	now := time.Now().UTC()
	// The bus append for the winning claim runs WHILE the handoff lock
	// is held inside ApplyClaim, so first-wins is enforced on the
	// authoritative bus, not just the in-memory registry.
	claimPayload := map[string]interface{}{
		"handoff_id":       id,
		"claiming_session": req.ClaimingSession,
		"claimed_at":       now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = s.busSessions.AppendEvent(BusHandoffs, EvtHandoffClaim, req.ClaimingSession, claimPayload)
		return err
	}
	result, appendErr := s.handoffRegistry.ApplyClaim(id, req.ClaimingSession, now, appendFn)
	if appendErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", appendErr.Error())
		return
	}
	if result.Rejection != "" {
		s.emitClaimRejected(id, req.ClaimingSession, result, now)
		status := http.StatusConflict
		kind := "conflict"
		if result.Rejection == ClaimRejectedOfferNotFound {
			status = http.StatusNotFound
			kind = "not_found"
		}
		writeJSONError(w, status, kind, fmt.Sprintf(
			"claim rejected: %s", result.Rejection))
		return
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"ok":         true,
		"handoff_id": id,
		"seq":        evt.Seq,
		"hash":       evt.Hash,
		"handoff":    result.Offer,
		"offer":      result.Offer.OfferPayload, // the canonical offer payload
	})
}

// emitClaimRejected writes a handoff.claim_rejected event to bus_handoffs so
// observers can audit failed claim attempts (amendment #4). Emission failure
// is logged but non-fatal — the handler already returned the HTTP error.
//
// Per the amendment the payload ALWAYS includes reason, attempting_session,
// and conflicting_session (empty string when inapplicable, e.g. the offer
// never existed or TTL expired). This gives audit consumers a stable schema
// to query against.
func (s *Server) emitClaimRejected(handoffID, attemptingSession string,
	result ClaimResult, now time.Time) {
	payload := map[string]interface{}{
		"handoff_id":          handoffID,
		"attempting_session":  attemptingSession,
		"conflicting_session": result.ConflictingSession, // "" when N/A
		"reason":              string(result.Rejection),
		"rejected_at":         now.Format(time.RFC3339Nano),
	}
	if _, err := s.busSessions.AppendEvent(BusHandoffs, EvtHandoffClaimRejected, attemptingSession, payload); err != nil {
		slog.Warn("handoff: claim_rejected emit failed",
			"handoff_id", handoffID, "reason", string(result.Rejection), "err", err)
	}
}

// ─── POST /v1/handoffs/{id}/complete ─────────────────────────────────────────

type handoffCompleteRequest struct {
	CompletingSession string `json:"completing_session"`
	Outcome           string `json:"outcome"`
	Notes             string `json:"notes,omitempty"`
	NextHandoffID     string `json:"next_handoff_id,omitempty"`
}

func (s *Server) handleHandoffComplete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "handoff_id required in path")
		return
	}
	var req handoffCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	if err := ValidateSessionID(req.CompletingSession); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"completing_session: "+err.Error())
		return
	}
	if req.Outcome == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "outcome is required")
		return
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"handoff_id":         id,
		"completing_session": req.CompletingSession,
		"outcome":            req.Outcome,
		"notes":              req.Notes,
		"next_handoff_id":    req.NextHandoffID,
		"completed_at":       now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = s.busSessions.AppendEvent(BusHandoffs, EvtHandoffComplete, req.CompletingSession, payload)
		return err
	}
	stored, reason, appendErr := s.handoffRegistry.ApplyComplete(
		id, req.CompletingSession, req.Outcome, req.Notes, req.NextHandoffID, now, appendFn,
	)
	if appendErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", appendErr.Error())
		return
	}
	if reason == ClaimRejectedOfferNotFound {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("handoff %q not found", id))
		return
	}
	if reason == ClaimRejectedOutOfOrder {
		writeJSONError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("handoff %q cannot complete in state %q", id, stored.State))
		return
	}
	writeJSONResp(w, http.StatusOK, handoffWriteResponse{
		OK: true, HandoffID: id, Seq: evt.Seq, Hash: evt.Hash, Handoff: stored,
	})
}
