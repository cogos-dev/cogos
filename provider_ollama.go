// provider_ollama.go — OllamaProvider
//
// Implements Provider against a local Ollama server (http://localhost:11434).
// Uses /api/chat for multi-turn conversations (not /api/generate).
// Streaming: Ollama returns newline-delimited JSON chunks.
// think=false: disables qwen3's thinking mode to avoid silent token burn.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaProvider implements Provider against a local Ollama server.
type OllamaProvider struct {
	name          string
	endpoint      string // e.g. "http://localhost:11434"
	model         string
	contextWindow int // num_ctx to send per request; 0 = Ollama default (4096)
	timeout       time.Duration
	client        *http.Client
}

// NewOllamaProvider creates an OllamaProvider from a ProviderConfig.
func NewOllamaProvider(name string, cfg ProviderConfig) *OllamaProvider {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &OllamaProvider{
		name:          name,
		endpoint:      strings.TrimRight(endpoint, "/"),
		model:         cfg.Model,
		contextWindow: cfg.ContextWindow,
		timeout:       timeout,
		client:        &http.Client{Timeout: timeout},
	}
}

// Name returns the provider identifier.
func (p *OllamaProvider) Name() string { return p.name }

// Available checks if Ollama is running and the configured model is loaded.
func (p *OllamaProvider) Available(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false
	}
	// Accept exact name or prefix (e.g. "qwen2.5:9b" matches "qwen2.5:9b-instruct").
	for _, m := range tags.Models {
		if m.Name == p.model || strings.HasPrefix(m.Name, p.model) {
			return true
		}
	}
	return false
}

// Capabilities returns what Ollama supports.
func (p *OllamaProvider) Capabilities() ProviderCapabilities {
	ctxTokens := p.contextWindow
	if ctxTokens <= 0 {
		ctxTokens = 4096 // Ollama default when num_ctx not set
	}
	return ProviderCapabilities{
		Capabilities:       []Capability{CapStreaming, CapJSON},
		MaxContextTokens:   ctxTokens,
		MaxOutputTokens:    4096,
		ModelsAvailable:    []string{p.model},
		IsLocal:            true,
		AgenticHarness:     false,
		CostPerInputToken:  0,
		CostPerOutputToken: 0,
	}
}

// ContextWindow returns the configured num_ctx for this provider.
func (p *OllamaProvider) ContextWindow() int {
	return p.contextWindow
}

// Ping measures round-trip latency to the Ollama server.
func (p *OllamaProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/api/version", nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return time.Since(start), nil
}

// ── Ollama wire types ─────────────────────────────────────────────────────────

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"` // false = disable thinking mode (qwen3)
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Model     string        `json:"model"`
	CreatedAt string        `json:"created_at"`
	Message   ollamaMessage `json:"message"`
	Done      bool          `json:"done"`
	// Token counts (only in final streaming chunk or non-streaming response).
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// buildOllamaRequest converts a CompletionRequest to Ollama's /api/chat format.
// contextWindow sets num_ctx on the request; 0 means omit (use Ollama default of 4096).
func buildOllamaRequest(model string, req *CompletionRequest, stream bool, contextWindow int) *ollamaChatRequest {
	msgs := make([]ollamaMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	opts := map[string]any{}
	if contextWindow > 0 {
		opts["num_ctx"] = contextWindow
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.MaxTokens != 0 {
		opts["num_predict"] = req.MaxTokens
	}

	return &ollamaChatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   stream,
		Think:    false, // prevent silent token burn in qwen3 thinking mode
		Options:  opts,
	}
}

// effectiveModel returns the model to send to Ollama: request override if set,
// otherwise the provider's configured default.
func (p *OllamaProvider) effectiveModel(req *CompletionRequest) string {
	if req.ModelOverride != "" {
		return req.ModelOverride
	}
	return p.model
}

// Complete sends a non-streaming request and returns the full response.
func (p *OllamaProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	payload := buildOllamaRequest(model, req, false, p.contextWindow)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(data))
	}

	var or ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&or); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	return &CompletionResponse{
		Content:    or.Message.Content,
		StopReason: "end_turn",
		Usage: TokenUsage{
			InputTokens:  or.PromptEvalCount,
			OutputTokens: or.EvalCount,
		},
		ProviderMeta: ProviderMeta{
			Provider: p.name,
			Model:    model,
			Latency:  time.Since(start),
		},
	}, nil
}

// Stream sends a streaming request and returns a channel of chunks.
// The channel closes when generation is complete or the context is cancelled.
func (p *OllamaProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	model := p.effectiveModel(req)
	payload := buildOllamaRequest(model, req, true, p.contextWindow)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal stream request: %w", err)
	}

	// Use a separate client without a timeout — streaming can be long.
	streamClient := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama: stream status %d: %s", resp.StatusCode, string(data))
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var chunk ollamaChatResponse
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				select {
				case ch <- StreamChunk{Error: fmt.Errorf("ollama: decode chunk: %w", err)}:
				case <-ctx.Done():
				}
				return
			}
			sc := StreamChunk{
				Delta: chunk.Message.Content,
				Done:  chunk.Done,
			}
			if chunk.Done {
				sc.Usage = &TokenUsage{
					InputTokens:  chunk.PromptEvalCount,
					OutputTokens: chunk.EvalCount,
				}
				sc.ProviderMeta = &ProviderMeta{
					Provider: p.name,
					Model:    model,
				}
			}
			select {
			case ch <- sc:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case ch <- StreamChunk{Error: fmt.Errorf("ollama: scan: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}
