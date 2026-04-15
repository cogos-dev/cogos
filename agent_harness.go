// agent_harness.go — Native Go agent harness for the homeostatic kernel loop.
//
// Runs as a goroutine inside the kernel process. Calls a local model (Gemma E4B
// via Ollama) through the OpenAI chat completions wire protocol. The loop is:
//
//   Observation → Assess (JSON mode) → Execute (tool loop) → Callback
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
	"net/http"
	"time"
)

// --- Wire protocol types (Ollama native /api/chat) ---
//
// Uses Ollama's native API instead of the OpenAI-compatible shim because:
// - Native API supports "think": false to control thinking mode at the source
// - OpenAI shim has no way to disable thinking (content bleeds into reasoning field)
// - Native API is the recommended path per Ollama docs
// See: https://github.com/ollama/ollama/issues/15288

// agentChatRequest is the Ollama native /api/chat request body.
type agentChatRequest struct {
	Model    string             `json:"model"`
	Messages []agentChatMessage `json:"messages"`
	Tools    []ToolDefinition   `json:"tools,omitempty"`
	Stream   bool               `json:"stream"`
	Think    bool               `json:"think"`              // explicit thinking control
	Format   string             `json:"format,omitempty"`   // "json" for structured output
}

// agentChatMessage is a single message in the conversation.
type agentChatMessage struct {
	Role       string          `json:"role"`                  // system, user, assistant, tool
	Content    string          `json:"content,omitempty"`     // text content
	ToolCalls  []agentToolCall `json:"tool_calls,omitempty"`  // assistant tool invocations
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
	Done       bool `json:"done"`
	DoneReason string `json:"done_reason,omitempty"`
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
			Timeout: 120 * time.Second,
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
		Model:    h.model,
		Messages: messages,
		Stream:   false,
		Think:    false, // disable thinking — we want clean JSON output
		Format:   "json",
	}

	resp, err := h.chatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("assess: %w", err)
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
			Model:    h.model,
			Messages: messages,
			Tools:    h.tools,
			Stream:   false,
			Think:    false, // disable thinking for tool loop
		}

		resp, err := h.chatCompletion(ctx, req)
		if err != nil {
			return "", fmt.Errorf("execute turn %d: %w", turn, err)
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

		// Dispatch each tool call and collect results.
		for _, tc := range msg.ToolCalls {
			result, err := h.dispatchTool(ctx, tc)
			if err != nil {
				// Tool errors go back to the model as content, not Go errors.
				result = []byte(fmt.Sprintf(`{"error": %q}`, err.Error()))
			}
			messages = append(messages, agentChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    string(result),
			})
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
