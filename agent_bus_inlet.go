// agent_bus_inlet.go — Dashboard chat inlet for the metabolic cycle.
//
// Wires the Mod³ dashboard to the cogos kernel's external-event channel:
//   Mod³ → POST /v1/bus/send bus_id=bus_dashboard_chat
//        → appendBusEvent dispatches to in-process handlers
//        → this file's handler → engine.Process.SubmitExternal
//
// The inverse direction (kernel → Mod³) is handled by the `respond` tool
// (agent_tools_respond.go), which publishes to bus_dashboard_response; Mod³
// subscribes via the existing /v1/events/stream SSE endpoint.
//
// Wiring: call InstallDashboardInlet(busMgr, process) once at daemon start
// alongside InstallTraceEmitter. The process argument may be nil (main daemon
// does not currently own a *engine.Process); in that case the handler still
// ensures the bus exists and logs incoming messages so the wiring is
// observable even before the v3 process is instantiated in this binary.
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/myrgic/cogos/internal/engine"
)

// --- Pending user-message queue ---
//
// The primary delivery path for dashboard user messages is this package-global
// FIFO. The harness's runCycle drains it at the top of each iteration and
// enriches its observation, so the cycle observes the message even though the
// daemon does not currently own an *engine.Process.
//
// The queue is bounded (pendingMsgCap) and non-blocking: when full, the oldest
// entry is dropped with a warn log. The zero-value is usable; no init needed.

// pendingUserMsg is one user message awaiting agent observation.
type pendingUserMsg struct {
	Text      string
	SessionID string
	Ts        time.Time
}

// pendingMsgCap caps the queue; messages beyond this drop the oldest entry.
// Sized generously for single-user bursts; keeps memory bounded if Mod³ ever
// runs away.
const pendingMsgCap = 100

var (
	pendingMu   sync.Mutex
	pendingMsgs []pendingUserMsg // FIFO (append tail, drain head)
)

// enqueuePendingUserMessage appends m to the pending queue. If the queue is
// already at pendingMsgCap, the oldest entry is dropped and a warn is logged.
func enqueuePendingUserMessage(m pendingUserMsg) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if len(pendingMsgs) >= pendingMsgCap {
		dropped := pendingMsgs[0]
		pendingMsgs = pendingMsgs[1:]
		log.Printf("[dashboard-inlet] warn: pending queue full (%d), dropping oldest msg (session=%s, age=%s)",
			pendingMsgCap, dropped.SessionID, time.Since(dropped.Ts).Round(time.Second))
	}
	pendingMsgs = append(pendingMsgs, m)
}

// drainPendingUserMessages returns the current queue and clears it. Callers
// receive an independent slice — mutation does not affect the queue.
func drainPendingUserMessages() []pendingUserMsg {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if len(pendingMsgs) == 0 {
		return nil
	}
	out := make([]pendingUserMsg, len(pendingMsgs))
	copy(out, pendingMsgs)
	pendingMsgs = pendingMsgs[:0]
	return out
}

// peekPendingUserMessages returns a copy of the queue without clearing it.
// Kept for future peek-and-ack semantics; currently unused by the harness.
func peekPendingUserMessages() []pendingUserMsg {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if len(pendingMsgs) == 0 {
		return nil
	}
	out := make([]pendingUserMsg, len(pendingMsgs))
	copy(out, pendingMsgs)
	return out
}

// dashboardChatBusID is the inbound bus (user → kernel). Mod³ produces here.
const dashboardChatBusID = "bus_dashboard_chat"

// dashboardResponseBusID is the outbound bus (kernel → user). Mod³ subscribes.
const dashboardResponseBusID = "bus_dashboard_response"

// dashboardInletProcess holds the engine.Process target for inbound messages.
// Allows late-binding: if the process is instantiated after the inlet is
// installed, RebindDashboardInlet can swap in the live instance without
// re-subscribing on the bus. Accessed via atomic Pointer to keep the hot path
// (handler) lock-free.
var dashboardInletProcess atomic.Pointer[engine.Process]

// dashboardInletBusMgr retains the bus manager for the respond tool.
var dashboardInletBusMgr atomic.Pointer[busSessionManager]

// InstallDashboardInlet wires bus_dashboard_chat → process.externalCh.
//
// mgr must be non-nil (no-op otherwise). process may be nil; it can be bound
// later via RebindDashboardInlet. The handler is registered on the bus
// manager and is not explicitly de-registered — lifetime tracks the daemon.
//
// Safe to call once at daemon start; subsequent calls overwrite state.
func InstallDashboardInlet(mgr *busSessionManager, process *engine.Process) {
	if mgr == nil {
		log.Printf("[dashboard-inlet] skip: nil bus manager")
		return
	}
	// Publish manager: keep the FIRST install's manager as the canonical
	// publisher for outbound agent responses. Subsequent installs (e.g. when
	// registering the handler on additional workspace managers) must NOT
	// overwrite this, otherwise responses end up written to a different
	// workspace's on-disk bus directory than SSE consumers are reading from.
	// The handler can still be added to every manager safely.
	if dashboardInletBusMgr.Load() == nil {
		dashboardInletBusMgr.Store(mgr)
	}
	if process != nil {
		dashboardInletProcess.Store(process)
	}

	ensureDashboardBuses(mgr)

	mgr.AddEventHandler("dashboard-inlet", handleDashboardChatEvent)
	if process == nil {
		log.Printf("[dashboard-inlet] installed (bus=%s, process=nil — submissions will be dropped until RebindDashboardInlet is called)", dashboardChatBusID)
	} else {
		log.Printf("[dashboard-inlet] installed (bus=%s)", dashboardChatBusID)
	}
}

