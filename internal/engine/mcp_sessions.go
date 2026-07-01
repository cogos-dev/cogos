// mcp_sessions.go — 8 native MCP tools over the kernel session/handoff
// registries. Complement (not replacement) to the 8 `cogos_*` Python bridge
// tools that live in cog-sandbox-mcp: per amendment #5 both surfaces coexist
// by design — bridge tools keep MCP-level ergonomics for agents already
// wired to the Python sandbox; these expose the same kernel truth with no
// Python dependency so a future native client (Wave widget, desktop app,
// direct `cog` CLI) can just speak MCP.
//
// Tool naming mirrors the rest of the kernel MCP surface: snake_case under
// the `cog_*` prefix. Input/output types live in this file so mcp_server.go
// stays focused on registration.
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// SetSessionsBackend wires the bus manager + registries so the kernel-side
// MCP tools can serve traffic. Safe to call post-construction.
func (m *MCPServer) SetSessionsBackend(
	bus *BusSessionManager,
	sessions *SessionRegistry,
	handoffs *HandoffRegistry,
) {
	m.busSessions = bus
	m.sessionRegistry = sessions
	m.handoffRegistry = handoffs
}

// sessionsBackendReady is true when SetSessionsBackend has been called.
func (m *MCPServer) sessionsBackendReady() bool {
	return m.busSessions != nil && m.sessionRegistry != nil && m.handoffRegistry != nil
}

// ─── registerSessionTools — called from registerTools ────────────────────────

// registerSessionTools installs the 8 cog_* session/handoff tools. Kept in
// its own method so mcp_server.go stays tidy; the registration site in
// registerTools calls this last.
//
// Porcelain/plumbing split (workflow wkweyu50g): register/list_sessions/
// heartbeat/end_session/list_handoffs/claim_handoff are the six session tools
// in the preload porcelain set — registered eagerly via mcp.AddTool.
// offer_handoff, complete_handoff, and fork_session are less frequently
// reached (offer/complete are the two ends of a lifecycle a session touches
// at most once each; fork is a rare RFC-0005 primitive) and are deferred.
func (m *MCPServer) registerSessionTools() {
	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_register_session",
		Description: "Register (or idempotently update) a session on " +
			"bus_sessions. Validates the session_id format " +
			"(lowercase, hyphen-separated, 3-component required). Required: " +
			"session_id, workspace, role. Optional: task, model, hostname, " +
			"status, current_task, context_usage. Identity binding (G0b): " +
			"when subject is provided a HarnessBindingCRD is created linking " +
			"session_id → subject via the RBAC provider. Optional iss sets " +
			"the OIDC issuer; binding_type defaults to \"agent\". When subject " +
			"is absent no binding is created (naked-by-default).",
	}), withToolObserver(m, "cog_register_session", m.toolRegisterSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_heartbeat_session",
		Description: "Emit a periodic keep-alive heartbeat for a session. " +
			"Updates LastSeen and optional status/context fields. Rejects " +
			"if the session is unregistered (404) or already ended (409).",
	}), withToolObserver(m, "cog_heartbeat_session", m.toolHeartbeatSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_end_session",
		Description: "Mark a session as ended. Optional handoff_id " +
			"references the outgoing handoff, if any. Rejects if the " +
			"session is unknown (404) or already ended (409).",
	}), withToolObserver(m, "cog_end_session", m.toolEndSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_list_sessions",
		Description: "List the roster of tracked sessions with an " +
			"is-active flag computed from the freshness window " +
			"(default 600s, override via active_within_seconds). Set " +
			"include_ended=true to also see sessions that exited.",
	}), withToolObserver(m, "cog_list_sessions", m.toolListSessions))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_list_handoffs",
		Description: "List tracked handoffs with optional filters: " +
			"state (open|claimed|completed|expired) and for_session. The " +
			"kernel maintains the derived view; the bus remains ground " +
			"truth.",
	}), withToolObserver(m, "cog_list_handoffs", m.toolListHandoffs))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_claim_handoff",
		Description: "Atomically claim an open handoff offer. First-wins " +
			"by kernel mutex: concurrent claims produce exactly one 200 " +
			"and N-1 409 responses, each of which emits a " +
			"handoff.claim_rejected event on bus_handoffs for audit. " +
			"Returns the full offer payload on success.",
	}), withToolObserver(m, "cog_claim_handoff", m.toolClaimHandoff))

	// ── Deferred (plumbing) ──────────────────────────────────────────────

	trackToolDeferred(m, &mcp.Tool{
		Name: "cog_offer_handoff",
		Description: "Post a handoff.offer to bus_handoffs. Mints a new " +
			"handoff_id (ho-<ms>-<hex>). task.title, task.goal, and a " +
			"non-empty task.next_steps list are required. TTL defaults to " +
			"3600s; expired offers are 409 on claim. Returns the full " +
			"offer payload so the caller can inspect what was written.",
	}, withToolObserver(m, "cog_offer_handoff", m.toolOfferHandoff))

	trackToolDeferred(m, &mcp.Tool{
		Name: "cog_complete_handoff",
		Description: "Mark a claimed handoff as completed. Rejected with " +
			"409 if the handoff has not been claimed yet or is already " +
			"complete. Optional outcome, notes, and next_handoff_id.",
	}, withToolObserver(m, "cog_complete_handoff", m.toolCompleteHandoff))

	// RFC-0005: session forking primitive.
	m.registerForkSessionTool()
}

