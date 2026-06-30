// tool_loop.go — Internal tool execution loop for the v3 kernel
//
// When the inference provider returns tool_calls, the kernel:
// 1. Checks if each tool is a kernel-owned tool
// 2. Executes kernel tools internally (no HTTP round-trip)
// 3. Injects tool results into the conversation
// 4. Re-calls the provider until it produces text or hits max iterations
//
// Client-owned tools are passed back to the client in the response.
// The kernel acts as a tool call router: execute what it owns, forward what it doesn't.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxToolLoopIterations = 10

// noProgressRepeatLimit is the number of consecutive identical tool-call
// signatures that trigger an early exit with ErrToolLoopNoProgress (ADR-031).
const noProgressRepeatLimit = 3

// ErrToolLoopMaxTurns is returned by RunToolLoopWithTranscript when the loop
// reaches maxToolLoopIterations without the model emitting a final text
// response. Callers (dispatchSlot, executeCycleTaskWithPrompt) check for this
// sentinel to mark results as max-turns-exceeded rather than silent success.
// (ADR-031 autonomic/fail-fast)
var ErrToolLoopMaxTurns = &toolLoopSentinel{code: "max_turns", msg: "tool loop exceeded max iterations"}

// ErrToolLoopNoProgress is returned when the loop detects the model repeating
// the same tool call (name + args) for noProgressRepeatLimit consecutive
// iterations. Autonomic, no model call — the loop breaks immediately.
// (ADR-031 autonomic/fail-fast)
var ErrToolLoopNoProgress = &toolLoopSentinel{code: "no_progress", msg: "tool loop detected no-progress: identical tool call repeated"}

// toolLoopSentinel is the typed sentinel for structured loop-exit signals.
// Exported as *toolLoopSentinel so callers can errors.Is against the package
// vars; never constructed outside this file.
type toolLoopSentinel struct {
	code string
	msg  string
}

func (e *toolLoopSentinel) Error() string { return e.msg }
func (e *toolLoopSentinel) Code() string  { return e.code }

// KernelToolRegistry holds the kernel's tool definitions and executors.
// Populated at startup from the MCP server's tool set.
type KernelToolRegistry struct {
	cfg         *Config
	definitions []ToolDefinition
	executors   map[string]toolExecutor
	proprio     *ProprioceptiveLogger
}

// toolExecutor is a function that takes JSON arguments and returns a JSON result.
type toolExecutor func(ctx context.Context, arguments string) (string, error)

