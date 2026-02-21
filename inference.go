// Inference Module - Shared inference engine with request registry
//
// This module provides:
// - InferenceRequest/Response types for unified inference interface
// - RequestRegistry for tracking in-flight requests
// - RunInference/RunInferenceStream for executing Claude CLI
//
// Used by both serve.go (HTTP server) and cog.go (CLI infer command)

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// === INFERENCE REQUEST/RESPONSE TYPES ===

// ErrorType classifies inference errors for smart recovery
type ErrorType int

const (
	ErrorNone            ErrorType = iota
	ErrorRateLimit                 // 429 - retry with backoff
	ErrorContextOverflow           // Context too long - compress and retry
	ErrorAuth                      // Authentication failure - fail fast
	ErrorTransient                 // Transient failure - retry with backoff
	ErrorFatal                     // Fatal error - don't retry
)

// String returns human-readable error type
func (e ErrorType) String() string {
	switch e {
	case ErrorNone:
		return "none"
	case ErrorRateLimit:
		return "rate_limit"
	case ErrorContextOverflow:
		return "context_overflow"
	case ErrorAuth:
		return "auth"
	case ErrorTransient:
		return "transient"
	case ErrorFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// classifyError determines the error type from an error message
func classifyError(err error) ErrorType {
	if err == nil {
		return ErrorNone
	}
	errMsg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(errMsg, "429") || strings.Contains(errMsg, "rate limit") || strings.Contains(errMsg, "too many requests"):
		return ErrorRateLimit
	case strings.Contains(errMsg, "context_length") || strings.Contains(errMsg, "context length") || strings.Contains(errMsg, "too long"):
		return ErrorContextOverflow
	case strings.Contains(errMsg, "auth") || strings.Contains(errMsg, "unauthorized") || strings.Contains(errMsg, "401"):
		return ErrorAuth
	case strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "connection"):
		return ErrorTransient
	default:
		return ErrorTransient // Default to transient for unknown errors
	}
}

// === CONTEXT STATE (Four-Tier Context Pipeline) ===

// ContextTier represents a single tier of context with metadata
type ContextTier struct {
	Content string `json:"content"`
	Tokens  int    `json:"tokens"`
	Source  string `json:"source,omitempty"`
}

// ContextState represents the full context.cog.json structure
// Used for context-aware invocation of the inference engine
type ContextState struct {
	// Tier 1: Identity (stable, ~1/3 of budget)
	Tier1Identity *ContextTier `json:"tier1_identity,omitempty"`

	// Tier 2: Temporal (session state, signals, history)
	Tier2Temporal *ContextTier `json:"tier2_temporal,omitempty"`

	// Tier 3: Present (current message context)
	Tier3Present *ContextTier `json:"tier3_present,omitempty"`

	// Tier 4: Semantic (constellation knowledge graph)
	Tier4Semantic *ContextTier `json:"tier4_semantic,omitempty"`

	// Model selection (optional override)
	Model string `json:"model,omitempty"`

	// Metadata
	TotalTokens    int     `json:"total_tokens,omitempty"`
	CoherenceScore float64 `json:"coherence_score,omitempty"`
	ShouldRefresh  bool    `json:"should_refresh,omitempty"`

	// TAA signals (from Tier 2 temporal analysis)
	Anchor string `json:"anchor,omitempty"` // Current conversation topic
	Goal   string `json:"goal,omitempty"`   // Detected user intent
}

