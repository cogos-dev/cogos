// agent_harness.go — Native Go agent harness for the homeostatic kernel loop.
//
// Runs as a goroutine inside the kernel process. Calls a local model (Gemma E4B
// via Ollama) through the OpenAI chat completions wire protocol. The loop is:
//
//	Observation → Assess (JSON mode) → Execute (tool loop) → Callback
//
// No framework dependencies. Uses net/http directly against Ollama's
// OpenAI-compatible /v1/chat/completions endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/trace"
)

// cycleIDFromContext extracts a cycle-trace correlation ID from ctx, or
// returns "" if none. See trace_emit.go for the key type.
func cycleIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(cycleIDKey{}).(string)
	return v
}

// WithCycleID returns ctx carrying the given cycle-trace ID. Callers (e.g.
// ServeAgent.runCycle) should wrap ctx with this before invoking Assess /
// Execute so tool-dispatch emission can correlate events across the
// iteration.
func WithCycleID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, cycleIDKey{}, id)
}

// sessionIDKey is the context key that carries the dashboard session_id of
// the user turn currently being processed. The respond tool reads this so
// its reply can be tagged with the originating session, letting Mod³ filter
// broadcasts and avoid cross-talk between simultaneously-connected clients.
type sessionIDKey struct{}

// sessionIDFromContext extracts the dashboard session_id from ctx, or "" if
// none. Empty string is the explicit "no session" signal — publishers treat
// it as a broadcast to anyone listening (the legacy behavior).
func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionIDKey{}).(string)
	return v
}

// WithSessionID returns ctx carrying the given dashboard session_id. Set by
// ServeAgent.runCycle when a pending user message is being observed so the
// downstream respond tool can stamp its reply with the correct session.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// sessionIDsKey carries the de-duplicated list of dashboard session_ids for a
// cycle that drained multiple user messages. The respond tool fans out its
// reply across this list so every originating session/tab sees the response —
// without it, a cycle consuming messages from N clients would only reply on
// whichever session_id happened to be first in the pending queue.
//
// When absent (or empty), publishers fall back to sessionIDFromContext — the
// single-session path preserved for the common case of one message per cycle.
type sessionIDsKey struct{}

// sessionIDsFromContext returns the fan-out list of session_ids for the
// current cycle's reply, or nil if none was set. An empty slice is treated as
// no-list (callers should use the single-id path).
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

// WithSessionIDs returns ctx carrying the given fan-out list of session_ids.
// Set by ServeAgent.runCycle after draining the pending queue so downstream
// publishers (respond tool, auto-fallback) can emit one reply per unique
// session. Nil/empty ids are ignored — callers keep WithSessionID for the
// single-session case.
func WithSessionIDs(ctx context.Context, ids []string) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	// Copy so later mutations on the caller's slice can't alter the stored
	// list — ctx values are treated as immutable by convention.
	cp := make([]string, len(ids))
	copy(cp, ids)
	return context.WithValue(ctx, sessionIDsKey{}, cp)
}

// --- Wire protocol types (Ollama native /api/chat) ---
//
// Uses Ollama's native API instead of the OpenAI-compatible shim because:
// - Native API supports "think": false to control thinking mode at the source
// - OpenAI shim has no way to disable thinking (content bleeds into reasoning field)
// - Native API is the recommended path per Ollama docs
// See: https://github.com/ollama/ollama/issues/15288

// agentChatRequest is the Ollama native /api/chat request body.
type agentChatRequest struct {
	Model    string                 `json:"model"`
	Messages []agentChatMessage     `json:"messages"`
	Tools    []ToolDefinition       `json:"tools,omitempty"`
	Stream   bool                   `json:"stream"`
	Think    bool                   `json:"think"`             // explicit thinking control
	Format   string                 `json:"format,omitempty"`  // "json" for structured output
	Options  map[string]interface{} `json:"options,omitempty"` // Ollama model options (num_ctx, temperature, etc.)
	// KeepAlive controls how long Ollama keeps the model resident after the
	// request completes. -1 keeps the model loaded indefinitely, 0 unloads
	// immediately. Set to -1 by callers so dispatch cycles spaced past
	// Ollama's 5-minute default keep-alive don't pay cold-start. Dropped
	// on the OpenAI-compat path (chatCompletionOpenAI translates to its
	// own struct that does not forward this field).
	KeepAlive any `json:"keep_alive,omitempty"`
}

