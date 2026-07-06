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
	"sync"
	"time"
)

const (
	// openaiCompatDefaultEndpoint is the fallback endpoint when no Endpoint is
	// configured and the COGOS_LLM_ENDPOINT environment variable is unset.
	// Defaults to LM Studio's loopback port (the live local backend since
	// PR #417 decommissioned Ollama's :11434 as the default — see
	// provider_ollama.go's decommission header and router.go's
	// probeLocalBackend/defaultProvidersConfig, both of which already probe
	// LM Studio :1234 first). This constant had drifted back to Ollama's
	// :11434 after that decommission, which meant a no-provider dispatch
	// (resolveLocalLLMEndpoint -> NewOpenAICompatProvider) dead-ended
	// connection-refused instead of reaching the resident LM Studio backend
	// (flight review 2026-07-03 §5.2). Override via config or env for other
	// servers (vLLM, llama.cpp, remote hosts, etc.).
	openaiCompatDefaultEndpoint = "http://localhost:1234"
	openaiCompatDefaultMaxToks  = 4096
)

// OpenAICompatProvider implements Provider against any OpenAI-compatible server.
type OpenAICompatProvider struct {
	name           string
	endpoint       string // e.g. "http://<inference-host>:<port>" (local or remote)
	apiKey         string // optional; some local servers don't require auth
	model          string
	maxTokens      int
	timeout        time.Duration
	client         *http.Client
	defaultOptions map[string]interface{} // extra fields merged into every request body

	// availMu guards the Available() TTL cache (#441). The router's probeAll and
	// the per-request /v1/providers handler can call Available() concurrently;
	// the mutex is held across the probe so concurrent callers collapse into a
	// single GET /v1/models rather than fanning out N identical requests.
	availMu     sync.Mutex
	availResult bool
	availAt     time.Time
}

// NewOpenAICompatProvider creates an OpenAICompatProvider from a ProviderConfig.
//
// Endpoint resolution order: cfg.Endpoint > COGOS_LLM_ENDPOINT env >
// openaiCompatDefaultEndpoint (localhost LM Studio default). This lets users
// point at an arbitrary OpenAI-compatible server without editing config.
func NewOpenAICompatProvider(name string, cfg ProviderConfig) *OpenAICompatProvider {
	endpoint := resolveLocalLLMEndpoint(cfg.Endpoint)
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		// #432: a fixed ceiling below realistic local latency is an
		// infinite-retry engine — local models under concurrent-slot prefill
		// load observed 2-4 min single-turn latency in the incident that
		// closed this issue. The previous 60s fallback here silently
		// under-timed any provider config that omitted `timeout:`, so a
		// caller relying on the default would hit the same abandon-and-retry
		// failure mode as the (now-fixed) 30s dispatch default in
		// agent_dispatch_query.go. Match that default so both ceilings agree
		// absent explicit config; providers.yaml's committed defaults
		// (lmstudio-darkstar: 300s) remain the recommended explicit value.
		timeout = time.Duration(dispatchTimeoutDefault) * time.Second
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = openaiCompatDefaultMaxToks
	}
	apiKey := ""
	if cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}

	// Extract default_options from the provider's Options map. These key/value
	// pairs are merged into every request body verbatim, allowing per-provider
	// request shaping without new struct fields.
	//
	// Example: set `reasoning_effort: "none"` on a foveal/conversational provider
	// to suppress Eclipse 26b A4B thinking tokens (empirically verified 2026-05-15:
	// `reasoning_effort: "none"` removes reasoning_content entirely; "minimal" does
	// not). The peripheral/deliberation variant omits this option so thinking runs.
	//
	// Future direction (Option B): a separate `lmstudio-eclipse-peripheral` provider
	// entry pointing at the same endpoint+model but without default_options, so the
	// kernel can route deliberation work to the thinking-enabled variant explicitly.
	var defaultOpts map[string]interface{}
	if raw, ok := cfg.Options["default_options"]; ok {
		if m, ok := raw.(map[string]interface{}); ok && len(m) > 0 {
			defaultOpts = m
		}
	}

	return &OpenAICompatProvider{
		name:           name,
		endpoint:       normalizeLocalLLMEndpoint(endpoint),
		apiKey:         apiKey,
		model:          cfg.Model,
		maxTokens:      maxTokens,
		timeout:        timeout,
		client:         &http.Client{Timeout: timeout},
		defaultOptions: defaultOpts,
	}
}