// BuildContextString assembles the full context string from tiers
func (cs *ContextState) BuildContextString() string {
	if cs == nil {
		return ""
	}

	var parts []string

	// Add tiers in order
	if cs.Tier1Identity != nil && cs.Tier1Identity.Content != "" {
		parts = append(parts, cs.Tier1Identity.Content)
	}
	if cs.Tier2Temporal != nil && cs.Tier2Temporal.Content != "" {
		parts = append(parts, cs.Tier2Temporal.Content)
	}
	if cs.Tier3Present != nil && cs.Tier3Present.Content != "" {
		parts = append(parts, cs.Tier3Present.Content)
	}
	if cs.Tier4Semantic != nil && cs.Tier4Semantic.Content != "" {
		parts = append(parts, cs.Tier4Semantic.Content)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// chainSystemPrompt combines TAA context and client system prompt into a single
// header chain, separated by ---. TAA context comes first (identity, temporal,
// present, semantic), followed by any client-provided system instructions.
func chainSystemPrompt(req *InferenceRequest) string {
	var taaBlock string
	if req.ContextState != nil {
		taaBlock = req.ContextState.BuildContextString()
	}
	switch {
	case taaBlock != "" && req.SystemPrompt != "":
		return taaBlock + "\n\n---\n\n" + req.SystemPrompt
	case taaBlock != "":
		return taaBlock
	default:
		return req.SystemPrompt
	}
}

// ContextMetrics captures metrics about context used in inference
type ContextMetrics struct {
	TotalTokens     int            `json:"total_tokens"`
	TierBreakdown   map[string]int `json:"tier_breakdown"`
	CoherenceScore  float64        `json:"coherence_score"`
	CompressionUsed bool           `json:"compression_used"`
}

// InferenceRequest represents input to the inference engine
type InferenceRequest struct {
	ID           string          // Unique request ID (auto-generated if empty)
	Prompt       string          // User prompt
	SystemPrompt string          // Optional system prompt
	Model        string          // Model to use (empty = default)
	Schema       json.RawMessage // Optional JSON schema for structured output
	MaxTokens    *int            // Optional max tokens
	Origin       string          // Where request came from: "cli", "http", "hook", "fleet"
	Stream       bool            // Whether to stream
	Context      context.Context // For cancellation

	// Context pipeline (new)
	ContextState *ContextState // Four-tier context for context-aware invocation

	// Tool definitions
	Tools        []json.RawMessage // OpenAI-format tool definitions from client
	AllowedTools []string          // Claude CLI --allowed-tools patterns (e.g. "Bash", "Bash(git:*)")

	// Workspace override — when set, Claude CLI runs in this directory
	// instead of the kernel's workspace. Used for per-request workspace
	// targeting (e.g., OpenClaw workspace via UCP).
	WorkspaceRoot string

	// MCP bridge configuration
	MCPConfig     string // Path to generated --mcp-config JSON file
	OpenClawURL   string // OpenClaw gateway URL for bridge proxy
	OpenClawToken string // Auth token for OpenClaw
	SessionID     string // Session context for tool execution

	// Retry configuration
	MaxRetries int           // Max retry attempts (0 = use default)
	Timeout    time.Duration // Request timeout (0 = use default)
}

// InferenceResponse represents output from the inference engine
type InferenceResponse struct {
	ID               string `json:"id"`
	Content          string `json:"content"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	FinishReason     string `json:"finish_reason"`
	Error            error  `json:"-"`
	ErrorMessage     string `json:"error,omitempty"`

	// Context metrics (new - from context pipeline)
	ContextMetrics *ContextMetrics `json:"context_metrics,omitempty"`

	// Error classification (new - for smart recovery)
	ErrorType ErrorType `json:"error_type,omitempty"`
}

// StreamChunkInference represents a single chunk in a streaming response
type StreamChunkInference struct {
	ID           string `json:"id"`
	Content      string `json:"content"`
	Done         bool   `json:"done"`
	FinishReason string `json:"finish_reason,omitempty"`
	Error        error  `json:"-"`

	// Rich streaming fields
	EventType   string          `json:"event_type,omitempty"`   // text, tool_use, tool_result
	ToolCall    *ToolCallData   `json:"tool_call,omitempty"`    // Tool call information
	ToolResult  *ToolResultData `json:"tool_result,omitempty"`  // Tool result information
	Usage       *UsageData      `json:"usage,omitempty"`        // Token usage data
	SessionInfo *SessionInfo    `json:"session_info,omitempty"` // Session metadata
}

// ToolCallData represents a tool call in streaming
type ToolCallData struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResultData represents a tool result in streaming
type ToolResultData struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

// UsageData represents token usage in streaming
type UsageData struct {
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	CacheReadTokens   int     `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int     `json:"cache_create_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
}

// SessionInfo represents session metadata in streaming
type SessionInfo struct {
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools,omitempty"`
}

// === PROVIDER TYPES ===

// ProviderType identifies the inference provider
type ProviderType string

const (
	ProviderClaude     ProviderType = "claude"     // Claude CLI (default)
	ProviderOpenAI     ProviderType = "openai"     // OpenAI API
	ProviderOpenRouter ProviderType = "openrouter" // OpenRouter API
	ProviderOllama     ProviderType = "ollama"     // Ollama (local)
	ProviderLocal      ProviderType = "local"      // Local kernel endpoint (self-reference)
	ProviderCustom     ProviderType = "custom"     // Any OpenAI-compatible endpoint
)

// ProviderConfig holds configuration for an inference provider
type ProviderConfig struct {
	Type    ProviderType `json:"type"`
	BaseURL string       `json:"base_url"`
	APIKey  string       `json:"api_key"`
	Model   string       `json:"model"` // Default model for this provider
}

// DefaultProviders returns the default provider configurations
// API keys are read from environment variables
func DefaultProviders() map[ProviderType]*ProviderConfig {
	// Ollama port can be customized via OLLAMA_HOST
	ollamaHost := os.Getenv("OLLAMA_HOST")
	if ollamaHost == "" {
		ollamaHost = "http://localhost:11434"
	}

	// Local kernel port
	localPort := os.Getenv("COG_KERNEL_PORT")
	if localPort == "" {
		localPort = "5100"
	}

	return map[ProviderType]*ProviderConfig{
		ProviderOpenAI: {
			Type:    ProviderOpenAI,
			BaseURL: "https://api.openai.com/v1",
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			Model:   "gpt-4o-mini",
		},
		ProviderOpenRouter: {
			Type:    ProviderOpenRouter,
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  os.Getenv("OPENROUTER_API_KEY"),
			Model:   "anthropic/claude-3-haiku",
		},
		ProviderOllama: {
			Type:    ProviderOllama,
			BaseURL: ollamaHost + "/v1", // Ollama's OpenAI-compatible endpoint
			APIKey:  "",                 // Ollama doesn't require API key
			Model:   "llama3.2",
		},
		ProviderLocal: {
			Type:    ProviderLocal,
			BaseURL: "http://localhost:" + localPort + "/v1",
			APIKey:  "",       // Local kernel doesn't require API key
			Model:   "claude", // Route to Claude by default
		},
	}
}

// ParseModelProvider extracts the provider and model from a model string
// Formats:
//   - "claude" or "" -> (ProviderClaude, "claude")
//   - "openai/gpt-4o" -> (ProviderOpenAI, "gpt-4o")
//   - "openrouter/anthropic/claude-3-haiku" -> (ProviderOpenRouter, "anthropic/claude-3-haiku")
//   - "ollama/llama3.2" -> (ProviderOllama, "llama3.2")
//   - "local/claude" -> (ProviderLocal, "claude") - routes through local kernel
//   - "http://localhost:8080|model-name" -> (ProviderCustom, model with custom URL)
func ParseModelProvider(model string) (ProviderType, string, *ProviderConfig) {
	if model == "" || model == "claude" {
		return ProviderClaude, "claude", nil
	}

	// Check for URL-based custom provider
	if strings.HasPrefix(model, "http://") || strings.HasPrefix(model, "https://") {
		// Format: "http://localhost:8080|model-name"
		parts := strings.SplitN(model, "|", 2)
		baseURL := parts[0]
		modelName := ""
		if len(parts) > 1 {
			modelName = parts[1]
		}
		return ProviderCustom, modelName, &ProviderConfig{
			Type:    ProviderCustom,
			BaseURL: baseURL,
			Model:   modelName,
		}
	}

	// Check for prefixed providers
	if strings.HasPrefix(model, "openai/") {
		return ProviderOpenAI, strings.TrimPrefix(model, "openai/"), nil
	}
	if strings.HasPrefix(model, "openrouter/") {
		return ProviderOpenRouter, strings.TrimPrefix(model, "openrouter/"), nil
	}
	if strings.HasPrefix(model, "ollama/") {
		return ProviderOllama, strings.TrimPrefix(model, "ollama/"), nil
	}
	if strings.HasPrefix(model, "local/") {
		return ProviderLocal, strings.TrimPrefix(model, "local/"), nil
	}

	// Default to Claude CLI for anything else
	return ProviderClaude, model, nil
}

// === OPENAI-COMPATIBLE API TYPES ===

// OpenAIChatRequest is the request format for OpenAI-compatible APIs
type OpenAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []OpenAIChatMessage `json:"messages"`
	MaxTokens   *int                `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stream      bool                `json:"stream"`
}

// OpenAIChatMessage is a single message in the chat
type OpenAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAIChatResponse is the response format for OpenAI-compatible APIs
type OpenAIChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// OpenAIStreamChunk is a single chunk in a streaming response
type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

// === REQUEST REGISTRY ===

// RequestEntry represents a tracked request in the registry
type RequestEntry struct {
	ID      string             `json:"id"`
	Origin  string             `json:"origin"`
	Model   string             `json:"model"`
	Started time.Time          `json:"started"`
	Status  string             `json:"status"` // "running", "completed", "cancelled", "failed"
	Cancel  context.CancelFunc `json:"-"`
	Prompt  string             `json:"prompt,omitempty"` // First 100 chars for display
}

// RequestRegistry tracks in-flight inference requests
type RequestRegistry struct {
	mu       sync.RWMutex
	requests map[string]*RequestEntry
}

// NewRequestRegistry creates a new request registry
func NewRequestRegistry() *RequestRegistry {
	return &RequestRegistry{
		requests: make(map[string]*RequestEntry),
	}
}

// Register adds a new request to the registry
func (r *RequestRegistry) Register(req *InferenceRequest, cancel context.CancelFunc) *RequestEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Generate ID if not provided
	if req.ID == "" {
		req.ID = generateRequestID(req.Origin)
	}

	// Truncate prompt for display
	promptPreview := req.Prompt
	if len(promptPreview) > 100 {
		promptPreview = promptPreview[:100] + "..."
	}

	entry := &RequestEntry{
		ID:      req.ID,
		Origin:  req.Origin,
		Model:   req.Model,
		Started: time.Now(),
		Status:  "running",
		Cancel:  cancel,
		Prompt:  promptPreview,
	}

	r.requests[req.ID] = entry
	return entry
}

// Complete marks a request as completed with given status
func (r *RequestRegistry) Complete(id string, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.requests[id]; ok {
		entry.Status = status
	}
}

// Cancel cancels a request by ID, returns true if found and cancelled
func (r *RequestRegistry) Cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.requests[id]; ok {
		if entry.Cancel != nil {
			entry.Cancel()
		}
		entry.Status = "cancelled"
		return true
	}
	return false
}

// Get retrieves a request entry by ID (returns a copy to prevent data races)
func (r *RequestRegistry) Get(id string) *RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.requests[id]; ok {
		entryCopy := *entry
		return &entryCopy
	}
	return nil
}

// List returns all request entries (copies to prevent data races)
func (r *RequestRegistry) List() []RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]RequestEntry, 0, len(r.requests))
	for _, entry := range r.requests {
		entries = append(entries, *entry)
	}
	return entries
}

// ListRunning returns only running request entries (copies to prevent data races)
func (r *RequestRegistry) ListRunning() []RequestEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]RequestEntry, 0)
	for _, entry := range r.requests {
		if entry.Status == "running" {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// Remove removes a request from the registry
func (r *RequestRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.requests, id)
}

// Cleanup removes completed/failed/cancelled requests older than duration
func (r *RequestRegistry) Cleanup(maxAge time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	count := 0

	for id, entry := range r.requests {
		if entry.Status != "running" && entry.Started.Before(cutoff) {
			delete(r.requests, id)
			count++
		}
	}
	return count
}

// === ID GENERATION ===

// generateRequestID creates a unique request ID with format: req-{origin}-{timestamp}-{random}
func generateRequestID(origin string) string {
	if origin == "" {
		origin = "unknown"
	}

	// Timestamp component (compact)
	ts := time.Now().Unix()

	// Random component
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	randomHex := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("req-%s-%d-%s", origin, ts, randomHex)
}

// === INFERENCE EXECUTION ===

// Default retry configuration
const (
	DefaultMaxRetries = 3
	DefaultTimeout    = 2 * time.Minute
	BaseRetryDelay    = time.Second
)

// buildClaudeArgs constructs the Claude CLI arguments from an InferenceRequest
// Supports both legacy mode (SystemPrompt) and new context-aware mode (ContextState)
func buildClaudeArgs(req *InferenceRequest) []string {
	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--include-partial-messages", // Enable rich streaming with content_block_delta events
		"--verbose",
		"--dangerously-skip-permissions", // Allow tool execution without prompts
	}

	// Build system prompt: chain TAA context + client system prompt
	if sp := chainSystemPrompt(req); sp != "" {
		args = append(args, "--append-system-prompt", sp)
	}

	// Use request-level schema if provided
	var schema json.RawMessage
	if len(req.Schema) > 0 {
		schema = req.Schema
	}

	// Add JSON schema if requested
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}

	// Determine model source
	// Priority: ContextState.Model > req.Model
	model := req.Model
	if req.ContextState != nil && req.ContextState.Model != "" {
		model = req.ContextState.Model
	}

	// Map model IDs to Claude CLI aliases
	// Claude CLI expects: "opus", "sonnet", or full model names like "claude-sonnet-4-5-20250929"
	if model != "" && model != "claude" {
		// Map common model IDs to aliases
		switch model {
		case "claude-opus-4-5-20250929", "opus-4-5", "opus":
			model = "opus"
		case "claude-sonnet-4-5-20250929", "sonnet-4-5", "sonnet":
			model = "sonnet"
		}
		args = append(args, "--model", model)
	}

	// Note: Claude CLI doesn't have a max-tokens option
	// The max_tokens parameter from the request is ignored for Claude CLI
	// but may be used by other providers (OpenAI, OpenRouter)

	// Forward tool control to Claude CLI
	if len(req.AllowedTools) > 0 {
		// Explicit allowed-tools list takes priority
		args = append(args, "--allowed-tools", strings.Join(req.AllowedTools, ","))
	} else if len(req.Tools) > 0 {
		// Map OpenAI-format tool definitions to Claude CLI tool names
		if mapped := mapToolsToCLINames(req.Tools); len(mapped) > 0 {
			args = append(args, "--allowed-tools", strings.Join(mapped, ","))
		}
	}

	// MCP bridge configuration
	if req.MCPConfig != "" {
		args = append(args, "--mcp-config", req.MCPConfig)
	}

	return args
}

// generateMCPConfig creates a temporary MCP config JSON file for Claude CLI's --mcp-config flag.
// The config tells Claude CLI to spawn `cog mcp serve --bridge` as an MCP server,
// enabling access to both CogOS and OpenClaw tools.
func generateMCPConfig(req *InferenceRequest) (string, error) {
	if req.OpenClawURL == "" {
		return "", fmt.Errorf("OpenClawURL required for MCP bridge")
	}

	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	_, span := tracer.Start(ctx, "mcp.config.generate",
		trace.WithAttributes(
			attribute.String("openclaw.url", req.OpenClawURL),
			attribute.Int("tools.count", len(req.Tools)),
		),
	)
	defer span.End()

	// Find the cog binary path
	cogBin, err := os.Executable()
	if err != nil {
		cogBin = "cog" // Fallback to PATH lookup
	}

	// Resolve workspace root for COG_ROOT env var
	root, _, err := ResolveWorkspace()
	if err != nil {
		span.RecordError(err)
		return "", fmt.Errorf("resolve workspace: %w", err)
	}

	env := map[string]string{
		"COG_ROOT":       root,
		"OPENCLAW_URL":   req.OpenClawURL,
		"OPENCLAW_TOKEN": req.OpenClawToken,
		"SESSION_ID":     req.SessionID,
	}

	// Propagate trace context to the bridge subprocess via TRACEPARENT env var.
	// This enables distributed tracing: the bridge creates child spans linked
	// to the parent trace from serve.go's HTTP handler.
	spanCtx := span.SpanContext()
	if spanCtx.IsValid() {
		traceparent := fmt.Sprintf("00-%s-%s-%s",
			spanCtx.TraceID().String(),
			spanCtx.SpanID().String(),
			spanCtx.TraceFlags().String(),
		)
		env["TRACEPARENT"] = traceparent
		span.SetAttributes(attribute.String("traceparent", traceparent))
	}

	// Pass OTEL exporter endpoint so the bridge subprocess can also export traces
	if otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); otelEndpoint != "" {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = otelEndpoint
	}

	// Convert OpenAI-format tools from request body into MCP format for the bridge.
	// This is the push path: tools declared by the caller flow into the MCP bridge
	// via the TOOL_REGISTRY env var, rather than the bridge pulling from an endpoint.
	if len(req.Tools) > 0 {
		mcpTools := convertOpenAIToolsToMCP(req.Tools)
		if len(mcpTools) > 0 {
			toolsJSON, err := json.Marshal(mcpTools)
			if err != nil {
				log.Printf("[MCP] Warning: failed to serialize tool registry: %v", err)
			} else {
				env["TOOL_REGISTRY"] = string(toolsJSON)
				log.Printf("[MCP] Tool registry: %d tools from request body", len(mcpTools))
				span.SetAttributes(attribute.Int("tools.registry_count", len(mcpTools)))
			}
		}
	}

	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"cogos-bridge": map[string]interface{}{
				"command": cogBin,
				"args":    []string{"mcp", "serve", "--bridge"},
				"env":     env,
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "cog-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write config: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("close config: %w", err)
	}

	log.Printf("[MCP] Generated bridge config: %s (cog=%s, url=%s, session=%s)", tmpFile.Name(), cogBin, req.OpenClawURL, req.SessionID)
	return tmpFile.Name(), nil
}

// mapToolsToCLINames extracts function names from OpenAI-format tool definitions
// and maps them to Claude CLI built-in tool names where possible.
func mapToolsToCLINames(tools []json.RawMessage) []string {
	var result []string
	seen := make(map[string]bool)

	for _, raw := range tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Function.Name == "" {
			continue
		}

		cliName := mapToolName(tool.Function.Name)
		if cliName == "" {
			log.Printf("[tools] Unknown tool name %q — skipping", tool.Function.Name)
			continue
		}
		if !seen[cliName] {
			seen[cliName] = true
			result = append(result, cliName)
		}
	}
	return result
}

// mapToolName maps an OpenAI-format tool function name to a Claude CLI tool name.
func mapToolName(name string) string {
	switch strings.ToLower(name) {
	case "exec", "bash", "shell":
		return "Bash"
	case "read", "file_read":
		return "Read"
	case "write", "file_write":
		return "Write"
	case "edit", "apply-patch", "apply_patch":
		return "Edit"
	case "search", "grep":
		return "Grep"
	case "glob", "find":
		return "Glob"
	default:
		return ""
	}
}

// buildContextMetrics extracts metrics from ContextState for response
func buildContextMetrics(ctx *ContextState) *ContextMetrics {
	if ctx == nil {
		return nil
	}

	tierBreakdown := make(map[string]int)
	totalTokens := 0

	if ctx.Tier1Identity != nil {
		tierBreakdown["tier1_identity"] = ctx.Tier1Identity.Tokens
		totalTokens += ctx.Tier1Identity.Tokens
	}
	if ctx.Tier2Temporal != nil {
		tierBreakdown["tier2_temporal"] = ctx.Tier2Temporal.Tokens
		totalTokens += ctx.Tier2Temporal.Tokens
	}
	if ctx.Tier3Present != nil {
		tierBreakdown["tier3_present"] = ctx.Tier3Present.Tokens
		totalTokens += ctx.Tier3Present.Tokens
	}

	// Use provided total if available, otherwise use computed
	if ctx.TotalTokens > 0 {
		totalTokens = ctx.TotalTokens
	}

	return &ContextMetrics{
		TotalTokens:     totalTokens,
		TierBreakdown:   tierBreakdown,
		CoherenceScore:  ctx.CoherenceScore,
		CompressionUsed: false, // Set by caller if compression was applied
	}
}

// === HTTP INFERENCE (OpenAI-Compatible APIs) ===

// runHTTPInference executes a non-streaming inference request against an OpenAI-compatible API
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
			ErrorType:    classifyHTTPError(resp.StatusCode),
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
		ContextMetrics:   buildContextMetrics(req.ContextState),
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
		Stream:   true,
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
	httpReq.Header.Set("Accept", "text/event-stream")
	if config.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if providerType == ProviderOpenRouter {
		httpReq.Header.Set("HTTP-Referer", "https://cogos.dev")
		httpReq.Header.Set("X-Title", "CogOS Kernel")
	}

	// Execute request
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Check for error status
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	// Create output channel
	chunks := make(chan StreamChunkInference, 100)

	// Process stream in goroutine — producer owns the channel close
	go func() {
		defer close(chunks)
		defer resp.Body.Close()

		// safeSend sends a chunk to the channel, respecting context cancellation.
		// Returns false if the context was cancelled and the goroutine should exit.
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
			// Check for cancellation
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
					safeSend(StreamChunkInference{
						ID:           req.ID,
						Done:         true,
						FinishReason: chunk.Choices[0].FinishReason,
					})
					return
				}
			}
		}
	}()

	return chunks, nil
}

// classifyHTTPError maps HTTP status codes to ErrorType
func classifyHTTPError(statusCode int) ErrorType {
	switch {
	case statusCode == 401 || statusCode == 403:
		return ErrorAuth
	case statusCode == 429:
		return ErrorRateLimit
	case statusCode >= 500:
		return ErrorTransient
	default:
		return ErrorFatal
	}
}

// === DYNAMIC CONTEXT INJECTION ===

// ContinuationState represents the eigenfield continuation state
type ContinuationState struct {
	SessionID          string `json:"session_id"`
	Timestamp          string `json:"timestamp"`
	Trigger            string `json:"trigger"`
	Focus              string `json:"focus"`
	ContinuationPrompt string `json:"continuation_prompt"`
}

// readContinuationState reads the continuation state for eigenfield persistence
func readContinuationState() (*ContinuationState, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return nil, err
	}

	continuationFile := filepath.Join(root, ".cog", "run", "continuation.json")
	data, err := os.ReadFile(continuationFile)
	if err != nil {
		return nil, err // File doesn't exist or unreadable
	}

	var state ContinuationState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// injectContinuationContext modifies the request to include continuation context
// This enables eigenfield persistence across compaction
func injectContinuationContext(req *InferenceRequest) {
	// Only inject for certain origins (not for internal/hook requests)
	if req.Origin == "hook" || req.Origin == "internal" {
		return
	}

	state, err := readContinuationState()
	if err != nil {
		// No continuation state available - this is fine
		return
	}

	// Only inject if there's a continuation prompt
	if state.ContinuationPrompt == "" {
		return
	}

	// Prepend continuation context to system prompt
	continuationContext := fmt.Sprintf("[Eigenfield Continuation] %s\n\n", state.ContinuationPrompt)

	if req.SystemPrompt == "" {
		req.SystemPrompt = continuationContext
	} else {
		req.SystemPrompt = continuationContext + req.SystemPrompt
	}
}

// RunInference executes a non-streaming inference request
// Routes to appropriate provider based on model prefix:
//   - "claude" or "" -> Claude CLI (default)
//   - "openai/..." -> OpenAI API
//   - "openrouter/..." -> OpenRouter API
//   - "http://..." -> Custom OpenAI-compatible endpoint
func RunInference(req *InferenceRequest, registry *RequestRegistry) (*InferenceResponse, error) {
	// Inject continuation context for eigenfield persistence
	injectContinuationContext(req)

	// Ensure context is set with a timeout to prevent indefinite hangs
	if req.Context == nil {
		var cancel context.CancelFunc
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		req.Context, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	}

	// Ensure ID is set early for consistent tracking
	if req.ID == "" {
		req.ID = generateRequestID(req.Origin)
	}

	// Dispatch PreInference hooks (allows context injection, blocking)
	preInferenceData := map[string]interface{}{
		"request_id":    req.ID,
		"prompt":        req.Prompt,
		"system_prompt": req.SystemPrompt,
		"model":         req.Model,
		"origin":        req.Origin,
	}
	if hookResult := dispatch("PreInference", "", preInferenceData); hookResult != nil {
		// Check if hook wants to block
		if hookResult.Decision == "block" {
			return nil, fmt.Errorf("inference blocked by hook: %s", hookResult.Message)
		}
		// Check if hook injected additional context
		if hookResult.AdditionalContext != "" {
			if req.SystemPrompt == "" {
				req.SystemPrompt = hookResult.AdditionalContext
			} else {
				req.SystemPrompt = hookResult.AdditionalContext + "\n\n" + req.SystemPrompt
			}
		}
	}

	// Parse provider from model string
	providerType, modelName, customConfig := ParseModelProvider(req.Model)

	// Route to HTTP providers for non-Claude models
	if providerType != ProviderClaude {
		// Track start time
		startTime := time.Now()

		// Emit start event
		emitInferenceStart(req)
		setInferenceActiveSignal(req.ID, modelName, req.Origin)

		// Register request if registry provided
		ctx, cancel := context.WithCancel(req.Context)
		defer cancel()
		req.Context = ctx

		if registry != nil {
			registry.Register(req, cancel)
		}

		// Execute HTTP inference
		resp, err := runHTTPInference(req, providerType, modelName, customConfig)

		// Update registry and emit events
		if registry != nil {
			if err != nil {
				registry.Complete(req.ID, "failed")
				emitInferenceError(req.ID, err.Error())
			} else {
				registry.Complete(req.ID, "completed")
				emitInferenceComplete(req, resp, startTime)
			}
		}
		clearInferenceActiveSignal()

		return resp, err
	}

	// === CLAUDE CLI PATH (default) ===

	// Track start time for duration calculation
	startTime := time.Now()

	// Set up context and cancellation
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// OTEL: top-level inference span
	ctx, span := tracer.Start(ctx, "inference.sync",
		trace.WithAttributes(
			attribute.String("model", req.Model),
			attribute.String("origin", req.Origin),
			attribute.Int("tool_count", len(req.Tools)),
		),
	)
	defer span.End()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register the request
	var entry *RequestEntry
	if registry != nil {
		entry = registry.Register(req, cancel)
		defer func() {
			// Update status based on result
			if entry.Status == "running" {
				registry.Complete(req.ID, "completed")
			}
		}()
	}

	// Emit INFERENCE_START event and set signal
	emitInferenceStart(req)
	setInferenceActiveSignal(req.ID, modelName, req.Origin)

	// Build Claude CLI arguments
	args := buildClaudeArgs(req)

	// OTEL: child span for CLI execution
	_, cliSpan := tracer.Start(ctx, "claude.cli.exec",
		trace.WithAttributes(
			attribute.Int("arg_count", len(args)),
		),
	)

	// Create command with context for cancellation
	cmd := exec.CommandContext(ctx, claudeCommand, args...)

	// Set working directory for Claude CLI.
	// Priority: request-specific workspace (e.g., OpenClaw workspace via UCP)
	// → DEFAULT_CLIENT_WORKSPACE env var (for callers that don't send UCP)
	// → kernel workspace (ResolveWorkspace) → inherit from parent process.
	if req.WorkspaceRoot != "" {
		cmd.Dir = req.WorkspaceRoot
	} else if defaultWs := os.Getenv("DEFAULT_CLIENT_WORKSPACE"); defaultWs != "" {
		cmd.Dir = defaultWs
	} else if wsRoot, _, wsErr := ResolveWorkspace(); wsErr == nil {
		cmd.Dir = wsRoot
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if registry != nil {
			registry.Complete(req.ID, "failed")
		}
		emitInferenceError(req.ID, "failed to create stdout pipe: "+err.Error())
		clearInferenceActiveSignal()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Capture stderr for better error messages
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		stdout.Close() // Close the pipe to prevent resource leak
		if registry != nil {
			registry.Complete(req.ID, "failed")
		}
		emitInferenceError(req.ID, "failed to start Claude: "+err.Error())
		clearInferenceActiveSignal()
		return nil, fmt.Errorf("failed to start Claude: %w", err)
	}

	// Collect output
	var content strings.Builder
	var promptTokens, completionTokens int
	var finishReason string

	// Debug: capture raw stream if COG_DEBUG_INFERENCE is set
	debugFile := os.Getenv("COG_DEBUG_INFERENCE")
	var debugWriter *os.File
	if debugFile != "" {
		if f, err := os.OpenFile(debugFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			debugWriter = f
			defer debugWriter.Close()
			fmt.Fprintf(debugWriter, "\n=== Inference Request %s ===\n", req.ID)
		}
	}

	scanner := bufio.NewScanner(stdout)
	// Increase buffer size to handle large Claude outputs (e.g., extended thinking blocks)
	// Default is 64KB, increase to 1MB
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Write raw line to debug file
		if debugWriter != nil {
			fmt.Fprintf(debugWriter, "%s\n", line)
		}

		var claudeMsg ClaudeStreamMessage
		if err := json.Unmarshal([]byte(line), &claudeMsg); err != nil {
			continue
		}

		switch claudeMsg.Type {
		case "assistant":
			// Extract content from nested message structure
			if claudeMsg.Message != nil {
				for _, c := range claudeMsg.Message.Content {
					switch c.Type {
					case "text":
						if c.Text != "" {
							content.WriteString(c.Text)
						}
					case "tool_use":
						// JSON schema output comes as tool_use with StructuredOutput
						if c.Name == "StructuredOutput" && len(c.Input) > 0 {
							content.Write(c.Input)
						}
					}
				}
				// Get usage from message
				if claudeMsg.Message.Usage != nil {
					if claudeMsg.Message.Usage.InputTokens > 0 {
						promptTokens = claudeMsg.Message.Usage.InputTokens
					}
					if claudeMsg.Message.Usage.OutputTokens > 0 {
						completionTokens = claudeMsg.Message.Usage.OutputTokens
					}
				}
				if claudeMsg.Message.StopReason != "" {
					finishReason = claudeMsg.Message.StopReason
				}
			}
		case "result":
			// Final result message - get usage from top level
			if claudeMsg.Usage != nil {
				if claudeMsg.Usage.InputTokens > 0 {
					promptTokens = claudeMsg.Usage.InputTokens
				}
				if claudeMsg.Usage.OutputTokens > 0 {
					completionTokens = claudeMsg.Usage.OutputTokens
				}
			}
			// Prefer structured_output for JSON schema responses
			if content.Len() == 0 && len(claudeMsg.StructuredOutput) > 0 {
				content.Write(claudeMsg.StructuredOutput)
			}
			// Fallback to result text if no content extracted
			if content.Len() == 0 && claudeMsg.Result != "" {
				content.WriteString(claudeMsg.Result)
			}
			finishReason = "stop"
		}
	}

	// Wait for process to complete
	waitErr := cmd.Wait()

	// OTEL: end CLI span with exit code
	if waitErr != nil {
		cliSpan.SetAttributes(attribute.Int("exit_code", 1))
	} else {
		cliSpan.SetAttributes(attribute.Int("exit_code", 0))
	}
	cliSpan.End()

	// OTEL: record token counts on the parent inference span
	span.SetAttributes(
		attribute.Int("tokens.input", promptTokens),
		attribute.Int("tokens.output", completionTokens),
	)

	// Check for context cancellation
	if ctx.Err() == context.Canceled {
		if registry != nil {
			registry.Complete(req.ID, "cancelled")
		}
		emitInferenceError(req.ID, "request cancelled")
		clearInferenceActiveSignal()
		return nil, fmt.Errorf("request cancelled")
	}

	// Build response
	response := &InferenceResponse{
		ID:               req.ID,
		Content:          content.String(),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		FinishReason:     finishReason,
		ContextMetrics:   buildContextMetrics(req.ContextState),
	}

	if waitErr != nil {
		if registry != nil {
			registry.Complete(req.ID, "failed")
		}
		// Include stderr in error message for better debugging
		errMsg := waitErr.Error()
		if stderrBuf.Len() > 0 {
			errMsg = fmt.Sprintf("%s: %s", errMsg, strings.TrimSpace(stderrBuf.String()))
		}
		response.Error = waitErr
		response.ErrorMessage = errMsg
		response.ErrorType = classifyError(waitErr)
		emitInferenceError(req.ID, errMsg)
		clearInferenceActiveSignal()
	} else {
		// Emit success event
		emitInferenceComplete(req, response, startTime)
		clearInferenceActiveSignal()

		// Dispatch PostInference hooks (for artifact extraction, logging)
		postInferenceData := map[string]interface{}{
			"request_id":        req.ID,
			"prompt":            req.Prompt,
			"response":          response.Content,
			"model":             req.Model,
			"origin":            req.Origin,
			"prompt_tokens":     response.PromptTokens,
			"completion_tokens": response.CompletionTokens,
		}
		dispatch("PostInference", "", postInferenceData)
	}

	return response, waitErr
}

// RunInferenceWithRetry executes inference with automatic retry for transient errors
// Implements exponential backoff for rate limits and transient failures
// Does not retry for auth errors or fatal errors
func RunInferenceWithRetry(req *InferenceRequest, registry *RequestRegistry) (*InferenceResponse, error) {
	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}

	var lastResp *InferenceResponse
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Clone request ID for retries to avoid duplicate IDs
		if attempt > 0 {
			req.ID = generateRequestID(req.Origin + "-retry")
		}

		resp, err := RunInference(req, registry)

		// Success - return immediately
		if err == nil && (resp.Error == nil || resp.ErrorMessage == "") {
			return resp, nil
		}

		lastResp = resp
		lastErr = err

		// Classify the error
		var errType ErrorType
		if err != nil {
			errType = classifyError(err)
		} else if resp != nil && resp.ErrorMessage != "" {
			errType = classifyError(fmt.Errorf("%s", resp.ErrorMessage))
		}

		// Don't retry for auth or fatal errors
		if errType == ErrorAuth || errType == ErrorFatal {
			return resp, err
		}

		// Don't retry on last attempt
		if attempt == maxRetries-1 {
			break
		}

		// Calculate delay with exponential backoff
		delay := BaseRetryDelay * time.Duration(1<<uint(attempt))

		// For rate limits, use longer delays
		if errType == ErrorRateLimit {
			delay = delay * 2
		}

		// Cap delay at 30 seconds
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}

		// Log retry attempt
		fmt.Fprintf(os.Stderr, "Inference retry %d/%d after %v (error type: %s)\n",
			attempt+1, maxRetries, delay, errType)

		// Wait before retry
		select {
		case <-time.After(delay):
			// Continue to next attempt
		case <-req.Context.Done():
			// Context cancelled during wait
			return lastResp, fmt.Errorf("cancelled during retry wait: %w", req.Context.Err())
		}
	}

	// All retries exhausted
	if lastErr != nil {
		return lastResp, fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
	}
	if lastResp != nil && lastResp.ErrorMessage != "" {
		return lastResp, fmt.Errorf("max retries (%d) exceeded: %s", maxRetries, lastResp.ErrorMessage)
	}
	return lastResp, fmt.Errorf("max retries (%d) exceeded", maxRetries)
}

// RunInferenceStream executes a streaming inference request
// Routes to appropriate provider based on model prefix:
//   - "claude" or "" -> Claude CLI (default)
//   - "openai/..." -> OpenAI API
//   - "openrouter/..." -> OpenRouter API
//   - "http://..." -> Custom OpenAI-compatible endpoint
// Returns a channel that receives chunks and closes when done
func RunInferenceStream(req *InferenceRequest, registry *RequestRegistry) (<-chan StreamChunkInference, error) {
	// Inject continuation context for eigenfield persistence
	injectContinuationContext(req)

	// timeoutCancel releases the timeout context created when req.Context is nil.
	// Defaults to no-op so all code paths can call it unconditionally.
	timeoutCancel := func() {}
	if req.Context == nil {
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		var cancel context.CancelFunc
		req.Context, cancel = context.WithTimeout(context.Background(), timeout)
		timeoutCancel = cancel
	}

	// Ensure ID is set early for consistent tracking
	if req.ID == "" {
		req.ID = generateRequestID(req.Origin)
	}

	// Dispatch PreInference hooks (allows context injection, blocking)
	preInferenceData := map[string]interface{}{
		"request_id":    req.ID,
		"prompt":        req.Prompt,
		"system_prompt": req.SystemPrompt,
		"model":         req.Model,
		"origin":        req.Origin,
	}
	if hookResult := dispatch("PreInference", "", preInferenceData); hookResult != nil {
		// Check if hook wants to block
		if hookResult.Decision == "block" {
			timeoutCancel()
			return nil, fmt.Errorf("inference blocked by hook: %s", hookResult.Message)
		}
		// Check if hook injected additional context
		if hookResult.AdditionalContext != "" {
			if req.SystemPrompt == "" {
				req.SystemPrompt = hookResult.AdditionalContext
			} else {
				req.SystemPrompt = hookResult.AdditionalContext + "\n\n" + req.SystemPrompt
			}
		}
	}

	// Parse provider from model string
	providerType, modelName, customConfig := ParseModelProvider(req.Model)

	// Route to HTTP providers for non-Claude models
	if providerType != ProviderClaude {
		// Emit start event
		emitInferenceStart(req)
		setInferenceActiveSignal(req.ID, modelName, req.Origin)

		// Set up context and cancellation
		ctx, cancel := context.WithCancel(req.Context)
		req.Context = ctx

		// Register request if registry provided
		if registry != nil {
			registry.Register(req, cancel)
		}

		// Execute HTTP streaming inference
		chunks, err := runHTTPInferenceStream(req, providerType, modelName, customConfig)
		if err != nil {
			cancel()
			timeoutCancel()
			if registry != nil {
				registry.Complete(req.ID, "failed")
			}
			emitInferenceError(req.ID, err.Error())
			clearInferenceActiveSignal()
			return nil, err
		}

		// Wrap channel to handle cleanup with context-aware sends
		wrappedChunks := make(chan StreamChunkInference, 100)
		go func() {
			defer close(wrappedChunks)
			defer cancel()
			defer timeoutCancel()
			defer clearInferenceActiveSignal()

			for {
				select {
				case <-ctx.Done():
					// Consumer went away or context cancelled — stop forwarding
					if registry != nil {
						registry.Complete(req.ID, "cancelled")
					}
					return
				case chunk, ok := <-chunks:
					if !ok {
						// Inner channel closed
						return
					}
					// Use select to avoid blocking if consumer is gone
					select {
					case wrappedChunks <- chunk:
					case <-ctx.Done():
						if registry != nil {
							registry.Complete(req.ID, "cancelled")
						}
						return
					}
					if chunk.Done {
						if registry != nil {
							if chunk.Error != nil {
								registry.Complete(req.ID, "failed")
							} else {
								registry.Complete(req.ID, "completed")
							}
						}
						return
					}
				}
			}
		}()

		return wrappedChunks, nil
	}

	// === CLAUDE CLI PATH (default) ===

	// Track start time for duration calculation
	startTime := time.Now()

	// Set up context and cancellation
	ctx := req.Context

	// OTEL: top-level inference span
	ctx, span := tracer.Start(ctx, "inference.stream",
		trace.WithAttributes(
			attribute.String("model", req.Model),
			attribute.String("origin", req.Origin),
			attribute.Int("tool_count", len(req.Tools)),
		),
	)

	ctx, cancel := context.WithCancel(ctx)

	// Register the request
	var entry *RequestEntry
	if registry != nil {
		entry = registry.Register(req, cancel)
	}

	// Emit INFERENCE_START event and set signal
	emitInferenceStart(req)
	setInferenceActiveSignal(req.ID, modelName, req.Origin)

	// Build Claude CLI arguments
	args := buildClaudeArgs(req)

	// OTEL: child span for CLI execution
	_, cliSpan := tracer.Start(ctx, "claude.cli.exec",
		trace.WithAttributes(
			attribute.Int("arg_count", len(args)),
		),
	)

	// Create command with context for cancellation
	cmd := exec.CommandContext(ctx, claudeCommand, args...)

	// Set working directory for Claude CLI.
	// Priority: request-specific workspace (e.g., OpenClaw workspace via UCP)
	// → DEFAULT_CLIENT_WORKSPACE env var (for callers that don't send UCP)
	// → kernel workspace (ResolveWorkspace) → inherit from parent process.
	if req.WorkspaceRoot != "" {
		cmd.Dir = req.WorkspaceRoot
	} else if defaultWs := os.Getenv("DEFAULT_CLIENT_WORKSPACE"); defaultWs != "" {
		cmd.Dir = defaultWs
	} else if wsRoot, _, wsErr := ResolveWorkspace(); wsErr == nil {
		cmd.Dir = wsRoot
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cliSpan.End()
		span.End()
		cancel()
		timeoutCancel()
		if registry != nil {
			registry.Complete(req.ID, "failed")
		}
		emitInferenceError(req.ID, "failed to create stdout pipe: "+err.Error())
		clearInferenceActiveSignal()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdout.Close() // Close the pipe to prevent resource leak
		cliSpan.End()
		span.End()
		cancel()
		timeoutCancel()
		if registry != nil {
			registry.Complete(req.ID, "failed")
		}
		emitInferenceError(req.ID, "failed to start Claude: "+err.Error())
		clearInferenceActiveSignal()
		return nil, fmt.Errorf("failed to start Claude: %w", err)
	}

	// Create output channel
	chunks := make(chan StreamChunkInference, 100)

	// Process output in goroutine
	go func() {
		defer close(chunks)
		defer cancel()
		defer timeoutCancel()
		defer span.End()
		defer cliSpan.End() // safe: OTEL End() is idempotent

		// safeSend sends a chunk to the channel, respecting context cancellation.
		// Returns false if the context was cancelled and the goroutine should exit.
		safeSend := func(chunk StreamChunkInference) bool {
			select {
			case chunks <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Track token counts for completion event
		var promptTokens, completionTokens int
		var cacheReadTokens, cacheCreateTokens int
		var costUSD float64
		var finishReason string
		var fullContent strings.Builder // Accumulate for PostInference hook

		// Track active tool calls for rich streaming
		activeToolCalls := make(map[int]*ToolCallData) // index -> tool call
		var sessionID, sessionModel string
		var sessionTools []string

		scanner := bufio.NewScanner(stdout)
		// Increase buffer for large tool results (default 64KB is too small for file reads)
		const maxScannerSize = 4 * 1024 * 1024 // 4MB
		scanner.Buffer(make([]byte, maxScannerSize), maxScannerSize)

		gotContent := false
		gotStreamContent := false // Track if we received content via stream_event (to avoid duplicates from assistant messages)
		for scanner.Scan() {
			// Check for cancellation
			select {
			case <-ctx.Done():
				if registry != nil {
					registry.Complete(req.ID, "cancelled")
				}
				emitInferenceError(req.ID, "request cancelled")
				clearInferenceActiveSignal()
				safeSend(StreamChunkInference{
					ID:    req.ID,
					Done:  true,
					Error: ctx.Err(),
				})
				return
			default:
			}

			line := scanner.Text()
			if line == "" {
				continue
			}

			// First, try to parse as a generic message to check the type
			var msgType struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(line), &msgType); err != nil {
				continue
			}

			// Handle stream_event type for rich streaming (--include-partial-messages)
			if msgType.Type == "stream_event" {
				var streamEvent struct {
					Type  string          `json:"type"`
					Event json.RawMessage `json:"event"`
				}
				if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
					continue
				}

				// Parse the inner event
				var eventData struct {
					Type         string `json:"type"`
					Index        int    `json:"index,omitempty"`
					ContentBlock *struct {
						Type  string          `json:"type"`
						Text  string          `json:"text,omitempty"`
						ID    string          `json:"id,omitempty"`
						Name  string          `json:"name,omitempty"`
						Input json.RawMessage `json:"input,omitempty"`
					} `json:"content_block,omitempty"`
					Delta *struct {
						Type        string `json:"type"`
						Text        string `json:"text,omitempty"`
						PartialJSON string `json:"partial_json,omitempty"`
					} `json:"delta,omitempty"`
					Message json.RawMessage `json:"message,omitempty"`
					Usage   *struct {
						InputTokens       int `json:"input_tokens,omitempty"`
						OutputTokens      int `json:"output_tokens,omitempty"`
						CacheReadTokens   int `json:"cache_read_input_tokens,omitempty"`
						CacheCreateTokens int `json:"cache_creation_input_tokens,omitempty"`
					} `json:"usage,omitempty"`
				}
				if err := json.Unmarshal(streamEvent.Event, &eventData); err != nil {
					continue
				}

				switch eventData.Type {
				case "content_block_start":
					// New content block starting
					if eventData.ContentBlock != nil {
						switch eventData.ContentBlock.Type {
						case "tool_use":
							// OTEL: record tool_use event on CLI span
							cliSpan.AddEvent("tool_use", trace.WithAttributes(
								attribute.String("tool.name", eventData.ContentBlock.Name),
							))
							// Track tool call start
							activeToolCalls[eventData.Index] = &ToolCallData{
								ID:        eventData.ContentBlock.ID,
								Name:      eventData.ContentBlock.Name,
								Arguments: json.RawMessage(""),
							}
							// Emit tool_use start event
							if !safeSend(StreamChunkInference{
								ID:        req.ID,
								EventType: "tool_use_start",
								ToolCall: &ToolCallData{
									ID:   eventData.ContentBlock.ID,
									Name: eventData.ContentBlock.Name,
								},
								Done: false,
							}) {
								return
							}
						}
					}

				case "content_block_delta":
					// Streaming delta content
					if eventData.Delta != nil {
						switch eventData.Delta.Type {
						case "text_delta":
							// Token-by-token text streaming
							if eventData.Delta.Text != "" {
								gotContent = true
								gotStreamContent = true // Mark that we're receiving content via stream_event
								fullContent.WriteString(eventData.Delta.Text)
								if !safeSend(StreamChunkInference{
									ID:        req.ID,
									Content:   eventData.Delta.Text,
									EventType: "text",
									Done:      false,
								}) {
									return
								}
							}
						case "input_json_delta":
							// Streaming tool call arguments
							if tc, ok := activeToolCalls[eventData.Index]; ok {
								// Append to arguments
								tc.Arguments = append(tc.Arguments, []byte(eventData.Delta.PartialJSON)...)
								// Emit partial tool call update
								if !safeSend(StreamChunkInference{
									ID:        req.ID,
									Content:   eventData.Delta.PartialJSON,
									EventType: "tool_use_delta",
									ToolCall: &ToolCallData{
										ID:        tc.ID,
										Name:      tc.Name,
										Arguments: json.RawMessage(eventData.Delta.PartialJSON),
									},
									Done: false,
								}) {
									return
								}
							}
						}
					}

				case "content_block_stop":
					// Content block finished
					if tc, ok := activeToolCalls[eventData.Index]; ok {
						// Emit completed tool call
						if !safeSend(StreamChunkInference{
							ID:        req.ID,
							EventType: "tool_use",
							ToolCall:  tc,
							Done:      false,
						}) {
							return
						}
						delete(activeToolCalls, eventData.Index)
					}

				case "message_start":
					// Capture session info from message_start
					if len(eventData.Message) > 0 {
						var msgStart struct {
							ID    string `json:"id"`
							Model string `json:"model"`
							Usage *struct {
								InputTokens       int `json:"input_tokens,omitempty"`
								CacheReadTokens   int `json:"cache_read_input_tokens,omitempty"`
								CacheCreateTokens int `json:"cache_creation_input_tokens,omitempty"`
							} `json:"usage,omitempty"`
						}
						if err := json.Unmarshal(eventData.Message, &msgStart); err == nil {
							sessionID = msgStart.ID
							sessionModel = msgStart.Model
							if msgStart.Usage != nil {
								promptTokens = msgStart.Usage.InputTokens
								cacheReadTokens = msgStart.Usage.CacheReadTokens
								cacheCreateTokens = msgStart.Usage.CacheCreateTokens
							}
							// Emit session info
							if !safeSend(StreamChunkInference{
								ID:        req.ID,
								EventType: "session_start",
								SessionInfo: &SessionInfo{
									SessionID: sessionID,
									Model:     sessionModel,
									Tools:     sessionTools,
								},
								Done: false,
							}) {
								return
							}
						}
					}

				case "message_delta":
					// Message completion with usage
					if eventData.Usage != nil {
						completionTokens = eventData.Usage.OutputTokens
					}

				case "message_stop":
					// Message complete
					finishReason = "stop"
				}
				continue
			}

			// Handle system/init for session metadata
			if msgType.Type == "system" {
				var sysMsg struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype,omitempty"`
					Session *struct {
						ID    string   `json:"id"`
						Model string   `json:"model"`
						Tools []string `json:"tools,omitempty"`
					} `json:"session,omitempty"`
				}
				if err := json.Unmarshal([]byte(line), &sysMsg); err == nil {
					if sysMsg.Subtype == "init" && sysMsg.Session != nil {
						sessionID = sysMsg.Session.ID
						sessionModel = sysMsg.Session.Model
						sessionTools = sysMsg.Session.Tools
						// Emit session info
						if !safeSend(StreamChunkInference{
							ID:        req.ID,
							EventType: "session_info",
							SessionInfo: &SessionInfo{
								SessionID: sessionID,
								Model:     sessionModel,
								Tools:     sessionTools,
							},
							Done: false,
						}) {
							return
						}
					}
				}
				continue
			}

			// Fall back to original ClaudeStreamMessage parsing
			var claudeMsg ClaudeStreamMessage
			if err := json.Unmarshal([]byte(line), &claudeMsg); err != nil {
				continue
			}

			switch claudeMsg.Type {
			case "assistant":
				// Extract content from nested message structure
				if claudeMsg.Message != nil {
					for _, c := range claudeMsg.Message.Content {
						switch c.Type {
						case "text":
							// Skip if we already received this content via stream_event (avoid duplicates)
							if c.Text != "" && !gotStreamContent {
								gotContent = true
								fullContent.WriteString(c.Text) // Accumulate for PostInference
								if !safeSend(StreamChunkInference{
									ID:        req.ID,
									Content:   c.Text,
									EventType: "text",
									Done:      false,
								}) {
									return
								}
							}
						case "tool_use":
							// JSON schema output comes as tool_use with StructuredOutput
							if c.Name == "StructuredOutput" && len(c.Input) > 0 {
								gotContent = true
								fullContent.WriteString(string(c.Input)) // Accumulate for PostInference
								if !safeSend(StreamChunkInference{
									ID:        req.ID,
									Content:   string(c.Input),
									EventType: "text",
									Done:      false,
								}) {
									return
								}
							} else if c.Name != "" {
								// OTEL: record tool_use event on CLI span
								cliSpan.AddEvent("tool_use", trace.WithAttributes(
									attribute.String("tool.name", c.Name),
								))
								// Emit tool use event
								if !safeSend(StreamChunkInference{
									ID:        req.ID,
									EventType: "tool_use",
									ToolCall: &ToolCallData{
										ID:        c.ID,
										Name:      c.Name,
										Arguments: c.Input,
									},
									Done: false,
								}) {
									return
								}
							}
						}
					}
					// Capture usage from message
					if claudeMsg.Message.Usage != nil {
						if claudeMsg.Message.Usage.InputTokens > 0 {
							promptTokens = claudeMsg.Message.Usage.InputTokens
						}
						if claudeMsg.Message.Usage.OutputTokens > 0 {
							completionTokens = claudeMsg.Message.Usage.OutputTokens
						}
					}
					if claudeMsg.Message.StopReason != "" {
						finishReason = claudeMsg.Message.StopReason
					}
				}
			case "user":
				// Handle tool results - these come as user messages with tool_result content
				if claudeMsg.Message != nil {
					for _, c := range claudeMsg.Message.Content {
						if c.Type == "tool_result" && c.ToolUseID != "" {
							// OTEL: record tool_result event on CLI span
							cliSpan.AddEvent("tool_result", trace.WithAttributes(
								attribute.String("tool.id", c.ToolUseID),
							))
							if DebugMode.Load() {
								log.Printf("[DEBUG] Received tool_result for tool %s (isError=%v)", c.ToolUseID, c.IsError)
							}
							// Emit tool result event
							if !safeSend(StreamChunkInference{
								ID:        req.ID,
								EventType: "tool_result",
								ToolResult: &ToolResultData{
									ToolCallID: c.ToolUseID,
									Content:    c.Content,
									IsError:    c.IsError,
								},
								Done: false,
							}) {
								return
							}
						}
					}
				}
			case "result":
				// Capture usage from result - but DON'T emit Done yet!
				// In agentic mode, Claude may continue generating after tool results.
				// We'll emit Done only when the process actually exits.
				if DebugMode.Load() {
					log.Printf("[DEBUG] Received 'result' message from Claude CLI (NOT emitting Done)")
				}
				if claudeMsg.Usage != nil {
					if claudeMsg.Usage.InputTokens > 0 {
						promptTokens = claudeMsg.Usage.InputTokens
					}
					if claudeMsg.Usage.OutputTokens > 0 {
						completionTokens = claudeMsg.Usage.OutputTokens
					}
				}
				// Parse cost from result if available
				var resultMsg struct {
					CostUSD float64 `json:"cost_usd,omitempty"`
				}
				json.Unmarshal([]byte(line), &resultMsg)
				if resultMsg.CostUSD > 0 {
					costUSD = resultMsg.CostUSD
				}

				// Prefer structured_output for JSON schema responses
				if !gotContent && len(claudeMsg.StructuredOutput) > 0 {
					if !safeSend(StreamChunkInference{
						ID:        req.ID,
						Content:   string(claudeMsg.StructuredOutput),
						EventType: "text",
						Done:      false,
					}) {
						return
					}
					gotContent = true
				}
				// Fallback to result text if no content yet
				if !gotContent && claudeMsg.Result != "" {
					if !safeSend(StreamChunkInference{
						ID:        req.ID,
						Content:   claudeMsg.Result,
						EventType: "text",
						Done:      false,
					}) {
						return
					}
					gotContent = true
				}
				finishReason = "stop"
				// NOTE: Don't emit Done here - wait for process to exit
			}
		}

		// Check for scanner errors (e.g., buffer overflow)
		if err := scanner.Err(); err != nil {
			log.Printf("[ERROR] Scanner error while reading Claude CLI output: %v", err)
			emitInferenceError(req.ID, "scanner error: "+err.Error())
			clearInferenceActiveSignal()
			cliSpan.End()
			safeSend(StreamChunkInference{
				ID:    req.ID,
				Done:  true,
				Error: fmt.Errorf("scanner error: %w", err),
			})
			cmd.Process.Kill() // Clean up the process
			return
		}

		// Wait for process to complete
		if DebugMode.Load() {
			log.Printf("[DEBUG] Scanner loop finished, waiting for Claude CLI to exit...")
		}
		waitErr := cmd.Wait()
		if DebugMode.Load() {
			log.Printf("[DEBUG] Claude CLI exited (err=%v), will now emit Done chunk", waitErr)
		}

		// OTEL: end CLI span with exit code
		if waitErr != nil {
			cliSpan.SetAttributes(attribute.Int("exit_code", 1))
		} else {
			cliSpan.SetAttributes(attribute.Int("exit_code", 0))
		}
		cliSpan.End()

		// OTEL: record token counts on the parent inference span
		span.SetAttributes(
			attribute.Int("tokens.input", promptTokens),
			attribute.Int("tokens.output", completionTokens),
		)

		// Update registry status
		if registry != nil {
			if entry.Status == "running" {
				if waitErr != nil {
					registry.Complete(req.ID, "failed")
				} else {
					registry.Complete(req.ID, "completed")
				}
			}
		}

		// Send final chunk if not already sent
		if waitErr != nil {
			emitInferenceError(req.ID, waitErr.Error())
			clearInferenceActiveSignal()
			safeSend(StreamChunkInference{
				ID:    req.ID,
				Done:  true,
				Error: waitErr,
			})
		} else {
			// Emit completion event
			resp := &InferenceResponse{
				ID:               req.ID,
				Content:          fullContent.String(),
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				FinishReason:     finishReason,
			}
			emitInferenceComplete(req, resp, startTime)
			clearInferenceActiveSignal()

			// Emit final Done chunk with usage - NOW that process has exited
			safeSend(StreamChunkInference{
				ID:           req.ID,
				Done:         true,
				FinishReason: finishReason,
				Usage: &UsageData{
					InputTokens:       promptTokens,
					OutputTokens:      completionTokens,
					CacheReadTokens:   cacheReadTokens,
					CacheCreateTokens: cacheCreateTokens,
					CostUSD:           costUSD,
				},
			})

			// Dispatch PostInference hooks (for artifact extraction, logging)
			postInferenceData := map[string]interface{}{
				"request_id":        req.ID,
				"prompt":            req.Prompt,
				"response":          fullContent.String(),
				"model":             req.Model,
				"origin":            req.Origin,
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
			}
			dispatch("PostInference", "", postInferenceData)
		}
	}()

	return chunks, nil
}