// ─── input/output types ──────────────────────────────────────────────────────

type registerSessionInput struct {
	SessionID    string                 `json:"session_id"`
	Workspace    string                 `json:"workspace"`
	Role         string                 `json:"role"`
	Task         string                 `json:"task,omitempty"`
	Model        string                 `json:"model,omitempty"`
	Hostname     string                 `json:"hostname,omitempty"`
	Status       string                 `json:"status,omitempty"`
	CurrentTask  string                 `json:"current_task,omitempty"`
	ContextUsage float64                `json:"context_usage,omitempty"`
	Extras       map[string]interface{} `json:"extras,omitempty"`

	// Identity binding fields (G0b — all optional; naked-by-default).
	// When Subject is non-empty and a harnessBackend is wired, a
	// HarnessBindingCRD is created linking sessionID → Subject.
	Subject     string `json:"subject,omitempty"`      // identity sub-slug (e.g. "chaz", "cog")
	Iss         string `json:"iss,omitempty"`          // OIDC issuer hint (e.g. "anthropic.claude-code")
	BindingType string `json:"binding_type,omitempty"` // "agent" (default) or "user"
}

type heartbeatSessionInput struct {
	SessionID    string  `json:"session_id"`
	Status       string  `json:"status,omitempty"`
	ContextUsage float64 `json:"context_usage,omitempty"`
	CurrentTask  string  `json:"current_task,omitempty"`
}

type endSessionInput struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
	HandoffID string `json:"handoff_id,omitempty"`
}

type listSessionsInput struct {
	ActiveWithinSeconds int  `json:"active_within_seconds,omitempty"`
	IncludeEnded        bool `json:"include_ended,omitempty"`
}

type offerHandoffInput struct {
	FromSession     string                   `json:"from_session"`
	ToSession       string                   `json:"to_session,omitempty"`
	Reason          string                   `json:"reason,omitempty"`
	TTLSeconds      int                      `json:"ttl_seconds,omitempty"`
	Task            map[string]interface{}   `json:"task"`
	BootstrapPrompt string                   `json:"bootstrap_prompt"`
	BusContextRefs  []map[string]interface{} `json:"bus_context_refs,omitempty"`
	MemoryRefs      []string                 `json:"memory_refs,omitempty"`
}

type listHandoffsInput struct {
	State      string `json:"state,omitempty"`
	ForSession string `json:"for_session,omitempty"`
}

type claimHandoffInput struct {
	HandoffID       string `json:"handoff_id"`
	ClaimingSession string `json:"claiming_session"`
}

type completeHandoffInput struct {
	HandoffID         string `json:"handoff_id"`
	CompletingSession string `json:"completing_session"`
	Outcome           string `json:"outcome"`
	Notes             string `json:"notes,omitempty"`
	NextHandoffID     string `json:"next_handoff_id,omitempty"`
}

// ─── handlers ────────────────────────────────────────────────────────────────