// NewKernelToolRegistry builds the tool registry from the MCP server.
func NewKernelToolRegistry(mcpSrv *MCPServer) *KernelToolRegistry {
	reg := &KernelToolRegistry{
		cfg:       mcpSrv.cfg,
		executors: make(map[string]toolExecutor),
	}
	if mcpSrv.cfg != nil && mcpSrv.cfg.WorkspaceRoot != "" {
		reg.proprio = NewProprioceptiveLogger(filepath.Join(mcpSrv.cfg.WorkspaceRoot, ".cog", "run", "proprioceptive.jsonl"))
	}

	// Register each tool with its schema and executor.
	// The schemas come from the MCP tool definitions.
	// The executors call the same Go functions as the MCP handlers.

	type toolEntry struct {
		name        string
		description string
		schema      map[string]interface{}
		executor    toolExecutor
	}

	tools := []toolEntry{
		{
			name:        "cog_resolve_uri",
			description: "Resolve a cog: URI to its filesystem path and metadata",
			schema:      objectSchema("uri", "A cog: URI to resolve"),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolResolveURI, resolveURIInput{}),
		},
		{
			// ADR-045: description kept in sync with MCP registration in mcp_server.go.
			name:        "cog_search_memory",
			description: "Full-text and semantic search over the CogDoc memory corpus. Returns ranked results with salience scores. Fallback: ./scripts/cog memory search \"query\"",
			schema: mergeSchemas(
				objectSchema("query", "Search query string"),
				optionalSchema("limit", "number", "Max results (default 10)"),
				optionalSchema("sector", "string", "Filter by memory sector"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolSearchMemory, searchMemoryInput{}),
		},
		{
			// ADR-045: description kept in sync with MCP registration in mcp_server.go.
			name:        "cog_read_cogdoc",
			description: "Read a CogDoc by URI or path. Resolves cog: URIs automatically. Returns full content with parsed frontmatter and optional section extraction via #fragment. Fallback: ./scripts/cog memory read <path>",
			schema: mergeSchemas(
				objectSchema("uri", "A cog: URI pointing to the CogDoc"),
				optionalSchema("section", "string", "Section name to extract"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolReadCogdoc, readCogdocInput{}),
		},
		{
			name:        "cog_patch_frontmatter",
			description: "Merge description, tags, or type patches into a CogDoc frontmatter block.",
			schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"uri": map[string]interface{}{
						"type":        "string",
						"description": "A cog: URI pointing to the CogDoc",
					},
					"patches": map[string]interface{}{
						"type":        "object",
						"description": "Frontmatter fields to merge into the CogDoc",
						"properties": map[string]interface{}{
							"description": map[string]interface{}{"type": "string", "description": "One-line summary for the CogDoc"},
							"tags": map[string]interface{}{
								"type":        "array",
								"description": "Classification tags",
								"items":       map[string]interface{}{"type": "string"},
							},
							"type": map[string]interface{}{"type": "string", "description": "CogDoc type"},
						},
					},
				},
				"required": []string{"uri", "patches"},
			},
			executor: makeExecutor(mcpSrv, mcpSrv.toolPatchFrontmatter, patchFrontmatterInput{}),
		},
		{
			name:        "cog_write_cogdoc",
			description: "Write or update a CogDoc at the specified memory path",
			schema: mergeSchemas(
				objectSchema("path", "Memory-relative path"),
				requiredSchema("title", "string", "Document title"),
				requiredSchema("content", "string", "Markdown content"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolWriteCogdoc, writeCogdocInput{}),
		},
		{
			name:        "cog_query_field",
			description: "Query the attentional field. Returns top-N items by salience.",
			schema: mergeSchemas(
				optionalSchema("sector", "string", "Filter by memory sector"),
				optionalSchema("limit", "number", "Max results (default 20)"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolQueryField, queryFieldInput{}),
		},
		{
			// ADR-045: description kept in sync with MCP registration in mcp_server.go.
			name:        "cog_check_coherence",
			description: "Run coherence validation against the workspace. Checks URI resolution, frontmatter validity, and reference integrity. Fallback: ./scripts/cog coherence check",
			schema:      mergeSchemas(optionalSchema("scope", "string", "structural/navigational/canonical")),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolCheckCoherence, checkCoherenceInput{}),
		},
		{
			name:        "cog_get_state",
			description: "Get the continuous process state — uptime, field size, stats",
			schema:      mergeSchemas(optionalSchema("verbose", "boolean", "Include detailed info")),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolGetState, getStateInput{}),
		},
		{
			name:        "cog_get_trust",
			description: "Return kernel identity and trust metadata",
			schema:      mergeSchemas(),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolGetTrust, getTrustInput{}),
		},
		{
			name:        "cog_get_nucleus",
			description: "Return identity context — name, role, summary",
			schema:      mergeSchemas(optionalSchema("include_config", "boolean", "Include workspace config")),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolGetNucleus, getNucleusInput{}),
		},
		{
			name:        "cog_emit_event",
			description: "Emit a custom event to the workspace ledger",
			schema: mergeSchemas(
				objectSchema("type", "Event type identifier"),
				optionalSchema("payload", "string", "JSON payload"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolEmitEvent, emitEventInput{}),
		},
		{
			name:        "cog_get_index",
			description: "Return the CogDoc index with metadata and reference graph",
			schema:      mergeSchemas(optionalSchema("sector", "string", "Filter by memory sector")),
			executor:    makeExecutor(mcpSrv, mcpSrv.toolGetIndex, getIndexInput{}),
		},
		{
			name:        "cog_assemble_context",
			description: "Build a context package for a given token budget",
			schema: mergeSchemas(
				requiredSchema("budget", "number", "Token budget"),
				optionalSchema("focus", "string", "Focus topic to bias selection"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolAssembleContext, assembleContextInput{}),
		},
		{
			name:        "cog_ingest",
			description: "Ingest external material into the CogOS knowledge substrate",
			schema: mergeSchemas(
				requiredSchema("source", "string", "Data source: discord, chatgpt, claude, gemini, url, file"),
				requiredSchema("format", "string", "Input format: url, conversation, message, document"),
				requiredSchema("data", "string", "Raw material to ingest"),
				optionalSchema("metadata", "object", "Optional context map"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolIngest, ingestInput{}),
		},
		// Audit-scope tools — read-only filesystem access within the workspace root.
		// Listed here so they are in the full kernel registry; they are only
		// visible to harness dispatches when scope="audit" (or a superset).
		{
			name:        "cog_read_file",
			description: "Read an arbitrary file within the workspace root. Returns line-numbered content with optional offset/limit. Rejects paths outside the workspace root.",
			schema: mergeSchemas(
				requiredSchema("path", "string", "Absolute path within workspace root"),
				optionalSchema("offset", "integer", "Line offset (0-based, default 0)"),
				optionalSchema("limit", "integer", "Maximum lines to return (default 500)"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolReadFile, readFileInput{}),
		},
		{
			name:        "cog_grep_files",
			description: "Regex search over files within the workspace root (ripgrep-style). Returns matching lines with path and line number. Rejects search paths outside the workspace root.",
			schema: mergeSchemas(
				requiredSchema("pattern", "string", "Regular expression pattern to search for"),
				optionalSchema("path", "string", "Directory or file to search (defaults to workspace root)"),
				optionalSchema("max_results", "integer", "Maximum matches to return (default 50)"),
			),
			executor: makeExecutor(mcpSrv, mcpSrv.toolGrepFiles, grepFilesInput{}),
		},
	}

	for _, t := range tools {
		reg.definitions = append(reg.definitions, ToolDefinition{
			Name:        t.name,
			Description: t.description,
			InputSchema: t.schema,
		})
		reg.executors[t.name] = t.executor
	}

	return reg
}

// IsKernelTool returns true if the named tool is owned by the kernel.
func (r *KernelToolRegistry) IsKernelTool(name string) bool {
	_, ok := r.executors[name]
	return ok
}

// Execute runs a kernel tool and returns the result as a string.
func (r *KernelToolRegistry) Execute(ctx context.Context, name, arguments string) (string, error) {
	executor, ok := r.executors[name]
	if !ok {
		return "", fmt.Errorf("unknown kernel tool: %s", name)
	}
	return executor(ctx, arguments)
}

// Definitions returns the tool definitions for inclusion in CompletionRequest.
func (r *KernelToolRegistry) Definitions() []ToolDefinition {
	return r.definitions
}

// Scoped returns a registry limited to the named tool subset. Empty names keep
// the original registry. Unknown names return an error instead of silently
// widening the scope.
func (r *KernelToolRegistry) Scoped(names []string) (*KernelToolRegistry, error) {
	if r == nil {
		return nil, fmt.Errorf("nil kernel tool registry")
	}
	if len(names) == 0 {
		return r, nil
	}

	out := &KernelToolRegistry{
		cfg:       r.cfg,
		proprio:   r.proprio,
		executors: make(map[string]toolExecutor, len(names)),
	}
	for _, name := range names {
		exec, ok := r.executors[name]
		if !ok {
			return nil, fmt.Errorf("kernel tool %q not registered", name)
		}
		def, ok := lookupToolDefinition(r.definitions, name)
		if !ok {
			return nil, fmt.Errorf("kernel tool %q missing definition", name)
		}
		out.executors[name] = exec
		out.definitions = append(out.definitions, def)
	}
	return out, nil
}

func toolCallValidationEnabled(provider Provider, cfg *Config) bool {
	caps := provider.Capabilities()
	if caps.HasCapability(CapToolUse) {
		return false
	}
	if !caps.HasCapability(CapToolCallValidation) {
		return false
	}
	if cfg == nil {
		return true
	}
	return cfg.ToolCallValidationEnabled
}

func (r *KernelToolRegistry) logRejectedToolCall(providerName string, tc ToolCall, validation ToolCallValidationResult) {
	if r == nil {
		return
	}
	if r.proprio == nil && r.cfg != nil && r.cfg.WorkspaceRoot != "" {
		r.proprio = NewProprioceptiveLogger(filepath.Join(r.cfg.WorkspaceRoot, ".cog", "run", "proprioceptive.jsonl"))
	}
	if r.proprio == nil {
		return
	}
	r.proprio.Log(ProprioceptiveEntry{
		Event:       "tool_call_rejected",
		Provider:    providerName,
		ToolName:    tc.Name,
		ToolCallID:  tc.ID,
		ToolArgs:    truncateString(tc.Arguments, 500),
		Reason:      validation.Reason,
		Query:       fmt.Sprintf("tool_call:%s", tc.Name),
		ResponseLen: len(tc.Arguments),
	})
}

// ToolCallValidationResult describes whether a model-emitted tool call is safe to run.
type ToolCallValidationResult struct {
	Valid    bool
	Reason   string
	ToolName string
}

// ValidateToolCall verifies that the model requested a known tool with arguments
// that match the registered input schema.
func ValidateToolCall(tc ToolCall, toolDefs []ToolDefinition) ToolCallValidationResult {
	result := ToolCallValidationResult{
		ToolName: tc.Name,
	}

	def, ok := lookupToolDefinition(toolDefs, tc.Name)
	if !ok {
		result.Reason = fmt.Sprintf("unknown tool %q", tc.Name)
		return result
	}

	args := map[string]interface{}{}
	raw := strings.TrimSpace(tc.Arguments)
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			result.Reason = fmt.Sprintf("invalid JSON arguments: %v", err)
			return result
		}
	}

	if hasEmbeddedResult(args) {
		result.Reason = "embedded result field is not allowed in tool arguments"
		return result
	}

	if reason := validateObjectAgainstSchema(args, def.InputSchema, ""); reason != "" {
		result.Reason = reason
		return result
	}

	result.Valid = true
	return result
}

