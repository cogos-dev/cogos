// serve_internal_tools.go — chat-handler glue for executing MCP-internal
// tools (cog_*, mod3_*, ...) in-process.
//
// Closes myrgic/cogos#94. Background: #89 made the kernel auto-inject
// its MCP tool registry into kernel-agent chat requests so the inference
// provider sees real tool definitions. But the chat handler still routed
// every tool_use event to the client via OpenAI tool_calls, so the
// dashboard (which has no executor for cog_*) saw the call evaporate and
// the turn ended silently. This file owns the missing layer: split a
// provider's tool_calls into "the kernel will run this" vs. "forward to
// the client", plus the small messaging helpers the chat-handler loop
// uses to keep the conversation coherent across hops.
package engine

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// splitToolCallsByOwnership partitions a provider's emitted tool calls into
// the kernel-internal MCP set (executable here via [MCPServer.CallTool])
// and "everything else" (forwarded to the client). The MCP server's
// IsInternalTool snapshot is the authoritative source of truth, with a
// fallback through classifyToolOwnership for the Bash/Read/Write surface
// — though those typically arrive on the kernel via the existing
// claude-CLI/MCP-bridge path, not here.
//
// A nil [MCPServer] returns (nil, calls) so the chat path falls back to
// the pre-#94 client-forwarding behaviour without breaking.
func splitToolCallsByOwnership(calls []ToolCall, m *MCPServer) (internal, external []ToolCall) {
	if len(calls) == 0 {
		return nil, nil
	}
	for _, tc := range calls {
		if m != nil && m.IsInternalTool(tc.Name) {
			internal = append(internal, tc)
			continue
		}
		external = append(external, tc)
	}
	return internal, external
}

// appendToolHopMessages records the assistant turn that triggered the
// internal tool calls so the next provider call sees the full transcript.
// Without this, the model loses the link between its tool_use and the
// upcoming tool_result and may double-call.
//
// For the OpenAI-compat surface, an assistant message that emitted tool
// calls is represented as Role="assistant" with a populated ToolCalls
// slice and (typically) empty Content. We pass through resp.Content
// unchanged; some providers emit reasoning text alongside tool_use, and
// dropping it would change the model's interpretation of its own prior
// turn.
func appendToolHopMessages(msgs []ProviderMessage, resp *CompletionResponse, internal []ToolCall) []ProviderMessage {
	if resp == nil || len(internal) == 0 {
		return msgs
	}
	return append(msgs, ProviderMessage{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: internal,
	})
}

// executeInternalToolCall emits the observability events that pair with the
// in-process MCP tool invocation. The tool.call event is emitted before the
// call so the ledger row exists even if the kernel crashes mid-call;
// resolvePendingToolCall (called by the loop) closes the pair into a
// tool.result entry. Source is ToolSourceMCP because the call goes through
// the MCP server's in-process transport — distinguishing it from
// ToolSourceOpenAI client-forwarded entries on the same chat turn.
func (s *Server) executeInternalToolCall(_ context.Context, providerName string, tc ToolCall) {
	if s == nil || s.process == nil {
		return
	}
	s.process.emitToolCall(ToolCallEvent{
		CallID:    nonEmptyID(tc.ID),
		ToolName:  tc.Name,
		Arguments: json.RawMessage(tc.Arguments),
		Source:    ToolSourceMCP,
		Ownership: ToolOwnershipKernel,
		Provider:  providerName,
		SessionID: s.process.SessionID(),
	})
	s.process.registerPendingToolCall(nonEmptyID(tc.ID), tc.Name, ToolSourceMCP, 0)
}

// nonEmptyID returns id if non-empty, else a fresh UUID. Some providers omit
// IDs on tool_use events; the ledger pair must still resolve so we mint one.
func nonEmptyID(id string) string {
	if id != "" {
		return id
	}
	return uuid.New().String()
}

// truncateForTurn caps a tool-result string to a reasonable size for storage
// in the turn record's Result field. The kernel keeps the full result in the
// in-flight conversation; the turn record is for retrospective inspection.
func truncateForTurn(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}