// RebindDashboardInlet swaps in a live engine.Process after installation.
// Useful when the main package gains a process instance later in the startup
// sequence than InstallTraceEmitter runs.
func RebindDashboardInlet(process *engine.Process) {
	if process == nil {
		return
	}
	dashboardInletProcess.Store(process)
	log.Printf("[dashboard-inlet] process bound")
}

// ensureDashboardBuses creates the chat + response bus directories and
// registry entries if they don't already exist. Mirrors ensureTraceBus.
func ensureDashboardBuses(mgr *busSessionManager) {
	for _, busID := range [...]string{dashboardChatBusID, dashboardResponseBusID} {
		busDir := filepath.Join(mgr.busesDir(), busID)
		if err := os.MkdirAll(busDir, 0o755); err != nil {
			log.Printf("[dashboard-inlet] create bus dir %s: %v", busID, err)
			continue
		}
		eventsFile := filepath.Join(busDir, "events.jsonl")
		if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
			if f, err := os.Create(eventsFile); err == nil {
				f.Close()
			}
		}
		if err := mgr.registerBus(busID, "kernel:dashboard", "kernel:dashboard"); err != nil {
			log.Printf("[dashboard-inlet] register bus %s: %v", busID, err)
		}
	}
}

// handleDashboardChatEvent is the bus handler for dashboardChatBusID.
// It filters out non-target buses, extracts a user_message payload, builds a
// GateEvent, and submits to the engine process with non-blocking semantics.
//
// Expected Mod³ payload shape:
//
//	{"type": "user_message", "text": "hello agent", "session_id": "...", "ts": "..."}
//
// Block.Type is set by Mod³ on the /v1/bus/send call; we accept either
// "user_message" or the generic "message" type and use the payload's "text"
// or "content" field. Everything else is dropped.
func handleDashboardChatEvent(busID string, block *CogBlock) {
	if busID != dashboardChatBusID || block == nil {
		return
	}

	// Skip anything that originated from the kernel itself — otherwise a
	// future self-echo could create a feedback loop.
	if block.From == "kernel:cogos" || block.From == "kernel:dashboard" {
		return
	}

	text := extractDashboardText(block)
	if text == "" {
		log.Printf("[dashboard-inlet] drop: no text in block seq=%d from=%s type=%s", block.Seq, block.From, block.Type)
		return
	}

	// Parse ts / session_id once — both the pending-queue path and the
	// (future) engine.Process path use them.
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

	// Primary delivery: enqueue onto the pending queue the harness's
	// runCycle drains. This is what actually reaches the running loop.
	enqueuePendingUserMessage(pendingUserMsg{
		Text:      text,
		SessionID: sessionID,
		Ts:        ts,
	})
	log.Printf("[dashboard-inlet] queued user message (text_len=%d session=%s from=%s)", len(text), sessionID, block.From)

	// Secondary delivery: if an engine.Process has been bound via
	// RebindDashboardInlet, also forward the message as a GateEvent.
	// Forward-compatible — currently no-op in the daemon, which does not
	// own a *engine.Process.
	proc := dashboardInletProcess.Load()
	if proc == nil {
		return
	}

	evt := &engine.GateEvent{
		Type:      "user.message",
		Content:   text,
		Timestamp: ts,
		SessionID: sessionID,
		Data: map[string]interface{}{
			"source":     "dashboard",
			"bus_id":     busID,
			"block_seq":  block.Seq,
			"block_hash": block.Hash,
			"from":       block.From,
			"mod3_type":  block.Type,
		},
	}

	if !proc.SubmitExternal(evt) {
		log.Printf("[dashboard-inlet] warn: externalCh full, dropping dashboard message (seq=%d from=%s)", block.Seq, block.From)
	}
}

// extractDashboardText pulls the message body out of a dashboard chat block.
// Accepts either "text" or "content" on the payload.
func extractDashboardText(block *CogBlock) string {
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

// publishDashboardResponse is the inverse of the inlet: it publishes a
// structured agent_response onto bus_dashboard_response for Mod³ to consume.
// Exposed to the respond tool in agent_tools_respond.go; kept in this file so
// the two directions of the chat channel sit next to each other.
//
// sessionID, when non-empty, is included in the payload so subscribers can
// filter on it — without it Mod³ broadcasts replies to every connected
// client and multi-client setups see cross-talk. Pair with the dashboard
// inlet's per-turn session_id capture (handleDashboardChatEvent populates
// pendingUserMsg.SessionID; ServeAgent.runCycle threads it through ctx via
// WithSessionID, the respond tool reads it back here).
func publishDashboardResponse(text, reasoning, sessionID string) (int, error) {
	mgr := dashboardInletBusMgr.Load()
	if mgr == nil {
		return 0, errDashboardNotInstalled
	}

	// Defensive: make sure the response bus exists even if Install was called
	// with only a chat subscriber (e.g. a misordered startup).
	ensureDashboardBuses(mgr)

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

	if _, err := mgr.appendBusEvent(dashboardResponseBusID, "agent_response", "kernel:cogos", payload); err != nil {
		return 0, err
	}
	return n, nil
}