// Name returns the provider identifier.
func (p *OpenAICompatProvider) Name() string { return p.name }

// Model returns the configured model identifier.
func (p *OpenAICompatProvider) Model() string { return p.model }

// Available reports whether the server is reachable and has a usable model.
// The result is cached for availCacheTTL so the router's periodic availability
// probe (and the per-request /v1/providers handler) don't issue a live
// GET /v1/models to the upstream on every call (#441). The mutex is held across
// the probe so concurrent callers collapse into a single request.
func (p *OpenAICompatProvider) Available(ctx context.Context) bool {
	p.availMu.Lock()
	defer p.availMu.Unlock()
	if !p.availAt.IsZero() && time.Since(p.availAt) < availCacheTTL {
		return p.availResult
	}
	fresh := p.probeAvailable(ctx)
	// Don't let a caller's cancelled/expired context poison the shared cache: a
	// probe that failed only because the caller went away (e.g. an HTTP client
	// disconnecting on /v1/providers, which passes r.Context()) says nothing
	// about provider health. Skip the cache write so the next caller re-probes,
	// instead of marking a healthy provider unavailable to everyone for the TTL.
	if !fresh && ctx.Err() != nil {
		return p.availResult
	}
	p.availResult = fresh
	p.availAt = time.Now()
	return p.availResult
}

// probeAvailable performs the live reachability check backing Available():
// GET /v1/models, then confirm the configured model (if any) is present. Call
// via Available() to get TTL caching; calling this directly bypasses the cache.
func (p *OpenAICompatProvider) probeAvailable(ctx context.Context) bool {
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
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"` // LM Studio / reasoning models (non-streaming)
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
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
	ID      string               `json:"id"`
	Choices []openaiChoice       `json:"choices"`
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
	ID      string               `json:"id"`
	Choices []openaiStreamChoice `json:"choices"`
	Usage   *openaiUsageResponse `json:"usage,omitempty"` // some servers send usage on final chunk
}

type openaiStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"` // pointer: null until final chunk
}

