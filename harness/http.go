// http.go handles inference via OpenAI-compatible HTTP APIs (OpenAI, OpenRouter,
// Ollama, and custom endpoints).
//
// These functions are called by Harness.RunInference and Harness.RunInferenceStream
// when ParseModelProvider returns a non-Claude provider. They build an OpenAI chat
// completion request, send it to the provider's /chat/completions endpoint, and
// parse the response (sync or SSE stream).
package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runHTTPInference executes a non-streaming inference request against an OpenAI-compatible API.
func runHTTPInference(req *InferenceRequest, providerType ProviderType, modelName string, customConfig *ProviderConfig) (*InferenceResponse, error) {
	// Get provider configuration
	var config *ProviderConfig
	if customConfig != nil {
		config = customConfig
	} else {
		providers := DefaultProviders()
		config = providers[providerType]
	}

	if config == nil {
		return nil, fmt.Errorf("no configuration for provider: %s", providerType)
	}
	// Only require API key for providers that need it (not Ollama, Local, or Custom)
	if config.APIKey == "" {
		switch providerType {
		case ProviderOpenAI:
			return nil, fmt.Errorf("API key not set for provider %s (set OPENAI_API_KEY)", providerType)
		case ProviderOpenRouter:
			return nil, fmt.Errorf("API key not set for provider %s (set OPENROUTER_API_KEY)", providerType)
			// Ollama, Local, and Custom don't require API keys
		}
	}

	// Build messages
	messages := []OpenAIChatMessage{}

	// Build system prompt: chain TAA context + client system prompt
	systemPrompt := chainSystemPrompt(req)
	if systemPrompt != "" {
		messages = append(messages, OpenAIChatMessage{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	// Add user prompt
	messages = append(messages, OpenAIChatMessage{
		Role:    "user",
		Content: req.Prompt,
	})

	// Build request body
	apiReq := OpenAIChatRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
	}
	if req.MaxTokens != nil {
		apiReq.MaxTokens = req.MaxTokens
	}

	jsonBody, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	url := config.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(req.Context, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if config.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if providerType == ProviderOpenRouter {
		httpReq.Header.Set("HTTP-Referer", "https://cogos.dev")
		httpReq.Header.Set("X-Title", "CogOS Kernel")
	}

	// Execute request
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for error status
	if resp.StatusCode != http.StatusOK {
		return &InferenceResponse{
			ID:           req.ID,
			Error:        fmt.Errorf("API error: %s", string(body)),
			ErrorMessage: string(body),
			ErrorType:    ClassifyHTTPError(resp.StatusCode),
		}, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	// Parse response
	var apiResp OpenAIChatResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Extract content
	content := ""
	finishReason := ""
	if len(apiResp.Choices) > 0 {
		content = apiResp.Choices[0].Message.Content
		finishReason = apiResp.Choices[0].FinishReason
	}

	return &InferenceResponse{
		ID:               req.ID,
		Content:          content,
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		FinishReason:     finishReason,
		ContextMetrics:   BuildContextMetrics(req.ContextState),
	}, nil
}

// runHTTPInferenceStream executes a streaming inference request against an OpenAI-compatible API
func runHTTPInferenceStream(req *InferenceRequest, providerType ProviderType, modelName string, customConfig *ProviderConfig) (<-chan StreamChunkInference, error) {
	// Get provider configuration
	var config *ProviderConfig
	if customConfig != nil {
		config = customConfig
	} else {
		providers := DefaultProviders()
		config = providers[providerType]
	}

	if config == nil {
		return nil, fmt.Errorf("no configuration for provider: %s", providerType)
	}
	// Only require API key for providers that need it
	if config.APIKey == "" {
		switch providerType {
		case ProviderOpenAI:
			return nil, fmt.Errorf("API key not set for provider %s (set OPENAI_API_KEY)", providerType)
		case ProviderOpenRouter:
			return nil, fmt.Errorf("API key not set for provider %s (set OPENROUTER_API_KEY)", providerType)
		}
	}

	// Build messages
	messages := []OpenAIChatMessage{}

	systemPrompt := chainSystemPrompt(req)
	if systemPrompt != "" {
		messages = append(messages, OpenAIChatMessage{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	messages = append(messages, OpenAIChatMessage{
		Role:    "user",
		Content: req.Prompt,
	})

	apiReq := OpenAIChatRequest{
		Model:         modelName,
		Messages:      messages,
		Stream:        true,
		StreamOptions: &StreamOptions{IncludeUsage: true},
	}
	if req.MaxTokens != nil {
		apiReq.MaxTokens = req.MaxTokens
	}

	jsonBody, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := config.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(req.Context, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if config.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	if providerType == ProviderOpenRouter {
		httpReq.Header.Set("HTTP-Referer", "https://cogos.dev")
		httpReq.Header.Set("X-Title", "CogOS Kernel")
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	chunks := make(chan StreamChunkInference, 100)

	go func() {
		defer close(chunks)
		defer resp.Body.Close()

		safeSend := func(chunk StreamChunkInference) bool {
			select {
			case chunks <- chunk:
				return true
			case <-req.Context.Done():
				return false
			}
		}

		reader := bufio.NewReader(resp.Body)
		for {
			select {
			case <-req.Context.Done():
				safeSend(StreamChunkInference{
					ID:    req.ID,
					Done:  true,
					Error: req.Context.Err(),
				})
				return
			default:
			}

			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					safeSend(StreamChunkInference{
						ID:           req.ID,
						Done:         true,
						FinishReason: "stop",
					})
				} else {
					safeSend(StreamChunkInference{
						ID:    req.ID,
						Done:  true,
						Error: err,
					})
				}
				return
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				safeSend(StreamChunkInference{
					ID:           req.ID,
					Done:         true,
					FinishReason: "stop",
				})
				return
			}

			var chunk OpenAIStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// Usage-only chunk (empty choices, usage present) —
			// emitted by providers that support stream_options.include_usage.
			if len(chunk.Choices) == 0 && chunk.Usage != nil {
				safeSend(StreamChunkInference{
					ID:   req.ID,
					Done: true,
					Usage: &UsageData{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
					},
					FinishReason: "stop",
				})
				return
			}

			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.Content != "" {
					if !safeSend(StreamChunkInference{
						ID:      req.ID,
						Content: delta.Content,
						Done:    false,
					}) {
						return
					}
				}
				if chunk.Choices[0].FinishReason != "" {
					// Build usage if present on the finish chunk
					var usage *UsageData
					if chunk.Usage != nil {
						usage = &UsageData{
							InputTokens:  chunk.Usage.PromptTokens,
							OutputTokens: chunk.Usage.CompletionTokens,
						}
					}
					safeSend(StreamChunkInference{
						ID:           req.ID,
						Done:         true,
						FinishReason: chunk.Choices[0].FinishReason,
						Usage:        usage,
					})
					return
				}
			}
		}
	}()

	return chunks, nil
}
