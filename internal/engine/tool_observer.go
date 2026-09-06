// tool_observer.go — Tool-call observability: emission + pending-call correlation.
//
// Agent S (survey-2026-04-21-agent-S-tool-bridge-design) specifies that every
// tool invocation observed by the kernel should emit a "tool.call" ledger event
// and — when the result is known — a paired "tool.result" event. The gate
// recognizer at gate.go:94 has accepted those types since day one but nothing
// emitted them. This file fills that gap and activates the scaffolded-but-never-
// wired per-tool-call observation surface identified in issue #22.
//
// The kernel observes two kinds of tool invocation:
//
//  1. Kernel-ownership (MCP handlers, internal tool-loop executions).
//     The kernel runs the tool inline; call and result are both emitted by
//     withToolObserver right around the handler invocation. No correlation
//     cache is needed — the result is known the moment the handler returns.
//
//  2. Client-ownership (provider returned a tool_call the client must execute).
//     The kernel emits the tool.call immediately, registers a pending entry,
//     and emits tool.result later when the client sends a matching role=tool
//     message back. The pending-call cache lives for a bounded TTL and is
//     swept periodically; entries that exceed the TTL get a
//     tool.result{status=timeout} event so the audit trail never leaves a
//     tool.call dangling forever.
//
// All emission flows through process.emitEvent → AppendEvent, i.e. the same
// hash-chained ledger Agent L's read tools query and Agent N's event bus tails.
// No orphan writer. The root-package tools_bridge.go is explicitly NOT the
// model for this file — that file bypasses AppendEvent and is slated for
// deletion with Agent I's purge.
package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool-call source taxonomy — where the invocation was observed.
const (
	ToolSourceMCP        = "mcp"
	ToolSourceOpenAI     = "openai-chat"
	ToolSourceAnthropic  = "anthropic-messages"
	ToolSourceKernelLoop = "kernel-loop"
)

// Tool-call ownership taxonomy — who executes the tool body.
const (
	ToolOwnershipKernel = "kernel"
	ToolOwnershipClient = "client"
)

// internalToolNames is the closed set of tool names the CogOS kernel knows
// how to execute itself — mostly Claude CLI built-ins routed via Claude
// CLI's `--allowed-tools`. Everything else is assumed to belong to the
// calling client (BrowserOS-style passthrough). Keep this in lockstep with
// harness/tools.go:internalToolCLINames; the two packages live in separate
// Go modules so we can't share the map directly.
var internalToolNames = map[string]struct{}{
	"exec":        {},
	"bash":        {},
	"shell":       {},
	"read":        {},
	"file_read":   {},
	"write":       {},
	"file_write":  {},
	"edit":        {},
	"apply-patch": {},
	"apply_patch": {},
	"search":      {},
	"grep":        {},
	"glob":        {},
	"find":        {},
}

// classifyToolOwnership returns the ownership label for a given tool name.
// Case-insensitive match against the internal-tool set; anything not in the
// set is ToolOwnershipClient so tool definitions from BrowserOS, custom
// agents, etc. survive the pipeline.
func classifyToolOwnership(name string) string {
	if _, ok := internalToolNames[strings.ToLower(name)]; ok {
		return ToolOwnershipKernel
	}
	return ToolOwnershipClient
}

// Tool-result status taxonomy — the outcome of an invocation.
const (
	ToolStatusPending  = "pending"
	ToolStatusSuccess  = "success"
	ToolStatusError    = "error"
	ToolStatusRejected = "rejected"
	ToolStatusTimeout  = "timeout"
)

// Output summary length cap — a stored excerpt of tool output. Keep in sync
// with Agent S §4.2 and §7.6 (truncate output to 200 chars at emit time).
const toolOutputSummaryChars = 200

// ToolCallEvent is the domain shape for a `tool.call` ledger entry.
// Fields map 1:1 to the ledger event payload spec in Agent S §4.2.
type ToolCallEvent struct {
	CallID        string          `json:"call_id"`
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Source        string          `json:"source"`
	Ownership     string          `json:"ownership"`
	Provider      string          `json:"provider,omitempty"`
	InteractionID string          `json:"interaction_id,omitempty"`
	TurnIndex     int             `json:"turn_index,omitempty"`
	SessionID     string          `json:"session_id"`
}

// ToolResultEvent mirrors ToolCallEvent for a completion.
type ToolResultEvent struct {
	CallID        string        `json:"call_id"`
	ToolName      string        `json:"tool_name"`
	Status        string        `json:"status"`
	Reason        string        `json:"reason,omitempty"`
	OutputLength  int           `json:"output_length"`
	OutputSummary string        `json:"output_summary,omitempty"`
	Duration      time.Duration `json:"-"`
	Source        string        `json:"source"`
	SessionID     string        `json:"session_id"`
}

