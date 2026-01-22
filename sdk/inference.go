package sdk

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/types"
)

// inferenceProjector handles cog://inference namespace.
// Provides the SDK interface to the Claude CLI inference engine.
type inferenceProjector struct {
	BaseProjector
	kernel *Kernel
	config *types.InferenceConfig
}

// CanMutate returns true - inference accepts requests.
func (p *inferenceProjector) CanMutate() bool {
	return true
}

// Resolve returns inference engine status.
//
// URIs:
//
//	cog://inference           -> Engine status
//	cog://inference/status    -> Detailed status
//	cog://inference/config    -> Current configuration
func (p *inferenceProjector) Resolve(ctx context.Context, uri *ParsedURI) (*Resource, error) {
	switch uri.Path {
	case "", "status":
		return p.resolveStatus(uri)
	case "config":
		return p.resolveConfig(uri)
	default:
		return nil, NotFoundError("Resolve", uri.Raw)
	}
}

// resolveStatus returns the inference engine status.
func (p *inferenceProjector) resolveStatus(uri *ParsedURI) (*Resource, error) {
	status := &types.InferenceStatus{
		Available: p.isClaudeAvailable(),
	}

	// Check if Claude CLI is installed
	claudePath, err := exec.LookPath(p.getClaudeCommand())
	if err == nil {
		status.ClaudeInstalled = true
		status.ClaudePath = claudePath
		status.Available = true
	}

	return NewJSONResource(uri.Raw, status)
}

// resolveConfig returns the current configuration.
func (p *inferenceProjector) resolveConfig(uri *ParsedURI) (*Resource, error) {
	config := p.config
	if config == nil {
		config = types.DefaultInferenceConfig()
	}
	return NewJSONResource(uri.Raw, config)
}

// Mutate processes an inference request.
//
// The mutation content should be a JSON-encoded InferenceRequest.
//
// Example:
//
//	req := types.InferenceRequest{
//	    Prompt: "What is 2+2?",
//	    Model: "sonnet",
//	}
//	data, _ := json.Marshal(req)
//	mutation := sdk.NewSetMutation(data)
//	kernel.Mutate("cog://inference", mutation)
func (p *inferenceProjector) Mutate(ctx context.Context, uri *ParsedURI, m *Mutation) error {
	if m.Op != MutationSet {
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("only 'set' operation supported for inference"))
	}

	// Parse the request
	var req types.InferenceRequest
	if err := json.Unmarshal(m.Content, &req); err != nil {
		return NewURIError("Mutate", uri.Raw, fmt.Errorf("invalid request: %w", err))
	}

	// Execute inference
	resp, err := p.runInference(ctx, &req)
	if err != nil {
		return NewURIError("Mutate", uri.Raw, err)
	}

	// Store response in metadata for retrieval
	if m.Metadata == nil {
		m.Metadata = make(map[string]any)
	}
	m.Metadata["response"] = resp

	return nil
}

// runInference executes an inference request using Claude CLI.
func (p *inferenceProjector) runInference(ctx context.Context, req *types.InferenceRequest) (*types.InferenceResponse, error) {
	startTime := time.Now()

	// Ensure ID is set
	if req.ID == "" {
		req.ID = p.generateRequestID(req.Origin)
	}

	// Resolve context URIs if any
	var contextContent string
	if len(req.Context) > 0 {
		contextContent = p.resolveContextURIs(ctx, req.Context)
	}

	// Build Claude CLI arguments
	args := p.buildClaudeArgs(req, contextContent)

	// Create command with context for cancellation
	cmd := exec.CommandContext(ctx, p.getClaudeCommand(), args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start Claude: %w", err)
	}

	// Collect output
	var content strings.Builder
	var promptTokens, completionTokens int
	var finishReason string

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var claudeMsg claudeStreamMessage
		if err := json.Unmarshal([]byte(line), &claudeMsg); err != nil {
			continue
		}

		p.processStreamMessage(&claudeMsg, &content, &promptTokens, &completionTokens, &finishReason)
	}

	// Wait for process to complete
	waitErr := cmd.Wait()

	// Check for context cancellation
	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("request cancelled")
	}

	// Build response
	response := &types.InferenceResponse{
		ID:           req.ID,
		Content:      content.String(),
		Model:        types.ResolveModelAlias(req.Model),
		InputTokens:  promptTokens,
		OutputTokens: completionTokens,
		StopReason:   finishReason,
		FinishReason: finishReason,
		Duration:     time.Since(startTime),
		Timestamp:    time.Now(),
	}

	if waitErr != nil {
		response.Error = waitErr.Error()
		response.ErrorType = string(p.classifyError(waitErr))
	}

	return response, waitErr
}

// buildClaudeArgs constructs the Claude CLI arguments.
func (p *inferenceProjector) buildClaudeArgs(req *types.InferenceRequest, contextContent string) []string {
	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}

	// Add system prompt (including context)
	systemPrompt := req.SystemPrompt
	if contextContent != "" {
		if systemPrompt != "" {
			systemPrompt = contextContent + "\n\n---\n\n" + systemPrompt
		} else {
			systemPrompt = contextContent
		}
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}

	// Add JSON schema if requested
	if len(req.Schema) > 0 {
		args = append(args, "--json-schema", string(req.Schema))
	}

	// Resolve and add model
	model := req.Model
	if model != "" {
		resolvedModel := types.ResolveModelAlias(model)
		if resolvedModel != "claude" {
			args = append(args, "--model", resolvedModel)
		}
	}

	// Add max tokens if specified
	if req.MaxTokens > 0 {
		args = append(args, "--max-tokens", fmt.Sprintf("%d", req.MaxTokens))
	}

	// Add tool restrictions
	if len(req.AllowedTools) > 0 {
		args = append(args, "--allowed-tools", strings.Join(req.AllowedTools, ","))
	}
	if len(req.DisallowedTools) > 0 {
		args = append(args, "--disallowed-tools", strings.Join(req.DisallowedTools, ","))
	}

	return args
}