// RunToolLoop executes the kernel tool loop. See RunToolLoopWithTranscript
// for the full detail — this wrapper drops the per-turn tool-call transcript
// for callers that don't need it. Preserved for backwards compatibility
// with existing tests / call sites.
func RunToolLoop(
	ctx context.Context,
	provider Provider,
	req *CompletionRequest,
	initialResp *CompletionResponse,
	registry *KernelToolRegistry,
) (*CompletionResponse, []ToolCall, error) {
	resp, clientCalls, _, err := RunToolLoopWithTranscript(ctx, provider, req, initialResp, registry)
	return resp, clientCalls, err
}

// RunToolLoopWithTranscript executes the kernel tool loop and returns a
// transcript of kernel-owned tool calls executed (and any rejected ones).
// The transcript is threaded back to chat handlers so it can be stored
// alongside the prompt/response in the turn.completed record (Agent R
// §5.4).
//
// Given a CompletionResponse with tool_calls, it:
// 1. Separates kernel tools from client tools
// 2. Executes kernel tools (capturing a ToolCallRecord for each)
// 3. Appends results to messages
// 4. Re-calls the provider
// 5. Repeats until no more kernel tool calls or max iterations
//
// Returns:
//   - final response
//   - any client tool calls that need forwarding
//   - per-turn tool-call transcript (kernel calls + rejections)
//   - error
func RunToolLoopWithTranscript(
	ctx context.Context,
	provider Provider,
	req *CompletionRequest,
	initialResp *CompletionResponse,
	registry *KernelToolRegistry,
) (*CompletionResponse, []ToolCall, []ToolCallRecord, error) {

	resp := initialResp
	var clientToolCalls []ToolCall
	var transcript []ToolCallRecord

	// No-progress tracking (ADR-031): record the last tool-call signature
	// (tool-name + raw arguments) and count consecutive repeats. A run of
	// noProgressRepeatLimit identical calls is treated as a stuck loop.
	var lastToolSig string
	var noProgressCount int

	for i := 0; i < maxToolLoopIterations; i++ {
		resp.ToolCalls = normalizeToolCallIDs(provider.Name(), i+1, resp.ToolCalls)
		if len(resp.ToolCalls) == 0 {
			return resp, clientToolCalls, transcript, nil
		}

		// Add the assistant message with tool calls to the conversation.
		assistantMsg := ProviderMessage{
			Role:      "assistant",
			ToolCalls: resp.ToolCalls,
		}
		if resp.Content != "" {
			assistantMsg.Content = resp.Content
		}
		req.Messages = append(req.Messages, assistantMsg)

		var cfg *Config
		if registry != nil {
			cfg = registry.cfg
		}
		if toolCallValidationEnabled(provider, cfg) {
			toolDefs := make([]ToolDefinition, 0, len(req.Tools))
			toolDefs = append(toolDefs, req.Tools...)
			if registry != nil {
				toolDefs = append(toolDefs, registry.Definitions()...)
			}

			var rejected []ToolCallValidationResult
			for _, tc := range resp.ToolCalls {
				validation := ValidateToolCall(tc, toolDefs)
				if validation.Valid {
					continue
				}

				rejected = append(rejected, validation)
				transcript = append(transcript, ToolCallRecord{
					ID:           tc.ID,
					Name:         tc.Name,
					Arguments:    tc.Arguments,
					Rejected:     true,
					RejectReason: validation.Reason,
				})
				recordToolCallRejection(provider.Name())
				if registry != nil {
					registry.logRejectedToolCall(provider.Name(), tc, validation)
				}
				slog.Warn("tool_loop: rejected tool call",
					"provider", provider.Name(),
					"tool", tc.Name,
					"reason", validation.Reason,
					"iteration", i+1,
				)
			}

			if len(rejected) > 0 {
				req.Messages = append(req.Messages, ProviderMessage{
					Role:    "system",
					Content: fmt.Sprintf("Tool call rejected: %s. Please try again with valid parameters.", rejected[0].Reason),
				})

				var err error
				resp, err = provider.Complete(ctx, req)
				if err != nil {
					return nil, clientToolCalls, transcript, fmt.Errorf("tool_loop re-call after rejection: %w", err)
				}

				slog.Info("tool_loop: provider re-called after tool rejection",
					"iteration", i+1,
					"rejected", len(rejected),
					"tool_calls", len(resp.ToolCalls),
					"has_content", resp.Content != "",
				)
				continue
			}
		}

		// Separate kernel vs client tool calls.
		var kernelCalls []ToolCall
		for _, tc := range resp.ToolCalls {
			if registry != nil && registry.IsKernelTool(tc.Name) {
				kernelCalls = append(kernelCalls, tc)
			} else {
				clientToolCalls = append(clientToolCalls, tc)
			}
		}

		// If no kernel calls, return — client needs to handle the rest.
		if len(kernelCalls) == 0 {
			return resp, clientToolCalls, transcript, nil
		}

		// No-progress guard (ADR-031): build a signature from the first kernel
		// call's name+args. If the model keeps issuing the same call
		// noProgressRepeatLimit times in a row, break autonomically without a
		// model call rather than letting the loop spin to maxToolLoopIterations.
		if len(kernelCalls) > 0 {
			sig := kernelCalls[0].Name + "\x00" + kernelCalls[0].Arguments
			if sig == lastToolSig {
				noProgressCount++
				if noProgressCount >= noProgressRepeatLimit {
					slog.Warn("tool_loop: no-progress break",
						"tool", kernelCalls[0].Name,
						"repeat", noProgressCount,
						"iteration", i+1,
					)
					return resp, clientToolCalls, transcript, ErrToolLoopNoProgress
				}
			} else {
				lastToolSig = sig
				noProgressCount = 1
			}
		}

		// Execute kernel tools and add results.
		for _, tc := range kernelCalls {
			slog.Info("tool_loop: executing kernel tool",
				"tool", tc.Name,
				"iteration", i+1,
			)

			start := time.Now()
			result, err := registry.Execute(ctx, tc.Name, tc.Arguments)
			dur := time.Since(start).Milliseconds()
			rec := ToolCallRecord{
				ID:         tc.ID,
				Name:       tc.Name,
				Arguments:  tc.Arguments,
				Result:     result,
				DurationMs: dur,
			}
			if err != nil {
				rec.Rejected = false
				rec.RejectReason = ""
				rec.Result = fmt.Sprintf(`{"error": %q}`, err.Error())
				result = rec.Result
				slog.Warn("tool_loop: tool execution failed",
					"tool", tc.Name,
					"err", err,
				)
			}
			transcript = append(transcript, rec)

			req.Messages = append(req.Messages, ProviderMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		// Respond hard-break (ADR-052 TERMINATE-as-break): if the model invoked
		// the "respond" tool and it succeeded (ok:true in the result JSON), the
		// turn is complete — break immediately rather than re-calling the model.
		// Gate B in dashboard_inlet.go may return ok:false for a duplicate call;
		// in that case we still break (the turn was already answered) to prevent
		// the loop continuing after a denied respond invocation.
		for _, tc := range kernelCalls {
			if tc.Name == engineRespondToolName {
				slog.Info("tool_loop: respond tool invoked — hard break (ADR-052)",
					"tool", tc.Name,
					"iteration", i+1,
				)
				return resp, clientToolCalls, transcript, nil
			}
		}

		// If there are also client tool calls, we need to stop and let the client handle them.
		if len(clientToolCalls) > 0 {
			// Return a synthetic response that includes both the text and pending client calls.
			return resp, clientToolCalls, transcript, nil
		}

		// Re-call the provider with the updated messages.
		var err error
		resp, err = provider.Complete(ctx, req)
		if err != nil {
			return nil, clientToolCalls, transcript, fmt.Errorf("tool_loop re-call: %w", err)
		}

		slog.Info("tool_loop: provider re-called",
			"iteration", i+1,
			"tool_calls", len(resp.ToolCalls),
			"has_content", resp.Content != "",
		)
	}

	// Max-turns structured exit (ADR-031): return ErrToolLoopMaxTurns so
	// dispatchSlot / executeCycleTaskWithPrompt can mark the result as
	// max-turns-exceeded rather than silently treating it as success.
	slog.Warn("tool_loop: max iterations reached", "max", maxToolLoopIterations)
	return resp, clientToolCalls, transcript, ErrToolLoopMaxTurns
}

// normalizeToolCallIDs assigns stable IDs to any tool calls that arrived
// without one. Any provider may omit IDs; this ensures downstream code never
// has to handle blank ToolCall.ID values.
func normalizeToolCallIDs(providerName string, iteration int, calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	prefix := sanitizeToolCallIDPrefix(providerName)
	for i := range calls {
		if strings.TrimSpace(calls[i].ID) != "" {
			continue
		}
		calls[i].ID = fmt.Sprintf("%s-call-%d-%d", prefix, iteration, i)
	}
	return calls
}

// sanitizeToolCallIDPrefix converts an arbitrary provider name into a
// lowercase alphanumeric-plus-dash string suitable for use in a generated ID.
func sanitizeToolCallIDPrefix(providerName string) string {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return "provider"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range providerName {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if prevDash {
				continue
			}
			b.WriteByte('-')
			prevDash = true
		}
	}
	sanitized := strings.Trim(b.String(), "-")
	if sanitized == "" {
		return "provider"
	}
	return sanitized
}

// ── Schema helpers ───────────────────────────────────────────────────────────

func objectSchema(requiredField, description string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			requiredField: map[string]interface{}{
				"type":        "string",
				"description": description,
			},
		},
		"required": []string{requiredField},
	}
}

