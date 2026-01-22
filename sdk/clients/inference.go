package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/types"
)

// InferenceClient provides ergonomic access to cog://inference
//
// This client wraps the inference projector, which invokes the Claude CLI
// for LLM completions. It supports:
//   - Single prompts with Complete()
//   - Streaming responses with Stream()
//   - Chat-style multi-turn with Chat()
//   - Context injection from cog:// URIs
//
// All methods are goroutine-safe.
type InferenceClient struct {
	kernel      *sdk.Kernel
	model       string
	maxTokens   int
	temperature float64
	contextURIs []string
}

// NewInferenceClient creates a new InferenceClient with default settings.
func NewInferenceClient(k *sdk.Kernel) *InferenceClient {
	return &InferenceClient{
		kernel: k,
	}
}

// WithModel returns a new InferenceClient with the specified model.
// Model can be an alias (sonnet, opus, haiku) or a full model ID.
//
// Example:
//
//	client := c.Inference.WithModel("sonnet")
//	resp, err := client.Complete("What is 2+2?")
func (c *InferenceClient) WithModel(model string) *InferenceClient {
	clone := *c
	clone.model = model
	return &clone
}

// WithMaxTokens returns a new InferenceClient with the specified max tokens.
func (c *InferenceClient) WithMaxTokens(n int) *InferenceClient {
	clone := *c
	clone.maxTokens = n
	return &clone
}

// WithTemperature returns a new InferenceClient with the specified temperature.
// Temperature should be in the range [0.0, 1.0].
func (c *InferenceClient) WithTemperature(t float64) *InferenceClient {
	clone := *c
	clone.temperature = t
	return &clone
}

// WithContext returns a new InferenceClient that includes the specified URIs as context.
// URIs are resolved and included in the system prompt.
//
// Example:
//
//	client := c.Inference.WithContext("cog://mem/semantic/insights/eigenform", "cog://identity")
//	resp, err := client.Complete("Summarize the eigenform concept.")
func (c *InferenceClient) WithContext(uris ...string) *InferenceClient {
	clone := *c
	clone.contextURIs = append([]string{}, uris...)
	return &clone
}

// Complete sends a prompt and returns the completion.
//
// Example:
//
//	resp, err := c.Inference.Complete("What is the eigenform?")
//	fmt.Println(resp.Content)
func (c *InferenceClient) Complete(prompt string) (*types.InferenceResponse, error) {
	return c.CompleteContext(context.Background(), prompt)
}

