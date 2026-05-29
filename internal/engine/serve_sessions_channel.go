// serve_sessions_channel.go — kernel-side HTTP forwarder for channel-session
// registration (ADR-082, Wave 2 of the mod3-kernel integration).
//
// Design locks (Wave 2 handoff):
//
//  1. Session authority = kernel-owned. The kernel mints `session_id` if the
//     caller doesn't supply one. Mod3's SessionRegistry stores per-channel
//     state (voice, queue, device) keyed on the kernel-issued ID. If mod3
//     crashes, the kernel knows which sessions existed; mod3 rebuilds on
//     re-register.
//  2. MCP transport = HTTP proxy. Kernel reaches mod3 over plain HTTP at
//     Config.Mod3URL (default http://localhost:7860), not stdio MCP.
//
// Path namespace choice: these routes live under `/v1/channel-sessions/*`,
// NOT `/v1/sessions/*`. The kernel's existing `/v1/sessions/*` family
// (serve_sessions_mgmt.go) serves agent-session state (3-component hyphen-
// validated session IDs, workspace/role required, tied to the handoff
// protocol). Channel-participant registration has an incompatible shape
// (short UUID session IDs, participant_id/participant_type/voice/device
// fields). Rather than weaken ValidateSessionID (which would cascade into
// handoff claim semantics) we namespace the new concern. The channel-provider
// RFC's guidance to "use the same cogos_session_register primitive with a
// participant_type discriminator" remains aspirational at the MCP tool layer
// (Wave 3); at the HTTP layer the two surfaces coexist cleanly.
//
// Routes owned by this file:
//
//	POST /v1/channel-sessions/register             — mint+forward to mod3
//	POST /v1/channel-sessions/{id}/deregister      — forward deregister
//	GET  /v1/channel-sessions                      — list (kernel view + mod3 list)
//	GET  /v1/channel-sessions/{id}                 — single-session detail
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ─── in-memory registry for kernel-owned channel-session identity ────────────

// ChannelSessionRecord is the kernel's identity-authority record for a
// channel-participant session. Distinct from SessionState in sessions.go
// (which tracks agent sessions with strict 3-component hyphen IDs); the
// channel-session concern uses short UUIDs and caller-supplied participant
// metadata that the kernel passes through to mod3.
type ChannelSessionRecord struct {
	// SessionID is the kernel-authoritative ID. Either caller-supplied or
	// minted by the kernel (uuid short form).
	SessionID string `json:"session_id"`

	// ParticipantID / ParticipantType / PreferredVoice / PreferredOutputDevice
	// / Priority mirror the mod3 SessionRegisterRequest shape. The kernel
	// stores them so a post-crash re-register can replay identity cleanly.
	ParticipantID         string `json:"participant_id,omitempty"`
	ParticipantType       string `json:"participant_type,omitempty"`
	PreferredVoice        string `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string `json:"preferred_output_device,omitempty"`
	Priority              int    `json:"priority,omitempty"`

	// Kinds mirrors the channel-provider RFC's `kinds` metadata field
	// (e.g. ["audio"] for a mod3-provider registration). Mod3 ignores it
	// today; kept on the kernel record for downstream consumers that want
	// to filter by capability.
	Kinds []string `json:"kinds,omitempty"`

	// Metadata is an opaque pass-through map the RFC describes as
	// "provider_id/kinds in the metadata" — preserved verbatim on the
	// kernel record and forwarded to mod3 (which ignores unknown fields).
	Metadata map[string]any `json:"metadata,omitempty"`

	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`

	// Source records whether the session_id came from the caller or was
	// minted. Useful for audit; no functional impact.
	IDSource string `json:"id_source,omitempty"` // "caller" | "minted"

	// Iss / Sub carry the OIDC identity claims forwarded from mod3's seat
	// registration (Fix A — session-registration-unification). These are the
	// user_iss/user_sub fields from the channel-client seat; carrying them on
	// the kernel record makes the WHO visible to kernel consumers without
	// requiring a second round-trip to mod3.
	//
	// Dependency: fully populated only when mod3 is running the
	// feat/mod3-session-identity-claims branch or later (merged into main).
	// Callers that don't supply these fields leave them empty; the kernel
	// record is still valid and the callback is still idempotent.
	Iss string `json:"iss,omitempty"`
	Sub string `json:"sub,omitempty"`
}

