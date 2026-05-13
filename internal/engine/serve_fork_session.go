// serve_fork_session.go — HTTP surface for RFC-0005 session forking.
//
// Route registered by registerForkSessionRoute (called from
// registerSessionMgmtRoutes in serve_sessions_mgmt.go):
//
//	POST /v1/sessions/{id}/fork
//
// Request body: forkSessionHTTPRequest JSON.
// Response (201 Created): forkSessionHTTPResponse JSON.
// Errors:
//   - 400 Bad Request: invalid request body or overlay schema.
//   - 404 Not Found: parent session unknown.
//   - 501 Not Implemented: cross-workspace fork requested.
package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// forkSessionHTTPRequest mirrors forkSessionInput in mcp_fork_session.go but
// lives here so the HTTP path can evolve independently from the MCP path.
// parent_session_id is taken from the URL path parameter; all other fields
// are from the JSON body.
type forkSessionHTTPRequest struct {
	// ParentStateHash is optional; defaults to parent's current HEAD.
	ParentStateHash string          `json:"parent_state_hash,omitempty"`
	Overlay         *SessionOverlay `json:"overlay,omitempty"`
	// PinDuration is an ISO 8601 duration string, e.g. "P30D". Defaults to P7D.
	PinDuration    string `json:"pin_duration,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
}

// forkSessionHTTPResponse is the 201 Created body.
type forkSessionHTTPResponse struct {
	OK             bool   `json:"ok"`
	ChildSessionID string `json:"child_session_id"`
	ForkBlockHash  string `json:"fork_block_hash"`
	ForkPoint      int64  `json:"fork_point"`
	PinnedUntil    string `json:"pinned_until,omitempty"` // RFC3339
}

// ─── POST /v1/sessions/{id}/fork ─────────────────────────────────────────────

func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	if err := ValidateSessionID(parentID); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	var req forkSessionHTTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}

	// Cross-workspace forks are not implemented in v0.5.0 (RFC-0005 §Out of scope).
	// Detect by checking if the overlay contains a non-local identity ref with a
	// different workspace prefix. For v1 we simply stub 501 for any caller that
	// explicitly sets a workspace-qualified identity ref.
	if req.Overlay != nil && req.Overlay.Identity != nil {
		ref := req.Overlay.Identity.IdentityRef
		if len(ref) > 0 && isExternalWorkspaceRef(ref) {
			writeJSONError(w, http.StatusNotImplemented, "not_implemented",
				"cross-workspace forks are not supported in v0.5.0; deferred to constellation federation")
			return
		}
	}

	// Verify parent session exists.
	_, ok := s.sessionRegistry.Get(parentID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("parent session %q not found", parentID))
		return
	}

	now := time.Now().UTC()

	// Mint or validate child session ID.
	childID := req.ChildSessionID
	if childID == "" {
		childID = mintForkChildID(parentID, now)
	}
	if err := ValidateSessionID(childID); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("invalid child_session_id %q: %v", childID, err))
		return
	}

	// Parse pin duration.
	var pinnedUntil *time.Time
	if req.PinDuration != "" {
		d, err := parseISO8601Duration(req.PinDuration)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("invalid pin_duration %q: %v", req.PinDuration, err))
			return
		}
		t := now.Add(d)
		pinnedUntil = &t
	}

	// Resolve parent state hash.
	parentHash := req.ParentStateHash
	var forkPoint int64
	if parentHash == "" {
		hash, seq, err := s.busSessions.LatestEventHash(BusSessions)
		if err == nil {
			parentHash = hash
			forkPoint = seq
		}
	}

	// Build overlay (nil is fine).
	overlay := SessionOverlay{}
	if req.Overlay != nil {
		overlay = *req.Overlay
	}

	// Build fork body.
	body := SessionForkBody{
		ParentSessionHash: parentHash,
		ParentSessionID:   parentID,
		ChildSessionID:    childID,
		ForkPoint:         forkPoint,
		Overlay:           overlay,
		PinnedUntil:       pinnedUntil,
	}

	// Serialize body for bus event.
	var payloadMap map[string]interface{}
	b, err := json.Marshal(body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	if err := json.Unmarshal(b, &payloadMap); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}

	evt, err := s.busSessions.AppendEvent(BusSessions, string(KindSessionFork), parentID, payloadMap)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "bus_append_failed", err.Error())
		return
	}

	// Register the child session.
	parentState, _ := s.sessionRegistry.Get(parentID)
	childState := deriveChildState(parentState, childID, overlay, now)
	_, _, regErr := s.sessionRegistry.ApplyRegister(
		childState,
		time.Duration(defaultActiveWithinSeconds)*time.Second,
		now,
		nil,
	)
	if regErr != nil {
		slog.Warn("session.fork: child session register failed (HTTP path)",
			"operation", "fork",
			"parent_session_id", parentID,
			"child_session_id", childID,
			"err", regErr,
		)
	}

	// Update fork registry.
	if s.forkRegistry != nil {
		s.forkRegistry.Record(body, now)
	}

	// Structured log per RFC-0005 §Structured logs.
	slog.Info("session.fork: forked",
		"operation", "fork",
		"parent_session_id", parentID,
		"child_session_id", childID,
		"overlay_layers", overlay.OverlayLayers(),
		"ts", now.Format(time.RFC3339),
	)

	resp := forkSessionHTTPResponse{
		OK:             true,
		ChildSessionID: childID,
		ForkBlockHash:  evt.Hash,
		ForkPoint:      forkPoint,
	}
	if pinnedUntil != nil {
		resp.PinnedUntil = pinnedUntil.Format(time.RFC3339)
	}
	writeJSONResp(w, http.StatusCreated, resp)
}

// isExternalWorkspaceRef returns true if the identity ref uses a fully-qualified
// workspace prefix suggesting a different workspace (cross-workspace fork).
// v0.5.0 conservatively treats any ref with "://" that does NOT start with
// "cog://" as external. This heuristic is intentionally loose — real
// cross-workspace detection requires constellation registry lookup, which is
// out of scope for v0.5.0.
func isExternalWorkspaceRef(ref string) bool {
	if len(ref) == 0 {
		return false
	}
	// A ref starting with "cog://" is workspace-local.
	if len(ref) >= 6 && ref[:6] == "cog://" {
		return false
	}
	// Any other URI scheme (e.g. "workspace://...", "https://...") is external.
	for i := 0; i < len(ref)-2; i++ {
		if ref[i] == ':' && ref[i+1] == '/' && ref[i+2] == '/' {
			return true
		}
	}
	return false
}
