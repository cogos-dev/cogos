// dashboard_inlet.go — Engine-side dashboard chat bridge.
//
// This file restores the chat bridge that was removed in commit 83e0e71
// ("chore: remove root serveServer web, Track 5 Sweep") as collateral damage
// when serve_daemon.go was deleted.
//
// The bridge:
//   Mod³ → POST /v1/bus/send bus_id=bus_dashboard_chat
//        → BusSessionManager.AppendEvent dispatches to handlers
//        → handleEngineDashboardChatEvent enqueues to enginePendingMsgs
//        → LocalHarnessController.runCycle drains queue, enriches observation
//        → agent invokes `respond` tool OR ensureUserTurnReply fires fallback
//        → enginePublishDashboardResponse writes to bus_dashboard_response
//        → SSE subscribers (Mod³) receive the response
//
// Wiring: call InstallEngineDashboardInlet(mgr) once at server startup
// alongside SetBusSessionManager on the LocalHarnessController.
package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// --- Constants ---

// engineDashboardChatBusID is the inbound bus (user → kernel). Mod³ produces here.
const engineDashboardChatBusID = "bus_dashboard_chat"

// engineDashboardResponseBusID is the outbound bus (kernel → user). Mod³ subscribes.
const engineDashboardResponseBusID = "bus_dashboard_response"

// engineRespondToolName is the canonical name the model calls to reply.
const engineRespondToolName = "respond"

// enginePendingMsgCap caps the queue; messages beyond this drop the oldest.
const enginePendingMsgCap = 100

// --- Context helpers for session ID fan-out ---
//
// These mirror the helpers in the root package's agent_harness.go. They live
// here so internal/engine callers (runCycle, respond executor) don't need to
// import package main.

type sessionIDKey struct{}
type sessionIDsKey struct{}

// sessionIDFromContext extracts the dashboard session_id from ctx, or "".
func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionIDKey{}).(string)
	return v
}

// WithSessionID returns ctx carrying the given dashboard session_id.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// sessionIDsFromContext returns the fan-out list of session_ids, or nil.
func sessionIDsFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(sessionIDsKey{}).([]string)
	if len(v) == 0 {
		return nil
	}
	return v
}

// WithSessionIDs returns ctx carrying the fan-out list of session_ids.
func WithSessionIDs(ctx context.Context, ids []string) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	cp := make([]string, len(ids))
	copy(cp, ids)
	return context.WithValue(ctx, sessionIDsKey{}, cp)
}

// --- Pending user-message queue ---

// EnginePendingUserMsg is one user message awaiting agent observation.
type EnginePendingUserMsg struct {
	Text      string
	SessionID string
	Ts        time.Time
}

var (
	enginePendingMu   sync.Mutex
	enginePendingMsgs []EnginePendingUserMsg // FIFO (append tail, drain head)
)

// EnqueueEnginePendingUserMessage appends m to the engine pending queue.
// If the queue is at capacity, the oldest entry is dropped with a warning.
func EnqueueEnginePendingUserMessage(m EnginePendingUserMsg) {
	enginePendingMu.Lock()
	defer enginePendingMu.Unlock()
	if len(enginePendingMsgs) >= enginePendingMsgCap {
		dropped := enginePendingMsgs[0]
		enginePendingMsgs = enginePendingMsgs[1:]
		slog.Warn("dashboard-inlet: pending queue full, dropping oldest message",
			"cap", enginePendingMsgCap, "session", dropped.SessionID,
			"age", time.Since(dropped.Ts).Round(time.Second))
	}
	enginePendingMsgs = append(enginePendingMsgs, m)
}

// DrainEnginePendingUserMessages returns the current queue contents and clears
// it. The returned slice is independent — mutation does not affect the queue.
func DrainEnginePendingUserMessages() []EnginePendingUserMsg {
	enginePendingMu.Lock()
	defer enginePendingMu.Unlock()
	if len(enginePendingMsgs) == 0 {
		return nil
	}
	out := make([]EnginePendingUserMsg, len(enginePendingMsgs))
	copy(out, enginePendingMsgs)
	enginePendingMsgs = enginePendingMsgs[:0]
	return out
}

// --- Bus manager registry ---

// engineDashboardBusMgr holds the bus manager for publishing responses.
// Atomic pointer for lock-free hot-path reads in the respond executor.
var engineDashboardBusMgr atomic.Pointer[BusSessionManager]