// ChannelSessionRegistry is the in-memory map keyed by session_id. It holds
// kernel-owned identity only; per-channel state (assigned_voice, queue,
// device) lives in mod3 and is returned as part of the merged forward
// response. If mod3 crashes the kernel knows which sessions existed and
// callers can re-register to rebuild.
type ChannelSessionRegistry struct {
	mu   sync.RWMutex
	rows map[string]*ChannelSessionRecord
}

// NewChannelSessionRegistry returns an empty registry.
func NewChannelSessionRegistry() *ChannelSessionRegistry {
	return &ChannelSessionRegistry{rows: make(map[string]*ChannelSessionRecord)}
}

// Put stores (or overwrites) a record by session_id.
func (r *ChannelSessionRegistry) Put(rec ChannelSessionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := rec
	r.rows[rec.SessionID] = &cp
}

// Get returns a copy of the record for id, or (nil, false).
func (r *ChannelSessionRegistry) Get(id string) (*ChannelSessionRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false
	}
	cp := *row
	return &cp, true
}

// Delete removes the record for id. No-op if absent; returns whether the row
// was present before removal (handy for logging / telemetry).
func (r *ChannelSessionRegistry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rows[id]
	delete(r.rows, id)
	return ok
}

// Snapshot returns a copy of every record.
func (r *ChannelSessionRegistry) Snapshot() []*ChannelSessionRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ChannelSessionRecord, 0, len(r.rows))
	for _, row := range r.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// Len returns the number of tracked channel sessions.
func (r *ChannelSessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rows)
}

// ─── route registration ──────────────────────────────────────────────────────

// defaultMod3ForwardTimeout is the per-request timeout for channel-session
// HTTP forwards. 8s is deliberately shorter than WriteTimeout (300s) so a
// stalled mod3 doesn't block a kernel request the whole way through.
const defaultMod3ForwardTimeout = 8 * time.Second

// mod3HTTPClient is the shared net/http client for forwards. Tests override
// Server.mod3Client for isolation; when nil, handlers fall back to this.
var mod3HTTPClient = &http.Client{Timeout: defaultMod3ForwardTimeout}

// registerChannelSessionRoutes attaches channel-session routes onto mux.
// Called from NewServer.
func (s *Server) registerChannelSessionRoutes(mux *http.ServeMux) {
	s.route(mux, "POST /v1/channel-sessions/register", s.handleChannelSessionRegister)
	s.route(mux, "POST /v1/channel-sessions/{id}/deregister", s.handleChannelSessionDeregister)
	s.route(mux, "GET /v1/channel-sessions", s.handleChannelSessionList)
	s.route(mux, "GET /v1/channel-sessions/{id}", s.handleChannelSessionGet)

	// Issue #156: channel voice-presence. Returns the channel-membership view
	// from mod3 so the kernel can surface it without requiring a direct
	// mod3 import into callers. Mod3 is the authoritative source for which
	// channel-sessions are in which voice room; the kernel proxies the query.
	s.route(mux, "GET /v1/channels/{channel_id}/peers", s.handleChannelPeers)
}

// ─── wire types ──────────────────────────────────────────────────────────────