// pendingToolCall is the in-memory correlation-cache entry for a client-
// ownership call awaiting a result. Never written to disk.
type pendingToolCall struct {
	CallID    string
	ToolName  string
	Source    string
	SessionID string
	TurnIndex int
	EmittedAt time.Time
}

// pendingToolCallRegistry bounds the pending-call cache. Entries older than
// pendingToolCallTTL get an automatic tool.result{status=timeout} on the next
// sweep and are evicted. The ring is size-capped at pendingToolCallMaxEntries
// so a misbehaving client cannot grow the map without bound — oldest entries
// are evicted first (with a timeout result emitted for them).
const (
	pendingToolCallTTL        = 10 * time.Minute
	pendingToolCallSweep      = 60 * time.Second
	pendingToolCallMaxEntries = 1024
)

// pendingToolCallRegistry owns the pending-call correlation cache.
type pendingToolCallRegistry struct {
	mu      sync.Mutex
	entries map[string]*pendingToolCall // call_id → metadata
}

func newPendingToolCallRegistry() *pendingToolCallRegistry {
	return &pendingToolCallRegistry{entries: make(map[string]*pendingToolCall)}
}

// register adds a pending-call entry. If the cache is at capacity, the oldest
// entry is evicted with a timeout emission via evictFn (nil allowed in tests).
func (r *pendingToolCallRegistry) register(entry *pendingToolCall, evict func(*pendingToolCall)) {
	if entry == nil || entry.CallID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Cap: evict oldest when at capacity (excluding the new entry itself).
	if len(r.entries) >= pendingToolCallMaxEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, v := range r.entries {
			if oldestKey == "" || v.EmittedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.EmittedAt
			}
		}
		if oldestKey != "" {
			victim := r.entries[oldestKey]
			delete(r.entries, oldestKey)
			if evict != nil {
				evict(victim)
			}
		}
	}

	r.entries[entry.CallID] = entry
}

// take removes and returns the pending entry for a call_id, or nil if not found.
func (r *pendingToolCallRegistry) take(callID string) *pendingToolCall {
	if callID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[callID]
	if !ok {
		return nil
	}
	delete(r.entries, callID)
	return entry
}

// expired returns all entries with EmittedAt older than now-ttl and removes
// them from the registry. Used by the sweep goroutine.
func (r *pendingToolCallRegistry) expired(now time.Time, ttl time.Duration) []*pendingToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var victims []*pendingToolCall
	cutoff := now.Add(-ttl)
	for k, v := range r.entries {
		if v.EmittedAt.Before(cutoff) {
			victims = append(victims, v)
			delete(r.entries, k)
		}
	}
	return victims
}

// len returns the current number of pending entries (for tests / metrics).
func (r *pendingToolCallRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// emitToolCall appends a tool.call event to the session ledger via AppendEvent.
// The event shape matches Agent S §4.2.
func (p *Process) emitToolCall(e ToolCallEvent) {
	if p == nil || p.cfg == nil {
		return
	}
	sessionID := e.SessionID
	if sessionID == "" {
		sessionID = p.sessionID
	}

	data := map[string]interface{}{
		"call_id":   e.CallID,
		"tool_name": e.ToolName,
		"source":    e.Source,
		"ownership": e.Ownership,
	}
	if len(e.Arguments) > 0 {
		// Preserve the raw JSON. The ledger canonicalizer will stringify it.
		data["arguments"] = json.RawMessage(e.Arguments)
	}
	if e.Provider != "" {
		data["provider"] = e.Provider
	}
	if e.InteractionID != "" {
		data["interaction_id"] = e.InteractionID
	}
	if e.TurnIndex > 0 {
		data["turn_index"] = e.TurnIndex
	}

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "tool.call",
			Timestamp: nowISO(),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "kernel-v3"},
	}
	if err := AppendEvent(p.cfg.WorkspaceRoot, sessionID, env); err != nil {
		slog.Debug("process: emitToolCall append failed", "err", err, "call_id", e.CallID)
	}
}

// emitToolResult appends a tool.result event to the session ledger.
func (p *Process) emitToolResult(e ToolResultEvent) {
	if p == nil || p.cfg == nil {
		return
	}
	sessionID := e.SessionID
	if sessionID == "" {
		sessionID = p.sessionID
	}

	data := map[string]interface{}{
		"call_id":       e.CallID,
		"tool_name":     e.ToolName,
		"status":        e.Status,
		"output_length": e.OutputLength,
		"source":        e.Source,
		"duration_ms":   int(e.Duration.Milliseconds()),
	}
	if e.Reason != "" {
		data["reason"] = e.Reason
	}
	if e.OutputSummary != "" {
		data["output_summary"] = e.OutputSummary
	}

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "tool.result",
			Timestamp: nowISO(),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: "kernel-v3"},
	}
	if err := AppendEvent(p.cfg.WorkspaceRoot, sessionID, env); err != nil {
		slog.Debug("process: emitToolResult append failed", "err", err, "call_id", e.CallID)
	}
}

