// mcp_tool_defs.go — snapshot of the MCP tool registry as ToolDefinitions
// usable on the chat path (OpenAI-compat /v1/chat/completions, Anthropic-compat
// /v1/messages).
//
// Why this file exists: when the dashboard chat path targets the kernel-agent
// route with no client-supplied tools, the server needs to advertise its own
// MCP tool surface to the inference provider. The MCP server's internal
// schema-inferred input schemas are stored on copies (the SDK does
// `tt := *t` inside AddTool), so we can't read them off the originals.
// We instead spin up an in-process MCP client at construction time, call
// ListTools, and cache the resulting [ToolDefinition] slice. From then on
// the snapshot is read-only and lock-free — registerTools has already
// returned, so nothing mutates the set after this point.
//
// See myrgic/cogos#89 (auto-inject kernel tool registry on
// `model: kernel-agent`) for the user-visible motivation.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// snapshotToolDefinitions queries the in-process MCP server for its full tool
// list and returns them as kernel-side [ToolDefinition] values, ready to set
// on a [CompletionRequest.Tools]. The query runs over an in-memory transport
// pair; no real I/O happens. Any failure is logged and an empty slice is
// returned — callers should treat the auto-injection as best-effort.
func snapshotToolDefinitions(server *mcp.Server) []ToolDefinition {
	if server == nil {
		return nil
	}

	// Bound the listing call so a misbehaving in-process transport can't hang
	// the kernel boot path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: server connect failed", "err", err)
		return nil
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "cogos-tool-snapshot", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: client connect failed", "err", err)
		return nil
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		slog.Warn("mcp: tool-def snapshot: ListTools failed", "err", err)
		return nil
	}

	defs := make([]ToolDefinition, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		defs = append(defs, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: marshalInputSchema(t.InputSchema),
		})
	}
	return defs
}

// marshalInputSchema converts an mcp.Tool.InputSchema (typed `any`, but in
// practice either *jsonschema.Schema or map[string]any depending on whether
// the call came over the wire) into a plain map[string]any. Returns the
// canonical empty-object schema when conversion fails so providers always
// receive a valid JSON Schema object — never nil, never a non-object.
func marshalInputSchema(s any) map[string]interface{} {
	if s == nil {
		return map[string]interface{}{"type": "object"}
	}
	if m, ok := s.(map[string]interface{}); ok {
		// ListTools over a real transport would return a map already;
		// over the in-memory transport it does too because the schema
		// is JSON-marshaled and unmarshaled on its way through.
		return m
	}
	// Fallback: round-trip through JSON. Handles *jsonschema.Schema and any
	// other type that JSON-marshals to a schema object.
	data, err := json.Marshal(s)
	if err != nil {
		return map[string]interface{}{"type": "object"}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]interface{}{"type": "object"}
	}
	if m == nil {
		return map[string]interface{}{"type": "object"}
	}
	return m
}

// ToolDefinitions returns a copy of the MCP tool registry snapshot as
// [ToolDefinition] values. Returns nil if the snapshot was never populated
// (e.g. NewMCPServer was constructed without going through the snapshot path,
// or the snapshot failed). Safe for concurrent reads — the underlying slice
// is set once during construction and never mutated.
func (m *MCPServer) ToolDefinitions() []ToolDefinition {
	if m == nil || len(m.toolDefs) == 0 {
		return nil
	}
	out := make([]ToolDefinition, len(m.toolDefs))
	copy(out, m.toolDefs)
	return out
}

// IsInternalTool reports whether name corresponds to a tool registered on the
// MCP server snapshot — i.e. the kernel can execute it server-side via
// [MCPServer.CallTool]. The check is the authoritative source of truth for
// "is this name in the kernel's MCP namespace"; callers MUST NOT recreate it
// by string-matching `cog_*` / `mod3_*` because the surface evolves and
// extension hooks (eval, etc.) add tools at runtime.
//
// Returns false if the snapshot is empty (e.g. a test built MCPServer that
// never wired a snapshot) — callers should treat this as "no internal
// execution available; fall through to client forwarding".
func (m *MCPServer) IsInternalTool(name string) bool {
	if m == nil || len(m.toolDefs) == 0 || name == "" {
		return false
	}
	for i := range m.toolDefs {
		if m.toolDefs[i].Name == name {
			return true
		}
	}
	return false
}

// CallTool invokes the named MCP tool in-process and returns the result as
// a single concatenated text string plus an isError flag. The call goes over
// an in-memory transport pair (no real I/O), reusing the same machinery
// [snapshotToolDefinitions] uses for ListTools.
//
// argsJSON is the raw arguments JSON the model emitted in its tool_use event
// (an OpenAI-style tool_call.arguments string). An empty argsJSON is treated
// as an empty object so tools with all-optional inputs work without ceremony.
//
// Returns:
//   - resultText: concatenated text from every TextContent block in the
//     result (the same surface a model receives over the MCP wire), suitable
//     for embedding directly in a `tool_result` message.
//   - isError: true when the MCP layer reports the tool reported an error
//     (CallToolResult.IsError); the kernel still returns the text so the
//     model can react to the error message.
//   - err: non-nil only on transport-level failure (connect, marshal, RPC).
//     Tool-reported errors come back via isError, not err.
//
// This is the function the chat handler calls when the provider emits a
// tool_use for a name [MCPServer.IsInternalTool] returns true for. It is
// the only place the chat path executes MCP tools server-side, so the
// coupling stays auditable.
func (m *MCPServer) CallTool(ctx context.Context, name string, argsJSON []byte) (string, bool, error) {
	if m == nil || m.server == nil {
		return "", false, fmt.Errorf("mcp: server not initialised")
	}
	if name == "" {
		return "", false, fmt.Errorf("mcp: tool name required")
	}

	// Decode the raw args JSON into a generic value the SDK will marshal back
	// out over the in-memory transport. Empty args → empty object so the
	// server-side schema validation receives a valid JSON object.
	var args any = map[string]any{}
	if trimmed := bytes.TrimSpace(argsJSON); len(trimmed) > 0 {
		var decoded any
		if err := json.Unmarshal(trimmed, &decoded); err != nil {
			return "", false, fmt.Errorf("mcp: decode args for %q: %w", name, err)
		}
		args = decoded
	}

	// Bound the call so a misbehaving handler can't wedge the chat turn.
	// The 30s ceiling matches Claude CLI tool-loop timeouts and is well
	// below the HTTP server's 5-min write timeout, so the chat response
	// can still surface a useful error to the model.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := m.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return "", false, fmt.Errorf("mcp: server connect: %w", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "cogos-tool-call", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return "", false, fmt.Errorf("mcp: client connect: %w", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", false, fmt.Errorf("mcp: call %q: %w", name, err)
	}

	// Concatenate every TextContent block in arrival order. The MCP SDK uses
	// a Content interface; we extract the text via type assertion on the
	// concrete *mcp.TextContent. Non-text blocks (images, resources) are
	// skipped — the chat path can't render them as a tool_result string and
	// no current cog_* tool emits them.
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError, nil
}