// === GLOBAL REGISTRY ===

// GlobalRegistry is the shared registry for the serve module
var GlobalRegistry = NewRequestRegistry()

// StartRegistryCleanup starts a background goroutine that periodically
// removes completed/failed/cancelled entries older than 1 hour.
func StartRegistryCleanup() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			GlobalRegistry.Cleanup(1 * time.Hour)
		}
	}()
}

// === CLI COMMAND ===

// cmdInfer handles the "cog infer" command.
//
// Three modes:
//   --stateless          Zero bus interaction, like "claude -p". Nothing recorded, nothing read.
//   (default)            Records to bus (visible in peripheral context), but no TAA history loaded.
//   --profile <name>     Full continuity — bus history loaded into context assembly pipeline.
//
// --session <slug> names the conversation thread (default: "cli"). Multiple slugs
// give you parallel named conversations: --session debug, --session research, etc.
func cmdInfer(args []string) int {
	// Parse flags
	var (
		schemaPath   string
		systemPrompt string
		model        string
		jsonOutput   bool
		origin       string = "cli"
		prompt       string
		taaProfile   string
		contextURI   string
		sessionSlug  string
		stateless    bool
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--schema", "-s":
			if i+1 < len(args) {
				schemaPath = args[i+1]
				i++
			}
		case "--system":
			if i+1 < len(args) {
				systemPrompt = args[i+1]
				i++
			}
		case "--model", "-m":
			if i+1 < len(args) {
				model = args[i+1]
				i++
			}
		case "--profile", "-p":
			if i+1 < len(args) {
				taaProfile = args[i+1]
				i++
			}
		case "--context", "-c":
			if i+1 < len(args) {
				contextURI = args[i+1]
				i++
			}
		case "--session", "-S":
			if i+1 < len(args) {
				sessionSlug = args[i+1]
				i++
			}
		case "--stateless":
			stateless = true
		case "--json", "-j":
			jsonOutput = true
		case "--origin":
			if i+1 < len(args) {
				origin = args[i+1]
				i++
			}
		case "--help", "-h":
			printInferHelp()
			return 0
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", args[i])
				printInferHelp()
				return 1
			}
			prompt = args[i]
		}
	}

	if prompt == "" {
		fmt.Fprintf(os.Stderr, "Error: prompt is required\n")
		printInferHelp()
		return 1
	}

	// Check if claude CLI is available
	if _, err := exec.LookPath(claudeCommand); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Claude CLI not found in PATH\n")
		fmt.Fprintf(os.Stderr, "Install: npm install -g @anthropic-ai/claude-code\n")
		return 1
	}

	// Load schema if specified
	var schema json.RawMessage
	if schemaPath != "" {
		data, err := os.ReadFile(schemaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading schema file: %v\n", err)
			return 1
		}
		schema = data
	}

	// Derive session ID: --session slug > origin (default "cli")
	sessionID := origin
	if sessionSlug != "" {
		sessionID = sessionSlug
	}

	// Initialize bus chat unless --stateless.
	// Default mode: records to bus (visible in other sessions' peripheral context)
	// but doesn't load history into own context. --profile activates full continuity.
	var bc *busChat
	if !stateless {
		workspaceRoot, _, _ := ResolveWorkspace()
		if workspaceRoot != "" {
			bc = newBusChat(workspaceRoot)
		}
	}

	// Emit chat.request event (before context construction so it appears in bus history)
	var busID string
	var requestSeq int
	if bc != nil {
		var reqEvt *BusEventData
		busID, reqEvt, _ = bc.emitRequest(sessionID, prompt, origin)
		if reqEvt != nil {
			requestSeq = reqEvt.Seq
		}
	}

	// Build TAA context using bus history for conversation continuity
	var contextState *ContextState
	if !stateless && (contextURI != "" || taaProfile != "") {
		contextState = buildCLIContext(prompt, taaProfile, contextURI, bc, sessionID)
	}

	// Build request
	req := &InferenceRequest{
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		Model:        model,
		Schema:       schema,
		Origin:       origin,
		Stream:       false,
		Context:      context.Background(),
		ContextState: contextState,
	}

	// Run inference
	resp, err := RunInference(req, nil) // No registry for CLI

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if bc != nil && busID != "" {
			bc.emitError(busID, requestSeq, err.Error(), "inference_error")
		}
		return 1
	}

	// Emit chat.response event so next invocation sees this exchange
	if bc != nil && busID != "" && resp.Content != "" {
		bc.emitResponse(busID, requestSeq, resp.Content, model, 0, 0)
	}

	// Output result
	if jsonOutput {
		output, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(output))
	} else {
		fmt.Println(resp.Content)
	}

	return 0
}

