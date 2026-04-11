// provider_openai.go — OpenAICompatProvider
//
// Implements Provider against any OpenAI-compatible API server: LM Studio,
// vLLM, llama.cpp server, text-generation-webui, or the OpenAI API itself.
// Uses /v1/chat/completions for both streaming (SSE) and non-streaming.
// Discovery: GET /v1/models to enumerate available models.
//
// SSE format: "data: {...}\n\n" lines with "data: [DONE]" sentinel.
// No CGO dependencies — standard library net/http only.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	openaiCompatDefaultEndpoint = "http://localhost:1234"
	openaiCompatDefaultMaxToks  = 4096
)

// OpenAICompatProvider implements Provider against any OpenAI-compatible server.
type OpenAICompatProvider struct {
	name      string
	endpoint  string // e.g. "http://localhost:1234" or "http://192.168.10.191:1234"
	apiKey    string // optional; some local servers don't require auth
	model     string
	maxTokens int
	timeout   time.Duration
	client    *http.Client
}

// NewOpenAICompatProvider creates an OpenAICompatProvider from a ProviderConfig.
func NewOpenAICompatProvider(name string, cfg ProviderConfig) *OpenAICompatProvider {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = openaiCompatDefaultEndpoint
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = openaiCompatDefaultMaxToks
	}
	apiKey := ""
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	return &OpenAICompatProvider{
		name:      name,
		endpoint:  strings.TrimRight(endpoint, "/"),
		apiKey:    apiKey,
		model:     cfg.Model,
		maxTokens: maxTokens,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},
	}
}

// Name returns the provider identifier.
func (p *OpenAICompatProvider) Name() string { return p.name }

// Available checks if the server is reachable and has at least one model.
func (p *OpenAICompatProvider) Available(ctx context.Context) bool {
	models, err := p.listModels(ctx)
	if err != nil {
		return false
	}
	// If a specific model is configured, check it exists.
	if p.model != "" {
		for _, m := range models {
			if m == p.model || strings.HasPrefix(m, p.model) {
				return true
			}
		}
		// Model not found — the configured model isn't loaded on this server.
		return false
	}
	return len(models) > 0
}

// Capabilities returns what this provider supports.
func (p *OpenAICompatProvider) Capabilities() ProviderCapabilities {
	caps := []Capability{CapStreaming, CapJSON}
	maxCtx := 0
	maxOut := p.maxTokens
	if maxOut <= 0 {
		maxOut = openaiCompatDefaultMaxToks
	}
	models := []string{}
	if p.model != "" {
		models = []string{p.model}
	}
	return ProviderCapabilities{
		Capabilities:       caps,
		MaxContextTokens:   maxCtx, // unknown for generic endpoints; 0 = unspecified
		MaxOutputTokens:    maxOut,
		ModelsAvailable:    models,
		IsLocal:            true,
		AgenticHarness:     false,
		CostPerInputToken:  0, // local inference
		CostPerOutputToken: 0,
	}
}

// Ping probes the endpoint and returns measured latency.
func (p *OpenAICompatProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/v1/models", nil)
	if err != nil {
		return 0, fmt.Errorf("openai-compat: ping: build request: %w", err)
	}
	p.setHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("openai-compat: ping: %w", err)
	}
	resp.Body.Close()
	return time.Since(start), nil
}

// setHeaders applies auth and content-type headers.
func (p *OpenAICompatProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// effectiveModel returns the model to send: request-level override if set,
// otherwise the provider's configured default.
func (p *OpenAICompatProvider) effectiveModel(req *CompletionRequest) string {
	if req.ModelOverride != "" {
		return req.ModelOverride
	}
	return p.model
}

// listModels fetches the model list from /v1/models.
func (p *OpenAICompatProvider) listModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	p.setHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var result openaiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range result.Data {
		names = append(names, m.ID)
	}
	return names, nil
}

// ── OpenAI wire types ────────────────────────────────────────────────────────

type openaiModelsResponse struct {
	Data []openaiModel `json:"data"`
}

type openaiModel struct {
	ID string `json:"id"`
}