func requiredSchema(name, typ, description string) map[string]interface{} {
	return map[string]interface{}{
		"_name":       name,
		"_required":   true,
		"type":        typ,
		"description": description,
	}
}

func optionalSchema(name, typ, description string) map[string]interface{} {
	return map[string]interface{}{
		"_name":       name,
		"_required":   false,
		"type":        typ,
		"description": description,
	}
}

func mergeSchemas(parts ...map[string]interface{}) map[string]interface{} {
	props := make(map[string]interface{})
	var required []string

	for _, p := range parts {
		name, hasName := p["_name"].(string)
		if hasName {
			// This is a field definition
			prop := map[string]interface{}{
				"type":        p["type"],
				"description": p["description"],
			}
			props[name] = prop
			if req, ok := p["_required"].(bool); ok && req {
				required = append(required, name)
			}
		} else {
			// This is a full schema — merge its properties
			if existingProps, ok := p["properties"].(map[string]interface{}); ok {
				for k, v := range existingProps {
					props[k] = v
				}
			}
			if existingReq, ok := p["required"].([]string); ok {
				required = append(required, existingReq...)
			}
		}
	}

	schema := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// makeExecutor creates a toolExecutor from an MCP tool handler.
// It deserializes the arguments JSON into the input type, calls the handler,
// and serializes the result.
func makeExecutor[In any](mcpSrv *MCPServer, handler func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error), zero In) toolExecutor {
	return func(ctx context.Context, arguments string) (string, error) {
		var input In
		if arguments != "" && arguments != "{}" {
			if err := json.Unmarshal([]byte(arguments), &input); err != nil {
				return "", fmt.Errorf("parse tool arguments: %w", err)
			}
		}

		result, _, err := handler(ctx, &mcp.CallToolRequest{}, input)
		if err != nil {
			return "", err
		}

		// Extract text from the CallToolResult content.
		if result != nil && len(result.Content) > 0 {
			for _, c := range result.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					return tc.Text, nil
				}
			}
		}
		return "{}", nil
	}
}