// buildCLIContext constructs a ContextState for CLI inference.
//
// Two modes:
//  1. --profile <name>: Direct profile-based construction (same pipeline as HTTP)
//  2. --context <cog://context?...>: URI-based with query params for budget, tier, model
//
// If both are specified, --context takes precedence (its profile param overrides --profile).
// When bc (busChat) is non-nil, reads conversation history from the bus for continuity.
func buildCLIContext(prompt, taaProfile, contextURI string, bc *busChat, sessionID string) *ContextState {
	workspaceRoot, _, err := ResolveWorkspace()
	if err != nil {
		log.Printf("[TAA] CLI: workspace resolution failed: %v", err)
		return nil
	}

	// Build message history: bus history (if available) + current prompt
	var messages []ChatMessage
	if bc != nil && sessionID != "" {
		busID := fmt.Sprintf("bus_chat_%s", sessionID)
		busMessages, err := bc.manager.busEventsToMessages(busID, bc.config.MaxHistory)
		if err == nil && len(busMessages) > 0 {
			messages = busMessages
			log.Printf("[TAA] CLI: loaded %d messages from bus history", len(busMessages))
		}
	}
	// The current prompt was already emitted as a bus event, so it's included
	// in busMessages. If no bus history, fall back to single-message.
	if len(messages) == 0 {
		contentBytes, _ := json.Marshal(prompt)
		messages = []ChatMessage{{Role: "user", Content: contentBytes}}
	}

	// URI mode: parse cog://context?budget=50000&profile=default&model=sonnet
	if contextURI != "" {
		parsed, err := parseContextURI(contextURI)
		if err != nil {
			log.Printf("[TAA] CLI: invalid context URI %q: %v", contextURI, err)
			return nil
		}

		// Extract profile from URI or fall back to --taa flag
		profile := parsed.profile
		if profile == "" {
			profile = taaProfile
		}
		if profile == "" {
			profile = "default"
		}

		log.Printf("[TAA] CLI: context URI=%s profile=%s budget=%d", contextURI, profile, parsed.budget)

		var state *ContextState
		if profile != "" {
			state, err = ConstructContextStateWithProfile(messages, sessionID, workspaceRoot, profile)
		} else {
			state, err = ConstructContextState(messages, sessionID, workspaceRoot)
		}
		if err != nil {
			log.Printf("[TAA] CLI: context construction warning: %v", err)
		}
		if state != nil {
			// Override model from URI if specified
			if parsed.model != "" {
				state.Model = parsed.model
			}
			log.Printf("[TAA] CLI: context loaded, tokens=%d coherence=%.2f", state.TotalTokens, state.CoherenceScore)
		}
		return state
	}

	// Profile mode: --taa <profile>
	if taaProfile != "" {
		log.Printf("[TAA] CLI: profile=%s", taaProfile)
		state, err := ConstructContextStateWithProfile(messages, sessionID, workspaceRoot, taaProfile)
		if err != nil {
			log.Printf("[TAA] CLI: context construction warning: %v", err)
		}
		if state != nil {
			log.Printf("[TAA] CLI: context loaded, tokens=%d coherence=%.2f", state.TotalTokens, state.CoherenceScore)
		}
		return state
	}

	return nil
}