type openaiStreamDelta struct {
	Role             string                 `json:"role,omitempty"`
	Content          string                 `json:"content,omitempty"`
	ReasoningContent string                 `json:"reasoning_content,omitempty"` // LM Studio / reasoning models
	ToolCalls        []openaiStreamToolCall `json:"tool_calls,omitempty"`
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

	// Map tools. Skip when tool_choice is "none" — the caller has explicitly
	// opted out of tool use for this turn; sending schemas wastes tokens and
	// can bias models toward tool calls they should ignore.
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
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

// marshalRequest serializes an openaiChatRequest and merges in any
// provider-level defaultOptions. The merge happens after struct serialization
// so per-request fields always win over provider defaults when both are set.
// Returns an error if either serialization step fails.
func (p *OpenAICompatProvider) marshalRequest(payload *openaiChatRequest) ([]byte, error) {
	if len(p.defaultOptions) == 0 {
		return json.Marshal(payload)
	}
	// Serialize the struct, unmarshal into a generic map, inject defaults,
	// then re-serialize. This keeps all struct fields intact while allowing
	// arbitrary extra keys (e.g. reasoning_effort) without new typed fields.
	structBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(structBytes, &merged); err != nil {
		return nil, err
	}
	for k, v := range p.defaultOptions {
		// Only inject if the struct didn't set this key (struct fields win).
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// ── Complete ─────────────────────────────────────────────────────────────────

// Complete sends a non-streaming request and returns the full response.
func (p *OpenAICompatProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	payload := buildOpenAIRequest(model, req, false, p.maxTokens)
	body, err := p.marshalRequest(payload)
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

// CompleteCancelSafe is like Complete but propagates ctx-cancellation to the
// server by routing the request through Stream and aggregating the resulting
// chunks into a single CompletionResponse.
//
// Background (#432): a plain non-streaming /v1/chat/completions request that
// the kernel abandons (ctx cancel/timeout) does NOT reliably cause LM Studio
// (or other openai-compat servers) to abort generation server-side — the
// local http.Client gives up waiting for a response, but the TCP connection
// isn't torn down in time for the server to notice, so the model keeps
// generating headless in one of the server's parallel slots (confirmed via
// TestOpenAICompleteHTTPError's sibling probe: a canceled Complete() request
// leaves the server-side context alive). A streaming request's body-read loop
// is actively selecting on the connection, so client disconnection propagates
// immediately and the server aborts generation (confirmed clean shutdown in
// the 2026-07-04 incident for every request using chat_completion_stream_request/
// Stream()). Callers that need cancel-safety without changing their call
// shape (internal non-interactive consults, dispatch, tool-loop re-calls)
// should call this instead of Complete.
func (p *OpenAICompatProvider) CompleteCancelSafe(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	ch, err := p.Stream(ctx, req)
	if err != nil {
		return nil, err
	}

	var (
		content          strings.Builder
		reasoningContent strings.Builder
		toolCalls        []ToolCall
		toolCallsByIndex = map[int]*ToolCall{}
		toolCallOrder    []int
		stopReason       string
		usage            TokenUsage
		streamErr        error
	)

	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
			continue
		}
		if chunk.IsReasoning {
			reasoningContent.WriteString(chunk.Delta)
		} else if chunk.Delta != "" {
			content.WriteString(chunk.Delta)
		}
		if chunk.ToolCallDelta != nil {
			td := chunk.ToolCallDelta
			tc, ok := toolCallsByIndex[td.Index]
			if !ok {
				tc = &ToolCall{}
				toolCallsByIndex[td.Index] = tc
				toolCallOrder = append(toolCallOrder, td.Index)
			}
			if td.ID != "" {
				tc.ID = td.ID
			}
			if td.Name != "" {
				tc.Name = td.Name
			}
			if td.ArgsDelta != "" {
				tc.Arguments += td.ArgsDelta
			}
		}
		if chunk.Done {
			if chunk.StopReason != "" {
				stopReason = chunk.StopReason
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
		}
	}

	// If the context was canceled mid-stream, report that rather than a
	// partial/empty success — callers (assessCycle, dispatch, tool loop)
	// treat a cancel-safe error identically to a Complete() error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("openai-compat: cancel-safe complete: %w", ctxErr)
	}
	if streamErr != nil {
		return nil, fmt.Errorf("openai-compat: cancel-safe complete: %w", streamErr)
	}

	for _, idx := range toolCallOrder {
		toolCalls = append(toolCalls, *toolCallsByIndex[idx])
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}

	return &CompletionResponse{
		Content:          content.String(),
		ReasoningContent: reasoningContent.String(),
		ToolCalls:        toolCalls,
		StopReason:       stopReason,
		Usage:            usage,
		ProviderMeta: ProviderMeta{
			Provider: p.name,
			Model:    model,
			Latency:  time.Since(start),
		},
	}, nil
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
		cr.ReasoningContent = choice.Message.ReasoningContent
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
	body, err := p.marshalRequest(payload)
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
			// Reasoning/thinking content delta (LM Studio reasoning models,
			// e.g. Eclipse 26b A4B). Tagged with IsReasoning so the handler
			// can measure the thinking phase separately from answer generation.
			if choice.Delta.ReasoningContent != "" {
				if !send(StreamChunk{Delta: choice.Delta.ReasoningContent, IsReasoning: true}) {
					return
				}
			}

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