type openaiChatRequest struct {
	Model       string                 `json:"model"`
	Messages    []openaiMessage        `json:"messages"`
	Stream      bool                   `json:"stream"`
	MaxTokens   int                    `json:"max_tokens,omitempty"`
	Temperature *float64               `json:"temperature,omitempty"`
	TopP        *float64               `json:"top_p,omitempty"`
	Stop        []string               `json:"stop,omitempty"`
	Tools       []openaiTool           `json:"tools,omitempty"`
	ToolChoice  interface{}            `json:"tool_choice,omitempty"` // string or object
	Options     map[string]interface{} `json:"-"`                     // not sent, internal only
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiTool struct {
	Type     string             `json:"type"` // "function"
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type openaiToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"` // "function"
	Function openaiToolCallDetail `json:"function"`
}

type openaiToolCallDetail struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Non-streaming response.
type openaiChatResponse struct {
	ID      string              `json:"id"`
	Choices []openaiChoice      `json:"choices"`
	Usage   *openaiUsageResponse `json:"usage,omitempty"`
}

type openaiChoice struct {
	Index        int           `json:"index"`
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"` // "stop", "length", "tool_calls"
}

type openaiUsageResponse struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SSE streaming chunk.
type openaiStreamChunk struct {
	ID      string                   `json:"id"`
	Choices []openaiStreamChoice     `json:"choices"`
	Usage   *openaiUsageResponse     `json:"usage,omitempty"` // some servers send usage on final chunk
}

type openaiStreamChoice struct {
	Index        int                `json:"index"`
	Delta        openaiStreamDelta  `json:"delta"`
	FinishReason *string            `json:"finish_reason"` // pointer: null until final chunk
}

type openaiStreamDelta struct {
	Role      string                    `json:"role,omitempty"`
	Content   string                    `json:"content,omitempty"`
	ToolCalls []openaiStreamToolCall    `json:"tool_calls,omitempty"`
}

// openaiStreamToolCall is the streaming variant of a tool call delta.
// Unlike the non-streaming openaiToolCall, it includes an Index field
// that identifies which tool call the delta belongs to.
type openaiStreamToolCall struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"` // "function"
	Function openaiToolCallDetail `json:"function"`
}

// ── Request builder ──────────────────────────────────────────────────────────

// buildOpenAIRequest converts a CompletionRequest to the OpenAI wire format.
func buildOpenAIRequest(model string, req *CompletionRequest, stream bool, maxTokens int) *openaiChatRequest {
	msgs := make([]openaiMessage, 0, len(req.Messages)+1)

	// System prompt.
	if req.SystemPrompt != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: req.SystemPrompt})
	}

	// Context items prepended as system messages.
	for _, item := range req.Context {
		if item.Content == "" {
			continue
		}
		msgs = append(msgs, openaiMessage{
			Role:    "system",
			Content: fmt.Sprintf("[context id=%q zone=%s salience=%.2f]\n%s", item.ID, item.Zone, item.Salience, item.Content),
		})
	}

	// Conversation messages.
	for _, m := range req.Messages {
		msg := openaiMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		// Convert outbound tool calls on assistant messages.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openaiToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openaiToolCallDetail{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		msgs = append(msgs, msg)
	}

	or := &openaiChatRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}

	if maxTokens > 0 {
		or.MaxTokens = maxTokens
	}
	if req.MaxTokens > 0 {
		or.MaxTokens = req.MaxTokens
	}

	// Map tools.
	if len(req.Tools) > 0 {
		or.Tools = make([]openaiTool, len(req.Tools))
		for i, t := range req.Tools {
			or.Tools[i] = openaiTool{
				Type: "function",
				Function: openaiToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			}
		}
	}

	// Map tool choice.
	if req.ToolChoice != "" {
		or.ToolChoice = req.ToolChoice // "auto", "none", "required" pass through
	}

	return or
}

// ── Complete ─────────────────────────────────────────────────────────────────