// contextURIParams holds parsed parameters from a cog://context URI.
type contextURIParams struct {
	budget  int
	profile string
	model   string
	tier    string
}

// parseContextURI parses a cog://context URI into structured parameters.
// Accepts both full URI (cog://context?budget=50000) and shorthand ("context?budget=50000").
func parseContextURI(uri string) (*contextURIParams, error) {
	// Accept shorthand without cog:// prefix
	if !strings.HasPrefix(uri, "cog://") {
		uri = "cog://" + uri
	}

	parsed, err := sdk.ParseURI(uri)
	if err != nil {
		return nil, err
	}

	if parsed.Namespace != "context" {
		return nil, fmt.Errorf("expected cog://context namespace, got %q", parsed.Namespace)
	}

	return &contextURIParams{
		budget:  parsed.GetQueryInt("budget", 0),
		profile: parsed.GetQuery("profile"),
		model:   parsed.GetQuery("model"),
		tier:    parsed.GetQuery("tier"),
	}, nil
}

func printInferHelp() {
	fmt.Printf(`Infer - Run inference using shared engine

Usage: cog infer [options] <prompt>

Options:
  --schema, -s <path>    JSON schema file for structured output
  --system <prompt>      System prompt
  --model, -m <model>    Model to use (default: claude)
  --profile, -p <name>   Context assembly profile — enables full conversation continuity
  --context, -c <uri>    Context URI (cog://context?budget=50000&profile=default)
  --session, -S <slug>   Name the conversation thread (default: "cli")
  --stateless            Zero bus interaction — nothing recorded, nothing read
  --json, -j             Output as JSON (for programmatic use)
  --origin <origin>      Tag request origin (default: "cli")
  --help, -h             Show this help

Modes:
  (default)              Records to bus, visible in other sessions' peripheral context
  --profile <name>       Full continuity — loads bus history into context assembly pipeline
  --stateless            Like "claude -p" — pure one-shot, no side effects

Examples:
  cog infer "What is 2+2?"                                   # default: records to bus
  cog infer -p default "Explain the reconciliation loop"      # full continuity
  cog infer -p default -S debug "Why is X broken?"            # named session
  cog infer -S research "What does the paper say?"            # named, no profile
  cog infer --stateless "Quick one-off question"              # zero side effects
  cog infer -c "cog://context?profile=default" "Summarize recent work"
  cog infer -s .cog/schemas/tasks/summarize.schema.json "Summarize..."

Notes:
  - Requires Claude CLI installed (npm install -g @anthropic-ai/claude-code)
  - Uses the same inference engine as the serve command
  - --profile loads the full 4-tier context pipeline (identity, temporal, present, semantic)
  - --session creates a named bus (bus_chat_<slug>) for parallel conversation threads
  - --context accepts a cog://context URI with query params: budget, profile, model, tier
  - If both --profile and --context are specified, --context takes precedence
  - Without --profile, messages still accumulate on the bus for peripheral awareness
  - JSON output includes prompt/completion tokens and context metrics
`)
}

