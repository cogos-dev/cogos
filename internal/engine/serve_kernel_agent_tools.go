// serve_kernel_agent_tools.go — auto-injection of the kernel's MCP tool
// registry on the kernel-agent chat route.
//
// Closes myrgic/cogos#89 (auto-injection) and #94 (server-side execution
// of injected cog_* tools instead of client forwarding).
//
// Background: when a chat request lands on the kernel-agent route, the
// inference provider receives whatever ToolDefinitions the caller chose to
// pass through. The dashboard at ui/dashboard/dashboard.html builds
// `{model, messages, stream}` with no `tools` array, so the model is told
// about zero tools and falls back to narrating tool calls in prose
// ("I will use cog_read_cogdoc...") that never fire. The user perceives
// this as the kernel handoff being "duplicated/dropped/disconnected" —
// it's none of those; the tools are simply not plumbed.
//
// This helper resolves that asymmetry by treating the kernel-agent route as
// the authoritative provider of its own tools. When the caller did not
// supply any, we inject the snapshot the MCP server captured at startup
// (see mcp_tool_defs.go) and partition by ownership so the existing chat
// pipeline routes server-side tools (Bash/Read/etc.) to the kernel and
// external tools (cog_*, mod3_*, etc.) back through the MCP bridge or the
// client per classifyToolOwnership.
package engine

import "log/slog"

// injectKernelAgentTools sets creq.Tools (and the partitioned
// creq.ExternalTools) from the MCP server's cached tool registry snapshot.
// No-op if the snapshot is empty. Idempotent: callers must guard with
// `len(creq.Tools) == 0` so an explicit (even empty) caller-provided
// tools array still wins.
//
// The function is package-internal and intentionally narrow — it's the
// single place the chat path reaches into the MCP registry, which keeps
// the coupling auditable.
func injectKernelAgentTools(creq *CompletionRequest, m *MCPServer) {
	if creq == nil || m == nil {
		return
	}
	defs := m.ToolDefinitions()
	if len(defs) == 0 {
		return
	}

	// Allocate fresh slices so we don't accidentally alias the snapshot.
	// The snapshot is read-only, but downstream code (provider adapters)
	// has historically appended to creq.Tools; copying keeps us safe from
	// future mutation regressions.
	tools := make([]ToolDefinition, len(defs))
	copy(tools, defs)
	creq.Tools = tools

	// Partition by ownership the same way the OpenAI-compat path does:
	// internal-ownership tools execute server-side; client-ownership tools
	// are forwarded back as tool_calls to whoever sent the request.
	//
	// Two ownership pools count as internal here:
	//   1. classifyToolOwnership returns ToolOwnershipKernel — Bash/Read/...
	//      built-ins routed via the existing claude-CLI/MCP-bridge path.
	//   2. m.IsInternalTool(name) — every tool the MCP server itself
	//      registered (cog_*, mod3_*, plus anything an extension wired in).
	//      The chat handler executes these in-process via [MCPServer.CallTool]
	//      and appends the tool_result back into the conversation; closes #94.
	//
	// Anything left over (browser_*, agent-defined tools, etc.) is forwarded
	// to the client as tool_calls — the BrowserOS-style passthrough path.
	for _, t := range tools {
		if classifyToolOwnership(t.Name) == ToolOwnershipKernel {
			continue
		}
		if m.IsInternalTool(t.Name) {
			continue
		}
		creq.ExternalTools = append(creq.ExternalTools, t)
	}

	slog.Info("chat: kernel-agent auto-injected MCP tool registry",
		"request_id", creq.Metadata.RequestID,
		"tool_count", len(tools),
		"external_count", len(creq.ExternalTools),
	)
}

// filterToolsByCapability removes tools from creq.Tools and creq.ExternalTools
// that the capabilityGater denies for subject. Called after injectKernelAgentTools
// when IdentityNakedDefault=true and the request is bound to an identity.
//
// Permit-by-default: when the gater has no envelope for subject (CanInvoke
// returns true for all tools), no filtering occurs and the injection is
// unchanged. The function is idempotent: calling it multiple times is safe.
func filterToolsByCapability(creq *CompletionRequest, subject string, gater capabilityGater) {
	if creq == nil || gater == nil || subject == "" {
		return
	}

	// Filter creq.Tools in-place (compact pattern).
	out := creq.Tools[:0]
	for _, t := range creq.Tools {
		if gater.CanInvoke(subject, t.Name) {
			out = append(out, t)
		}
	}
	removed := len(creq.Tools) - len(out)
	creq.Tools = out

	// Filter creq.ExternalTools in-place.
	extOut := creq.ExternalTools[:0]
	for _, t := range creq.ExternalTools {
		if gater.CanInvoke(subject, t.Name) {
			extOut = append(extOut, t)
		}
	}
	creq.ExternalTools = extOut

	if removed > 0 {
		slog.Info("chat: capability envelope filtered injected tools",
			"subject", subject,
			"removed", removed,
			"remaining", len(creq.Tools),
		)
	}
}

// admitClientSuppliedTools partitions CLIENT-SUPPLIED tool definitions into
// the two pools the inference pipeline uses, refusing any name the kernel
// itself owns.
//
// Ledger L06 / audit F2. Before this, both gateway surfaces copied every
// client-supplied tool definition into creq.Tools verbatim. A name matching
// an MCP-registered kernel tool (cog_*, mod3_*, ...) therefore reached the
// provider as a real, callable tool. When the model emitted a tool_use for
// it, splitToolCallsByOwnership classified the call as kernel-internal and
// [MCPServer.CallTool] executed it in-process with the client's arguments —
// so an external client could name a kernel tool in its request's tools[]
// and have the kernel run it on its behalf.
//
// The kernel's tool registry is not part of the client's tool surface. Only
// the kernel decides when to offer it, via injectKernelAgentTools on the
// kernel-agent path. A client-supplied definition that collides with a
// registered kernel tool is dropped here; its name is returned in rejected
// so the caller can log the refusal.
//
// Names in the classifyToolOwnership set (bash/read/write/...) keep their
// pre-existing treatment: admitted to Tools, withheld from ExternalTools.
// Those are the Claude-CLI/MCP-bridge surface, are not MCP-registered, and
// splitToolCallsByOwnership does not route them into CallTool.
//
// A nil MCPServer admits everything — the legacy/test path, matching the
// pre-existing nil-guards at both call sites.
func admitClientSuppliedTools(defs []ToolDefinition, m *MCPServer) (tools, external []ToolDefinition, rejected []string) {
	if len(defs) == 0 {
		return nil, nil, nil
	}
	tools = make([]ToolDefinition, 0, len(defs))
	for _, td := range defs {
		if m != nil && m.IsInternalTool(td.Name) {
			rejected = append(rejected, td.Name)
			continue
		}
		tools = append(tools, td)
		if classifyToolOwnership(td.Name) == ToolOwnershipKernel {
			continue
		}
		external = append(external, td)
	}
	return tools, external, rejected
}