// agentChatMessage is a single message in the conversation.
type agentChatMessage struct {
	Role       string          `json:"role"`                   // system, user, assistant, tool
	Content    string          `json:"content,omitempty"`      // text content
	ToolCalls  []agentToolCall `json:"tool_calls,omitempty"`   // assistant tool invocations
	ToolCallID string          `json:"tool_call_id,omitempty"` // for role=tool responses (OpenAI compat in Ollama)
}

// agentToolCall is a tool invocation returned by the model.
type agentToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // JSON object (Ollama native returns object, not string)
	} `json:"function"`
}

// agentChatResponse is the Ollama native /api/chat response.
type agentChatResponse struct {
	Model   string `json:"model"`
	Message struct {
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		ToolCalls []agentToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason,omitempty"`

	// Ollama performance metrics (nanoseconds)
	TotalDuration      int64 `json:"total_duration,omitempty"`
	LoadDuration       int64 `json:"load_duration,omitempty"`
	PromptEvalCount    int   `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount          int   `json:"eval_count,omitempty"`
	EvalDuration       int64 `json:"eval_duration,omitempty"`
}

// --- Tool definition types ---

// ToolDefinition is the OpenAI function-calling tool format.
type ToolDefinition struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolFunc is the signature for kernel-native tool implementations.
type ToolFunc func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// --- Assessment ---

// Assessment is the structured output from the assess phase.
type Assessment struct {
	Action  string  `json:"action"`  // "sleep", "consolidate", "repair", "observe", "escalate"
	Reason  string  `json:"reason"`  // why this action
	Urgency float64 `json:"urgency"` // 0-1
	Target  string  `json:"target"`  // what to act on (URI, path, etc)
}

// --- Harness ---

// AgentHarness runs a continuous observation-assessment-action loop
// using a local model via the OpenAI chat completions protocol.
type AgentHarness struct {
	ollamaURL  string
	model      string
	tools      []ToolDefinition
	toolFuncs  map[string]ToolFunc
	httpClient *http.Client
	maxTurns   int
}

// AgentHarnessConfig holds configuration for creating an AgentHarness.
type AgentHarnessConfig struct {
	OllamaURL string // e.g. "http://localhost:11434" (native API, no /v1 suffix)
	Model     string // e.g. "gemma4:e4b"
	MaxTurns  int    // safety limit per execution cycle (default: 10)
}

// NewAgentHarness creates a new agent harness with the given configuration.
func NewAgentHarness(cfg AgentHarnessConfig) *AgentHarness {
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 10
	}
	return &AgentHarness{
		ollamaURL: cfg.OllamaURL,
		model:     cfg.Model,
		tools:     nil,
		toolFuncs: make(map[string]ToolFunc),
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
		maxTurns: maxTurns,
	}
}

// RegisterTool adds a tool that the model can invoke.
func (h *AgentHarness) RegisterTool(def ToolDefinition, fn ToolFunc) {
	h.tools = append(h.tools, def)
	h.toolFuncs[def.Function.Name] = fn
}

// Assess sends observations to the model and returns a structured assessment.
// Uses JSON mode to get a typed Assessment back.
func (h *AgentHarness) Assess(ctx context.Context, systemPrompt, observation string) (*Assessment, error) {
	messages := []agentChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: observation},
	}

	req := agentChatRequest{
		Model:     h.model,
		Messages:  messages,
		Stream:    false,
		Think:     false, // disable thinking — we want clean JSON output
		Format:    "json",
		Options:   map[string]interface{}{"num_ctx": 8192}, // 8K context is plenty for assessment
		KeepAlive: -1,                                      // pin model in VRAM between cycles
	}

	resp, err := h.chatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
	}

	// Log Ollama performance metrics
	if resp.PromptEvalCount > 0 {
		promptTokS := float64(resp.PromptEvalCount) / (float64(resp.PromptEvalDuration) / 1e9)
		evalTokS := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
		totalS := float64(resp.TotalDuration) / 1e9
		log.Printf("[agent] assess metrics: %d prompt tok (%.0f tok/s) + %d eval tok (%.0f tok/s) = %.1fs total",
			resp.PromptEvalCount, promptTokS, resp.EvalCount, evalTokS, totalS)
	}

	content := resp.Message.Content

	var assessment Assessment
	if err := json.Unmarshal([]byte(content), &assessment); err != nil {
		return nil, fmt.Errorf("assess: parse assessment (raw=%q): %w", content, err)
	}
	return &assessment, nil
}

// Execute enters the tool loop: sends the execution prompt with tool definitions,
// dispatches tool calls to registered Go functions, feeds results back, and
// repeats until the model returns content without tool_calls or maxTurns is hit.
// Returns the model's final text response.
func (h *AgentHarness) Execute(ctx context.Context, systemPrompt, task string) (string, error) {
	messages := []agentChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task},
	}

	for turn := 0; turn < h.maxTurns; turn++ {
		req := agentChatRequest{
			Model:     h.model,
			Messages:  messages,
			Tools:     h.tools,
			Stream:    false,
			Think:     false,                                   // disable thinking for tool loop
			Options:   map[string]interface{}{"num_ctx": 8192}, // 8K context for tool loop
			KeepAlive: -1,                                      // pin model in VRAM between turns
		}

		resp, err := h.chatCompletion(ctx, req)
		if err != nil {
			return "", fmt.Errorf("execute turn %d: %w", turn, err)
		}

		// Log per-turn metrics
		if resp.PromptEvalCount > 0 {
			totalS := float64(resp.TotalDuration) / 1e9
			log.Printf("[agent] execute turn %d: %d prompt tok + %d eval tok = %.1fs",
				turn, resp.PromptEvalCount, resp.EvalCount, totalS)
		}

		msg := resp.Message

		// No tool calls — model is done. Return the content.
		if len(msg.ToolCalls) == 0 {
			return msg.Content, nil
		}

		// Append the assistant message with tool calls.
		messages = append(messages, agentChatMessage{
			Role:      "assistant",
			ToolCalls: msg.ToolCalls,
		})

		// Dispatch each tool call and collect results. Track whether the
		// sanctioned `wait` tool was invoked — if so, we terminate the loop
		// after this turn's dispatch rather than asking the model for another
		// round. This gives the model a clean "nothing to do" exit.
		var waitInvoked bool
		var waitReason string
		for _, tc := range msg.ToolCalls {
			// Dispatch with timing so we can emit a cycle.tool_dispatch trace
			// event. Emission is best-effort and never blocks the tool loop.
			toolStart := time.Now()
			result, err := h.dispatchTool(ctx, tc)
			toolDuration := time.Since(toolStart)
			if cycleID := cycleIDFromContext(ctx); cycleID != "" {
				ev, bErr := trace.NewToolDispatch(
					engine.TraceIdentity(),
					cycleID,
					tc.Function.Name,
					tc.Function.Arguments,
					toolDuration,
					err,
				)
				if bErr == nil {
					emitCycleEvent(ev)
				}
			}
			if err != nil {
				// Tool errors go back to the model as content, not Go errors.
				result = []byte(fmt.Sprintf(`{"error": %q}`, err.Error()))
			}
			if tc.Function.Name == waitToolName {
				waitInvoked = true
				if r := extractWaitReason(result); r != "" {
					waitReason = r
				}
			}
			messages = append(messages, agentChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    string(result),
			})
		}

		if waitInvoked {
			return fmt.Sprintf("waited: %s", waitReason), nil
		}
	}

	return "", fmt.Errorf("execute: hit max turns (%d) without completion", h.maxTurns)
}

// RunCycle performs one full observation-assessment-execution cycle.
// If the assessment says "sleep", no execution happens.
// Returns the assessment and any execution result.
func (h *AgentHarness) RunCycle(ctx context.Context, systemPrompt, observation string) (*Assessment, string, error) {
	assessment, err := h.Assess(ctx, systemPrompt, observation)
	if err != nil {
		return nil, "", err
	}

	// No action needed — return the assessment only.
	if assessment.Action == "sleep" {
		return assessment, "", nil
	}

	// Build the execution task from the assessment.
	task := fmt.Sprintf("Action: %s\nTarget: %s\nReason: %s",
		assessment.Action, assessment.Target, assessment.Reason)

	result, err := h.Execute(ctx, systemPrompt, task)
	if err != nil {
		return assessment, "", err
	}

	return assessment, result, nil
}

// --- Internal helpers ---

// chatCompletion sends a request to the OpenAI-compatible /v1/chat/completions endpoint.
func (h *AgentHarness) chatCompletion(ctx context.Context, req agentChatRequest) (*agentChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Use Ollama native /api/chat (not OpenAI-compat /v1/chat/completions)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.ollamaURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", httpResp.StatusCode, string(respBody))
	}

	var resp agentChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

// dispatchTool finds and calls the registered tool function.
func (h *AgentHarness) dispatchTool(ctx context.Context, tc agentToolCall) (json.RawMessage, error) {
	fn, ok := h.toolFuncs[tc.Function.Name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", tc.Function.Name)
	}
	return fn(ctx, json.RawMessage(tc.Function.Arguments))
}

// --- Phase 2: task-parameterized dispatch (cog_dispatch_to_harness) ---
//
// ExecuteScoped is the per-call variant of Execute that supports:
//   - per-call tool subset (scope narrowing) without mutating h.tools
//   - per-call backend override (LM Studio routing, custom model name)
//   - per-call think-flag override (peer reasoning vs JSON discipline)
//   - structured return: surfaces tool-call summaries + the respond-tool's
//     content (when the agent invoked respond instead of emitting plain text)
//
// Background: a dispatch from the foveal Claude into the peripheral Gemma
// swarm needs (a) deterministic tool envelope per role, (b) the actual user-
// visible text the agent produced, and (c) enough trace metadata to debug
// without touching the ledger. Execute returns "" when respond is the only
// thing the agent did; ExecuteScoped fixes that by remembering the respond
// argument's text and using it as the canonical Content.
//
// KV-cache discipline: each call rebuilds the messages slice fresh and posts
// to Ollama with its own context — Ollama's KV cache is request-keyed by the
// full prompt, so distinct dispatches don't collide. Concurrent calls are
// safe; correctness comes from the freshness of the messages slice rather
// than goroutine isolation.

// ExecuteScopedOptions controls a single ExecuteScoped invocation. The zero
// value behaves identically to Execute: harness defaults for system prompt,
// tools, backend, and think flag.
type ExecuteScopedOptions struct {
	// SystemPrompt overrides the default system prompt for this call only.
	// Empty string keeps the caller-supplied default.
	SystemPrompt string

	// AllowedTools, when non-empty, restricts the tool registry to names in
	// the slice. Names not present in h.toolFuncs surface as
	// ErrUnknownScopedTool. nil/empty uses the full default set.
	AllowedTools []string

	// BackendURL overrides h.ollamaURL for this call. Empty keeps the
	// default. Must point at an OpenAI-compatible-or-Ollama-native chat
	// endpoint; the dispatcher decides which family.
	BackendURL string

	// BackendKind selects how to talk to BackendURL: "ollama" (default,
	// /api/chat) or "openai" (LM Studio, /v1/chat/completions).
	BackendKind string

	// Model overrides h.model for this call. Empty keeps the default.
	Model string

	// Thinking, when non-nil, overrides the think flag for this call.
	Thinking *bool

	// MaxTurns overrides h.maxTurns. Zero keeps the default.
	MaxTurns int
}

// ErrUnknownScopedTool is returned by ExecuteScoped when AllowedTools
// references a tool name that is not in the harness registry. Surfaces
// as the per-slot Error in the dispatcher rather than a transport panic.
var ErrUnknownScopedTool = fmt.Errorf("scoped tool not registered")

// ExecuteScopedResult is the structured return from ExecuteScoped. Content
// is the canonical user-visible string (the respond tool's text, or the
// final assistant content if respond never ran), ToolCalls is the per-turn
// summary, and Turns counts how many tool-loop iterations actually ran.
type ExecuteScopedResult struct {
	Content   string                       `json:"content"`
	ToolCalls []ScopedExecuteToolCallEntry `json:"tool_calls"`
	Turns     int                          `json:"turns"`
}

// ScopedExecuteToolCallEntry is one tool invocation observed in the loop,
// truncated to keep result envelopes small.
type ScopedExecuteToolCallEntry struct {
	Name         string `json:"name"`
	ArgsDigest   string `json:"args_digest,omitempty"`
	ResultDigest string `json:"result_digest,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ExecuteScoped runs the tool loop with per-call overrides. See
// ExecuteScopedOptions for what each field does. Returns the structured
// result regardless of whether the model called respond or not.
func (h *AgentHarness) ExecuteScoped(ctx context.Context, task string, opts ExecuteScopedOptions) (*ExecuteScopedResult, error) {
	systemPrompt := opts.SystemPrompt
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = h.maxTurns
	}
	think := false
	if opts.Thinking != nil {
		think = *opts.Thinking
	}

	// Build the per-call tool view. We keep two parallel structures: the
	// allowed []ToolDefinition (sent to the model) and an allowed
	// map[string]ToolFunc (used to dispatch by name). Both default to the
	// harness's full registry when AllowedTools is empty.
	allowedDefs, allowedFuncs, err := h.scopeTools(opts.AllowedTools)
	if err != nil {
		return nil, err
	}

	model := opts.Model
	if model == "" {
		model = h.model
	}
	backendURL := opts.BackendURL
	if backendURL == "" {
		backendURL = h.ollamaURL
	}
	backendKind := opts.BackendKind
	if backendKind == "" {
		backendKind = backendKindOllama
	}

	messages := []agentChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task},
	}

	result := &ExecuteScopedResult{}
	respondContent := ""

	for turn := 0; turn < maxTurns; turn++ {
		req := agentChatRequest{
			Model:     model,
			Messages:  messages,
			Tools:     allowedDefs,
			Stream:    false,
			Think:     think,
			Options:   map[string]interface{}{"num_ctx": 8192},
			KeepAlive: -1, // pin model in VRAM across dispatch turns
		}

		resp, err := h.chatCompletionTo(ctx, backendURL, backendKind, req)
		if err != nil {
			return result, fmt.Errorf("execute-scoped turn %d: %w", turn, err)
		}
		result.Turns = turn + 1

		msg := resp.Message

		// No tool calls — model is done. Use respondContent if we captured
		// one earlier; otherwise the assistant's final content.
		if len(msg.ToolCalls) == 0 {
			if respondContent != "" {
				result.Content = respondContent
			} else {
				result.Content = msg.Content
			}
			return result, nil
		}

		messages = append(messages, agentChatMessage{
			Role:      "assistant",
			ToolCalls: msg.ToolCalls,
		})

		var waitInvoked bool
		var waitReason string
		for _, tc := range msg.ToolCalls {
			entry := ScopedExecuteToolCallEntry{
				Name:       tc.Function.Name,
				ArgsDigest: digestJSON(tc.Function.Arguments),
			}

			fn, ok := allowedFuncs[tc.Function.Name]
			if !ok {
				// Out-of-scope tool: tell the model it's not available
				// without aborting the loop, and record the violation.
				errMsg := fmt.Sprintf("tool %q is not in this dispatch's scope", tc.Function.Name)
				entry.Error = errMsg
				result.ToolCalls = append(result.ToolCalls, entry)
				blocked := json.RawMessage(fmt.Sprintf(`{"error": %q}`, errMsg))
				messages = append(messages, agentChatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    string(blocked),
				})
				continue
			}

			toolStart := time.Now()
			toolResult, toolErr := fn(ctx, json.RawMessage(tc.Function.Arguments))
			toolDuration := time.Since(toolStart)
			if cycleID := cycleIDFromContext(ctx); cycleID != "" {
				ev, bErr := trace.NewToolDispatch(
					engine.TraceIdentity(),
					cycleID,
					tc.Function.Name,
					tc.Function.Arguments,
					toolDuration,
					toolErr,
				)
				if bErr == nil {
					emitCycleEvent(ev)
				}
			}
			if toolErr != nil {
				entry.Error = toolErr.Error()
				toolResult = []byte(fmt.Sprintf(`{"error": %q}`, toolErr.Error()))
			}
			entry.ResultDigest = digestJSON(toolResult)

			if tc.Function.Name == waitToolName {
				waitInvoked = true
				if r := extractWaitReason(toolResult); r != "" {
					waitReason = r
				}
			}
			if tc.Function.Name == respondToolName {
				if t := extractRespondText(json.RawMessage(tc.Function.Arguments)); t != "" {
					respondContent = t
				}
			}

			result.ToolCalls = append(result.ToolCalls, entry)
			messages = append(messages, agentChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    string(toolResult),
			})
		}

		if waitInvoked {
			if respondContent != "" {
				result.Content = respondContent
			} else if waitReason != "" {
				result.Content = fmt.Sprintf("waited: %s", waitReason)
			} else {
				result.Content = "waited"
			}
			return result, nil
		}
	}

	if respondContent != "" {
		result.Content = respondContent
		return result, nil
	}
	return result, fmt.Errorf("execute-scoped: hit max turns (%d) without completion", maxTurns)
}