// CompleteContext is like Complete but accepts a context.
func (c *InferenceClient) CompleteContext(ctx context.Context, prompt string) (*types.InferenceResponse, error) {
	req := c.buildRequest(prompt)

	content, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	uri := "cog://inference"
	mutation := sdk.NewSetMutation(content)

	if err := c.kernel.MutateContext(ctx, uri, mutation); err != nil {
		return nil, err
	}

	// Resolve the response
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var resp types.InferenceResponse
	if err := json.Unmarshal(resource.Content, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &resp, nil
}

// CompleteSync is an alias for Complete (synchronous completion).
func (c *InferenceClient) CompleteSync(prompt string) (*types.InferenceResponse, error) {
	return c.Complete(prompt)
}

// Stream sends a prompt and returns a channel of streaming chunks.
// The channel is closed when the response is complete or an error occurs.
//
// Example:
//
//	chunks, err := c.Inference.Stream("Tell me a story.")
//	if err != nil {
//	    return err
//	}
//	for chunk := range chunks {
//	    if chunk.Error != "" {
//	        return errors.New(chunk.Error)
//	    }
//	    fmt.Print(chunk.Content)
//	}
func (c *InferenceClient) Stream(prompt string) (<-chan types.StreamChunk, error) {
	return c.StreamContext(context.Background(), prompt)
}

// StreamContext is like Stream but accepts a context.
func (c *InferenceClient) StreamContext(ctx context.Context, prompt string) (<-chan types.StreamChunk, error) {
	req := c.buildRequest(prompt)
	req.Stream = true

	content, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	uri := "cog://inference?stream=true"
	mutation := sdk.NewSetMutation(content)

	if err := c.kernel.MutateContext(ctx, uri, mutation); err != nil {
		return nil, err
	}

	// Create a channel for streaming
	chunks := make(chan types.StreamChunk, 100)

	// Start a goroutine to read chunks
	go func() {
		defer close(chunks)

		// Poll for chunks (the actual streaming is handled by the projector)
		// In a full implementation, this would use Watch or a streaming endpoint
		resource, err := c.kernel.ResolveContext(ctx, uri)
		if err != nil {
			chunks <- types.StreamChunk{
				Error: err.Error(),
				Done:  true,
			}
			return
		}

		// Parse as a response and convert to a single chunk
		var resp types.InferenceResponse
		if err := json.Unmarshal(resource.Content, &resp); err != nil {
			chunks <- types.StreamChunk{
				Error: err.Error(),
				Done:  true,
			}
			return
		}

		// Send the full content as one chunk (in a real implementation,
		// this would be multiple chunks from the streaming API)
		chunks <- types.StreamChunk{
			ID:           resp.ID,
			Content:      resp.Content,
			Done:         true,
			FinishReason: resp.StopReason,
		}
	}()

	return chunks, nil
}

// Chat sends a multi-turn conversation and returns the assistant response.
//
// Example:
//
//	messages := []types.Message{
//	    *types.NewUserMessage("What is 2+2?"),
//	    *types.NewAssistantMessage("2+2 equals 4."),
//	    *types.NewUserMessage("And what is 4+4?"),
//	}
//	resp, err := c.Inference.Chat(messages)
func (c *InferenceClient) Chat(messages []types.Message) (*types.InferenceResponse, error) {
	return c.ChatContext(context.Background(), messages)
}

// ChatContext is like Chat but accepts a context.
func (c *InferenceClient) ChatContext(ctx context.Context, messages []types.Message) (*types.InferenceResponse, error) {
	// Convert messages to a chat-formatted prompt
	// This is a simplified approach - a full implementation would use the API's native chat format
	prompt := ""
	for _, msg := range messages {
		switch msg.Role {
		case types.MessageRoleUser:
			prompt += fmt.Sprintf("Human: %s\n\n", msg.Content)
		case types.MessageRoleAssistant:
			prompt += fmt.Sprintf("Assistant: %s\n\n", msg.Content)
		case types.MessageRoleSystem:
			prompt += fmt.Sprintf("System: %s\n\n", msg.Content)
		}
	}

	// Add final assistant prompt
	prompt += "Assistant: "

	return c.CompleteContext(ctx, prompt)
}

// Status returns the current inference engine status.
//
// Example:
//
//	status, err := c.Inference.Status()
//	fmt.Printf("Available: %v, Active requests: %d\n", status.Available, status.ActiveRequests)
func (c *InferenceClient) Status() (*types.InferenceStatus, error) {
	return c.StatusContext(context.Background())
}

// StatusContext is like Status but accepts a context.
func (c *InferenceClient) StatusContext(ctx context.Context) (*types.InferenceStatus, error) {
	resource, err := c.kernel.ResolveContext(ctx, "cog://inference?status=true")
	if err != nil {
		return nil, err
	}

	var status types.InferenceStatus
	if err := json.Unmarshal(resource.Content, &status); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}

	return &status, nil
}

// IsAvailable returns true if the inference engine is ready.
func (c *InferenceClient) IsAvailable() bool {
	status, err := c.Status()
	if err != nil {
		return false
	}
	return status.Available
}

// buildRequest constructs an InferenceRequest from client settings.
func (c *InferenceClient) buildRequest(prompt string) *types.InferenceRequest {
	req := &types.InferenceRequest{
		Prompt: prompt,
	}

	if c.model != "" {
		req.Model = c.model
	}
	if c.maxTokens > 0 {
		req.MaxTokens = c.maxTokens
	}
	if c.temperature > 0 {
		req.Temperature = c.temperature
	}
	if len(c.contextURIs) > 0 {
		req.Context = c.contextURIs
	}

	return req
}

// Ask is a simple convenience method for single-prompt completion.
// Returns just the content string.
//
// Example:
//
//	answer, err := c.Inference.Ask("What is the meaning of life?")
func (c *InferenceClient) Ask(prompt string) (string, error) {
	resp, err := c.Complete(prompt)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", fmt.Errorf("inference error: %s", resp.Error)
	}
	return resp.Content, nil
}

// AskWithContext resolves URIs and includes them as context for the question.
//
// Example:
//
//	answer, err := c.Inference.AskWithContext("Summarize this.", "cog://mem/semantic/insights/eigenform")
func (c *InferenceClient) AskWithContext(prompt string, uris ...string) (string, error) {
	return c.WithContext(uris...).Ask(prompt)
}