// InstallEngineDashboardInlet wires bus_dashboard_chat → engine pending queue.
//
// mgr must be non-nil. The handler is registered once; subsequent calls to the
// same manager are idempotent (the bus manager deduplicates by handler name).
// Safe to call at daemon start alongside BusSessionManager setup.
func InstallEngineDashboardInlet(mgr *BusSessionManager) {
	if mgr == nil {
		slog.Warn("dashboard-inlet: skip: nil bus manager")
		return
	}
	// Store the first manager as the canonical publisher. Subsequent calls
	// (e.g. multi-workspace setups) must not overwrite so responses always
	// reach the same bus directory the SSE consumers are watching.
	if engineDashboardBusMgr.Load() == nil {
		engineDashboardBusMgr.Store(mgr)
	}

	ensureEngineDashboardBuses(mgr)
	mgr.AddEventHandler("engine-dashboard-inlet", handleEngineDashboardChatEvent)
	slog.Info("dashboard-inlet: engine inlet installed", "bus", engineDashboardChatBusID)
}

// ensureEngineDashboardBuses creates the chat + response bus directories if
// they don't already exist.
func ensureEngineDashboardBuses(mgr *BusSessionManager) {
	for _, busID := range [...]string{engineDashboardChatBusID, engineDashboardResponseBusID} {
		if err := mgr.EnsureBus(busID); err != nil {
			slog.Warn("dashboard-inlet: ensure bus failed", "bus_id", busID, "err", err)
			continue
		}
		if err := mgr.RegisterBus(busID, "kernel:dashboard", "kernel:dashboard"); err != nil {
			slog.Warn("dashboard-inlet: register bus failed", "bus_id", busID, "err", err)
		}
	}
}

// handleEngineDashboardChatEvent is the bus handler registered on the engine's
// BusSessionManager. It filters non-target buses, extracts the user message
// text, and enqueues it onto the pending FIFO.
//
// Expected Mod³ payload shape:
//
//	{"type": "user_message", "text": "hello agent", "session_id": "...", "ts": "..."}
func handleEngineDashboardChatEvent(busID string, block *BusBlock) {
	if busID != engineDashboardChatBusID || block == nil {
		return
	}
	// Skip self-originated events to prevent feedback loops.
	if block.From == "kernel:cogos" || block.From == "kernel:dashboard" {
		return
	}

	text := extractEngineDashboardText(block)
	if text == "" {
		slog.Debug("dashboard-inlet: drop: no text in block",
			"seq", block.Seq, "from", block.From, "type", block.Type)
		return
	}

	ts := time.Now().UTC()
	if block.Ts != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, block.Ts); err == nil {
			ts = parsed
		}
	}
	sessionID := ""
	if v, ok := block.Payload["session_id"].(string); ok {
		sessionID = v
	}

	EnqueueEnginePendingUserMessage(EnginePendingUserMsg{
		Text:      text,
		SessionID: sessionID,
		Ts:        ts,
	})
	slog.Info("dashboard-inlet: queued user message",
		"text_len", len(text), "session", sessionID, "from", block.From)
}

// extractEngineDashboardText pulls message body from a BusBlock payload.
// Accepts either "text" or "content" keys.
func extractEngineDashboardText(block *BusBlock) string {
	if block == nil || block.Payload == nil {
		return ""
	}
	if v, ok := block.Payload["text"].(string); ok && v != "" {
		return v
	}
	if v, ok := block.Payload["content"].(string); ok && v != "" {
		return v
	}
	return ""
}