// Complete sends a non-streaming request and returns the full response.
func (p *OpenAICompatProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	payload := buildOpenAIRequest(model, req, false, p.maxTokens)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai-compat: build request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai-compat: status %d: %s", resp.StatusCode, string(data))
	}

	var or openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
		return nil, fmt.Errorf("openai-compat: decode response: %w", err)
	}

	return parseOpenAIResponse(&or, model, p.name, time.Since(start)), nil
}

// parseOpenAIResponse converts an openaiChatResponse into a provider-agnostic
// CompletionResponse.
func parseOpenAIResponse(or *openaiChatResponse, model, providerName string, latency time.Duration) *CompletionResponse {
	cr := &CompletionResponse{
		ProviderMeta: ProviderMeta{
			Provider: providerName,
			Model:    model,
			Latency:  latency,
		},
	}

	if len(or.Choices) > 0 {
		choice := or.Choices[0]
		cr.Content = choice.Message.Content
		cr.StopReason = mapOpenAIFinishReason(choice.FinishReason)

		// Extract tool calls.
		for _, tc := range choice.Message.ToolCalls {
			cr.ToolCalls = append(cr.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	if cr.StopReason == "" {
		cr.StopReason = "end_turn"
	}

	if or.Usage != nil {
		cr.Usage = TokenUsage{
			InputTokens:  or.Usage.PromptTokens,
			OutputTokens: or.Usage.CompletionTokens,
		}
	}

	return cr
}

// mapOpenAIFinishReason converts OpenAI finish_reason to provider-agnostic stop reasons.
func mapOpenAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return reason
	}
}

// ── Stream ───────────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of incremental chunks.
// The channel closes when generation is complete or the context is cancelled.
func (p *OpenAICompatProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	model := p.effectiveModel(req)
	payload := buildOpenAIRequest(model, req, true, p.maxTokens)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: marshal stream request: %w", err)
	}

	// Use a no-timeout client for streaming — generation can run long.
	streamClient := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai-compat: build stream request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-compat: stream status %d: %s", resp.StatusCode, string(data))
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseOpenAISSE(ctx, resp.Body, ch, model, p.name)
	}()

	return ch, nil
}

// parseOpenAISSE reads an OpenAI-compatible SSE stream and sends StreamChunks.
//
// SSE format: each line is "data: <json>" terminated by "\n\n".
// The stream ends with "data: [DONE]".
//
// Tool calls arrive as incremental deltas with index, id, name, and argument
// fragments across multiple chunks.
func parseOpenAISSE(ctx context.Context, r io.Reader, ch chan<- StreamChunk, model, providerName string) {
	var finishReason string
	var usage *openaiUsageResponse

	send := func(sc StreamChunk) bool {
		select {
		case ch <- sc:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()

		// SSE data lines: "data: <payload>"
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			send(StreamChunk{Error: fmt.Errorf("openai-compat: decode SSE chunk: %w", err)})
			return
		}

		// Capture usage if present on this chunk.
		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			// Text content delta.
			if choice.Delta.Content != "" {
				if !send(StreamChunk{Delta: choice.Delta.Content}) {
					return
				}
			}

			// Tool call deltas.
			for _, tc := range choice.Delta.ToolCalls {
				tcd := &ToolCallDelta{
					Index: tc.Index,
				}
				// First chunk for a tool call has ID and name.
				if tc.ID != "" {
					tcd.ID = tc.ID
				}
				if tc.Function.Name != "" {
					tcd.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					tcd.ArgsDelta = tc.Function.Arguments
				}
				if !send(StreamChunk{ToolCallDelta: tcd}) {
					return
				}
			}

			// Capture finish reason.
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		send(StreamChunk{Error: fmt.Errorf("openai-compat: scan: %w", err)})
		return
	}

	// Emit the terminal Done chunk.
	stopReason := mapOpenAIFinishReason(finishReason)
	if stopReason == "" {
		stopReason = "end_turn"
	}
	final := StreamChunk{
		Done:       true,
		StopReason: stopReason,
		ProviderMeta: &ProviderMeta{
			Provider: providerName,
			Model:    model,
		},
	}
	if usage != nil {
		final.Usage = &TokenUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
		}
	}
	send(final)
}