// resolveContextURIs resolves context URIs and joins their content.
func (p *inferenceProjector) resolveContextURIs(ctx context.Context, uris []string) string {
	var parts []string

	for _, uri := range uris {
		resource, err := p.kernel.ResolveContext(ctx, uri)
		if err != nil {
			continue
		}
		if len(resource.Content) > 0 {
			parts = append(parts, string(resource.Content))
		}
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// processStreamMessage extracts content from Claude stream messages.
func (p *inferenceProjector) processStreamMessage(msg *claudeStreamMessage, content *strings.Builder, promptTokens, completionTokens *int, finishReason *string) {
	switch msg.Type {
	case "assistant":
		if msg.Message != nil {
			for _, c := range msg.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						content.WriteString(c.Text)
					}
				case "tool_use":
					if c.Name == "StructuredOutput" && len(c.Input) > 0 {
						content.Write(c.Input)
					}
				}
			}
			if msg.Message.Usage != nil {
				if msg.Message.Usage.InputTokens > 0 {
					*promptTokens = msg.Message.Usage.InputTokens
				}
				if msg.Message.Usage.OutputTokens > 0 {
					*completionTokens = msg.Message.Usage.OutputTokens
				}
			}
			if msg.Message.StopReason != "" {
				*finishReason = msg.Message.StopReason
			}
		}
	case "result":
		if msg.Usage != nil {
			if msg.Usage.InputTokens > 0 {
				*promptTokens = msg.Usage.InputTokens
			}
			if msg.Usage.OutputTokens > 0 {
				*completionTokens = msg.Usage.OutputTokens
			}
		}
		if content.Len() == 0 && len(msg.StructuredOutput) > 0 {
			content.Write(msg.StructuredOutput)
		}
		if content.Len() == 0 && msg.Result != "" {
			content.WriteString(msg.Result)
		}
		*finishReason = "stop"
	}
}

// classifyError determines the error type for retry logic.
func (p *inferenceProjector) classifyError(err error) types.InferenceErrorType {
	if err == nil {
		return types.ErrorNone
	}
	errMsg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(errMsg, "429") || strings.Contains(errMsg, "rate limit"):
		return types.ErrorRateLimit
	case strings.Contains(errMsg, "context_length") || strings.Contains(errMsg, "too long"):
		return types.ErrorContextOverflow
	case strings.Contains(errMsg, "auth") || strings.Contains(errMsg, "401"):
		return types.ErrorAuth
	case strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "connection"):
		return types.ErrorTransient
	default:
		return types.ErrorTransient
	}
}

// generateRequestID creates a unique request ID.
func (p *inferenceProjector) generateRequestID(origin string) string {
	if origin == "" {
		origin = "sdk"
	}
	ts := time.Now().Unix()
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	return fmt.Sprintf("req-%s-%d-%s", origin, ts, hex.EncodeToString(randomBytes))
}

// getClaudeCommand returns the Claude CLI command name.
func (p *inferenceProjector) getClaudeCommand() string {
	if p.config != nil && p.config.ClaudeCommand != "" {
		return p.config.ClaudeCommand
	}
	return "claude"
}

// isClaudeAvailable checks if Claude CLI is available.
func (p *inferenceProjector) isClaudeAvailable() bool {
	_, err := exec.LookPath(p.getClaudeCommand())
	return err == nil
}

// claudeStreamMessage represents Claude CLI stream-json output.
type claudeStreamMessage struct {
	Type             string            `json:"type"`
	Message          *claudeMessage    `json:"message,omitempty"`
	Usage            *claudeUsage      `json:"usage,omitempty"`
	Result           string            `json:"result,omitempty"`
	StructuredOutput json.RawMessage   `json:"structured_output,omitempty"`
}

type claudeMessage struct {
	Content    []claudeContent `json:"content"`
	Usage      *claudeUsage    `json:"usage,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
}

type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Infer is a convenience function to run inference with the SDK.
// This provides a simpler interface than using Mutate directly.
func (k *Kernel) Infer(ctx context.Context, prompt string, opts ...InferenceOption) (*types.InferenceResponse, error) {
	req := &types.InferenceRequest{
		Prompt: prompt,
		Origin: "sdk",
	}

	for _, opt := range opts {
		opt(req)
	}

	// Get the inference projector
	proj := k.GetProjector("inference")
	if proj == nil {
		return nil, NewError("Infer", ErrUnknownNamespace)
	}

	infProj, ok := proj.(*inferenceProjector)
	if !ok {
		return nil, NewError("Infer", fmt.Errorf("invalid inference projector type"))
	}

	return infProj.runInference(ctx, req)
}

// InferenceOption configures an inference request.
type InferenceOption func(*types.InferenceRequest)

// WithModel sets the model for inference.
func WithModel(model string) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.Model = model
	}
}

// WithMaxTokens sets the maximum tokens.
func WithMaxTokens(maxTokens int) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.MaxTokens = maxTokens
	}
}

// WithContext sets context URIs for inference.
func WithContext(uris ...string) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.Context = uris
	}
}

// WithSystemPrompt sets the system prompt.
func WithSystemPrompt(prompt string) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.SystemPrompt = prompt
	}
}

// WithSchema sets the JSON schema for structured output.
func WithSchema(schema json.RawMessage) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.Schema = schema
	}
}

// WithOrigin sets the request origin.
func WithOrigin(origin string) InferenceOption {
	return func(r *types.InferenceRequest) {
		r.Origin = origin
	}
}