func lookupToolDefinition(toolDefs []ToolDefinition, name string) (ToolDefinition, bool) {
	for i := len(toolDefs) - 1; i >= 0; i-- {
		if toolDefs[i].Name == name {
			return toolDefs[i], true
		}
	}
	return ToolDefinition{}, false
}

func hasEmbeddedResult(args map[string]interface{}) bool {
	for name := range args {
		if strings.EqualFold(name, "result") {
			return true
		}
	}
	return false
}

func validateObjectAgainstSchema(args map[string]interface{}, schema map[string]interface{}, path string) string {
	if len(schema) == 0 {
		return ""
	}

	for _, required := range schemaStringSlice(schema["required"]) {
		if _, ok := args[required]; !ok {
			if path == "" {
				return fmt.Sprintf("missing required parameter %q", required)
			}
			return fmt.Sprintf("missing required parameter %q in %s", required, path)
		}
	}

	props, _ := schema["properties"].(map[string]interface{})
	for name, value := range args {
		propSchema, ok := props[name].(map[string]interface{})
		if !ok {
			continue
		}
		fieldPath := name
		if path != "" {
			fieldPath = path + "." + name
		}
		if reason := validateValueAgainstSchema(value, propSchema, fieldPath); reason != "" {
			return reason
		}
	}

	return ""
}

