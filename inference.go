// inference.go — Kernel-side inference types, request registry, and CLI commands.
//
// Inference execution is delegated to the harness package (harness/).
// This file retains:
//   - Type definitions (InferenceRequest, InferenceResponse, ContextState, etc.)
//     used by kernel_harness.go type converters and serve.go handlers
//   - Provider types (ProviderType, ProviderConfig, DefaultProviders)
//   - RequestRegistry for tracking in-flight requests
//   - CLI commands (cog infer, cog inference list/status/use/test)
//   - Event emission and signal management

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
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

	// Anthropic cache metrics (zero for non-Claude providers)
	CacheReadTokens   int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`

	// Context metrics (from context pipeline)
	ContextMetrics *ContextMetrics `json:"context_metrics,omitempty"`

	// Error classification (for smart recovery)
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

	// Run inference via harness
	resp, err := HarnessRunInference(req, nil)

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