// === INFERENCE CLI COMMANDS (ADR-046) ===

// cmdInference handles the "cog inference" command group for provider management
func cmdInference(args []string) int {
	if len(args) == 0 {
		printInferenceHelp()
		return 0
	}

	switch args[0] {
	case "list":
		return cmdInferenceList(args[1:])
	case "status":
		return cmdInferenceStatus(args[1:])
	case "use":
		return cmdInferenceUse(args[1:])
	case "test":
		return cmdInferenceTest(args[1:])
	case "help", "-h", "--help":
		printInferenceHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown inference subcommand: %s\n", args[0])
		printInferenceHelp()
		return 1
	}
}

// cmdInferenceList lists all configured providers with status
func cmdInferenceList(args []string) int {
	// Fetch from local kernel if running, otherwise show defaults
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:5100/v1/providers")

	if err != nil {
		// Kernel not running, show default providers
		fmt.Println("PROVIDER     STATUS   ACTIVE  MODELS")
		fmt.Println("claude       unknown  *       (kernel not running)")
		fmt.Println("\nNote: Start kernel with 'cog serve' to see live status")
		return 0
	}
	defer resp.Body.Close()

	var data struct {
		Data []struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Status string   `json:"status"`
			Active bool     `json:"active"`
			Models []string `json:"models"`
		} `json:"data"`
		Active        string   `json:"active"`
		FallbackChain []string `json:"fallback_chain"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	// Print table header
	fmt.Printf("%-12s %-8s %-7s %s\n", "PROVIDER", "STATUS", "ACTIVE", "MODELS")

	for _, p := range data.Data {
		active := ""
		if p.Active {
			active = "*"
		}

		// Status indicator
		statusIcon := "?"
		switch p.Status {
		case "online":
			statusIcon = "✓"
		case "offline":
			statusIcon = "✗"
		case "degraded":
			statusIcon = "!"
		}

		// Truncate models list
		modelsStr := strings.Join(p.Models, ", ")
		if len(modelsStr) > 40 {
			modelsStr = modelsStr[:37] + "..."
		}

		fmt.Printf("%-12s %s %-6s %-7s %s\n", p.ID, statusIcon, p.Status, active, modelsStr)
	}

	fmt.Printf("\nActive: %s\n", data.Active)
	if len(data.FallbackChain) > 0 {
		fmt.Printf("Fallback: %s\n", strings.Join(data.FallbackChain, " -> "))
	}

	return 0
}

// cmdInferenceStatus shows health status of all providers
func cmdInferenceStatus(args []string) int {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://localhost:5100/v1/providers")

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Kernel not running on localhost:5100\n")
		fmt.Fprintf(os.Stderr, "Start with: cog serve\n")
		return 1
	}
	defer resp.Body.Close()

	var data struct {
		Data []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Health struct {
				LastCheck *string `json:"last_check"`
				LatencyMs *int    `json:"latency_ms"`
				Error     *string `json:"error"`
			} `json:"health"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	for _, p := range data.Data {
		statusIcon := "?"
		switch p.Status {
		case "online":
			statusIcon = "✓"
		case "offline":
			statusIcon = "✗"
		case "degraded":
			statusIcon = "!"
		}

		latency := ""
		if p.Health.LatencyMs != nil {
			latency = fmt.Sprintf("(%dms)", *p.Health.LatencyMs)
		}

		errMsg := ""
		if p.Health.Error != nil {
			errMsg = fmt.Sprintf(" - %s", *p.Health.Error)
		}

		fmt.Printf("%s %s: %s %s%s\n", statusIcon, p.Name, p.Status, latency, errMsg)
	}

	return 0
}