func (m *MCPServer) toolRegisterSession(ctx context.Context, req *mcp.CallToolRequest, in registerSessionInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured",
			"curl -X POST http://localhost:6931/v1/sessions/register -d '{...}'")
	}
	if err := ValidateSessionID(in.SessionID); err != nil {
		return textResult(err.Error())
	}
	if in.Workspace == "" || in.Role == "" {
		return textResult("workspace and role are required")
	}
	now := time.Now().UTC()
	state := SessionState{
		SessionID: in.SessionID, Workspace: in.Workspace, Role: in.Role,
		Task: in.Task, Model: in.Model, Hostname: in.Hostname,
		ContextUsage: in.ContextUsage, Status: in.Status, CurrentTask: in.CurrentTask,
		Extras: in.Extras, RegisteredAt: now, LastSeen: now,
	}
	// registered_at is written in the appendFn using the canonical value
	// supplied by ApplyRegister — for a re-register of an active session
	// this is the original time (T1), not `now`. Fixes #44.
	payload := map[string]interface{}{
		"session_id": in.SessionID, "workspace": in.Workspace, "role": in.Role,
		"task": in.Task, "model": in.Model, "hostname": in.Hostname,
		"status": in.Status, "current_task": in.CurrentTask,
		"context_usage": in.ContextUsage,
	}
	for k, v := range in.Extras {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	var evt *BusBlock
	appendFn := func(canonicalRegisteredAt time.Time) error {
		payload["registered_at"] = canonicalRegisteredAt.Format(time.RFC3339Nano)
		var err error
		evt, err = m.busSessions.AppendEvent(BusSessions, EvtSessionRegister, in.SessionID, payload)
		return err
	}
	stored, created, err := m.sessionRegistry.ApplyRegister(
		state, time.Duration(defaultActiveWithinSeconds)*time.Second, now, appendFn,
	)
	if err != nil {
		return fallbackResult(fmt.Sprintf("bus append failed: %v", err), "")
	}

	// G2 PART A: record transport↔harness correlation.
	// req.Session.ID() is the MCP-protocol transport session ID assigned by the
	// SDK to this client connection. in.SessionID is the CogOS harness session
	// ID chosen by the caller. Recording the mapping here gives tool-call
	// attribution sites (withToolObserver, toolIngest) a way to resolve the
	// bound subject from the transport session ID without inspecting the harness
	// session ID explicitly. When req.Session is nil or req.Session.ID() is
	// empty (e.g. in-process test calls via CallTool or direct handler calls),
	// record is a no-op — attribution falls back to nucleus.Name unchanged.
	if req != nil && req.Session != nil {
		m.correlation.record(req.Session.ID(), in.SessionID, in.Subject)
	}

	// G0(b): optional identity binding. When subject is provided and a
	// harnessBackend is wired, create a HarnessBindingCRD linking this
	// session to the subject identity. When subject is absent (or no backend
	// is wired), skip silently — naked-by-default contract.
	var bindingCreated bool
	if in.Subject != "" && m.harnessBackend != nil {
		bindingType := in.BindingType
		if bindingType == "" {
			bindingType = "agent"
		}
		name := in.SessionID + "/" + bindingType
		binding := &subidentity.HarnessBindingCRD{
			APIVersion: "cog.os/v1alpha1",
			Kind:       "HarnessBinding",
			Metadata:   subidentity.RBACMeta{Name: name},
			Spec: subidentity.HarnessBindingSpec{
				SessionID:   in.SessionID,
				Subject:     in.Subject,
				Type:        bindingType,
				HarnessType: in.Iss,
			},
		}
		m.harnessBackend.AttachHarness(binding)
		bindingCreated = true
	}

	result := map[string]any{
		"ok": true, "session_id": in.SessionID,
		"seq": evt.Seq, "hash": evt.Hash,
		"created": created, "session": stored,
	}
	if bindingCreated {
		result["binding_created"] = true
		result["binding_subject"] = in.Subject
		result["binding_type"] = in.BindingType
		if result["binding_type"] == "" {
			result["binding_type"] = "agent"
		}
	}
	return marshalResult(result)
}

func (m *MCPServer) toolHeartbeatSession(ctx context.Context, req *mcp.CallToolRequest, in heartbeatSessionInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	if err := ValidateSessionID(in.SessionID); err != nil {
		return textResult(err.Error())
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"session_id": in.SessionID, "status": in.Status,
		"context_usage": in.ContextUsage, "current_task": in.CurrentTask,
		"at": now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = m.busSessions.AppendEvent(BusSessions, EvtSessionHeartbeat, in.SessionID, payload)
		return err
	}
	stored, ok, err := m.sessionRegistry.ApplyHeartbeat(
		in.SessionID, in.ContextUsage, in.Status, in.CurrentTask, now, appendFn,
	)
	if !ok {
		return textResult(fmt.Sprintf("session %q is not registered", in.SessionID))
	}
	if err != nil {
		// Ended-session rejection or append failure. Ended is recognised
		// by the unchanged stored.Ended flag and evt==nil.
		if stored != nil && stored.Ended && evt == nil {
			return textResult(fmt.Sprintf("session %q is already ended", in.SessionID))
		}
		return fallbackResult(fmt.Sprintf("bus append failed: %v", err), "")
	}
	return marshalResult(map[string]any{
		"ok": true, "session_id": in.SessionID,
		"seq": evt.Seq, "hash": evt.Hash, "session": stored,
	})
}