// channelSessionRegisterRequest is the kernel-facing request body. Shape
// mirrors mod3's SessionRegisterRequest (see mod3/http_api.py line ~278) plus
// an optional session_id — when omitted, the kernel mints one.
//
// Wave 3.5 schema alignment with the channel-provider RFC's
// `cogos_session_register` primitive: `kinds` (array of adapter kinds the
// registrant participates in, e.g. ["audio"]) and `metadata` (opaque
// pass-through blob — RFC calls for provider_id/kinds to live in metadata)
// are both optional and flow through to mod3 unchanged (mod3 ignores
// unknown fields).
type channelSessionRegisterRequest struct {
	SessionID             string         `json:"session_id,omitempty"`
	ParticipantID         string         `json:"participant_id"`
	ParticipantType       string         `json:"participant_type,omitempty"`
	PreferredVoice        string         `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string         `json:"preferred_output_device,omitempty"`
	Priority              int            `json:"priority,omitempty"`
	Kinds                 []string       `json:"kinds,omitempty"`
	Metadata              map[string]any `json:"metadata,omitempty"`
	// Iss / Sub are OIDC identity claims forwarded from the mod3 seat that
	// triggered this register callback (Fix A — session-registration-unification).
	// Optional; absent means the seat was unattributed or the caller does not
	// carry identity (e.g. a direct kernel-originated registration).
	Iss string `json:"iss,omitempty"`
	Sub string `json:"sub,omitempty"`
}

// channelSessionResponse is the merged shape returned from the kernel. The
// `kernel` block is the identity record (authoritative owner); `mod3` is the
// live channel state (voice pool, queue, device). Callers get everything in
// one round trip. Unknown fields from mod3 are preserved under `mod3` as a
// raw JSON object.
type channelSessionResponse struct {
	Kernel *ChannelSessionRecord `json:"kernel"`
	Mod3   json.RawMessage       `json:"mod3,omitempty"`
}

// channelSessionListResponse is the shape of GET /v1/channel-sessions. The
// kernel list is always present; the mod3 block is an opaque pass-through so
// clients don't need to know mod3's schema.
type channelSessionListResponse struct {
	Kernel []*ChannelSessionRecord `json:"kernel"`
	Mod3   json.RawMessage         `json:"mod3,omitempty"`
}

// ─── shared register/deregister/list logic (Wave 3.5) ────────────────────────
//
// These methods are the single place session-ID minting, kernel registry
// commits, and mod3 forwarding happen. Both the HTTP handlers below and the
// mod3_register_session / mod3_deregister_session / mod3_list_sessions MCP
// tools (see mcp_modality_proxy.go) call through here so session-ID
// authority stays centralized — nobody reaches mod3's /v1/sessions/* surface
// directly except this one codepath.

// channelSessionForwardError classifies a failure in RegisterChannelSession /
// DeregisterChannelSession / ListChannelSessions so the caller (HTTP handler
// or MCP tool) can surface the right status/message without re-parsing.
type channelSessionForwardError struct {
	// Kind: "invalid_request" | "mod3_unreachable" | "mod3_rejected"
	Kind string
	// HTTPStatus is the status the caller should emit (400 for
	// invalid_request, 502 for mod3_unreachable, mod3's own status for
	// mod3_rejected).
	HTTPStatus int
	// Message is a human-readable description, safe to surface to callers.
	Message string
	// Mod3Body carries the raw JSON body mod3 returned when Kind ==
	// "mod3_rejected", so the HTTP handler can pass it through verbatim.
	Mod3Body json.RawMessage
}

func (e *channelSessionForwardError) Error() string { return e.Message }

// RegisterChannelSession is the Wave 2+3.5 shared entry point for channel-
// session registration. It mints a session_id when absent, forwards to mod3,
// and commits the kernel-side identity record on success.
//
// Callers:
//   - HTTP:  POST /v1/channel-sessions/register  (handleChannelSessionRegister)
//   - MCP:   mod3_register_session tool          (toolMod3RegisterSession)
//
// Returning a (resp, nil) pair means the caller should surface the merged
// response with 200 OK. Returning a non-nil *channelSessionForwardError
// tells the caller which status + body shape to surface.
func (s *Server) RegisterChannelSession(ctx context.Context, req channelSessionRegisterRequest) (*channelSessionResponse, *channelSessionForwardError) {
	if req.ParticipantID == "" {
		return nil, &channelSessionForwardError{
			Kind:       "invalid_request",
			HTTPStatus: http.StatusBadRequest,
			Message:    "participant_id is required",
		}
	}

	// Mint when absent — kernel is the session-ID authority (ADR-082).
	idSource := "caller"
	if req.SessionID == "" {
		req.SessionID = mintChannelSessionID()
		idSource = "minted"
	}
	if req.ParticipantType == "" {
		req.ParticipantType = "agent" // matches mod3's default
	}
	if req.PreferredOutputDevice == "" {
		req.PreferredOutputDevice = "system-default"
	}

	now := time.Now().UTC()
	record := ChannelSessionRecord{
		SessionID:             req.SessionID,
		ParticipantID:         req.ParticipantID,
		ParticipantType:       req.ParticipantType,
		PreferredVoice:        req.PreferredVoice,
		PreferredOutputDevice: req.PreferredOutputDevice,
		Priority:              req.Priority,
		Kinds:                 req.Kinds,
		Metadata:              req.Metadata,
		RegisteredAt:          now,
		LastSeen:              now,
		IDSource:              idSource,
		Iss:                   req.Iss,
		Sub:                   req.Sub,
	}

	// Forward to mod3 with the kernel-issued session_id. Mod3's body is the
	// same shape modulo the optional session_id field (mod3 requires it; we
	// always supply one). `kinds` and `metadata` are RFC-level fields mod3
	// currently ignores; we still forward them so mod3 can start consuming
	// them without a kernel change when it's ready.
	forwardBody := map[string]any{
		"session_id":              req.SessionID,
		"participant_id":          req.ParticipantID,
		"participant_type":        req.ParticipantType,
		"preferred_voice":         req.PreferredVoice,
		"preferred_output_device": req.PreferredOutputDevice,
		"priority":                req.Priority,
	}
	if len(req.Kinds) > 0 {
		forwardBody["kinds"] = req.Kinds
	}
	if len(req.Metadata) > 0 {
		forwardBody["metadata"] = req.Metadata
	}
	body, _ := json.Marshal(forwardBody)

	mod3Resp, status, err := s.forwardMod3(ctx, http.MethodPost,
		"/v1/sessions/register", bytes.NewReader(body))
	if err != nil {
		slog.Warn("channel-sessions: forward to mod3 failed",
			"session_id", req.SessionID, "err", err)
		return nil, &channelSessionForwardError{
			Kind:       "mod3_unreachable",
			HTTPStatus: http.StatusBadGateway,
			Message:    fmt.Sprintf("mod3 unreachable: %v", err),
		}
	}

	if status < 200 || status >= 300 {
		slog.Warn("channel-sessions: mod3 returned non-2xx",
			"session_id", req.SessionID, "status", status)
		return nil, &channelSessionForwardError{
			Kind:       "mod3_rejected",
			HTTPStatus: status,
			Message:    fmt.Sprintf("mod3 returned %d", status),
			Mod3Body:   mod3Resp,
		}
	}

	// Mod3 accepted — commit the kernel-side identity record.
	s.channelSessionRegistry.Put(record)
	slog.Info("channel-sessions: registered",
		"session_id", req.SessionID, "participant_id", req.ParticipantID,
		"id_source", idSource)

	return &channelSessionResponse{Kernel: &record, Mod3: mod3Resp}, nil
}

// DeregisterChannelSession is the shared entry point for deregistration.
// Forwards to mod3 and drops the kernel registry row on any non-5xx mod3
// response (including 404 — "mod3 forgot" is equivalent to "kernel should
// forget too"). On transport failure the kernel keeps the record so the
// caller can retry.
//
// Returns the raw mod3 body + status on success. Callers should surface
// both verbatim (writeJSONPassThrough on the HTTP side; the MCP tool
// wraps it as a JSON result).
func (s *Server) DeregisterChannelSession(ctx context.Context, sessionID string) (json.RawMessage, int, *channelSessionForwardError) {
	if sessionID == "" {
		return nil, 0, &channelSessionForwardError{
			Kind:       "invalid_request",
			HTTPStatus: http.StatusBadRequest,
			Message:    "session_id is required",
		}
	}

	mod3Resp, status, err := s.forwardMod3(ctx, http.MethodPost,
		"/v1/sessions/"+sessionID+"/deregister", nil)
	if err != nil {
		slog.Warn("channel-sessions: deregister forward failed",
			"session_id", sessionID, "err", err)
		return nil, 0, &channelSessionForwardError{
			Kind:       "mod3_unreachable",
			HTTPStatus: http.StatusBadGateway,
			Message:    fmt.Sprintf("mod3 unreachable: %v", err),
		}
	}

	// Kernel drops its identity record whenever mod3 successfully
	// acknowledges, including 404 ("never registered" is equivalent to
	// "not tracked"; clean slate in kernel matches clean slate in mod3).
	if status >= 200 && status < 500 {
		s.channelSessionRegistry.Delete(sessionID)
	}

	return mod3Resp, status, nil
}

// ListChannelSessions is the shared entry point for the merged list query.
// Returns the kernel snapshot plus mod3's raw `GET /v1/sessions` body.
//
// On mod3 transport failure: error (502). On mod3 non-2xx: returns the
// raw mod3 body with its status attached so the caller can surface intact.
func (s *Server) ListChannelSessions(ctx context.Context) (*channelSessionListResponse, int, *channelSessionForwardError) {
	mod3Resp, status, err := s.forwardMod3(ctx, http.MethodGet,
		"/v1/sessions", nil)
	if err != nil {
		slog.Warn("channel-sessions: list forward failed", "err", err)
		return nil, 0, &channelSessionForwardError{
			Kind:       "mod3_unreachable",
			HTTPStatus: http.StatusBadGateway,
			Message:    fmt.Sprintf("mod3 unreachable: %v", err),
		}
	}
	if status < 200 || status >= 300 {
		return nil, status, &channelSessionForwardError{
			Kind:       "mod3_rejected",
			HTTPStatus: status,
			Message:    fmt.Sprintf("mod3 returned %d", status),
			Mod3Body:   mod3Resp,
		}
	}
	return &channelSessionListResponse{
		Kernel: s.channelSessionRegistry.Snapshot(),
		Mod3:   mod3Resp,
	}, http.StatusOK, nil
}

// ─── POST /v1/channel-sessions/register ──────────────────────────────────────

func (s *Server) handleChannelSessionRegister(w http.ResponseWriter, r *http.Request) {
	var req channelSessionRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}

	resp, ferr := s.RegisterChannelSession(r.Context(), req)
	if ferr != nil {
		switch ferr.Kind {
		case "invalid_request":
			writeJSONError(w, ferr.HTTPStatus, "invalid_request", ferr.Message)
		case "mod3_unreachable":
			writeJSONError(w, ferr.HTTPStatus, "mod3_unreachable", ferr.Message)
		case "mod3_rejected":
			writeJSONPassThrough(w, ferr.HTTPStatus, ferr.Mod3Body)
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal", ferr.Message)
		}
		return
	}
	writeJSONResp(w, http.StatusOK, resp)
}

// ─── POST /v1/channel-sessions/{id}/deregister ───────────────────────────────

func (s *Server) handleChannelSessionDeregister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"session_id required in path")
		return
	}

	mod3Resp, status, ferr := s.DeregisterChannelSession(r.Context(), id)
	if ferr != nil {
		if ferr.Kind == "mod3_unreachable" {
			writeJSONError(w, ferr.HTTPStatus, "mod3_unreachable", ferr.Message)
			return
		}
		writeJSONError(w, ferr.HTTPStatus, ferr.Kind, ferr.Message)
		return
	}
	writeJSONPassThrough(w, status, mod3Resp)
}

// ─── GET /v1/channel-sessions ────────────────────────────────────────────────

func (s *Server) handleChannelSessionList(w http.ResponseWriter, r *http.Request) {
	resp, status, ferr := s.ListChannelSessions(r.Context())
	if ferr != nil {
		switch ferr.Kind {
		case "mod3_unreachable":
			writeJSONError(w, ferr.HTTPStatus, "mod3_unreachable", ferr.Message)
		case "mod3_rejected":
			writeJSONPassThrough(w, ferr.HTTPStatus, ferr.Mod3Body)
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal", ferr.Message)
		}
		return
	}
	writeJSONResp(w, status, resp)
}

// ─── GET /v1/channel-sessions/{id} ───────────────────────────────────────────

func (s *Server) handleChannelSessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"session_id required in path")
		return
	}

	mod3Resp, status, err := s.forwardMod3(r.Context(), http.MethodGet,
		"/v1/sessions/"+id, nil)
	if err != nil {
		slog.Warn("channel-sessions: get forward failed",
			"session_id", id, "err", err)
		writeJSONError(w, http.StatusBadGateway, "mod3_unreachable",
			fmt.Sprintf("mod3 unreachable: %v", err))
		return
	}
	if status < 200 || status >= 300 {
		writeJSONPassThrough(w, status, mod3Resp)
		return
	}

	kernelRec, _ := s.channelSessionRegistry.Get(id)
	resp := channelSessionResponse{
		Kernel: kernelRec,
		Mod3:   mod3Resp,
	}
	writeJSONResp(w, http.StatusOK, resp)
}

// ─── forwarder + helpers ─────────────────────────────────────────────────────

// forwardMod3 is the single HTTP egress point to mod3. Returns the raw
// response body, HTTP status, and a transport error (non-nil only when the
// request never got a status back — connection refused, DNS, TLS, timeout).
// Mod3 error bodies (4xx/5xx) come back with err == nil and caller decides
// how to surface them.
func (s *Server) forwardMod3(ctx context.Context, method, path string, body io.Reader) (json.RawMessage, int, error) {
	base := strings.TrimRight(s.cfg.Mod3URL, "/")
	if base == "" {
		return nil, 0, errors.New("Mod3URL not configured")
	}
	url := base + path

	// Scope a per-request timeout on top of the shared client's. The ambient
	// request context may have a longer deadline; we want the forward to
	// fail fast if mod3 stalls.
	reqCtx, cancel := context.WithTimeout(ctx, defaultMod3ForwardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	client := s.mod3Client
	if client == nil {
		client = mod3HTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	if len(raw) == 0 {
		// Mod3's deregister can return a JSON object; other endpoints
		// always return JSON; if the body is empty we substitute null so
		// json.RawMessage doesn't marshal an empty string (invalid JSON).
		raw = []byte("null")
	}
	if !json.Valid(raw) {
		// Surface as a synthetic JSON string; don't propagate raw bytes.
		wrapped, _ := json.Marshal(map[string]string{"raw": string(raw)})
		return json.RawMessage(wrapped), resp.StatusCode, nil
	}
	return json.RawMessage(raw), resp.StatusCode, nil
}

// ─── GET /v1/channels/{channel_id}/peers ─────────────────────────────────────

// handleChannelPeers returns the voice-presence view for a named channel.
// It proxies the query to mod3's GET /v1/channels/{channel_id}/peers endpoint
// (tracked in issue #156 — mod3-side implementation is a follow-up).
//
// Until mod3 exposes /v1/channels/{id}/peers the kernel falls back to the
// channel-session list filtered by the channel_id stored in session extras.
//
//	GET /v1/channels/{channel_id}/peers
//	200 → { channel_id: str, peers: [channelSessionRecord...], count: int }
//	503 → if mod3 is not reachable (returns kernel-only fallback)
func (s *Server) handleChannelPeers(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("channel_id")
	if channelID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"channel_id required in path")
		return
	}

	// Attempt to proxy to mod3 — mod3 is authoritative for channel membership.
	mod3Path := "/v1/channels/" + channelID + "/peers"
	body, status, err := s.forwardMod3(r.Context(), http.MethodGet, mod3Path, nil)
	if err == nil && status >= 200 && status < 300 {
		// mod3 answered — return its response verbatim.
		writeJSONPassThrough(w, status, body)
		return
	}

	// mod3 not reachable or returned an error — fall back to the kernel's
	// channel-session registry. This is a best-effort degraded view: it
	// shows sessions that registered via the kernel forwarder but does not
	// have mod3's voice-room assignment information.
	snap := s.channelSessionRegistry.Snapshot()
	peers := make([]*ChannelSessionRecord, 0, len(snap))
	for _, rec := range snap {
		// Filter: the channel_id is embedded as metadata["channel_id"] when
		// mod3 reports it back, or can be inferred from the participant_id
		// prefix convention (channel_id-<suffix>). We do a prefix match for now.
		if strings.HasPrefix(rec.ParticipantID, channelID+"-") ||
			(rec.Metadata != nil && rec.Metadata["channel_id"] == channelID) {
			peers = append(peers, rec)
		}
	}

	w.Header().Set("X-Cogos-Source", "kernel-fallback")
	if err != nil {
		w.Header().Set("X-Cogos-Mod3-Error", err.Error())
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"channel_id": channelID,
		"peers":      peers,
		"count":      len(peers),
		"source":     "kernel-fallback",
	})
}

// mintChannelSessionID returns a 12-char lowercase-hex short UUID. Short
// enough to be human-readable in logs, unique enough to avoid collisions
// across a single mod3 instance's lifetime.
func mintChannelSessionID() string {
	u := uuid.New()
	hex := strings.ReplaceAll(u.String(), "-", "")
	return "cs-" + hex[:12]
}

// writeJSONPassThrough writes the given status + raw JSON body verbatim. Used
// when mod3's response (especially errors) should flow to the caller intact.
func writeJSONPassThrough(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) == 0 {
		_, _ = w.Write([]byte("null"))
		return
	}
	_, _ = w.Write(body)
}