// registerPendingToolCall adds a client-ownership call to the correlation
// cache. When the matching role=tool message lands, resolvePendingToolCall
// will emit the paired tool.result.
func (p *Process) registerPendingToolCall(callID, toolName, source string, turnIndex int) {
	if p == nil || callID == "" {
		return
	}
	if p.pendingToolCalls == nil {
		p.pendingToolCalls = newPendingToolCallRegistry()
	}
	p.pendingToolCalls.register(&pendingToolCall{
		CallID:    callID,
		ToolName:  toolName,
		Source:    source,
		SessionID: p.sessionID,
		TurnIndex: turnIndex,
		EmittedAt: time.Now().UTC(),
	}, p.evictPendingToolCall)
}

// resolvePendingToolCall matches an inbound role=tool message's tool_call_id
// to a pending entry, emits a tool.result{status=success} for it, and removes
// it from the cache. If no match is found, nothing is emitted (the client may
// be forwarding a pre-kernel call or we may have restarted).
func (p *Process) resolvePendingToolCall(callID, output string) bool {
	if p == nil || callID == "" {
		return false
	}
	if p.pendingToolCalls == nil {
		return false
	}
	entry := p.pendingToolCalls.take(callID)
	if entry == nil {
		return false
	}
	p.emitToolResult(ToolResultEvent{
		CallID:        entry.CallID,
		ToolName:      entry.ToolName,
		Status:        ToolStatusSuccess,
		OutputLength:  len(output),
		OutputSummary: truncateString(output, toolOutputSummaryChars),
		Duration:      time.Since(entry.EmittedAt),
		Source:        entry.Source,
		SessionID:     entry.SessionID,
	})
	return true
}

// evictPendingToolCall is called when the ring is at capacity and must drop
// the oldest entry. We still emit a timeout result so the audit trail shows
// the call was observed, even though the correlation window closed.
func (p *Process) evictPendingToolCall(entry *pendingToolCall) {
	if entry == nil {
		return
	}
	p.emitToolResult(ToolResultEvent{
		CallID:    entry.CallID,
		ToolName:  entry.ToolName,
		Status:    ToolStatusTimeout,
		Reason:    "evicted: pending-call cache at capacity",
		Duration:  time.Since(entry.EmittedAt),
		Source:    entry.Source,
		SessionID: entry.SessionID,
	})
}

// sweepPendingToolCalls walks the registry for entries older than the TTL,
// emits a tool.result{status=timeout} for each, and removes them. Called by
// the sweep goroutine started in RunPendingToolCallSweeper.
func (p *Process) sweepPendingToolCalls() {
	if p == nil || p.pendingToolCalls == nil {
		return
	}
	victims := p.pendingToolCalls.expired(time.Now().UTC(), pendingToolCallTTL)
	for _, v := range victims {
		p.emitToolResult(ToolResultEvent{
			CallID:    v.CallID,
			ToolName:  v.ToolName,
			Status:    ToolStatusTimeout,
			Reason:    "pending-call TTL exceeded",
			Duration:  time.Since(v.EmittedAt),
			Source:    v.Source,
			SessionID: v.SessionID,
		})
	}
}

// RunPendingToolCallSweeper is a blocking helper that runs the TTL sweep on
// a ticker until ctx is cancelled. Exported so the process.Run loop or the
// daemon lifecycle can start it alongside the other tickers. Safe to call
// without a pending registry — it will initialize one lazily when an entry
// is first registered.
func (p *Process) RunPendingToolCallSweeper(ctx context.Context) {
	if p == nil {
		return
	}
	t := time.NewTicker(pendingToolCallSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweepPendingToolCalls()
		}
	}
}