func validateValueAgainstSchema(value interface{}, schema map[string]interface{}, path string) string {
	if len(schema) == 0 {
		return ""
	}

	typeName := schemaType(schema)
	if typeName == "" {
		if props, ok := schema["properties"].(map[string]interface{}); ok && len(props) > 0 {
			typeName = "object"
		}
	}

	switch typeName {
	case "":
		return ""
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Sprintf("parameter %q must be string", path)
		}
	case "integer", "int":
		if !isJSONInteger(value) {
			return fmt.Sprintf("parameter %q must be integer", path)
		}
	case "number":
		if !isJSONNumber(value) {
			return fmt.Sprintf("parameter %q must be number", path)
		}
	case "boolean", "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Sprintf("parameter %q must be boolean", path)
		}
	case "array":
		items, ok := value.([]interface{})
		if !ok {
			return fmt.Sprintf("parameter %q must be array", path)
		}
		itemSchema, _ := schema["items"].(map[string]interface{})
		for i, item := range items {
			if reason := validateValueAgainstSchema(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); reason != "" {
				return reason
			}
		}
	case "object":
		obj, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Sprintf("parameter %q must be object", path)
		}
		return validateObjectAgainstSchema(obj, schema, path)
	}

	return ""
}

func schemaType(schema map[string]interface{}) string {
	if raw, ok := schema["type"].(string); ok {
		return raw
	}
	return ""
}

func schemaStringSlice(raw interface{}) []string {
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func isJSONNumber(value interface{}) bool {
	switch n := value.(type) {
	case float64:
		return true
	case json.Number:
		_, err := n.Float64()
		return err == nil
	default:
		return false
	}
}

func isJSONInteger(value interface{}) bool {
	switch n := value.(type) {
	case float64:
		return n == float64(int64(n))
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return true
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		return err == nil && f == float64(int64(f))
	default:
		return false
	}
}