// scopeTools returns the per-call tool view. Empty AllowedTools yields the
// full default registry. Unknown names cause an error.
func (h *AgentHarness) scopeTools(allowed []string) ([]ToolDefinition, map[string]ToolFunc, error) {
	if len(allowed) == 0 {
		return h.tools, h.toolFuncs, nil
	}
	defs := make([]ToolDefinition, 0, len(allowed))
	funcs := make(map[string]ToolFunc, len(allowed))
	for _, name := range allowed {
		fn, ok := h.toolFuncs[name]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnknownScopedTool, name)
		}
		funcs[name] = fn
		// Find the matching definition by name; toolFuncs and tools are
		// kept in sync by RegisterTool, so the definition is guaranteed
		// to exist when fn does.
		for _, def := range h.tools {
			if def.Function.Name == name {
				defs = append(defs, def)
				break
			}
		}
	}
	return defs, funcs, nil
}

// ToolNames returns the names of all currently-registered tools. Useful for
// tests and for the dispatcher to validate AllowedTools without poking at
// internals.
func (h *AgentHarness) ToolNames() []string {
	out := make([]string, 0, len(h.toolFuncs))
	for name := range h.toolFuncs {
		out = append(out, name)
	}
	return out
}

// extractRespondText pulls the "text" field from the respond tool's JSON
// arguments. Returns "" on parse failure or empty text.
func extractRespondText(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return ""
	}
	return p.Text
}