// withToolObserver wraps an MCP tool handler so every invocation emits a
// tool.call before the handler runs and a tool.result after. The wrapper is
// generic over the handler's input type and is a zero-behavior-change wrapper
// otherwise — the original handler's return values propagate unchanged.
//
// Agent S §4.3.1 specifies this as the primary emission seam for the 10
// existing cog_* MCP handlers. Additionally, §2.2 calls for activating the
// dormant NormalizeMCPRequest→RecordBlock pipeline: every MCP tool call
// becomes a CogBlock just like chat and Anthropic calls, producing a
// cogblock.ingest event alongside the tool.call/tool.result pair.
func withToolObserver[In any](
	m *MCPServer,
	name string,
	h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error),
) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		callID := uuid.NewString()
		start := time.Now().UTC()
		sessionID := ""
		var interactionID string
		if m != nil && m.process != nil {
			sessionID = m.process.SessionID()
		}
		argsJSON, _ := json.Marshal(in) // best-effort; handler still runs if nil

		// Activate the §2.2 dormant path: MCP invocation → CogBlock →
		// cogblock.ingest. Gives the tool invocation the same first-class
		// ingress story chat and Anthropic calls have had all along.
		if m != nil && m.process != nil {
			block := NormalizeMCPRequest(name, argsJSON)
			block.SessionID = sessionID
			// G2 PART B: resolve the bound subject for this transport session.
			// m.resolveTransportSession looks up the correlation recorded by
			// toolRegisterSession. When found, the subject is the correct
			// attribution (the identity that registered this harness session).
			// When not found (production path for HTTP-originated tool calls — no register call
			// precedes them — and the in-process test path where req is nil or Session.ID() is
			// empty), fall back to nucleus.Name for attribution. Fail-open is deliberate: an
			// unregistered session still gets nucleus attribution rather than an empty subject.
			transportID := ""
			if req != nil && req.Session != nil {
				transportID = req.Session.ID()
			}
			if entry, ok := m.resolveTransportSession(transportID); ok && entry.Subject != "" {
				block.TargetIdentity = entry.Subject
			} else if m.nucleus != nil {
				block.TargetIdentity = m.nucleus.Name
			}
			m.process.RecordBlock(block)
			interactionID = block.ID
		}

		if m != nil && m.process != nil {
			m.process.emitToolCall(ToolCallEvent{
				CallID:        callID,
				ToolName:      name,
				Arguments:     argsJSON,
				Source:        ToolSourceMCP,
				Ownership:     ToolOwnershipKernel,
				InteractionID: interactionID,
				SessionID:     sessionID,
			})
		}

		// G2 PART C: capability-envelope gating (behind IdentityNakedDefault).
		//
		// When IdentityNakedDefault is true AND the transport session resolves to
		// a bound identity AND a capResolver is wired, check the identity's
		// capability envelope before running the handler. If the envelope denies
		// the tool, return an error result without executing the handler.
		//
		// When IdentityNakedDefault is false (default), capResolver is nil, or
		// the transport session has no bound subject: skip enforcement entirely.
		// This is byte-for-byte pre-G2 behaviour on the handler execution path.
		//
		// Permit-by-default: CanInvoke returns true when no envelope is declared
		// for the subject, so agents that have not advertised capabilities are
		// never blocked. Restriction requires an explicit deny entry or a
		// non-empty allow-list in the advertised Payload.
		if m != nil && m.cfg != nil && m.cfg.IdentityNakedDefault && m.capResolver != nil {
			capTransportID := ""
			if req != nil && req.Session != nil {
				capTransportID = req.Session.ID()
			}
			if entry, ok := m.resolveTransportSession(capTransportID); ok && entry.Subject != "" {
				if !m.capResolver.CanInvoke(entry.Subject, name) {
					denied, _, _ := fallbackResult(
						"capability envelope denied: subject "+entry.Subject+" is not permitted to invoke "+name,
						"",
					)
					return denied, nil, nil
				}
			}
		}

		result, data, err := h(ctx, req, in)

		status := ToolStatusSuccess
		reason := ""
		if err != nil {
			status = ToolStatusError
			reason = err.Error()
		} else if result != nil && result.IsError {
			// Handlers like fallbackResult mark IsError=true without returning
			// a Go error; surface that outcome distinctly so operators can
			// filter "handler succeeded but tool reported failure" from
			// genuine success.
			status = ToolStatusError
			reason = extractErrorText(result)
		}
		outText := extractTextContent(result)
		if m != nil && m.process != nil {
			m.process.emitToolResult(ToolResultEvent{
				CallID:        callID,
				ToolName:      name,
				Status:        status,
				Reason:        reason,
				OutputLength:  len(outText),
				OutputSummary: truncateString(outText, toolOutputSummaryChars),
				Duration:      time.Since(start),
				Source:        ToolSourceMCP,
				SessionID:     sessionID,
			})
		}
		return result, data, err
	}
}

// extractTextContent returns the concatenated text of all TextContent parts
// in an MCP CallToolResult. Empty when result is nil or carries no text.
func extractTextContent(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var total int
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			total += len(tc.Text)
		}
	}
	if total == 0 {
		return ""
	}
	// Fast path: most handlers have a single text content entry.
	if len(result.Content) == 1 {
		if tc, ok := result.Content[0].(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	// Concatenate.
	buf := make([]byte, 0, total)
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			buf = append(buf, tc.Text...)
		}
	}
	return string(buf)
}

// extractErrorText returns a short error-like reason from an IsError result.
// Falls back to the text content when no other signal is available.
func extractErrorText(result *mcp.CallToolResult) string {
	text := extractTextContent(result)
	return truncateString(text, toolOutputSummaryChars)
}