// cmdInferenceUse switches to a different provider
func cmdInferenceUse(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog inference use <provider>\n")
		fmt.Fprintf(os.Stderr, "Example: cog inference use openrouter\n")
		return 1
	}

	providerID := args[0]

	// POST to activate the provider
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://localhost:5100/v1/providers/%s/activate", providerID)

	body := strings.NewReader(`{"set_as_default": true}`)
	resp, err := client.Post(url, "application/json", body)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Kernel not running on localhost:5100\n")
		fmt.Fprintf(os.Stderr, "Start with: cog serve\n")
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "Error: Provider '%s' not found\n", providerID)
		fmt.Fprintf(os.Stderr, "Run 'cog inference list' to see available providers\n")
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		fmt.Fprintf(os.Stderr, "Error: %s\n", errResp.Error.Message)
		return 1
	}

	var result struct {
		Success  bool   `json:"success"`
		Active   string `json:"active"`
		Previous string `json:"previous"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	fmt.Printf("Switched to %s (was: %s)\n", result.Active, result.Previous)
	return 0
}

// cmdInferenceTest tests a specific provider
func cmdInferenceTest(args []string) int {
	providerID := ""
	if len(args) > 0 {
		providerID = args[0]
	}

	var url string
	if providerID == "" {
		// Test all providers - just do a status check
		return cmdInferenceStatus(args)
	}

	url = fmt.Sprintf("http://localhost:5100/v1/providers/%s/test", providerID)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(url, "application/json", nil)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Kernel not running on localhost:5100\n")
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "Error: Provider '%s' not found\n", providerID)
		return 1
	}

	var result struct {
		Provider  string  `json:"provider"`
		Status    string  `json:"status"`
		LatencyMs int     `json:"latency_ms"`
		TestModel string  `json:"test_model"`
		Error     *string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing response: %v\n", err)
		return 1
	}

	statusIcon := "✓"
	if result.Status != "online" {
		statusIcon = "✗"
	}

	errMsg := ""
	if result.Error != nil {
		errMsg = fmt.Sprintf(" - %s", *result.Error)
	}

	fmt.Printf("%s %s: %s (%dms)%s\n", statusIcon, result.Provider, result.Status, result.LatencyMs, errMsg)
	if result.TestModel != "" {
		fmt.Printf("  Tested with: %s\n", result.TestModel)
	}

	return 0
}

func printInferenceHelp() {
	fmt.Printf(`Inference - Provider management (ADR-046)

Usage: cog inference <command> [args...]

Commands:
  list                 List all providers with status
  status               Show health status of all providers  
  use <provider>       Switch to a different provider
  test [provider]      Test a specific provider (or all if none specified)

Examples:
  cog inference list                # Show all providers
  cog inference status              # Check health of all
  cog inference use openrouter      # Switch to OpenRouter
  cog inference test anthropic      # Test Anthropic connection

Available Providers:
  claude       Claude CLI (default, via Max subscription)
  openai       OpenAI API (requires OPENAI_API_KEY)
  openrouter   OpenRouter (requires OPENROUTER_API_KEY)
  ollama       Ollama local models (http://localhost:11434)
  local        Local kernel (for testing)

Notes:
  - Requires kernel running: cog serve
  - Provider switch persists until kernel restart
  - Health checks are cached for 60 seconds
`)
}

// === EVENT EMISSION (ADR-033) ===

// InferenceEvent represents an event in the inference event stream
type InferenceEvent struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Timestamp string                 `json:"ts"`
	Data      map[string]interface{} `json:"data"`
}

// eventSeq is a module-level sequence counter for events
var eventSeq int64

// generateEventID creates a unique event ID
func generateEventID() string {
	eventSeq++
	ts := time.Now().UnixMilli()
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)
	return fmt.Sprintf("evt_%x_%s", ts, hex.EncodeToString(randomBytes))
}

// getEventsDir returns the path to the events directory
func getEventsDir() (string, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return "", err
	}
	eventsDir := filepath.Join(root, ".cog", "var", "events")
	if err := os.MkdirAll(eventsDir, 0755); err != nil {
		return "", err
	}
	return eventsDir, nil
}

// getKernelEventFile returns the path to the kernel event file for today
func getKernelEventFile() (string, error) {
	eventsDir, err := getEventsDir()
	if err != nil {
		return "", err
	}
	date := time.Now().Format("2006-01-02")
	return filepath.Join(eventsDir, date+"-kernel.jsonl"), nil
}

// emitEvent writes an event to the kernel event stream
func emitEvent(eventType string, data map[string]interface{}) error {
	eventFile, err := getKernelEventFile()
	if err != nil {
		return err
	}

	event := InferenceEvent{
		ID:        generateEventID(),
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Data:      data,
	}

	line, err := json.Marshal(event)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(eventFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(string(line) + "\n")
	return err
}

// emitInferenceStart emits INFERENCE_START event
func emitInferenceStart(req *InferenceRequest) {
	promptPreview := req.Prompt
	if len(promptPreview) > 100 {
		promptPreview = promptPreview[:100] + "..."
	}

	model := req.Model
	if model == "" {
		model = "claude"
	}

	data := map[string]interface{}{
		"request_id":     req.ID,
		"model":          model,
		"origin":         req.Origin,
		"prompt_preview": promptPreview,
	}

	if err := emitEvent("INFERENCE_START", data); err != nil {
		// Log error but don't fail the request
		// Silently ignore event emission failures
		_ = err
	}
}

// emitInferenceComplete emits INFERENCE_COMPLETE event
func emitInferenceComplete(req *InferenceRequest, resp *InferenceResponse, startTime time.Time) {
	model := req.Model
	if model == "" {
		model = "claude"
	}

	durationMs := time.Since(startTime).Milliseconds()

	data := map[string]interface{}{
		"request_id":        req.ID,
		"model":             model,
		"duration_ms":       durationMs,
		"prompt_tokens":     resp.PromptTokens,
		"completion_tokens": resp.CompletionTokens,
		"finish_reason":     resp.FinishReason,
	}

	if err := emitEvent("INFERENCE_COMPLETE", data); err != nil {
		_ = err
	}
}

// emitInferenceError emits INFERENCE_ERROR event
func emitInferenceError(requestID string, errMsg string) {
	data := map[string]interface{}{
		"request_id": requestID,
		"error":      errMsg,
	}

	if err := emitEvent("INFERENCE_ERROR", data); err != nil {
		_ = err
	}
}

// === SIGNAL MANAGEMENT ===

// SignalData represents a signal in the signal field
type SignalData struct {
	SignalType  string                 `json:"signal_type"`
	Strength    float64                `json:"strength"`
	DepositedBy string                 `json:"deposited_by"`
	DepositedAt float64                `json:"deposited_at"`
	HalfLife    float64                `json:"half_life"`
	DecayType   string                 `json:"decay_type"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// SignalFieldState represents the persisted signal field state
type SignalFieldState struct {
	Signals    map[string]map[string]SignalData `json:"signals"`
	Metrics    map[string]int                   `json:"metrics"`
	SavedAt    float64                          `json:"saved_at"`
	SavedAtISO string                           `json:"saved_at_iso"`
}

// getSignalsDir returns the path to the signals directory
func getSignalsDir() (string, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return "", err
	}
	// ADR-033: Signals live in .cog/run/signals/
	signalsDir := filepath.Join(root, ".cog", "run", "signals")
	if err := os.MkdirAll(signalsDir, 0755); err != nil {
		return "", err
	}
	return signalsDir, nil
}

// loadSignalField loads the signal field state from disk
func loadSignalField() (*SignalFieldState, error) {
	signalsDir, err := getSignalsDir()
	if err != nil {
		return nil, err
	}

	stateFile := filepath.Join(signalsDir, "field_state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty state
			return &SignalFieldState{
				Signals: make(map[string]map[string]SignalData),
				Metrics: map[string]int{
					"total_deposits": 0,
					"total_senses":   0,
					"active_signals": 0,
				},
			}, nil
		}
		return nil, err
	}

	var state SignalFieldState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	if state.Signals == nil {
		state.Signals = make(map[string]map[string]SignalData)
	}
	if state.Metrics == nil {
		state.Metrics = map[string]int{
			"total_deposits": 0,
			"total_senses":   0,
			"active_signals": 0,
		}
	}

	return &state, nil
}