// digestJSON returns a short, log-safe view of a JSON value: trimmed of
// surrounding whitespace and clipped to digestMaxBytes characters with an
// ellipsis suffix when truncated. Returns "" for empty input.
func digestJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if len(s) > digestMaxBytes {
		return s[:digestMaxBytes] + "..."
	}
	return s
}

// digestMaxBytes caps the per-field size in tool-call summaries so a single
// dispatch result envelope stays well under any reasonable HTTP body limit
// even with N=4 and 10 tool calls per slot.
const digestMaxBytes = 200

// backendKindOllama and backendKindOpenAI select which wire protocol
// chatCompletionTo speaks. Ollama uses /api/chat with the native shape;
// OpenAI uses /v1/chat/completions and a slightly different request/response.
const (
	backendKindOllama = "ollama"
	backendKindOpenAI = "openai"
)

// chatCompletionTo is the per-call variant of chatCompletion: takes an
// explicit backend URL and kind so a dispatch can route to LM Studio without
// mutating h.ollamaURL. Ollama path is the existing /api/chat shape; OpenAI
// path translates to /v1/chat/completions and back.
func (h *AgentHarness) chatCompletionTo(ctx context.Context, backendURL, kind string, req agentChatRequest) (*agentChatResponse, error) {
	if backendURL == "" {
		backendURL = h.ollamaURL
	}
	if kind == "" {
		kind = backendKindOllama
	}
	if kind == backendKindOpenAI {
		return h.chatCompletionOpenAI(ctx, backendURL, req)
	}
	// Ollama native — same as chatCompletion but parameterized URL.
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", httpResp.StatusCode, string(respBody))
	}
	var resp agentChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