func (m *MCPServer) toolEndSession(ctx context.Context, req *mcp.CallToolRequest, in endSessionInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	if err := ValidateSessionID(in.SessionID); err != nil {
		return textResult(err.Error())
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"session_id": in.SessionID, "reason": in.Reason,
		"handoff_id": in.HandoffID,
		"ended_at":   now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = m.busSessions.AppendEvent(BusSessions, EvtSessionEnd, in.SessionID, payload)
		return err
	}
	stored, known, err := m.sessionRegistry.ApplyEnd(in.SessionID, in.Reason, in.HandoffID, now, appendFn)
	if !known {
		return textResult(fmt.Sprintf("session %q is not registered", in.SessionID))
	}
	if err != nil {
		if evt == nil && stored != nil && stored.Ended {
			return textResult(err.Error())
		}
		return fallbackResult(fmt.Sprintf("bus append failed: %v", err), "")
	}
	return marshalResult(map[string]any{
		"ok": true, "session_id": in.SessionID,
		"seq": evt.Seq, "hash": evt.Hash, "session": stored,
	})
}

func (m *MCPServer) toolListSessions(ctx context.Context, req *mcp.CallToolRequest, in listSessionsInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	now := time.Now().UTC()
	window := time.Duration(defaultActiveWithinSeconds) * time.Second
	if in.ActiveWithinSeconds > 0 {
		window = time.Duration(in.ActiveWithinSeconds) * time.Second
	}
	snap := m.sessionRegistry.Snapshot()
	entries := make([]sessionPresenceEntry, 0, len(snap))
	for _, row := range snap {
		if !in.IncludeEnded && row.Ended {
			continue
		}
		entries = append(entries, sessionPresenceEntry{
			SessionState: row,
			Active:       row.IsActive(window, now),
		})
	}
	return marshalResult(map[string]any{
		"sessions": entries,
		"count":    len(entries),
	})
}

func (m *MCPServer) toolOfferHandoff(ctx context.Context, req *mcp.CallToolRequest, in offerHandoffInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	if err := ValidateSessionID(in.FromSession); err != nil {
		return textResult("from_session: " + err.Error())
	}
	if in.ToSession != "" {
		if err := ValidateSessionID(in.ToSession); err != nil {
			return textResult("to_session: " + err.Error())
		}
	}
	if in.BootstrapPrompt == "" {
		return textResult("bootstrap_prompt is required")
	}
	if in.Task == nil {
		return textResult("task is required")
	}
	if title, _ := in.Task["title"].(string); title == "" {
		return textResult("task.title is required")
	}
	if goal, _ := in.Task["goal"].(string); goal == "" {
		return textResult("task.goal is required")
	}
	if steps, ok := in.Task["next_steps"].([]interface{}); !ok || len(steps) == 0 {
		return textResult("task.next_steps must be a non-empty list")
	}
	if in.TTLSeconds < 0 {
		return textResult("ttl_seconds must be >= 0")
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = 3600
	}

	now := time.Now().UTC()
	handoffID := mintHandoffID(now)
	state := HandoffState{
		HandoffID: handoffID, FromSession: in.FromSession,
		ToSession: in.ToSession, Reason: in.Reason,
		TTLSeconds: in.TTLSeconds, CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second),
	}
	payload := map[string]interface{}{
		"handoff_id":       handoffID,
		"from_session":     in.FromSession,
		"to_session":       in.ToSession,
		"reason":           in.Reason,
		"ttl_seconds":      in.TTLSeconds,
		"created_at":       now.Format(time.RFC3339Nano),
		"task":             in.Task,
		"bootstrap_prompt": in.BootstrapPrompt,
		"bus_context_refs": in.BusContextRefs,
		"memory_refs":      in.MemoryRefs,
	}
	state.OfferPayload = payload

	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = m.busSessions.AppendEvent(BusHandoffs, EvtHandoffOffer, in.FromSession, payload)
		return err
	}
	stored, err := m.handoffRegistry.ApplyOffer(state, now, appendFn)
	if err != nil {
		return fallbackResult(fmt.Sprintf("bus append failed: %v", err), "")
	}
	return marshalResult(map[string]any{
		"ok": true, "handoff_id": handoffID,
		"seq": evt.Seq, "hash": evt.Hash,
		"handoff": stored,
	})
}