// saveSignalField saves the signal field state to disk
func saveSignalField(state *SignalFieldState) error {
	signalsDir, err := getSignalsDir()
	if err != nil {
		return err
	}

	state.SavedAt = float64(time.Now().Unix())
	state.SavedAtISO = time.Now().Format(time.RFC3339)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	stateFile := filepath.Join(signalsDir, "field_state.json")
	tmpFile := stateFile + ".tmp"

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}

	return os.Rename(tmpFile, stateFile)
}

// signalFieldMu serializes read-modify-write access to the signal field file
var signalFieldMu sync.Mutex

// depositSignal deposits a signal at a location
func depositSignal(location, signalType, agentID string, halfLifeHours float64, metadata map[string]interface{}) error {
	signalFieldMu.Lock()
	defer signalFieldMu.Unlock()

	state, err := loadSignalField()
	if err != nil {
		return err
	}

	if state.Signals[location] == nil {
		state.Signals[location] = make(map[string]SignalData)
	}

	signal := SignalData{
		SignalType:  signalType,
		Strength:    1.0,
		DepositedBy: agentID,
		DepositedAt: float64(time.Now().Unix()),
		HalfLife:    halfLifeHours,
		DecayType:   "linear",
		Metadata:    metadata,
	}

	state.Signals[location][signalType] = signal
	state.Metrics["total_deposits"]++

	return saveSignalField(state)
}

// removeSignal removes a signal at a location
func removeSignal(location, signalType string) error {
	signalFieldMu.Lock()
	defer signalFieldMu.Unlock()

	state, err := loadSignalField()
	if err != nil {
		return err
	}

	if state.Signals[location] != nil {
		delete(state.Signals[location], signalType)
		if len(state.Signals[location]) == 0 {
			delete(state.Signals, location)
		}
	}

	return saveSignalField(state)
}

// setInferenceActiveSignal sets the inference.active signal
func setInferenceActiveSignal(requestID, model, origin string) {
	metadata := map[string]interface{}{
		"request_id": requestID,
		"model":      model,
		"origin":     origin,
		"started_at": time.Now().Format(time.RFC3339),
	}

	// Signal location is inference/active, half-life of 0.5 hours (30 min)
	if err := depositSignal("inference/active", "working", "kernel", 0.5, metadata); err != nil {
		_ = err
	}
}

// clearInferenceActiveSignal clears the inference.active signal
func clearInferenceActiveSignal() {
	if err := removeSignal("inference/active", "working"); err != nil {
		_ = err
	}
}