// chatCompletionOpenAI talks to a /v1/chat/completions endpoint (LM Studio,
// vLLM, OpenAI proper). Translates the Ollama-shaped request into the
// OpenAI shape and translates the response back. Tool-call ID and arguments
// shape match the Ollama native format already, so the rest of the loop
// doesn't need to know which backend served a turn.
func (h *AgentHarness) chatCompletionOpenAI(ctx context.Context, backendURL string, req agentChatRequest) (*agentChatResponse, error) {
	type openaiToolCallFn struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type openaiToolCall struct {
		ID       string           `json:"id"`
		Type     string           `json:"type"`
		Function openaiToolCallFn `json:"function"`
	}
	type openaiMessage struct {
		Role       string           `json:"role"`
		Content    string           `json:"content,omitempty"`
		ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
	}
	type openaiRequest struct {
		Model    string           `json:"model"`
		Messages []openaiMessage  `json:"messages"`
		Tools    []ToolDefinition `json:"tools,omitempty"`
		Stream   bool             `json:"stream"`
	}

	// Translate request — flatten our agentChatMessage into openaiMessage.
	// OpenAI's tool_calls.function.arguments is a JSON-encoded *string*,
	// not a JSON object — re-encode as needed.
	outMsgs := make([]openaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := openaiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, openaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openaiToolCallFn{
					Name:      tc.Function.Name,
					Arguments: string(tc.Function.Arguments),
				},
			})
		}
		outMsgs = append(outMsgs, om)
	}
	body, err := json.Marshal(openaiRequest{
		Model:    req.Model,
		Messages: outMsgs,
		Tools:    req.Tools,
		Stream:   false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http openai request: %w", err)
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read openai response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai http %d: %s", httpResp.StatusCode, string(respBody))
	}

	// Translate response — OpenAI returns choices[0].message; flatten.
	var rawResp struct {
		Choices []struct {
			Message openaiMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &rawResp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}
	out := &agentChatResponse{Done: true}
	if len(rawResp.Choices) == 0 {
		return out, nil
	}
	m := rawResp.Choices[0].Message
	out.Message.Role = m.Role
	out.Message.Content = m.Content
	for _, tc := range m.ToolCalls {
		ac := agentToolCall{ID: tc.ID, Type: "function"}
		ac.Function.Name = tc.Function.Name
		ac.Function.Arguments = json.RawMessage(tc.Function.Arguments)
		out.Message.ToolCalls = append(out.Message.ToolCalls, ac)
	}
	return out, nil
}