// enginePublishDashboardResponse publishes a structured agent_response to
// bus_dashboard_response for Mod³ to consume via /v1/events/stream SSE.
//
// sessionID, when non-empty, is included so Mod³ can filter to the originating
// client and avoid cross-talk in multi-session setups.
func enginePublishDashboardResponse(text, reasoning, sessionID string) (int, error) {
	mgr := engineDashboardBusMgr.Load()
	if mgr == nil {
		return 0, errEngineDashboardNotInstalled
	}

	ensureEngineDashboardBuses(mgr)

	payload := map[string]interface{}{
		"type": "agent_response",
		"text": text,
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if reasoning != "" {
		payload["reasoning"] = reasoning
	}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	n := len(raw)

	if _, err := mgr.AppendEvent(engineDashboardResponseBusID, "agent_response", "kernel:cogos", payload); err != nil {
		return 0, err
	}
	return n, nil
}

// errEngineDashboardNotInstalled is returned when the respond tool is invoked
// before InstallEngineDashboardInlet has wired a bus manager.
var errEngineDashboardNotInstalled = errDashboardNotReady("dashboard inlet not installed: call InstallEngineDashboardInlet first")

// errDashboardNotReady is a sentinel error type for the not-installed condition.
type errDashboardNotReady string

func (e errDashboardNotReady) Error() string { return string(e) }

// --- Respond tool for KernelToolRegistry ---

// engineRespondInvokeCount is incremented on every successful respond dispatch.
// The harness cycle snapshots before Execute and compares after to decide whether
// the auto-fallback should fire.
var engineRespondInvokeCount uint64

// EngineRespondInvokeSnapshot returns the current respond-call counter.
func EngineRespondInvokeSnapshot() uint64 {
	return atomic.LoadUint64(&engineRespondInvokeCount)
}

// EngineRespondInvokedSince reports whether respond was called since snapshot.
func EngineRespondInvokedSince(snapshot uint64) bool {
	return atomic.LoadUint64(&engineRespondInvokeCount) > snapshot
}

// engineRespondPublish is the seam for the respond executor. Tests swap this
// to capture output without a live bus manager.
var engineRespondPublish = enginePublishDashboardResponse

// engineRespondExecutor is the toolExecutor for the `respond` tool.
// It fans out across all session IDs in ctx (set by runCycle after draining
// the pending queue) and publishes one reply per unique session.
func engineRespondExecutor(ctx context.Context, arguments string) (string, error) {
	var p struct {
		Text      string `json:"text"`
		Reasoning string `json:"reasoning"`
	}
	if arguments != "" {
		if err := json.Unmarshal([]byte(arguments), &p); err != nil {
			return "", err
		}
	}
	if p.Text == "" {
		result, _ := json.Marshal(map[string]interface{}{
			"ok":    false,
			"error": "text is required",
		})
		return string(result), nil
	}

	// Fan out: one reply per originating session. Falls back to single-id
	// (or broadcast when id is empty) when the multi-session key is absent.
	sessionIDs := sessionIDsFromContext(ctx)
	if len(sessionIDs) == 0 {
		sessionIDs = []string{sessionIDFromContext(ctx)}
	}

	var (
		totalBytes int
		firstErr   error
		anySuccess bool
	)
	for _, sid := range sessionIDs {
		n, err := engineRespondPublish(p.Text, p.Reasoning, sid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		anySuccess = true
		totalBytes += n
	}

	if !anySuccess {
		// All publishes failed — do NOT bump counter so auto-fallback fires.
		errResult, _ := json.Marshal(map[string]interface{}{
			"ok":    false,
			"error": firstErr.Error(),
		})
		return string(errResult), nil
	}

	// Bump exactly once per tool invocation (N fan-out publishes = 1 call).
	atomic.AddUint64(&engineRespondInvokeCount, 1)

	result, _ := json.Marshal(map[string]interface{}{
		"ok":         true,
		"bytes":      totalBytes,
		"recipients": len(sessionIDs),
	})
	return string(result), nil
}

// respondToolDefinition returns the ToolDefinition for the `respond` tool.
func respondToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        engineRespondToolName,
		Description: "Send a response to the user in the current dashboard conversation. Use this after you have observed a user_message event and want to reply. The message is published on bus_dashboard_response for the Mod³ dashboard to render. Call at most once per user turn; use wait if no reply is warranted.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{
					"type":        "string",
					"description": "The reply text to show the user.",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "Optional internal reasoning/trace for auditing. Not shown to the user directly.",
				},
			},
			"required": []string{"text"},
		},
	}
}

// AddRespondTool injects the `respond` native tool into a KernelToolRegistry.
// Must be called before the registry is Scoped for the consolidation scope
// (or the tool name must already be present in the scope's tool list).
func AddRespondTool(r *KernelToolRegistry) {
	if r == nil {
		return
	}
	def := respondToolDefinition()
	r.definitions = append(r.definitions, def)
	r.executors[def.Name] = engineRespondExecutor
}