func (m *MCPServer) toolListHandoffs(ctx context.Context, req *mcp.CallToolRequest, in listHandoffsInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	now := time.Now().UTC()
	snap := m.handoffRegistry.Snapshot()
	out := make([]*HandoffState, 0, len(snap))
	for _, h := range snap {
		effective := h.State
		if effective == HandoffStateOpen && h.IsExpired(now) {
			effective = "expired"
		}
		if in.State != "" && effective != in.State {
			continue
		}
		if in.ForSession != "" && h.ToSession != "" && h.ToSession != in.ForSession {
			continue
		}
		out = append(out, h)
	}
	return marshalResult(map[string]any{
		"handoffs": out,
		"count":    len(out),
	})
}

func (m *MCPServer) toolClaimHandoff(ctx context.Context, req *mcp.CallToolRequest, in claimHandoffInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	if in.HandoffID == "" {
		return textResult("handoff_id is required")
	}
	if err := ValidateSessionID(in.ClaimingSession); err != nil {
		return textResult("claiming_session: " + err.Error())
	}
	now := time.Now().UTC()
	claimPayload := map[string]interface{}{
		"handoff_id":       in.HandoffID,
		"claiming_session": in.ClaimingSession,
		"claimed_at":       now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = m.busSessions.AppendEvent(BusHandoffs, EvtHandoffClaim, in.ClaimingSession, claimPayload)
		return err
	}
	result, appendErr := m.handoffRegistry.ApplyClaim(in.HandoffID, in.ClaimingSession, now, appendFn)
	if appendErr != nil {
		return fallbackResult(fmt.Sprintf("bus append failed: %v", appendErr), "")
	}
	if result.Rejection != "" {
		// Emit claim_rejected (amendment #4). Always include
		// conflicting_session even when empty — stable schema for audit.
		payload := map[string]interface{}{
			"handoff_id":          in.HandoffID,
			"attempting_session":  in.ClaimingSession,
			"conflicting_session": result.ConflictingSession,
			"reason":              string(result.Rejection),
			"rejected_at":         now.Format(time.RFC3339Nano),
		}
		_, _ = m.busSessions.AppendEvent(BusHandoffs, EvtHandoffClaimRejected, in.ClaimingSession, payload)
		return textResult(fmt.Sprintf("claim rejected: %s", result.Rejection))
	}
	return marshalResult(map[string]any{
		"ok":         true,
		"handoff_id": in.HandoffID,
		"seq":        evt.Seq,
		"hash":       evt.Hash,
		"handoff":    result.Offer,
		"offer":      result.Offer.OfferPayload,
	})
}

func (m *MCPServer) toolCompleteHandoff(ctx context.Context, req *mcp.CallToolRequest, in completeHandoffInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured", "")
	}
	if in.HandoffID == "" {
		return textResult("handoff_id is required")
	}
	if err := ValidateSessionID(in.CompletingSession); err != nil {
		return textResult("completing_session: " + err.Error())
	}
	if in.Outcome == "" {
		return textResult("outcome is required")
	}
	now := time.Now().UTC()
	payload := map[string]interface{}{
		"handoff_id":         in.HandoffID,
		"completing_session": in.CompletingSession,
		"outcome":            in.Outcome,
		"notes":              in.Notes,
		"next_handoff_id":    in.NextHandoffID,
		"completed_at":       now.Format(time.RFC3339Nano),
	}
	var evt *BusBlock
	appendFn := func() error {
		var err error
		evt, err = m.busSessions.AppendEvent(BusHandoffs, EvtHandoffComplete, in.CompletingSession, payload)
		return err
	}
	stored, reason, appendErr := m.handoffRegistry.ApplyComplete(
		in.HandoffID, in.CompletingSession, in.Outcome, in.Notes, in.NextHandoffID, now, appendFn,
	)
	if appendErr != nil {
		return fallbackResult(fmt.Sprintf("bus append failed: %v", appendErr), "")
	}
	if reason == ClaimRejectedOfferNotFound {
		return textResult(fmt.Sprintf("handoff %q not found", in.HandoffID))
	}
	if reason == ClaimRejectedOutOfOrder {
		return textResult(fmt.Sprintf("handoff %q cannot complete in state %q", in.HandoffID, stored.State))
	}
	return marshalResult(map[string]any{
		"ok":         true,
		"handoff_id": in.HandoffID,
		"seq":        evt.Seq,
		"hash":       evt.Hash,
		"handoff":    stored,
	})
}
