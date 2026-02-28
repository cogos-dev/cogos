package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// busChat provides bus event emission as a side-effect of HTTP inference.
// When a chat completion request arrives (from any source: OpenClaw gateway,
// curl, etc.), busChat writes chat.request and chat.response events to the
// appropriate bus, making conversations visible in CogField and queryable
// via the bus event chain.
type busChat struct {
	manager *busSessionManager
	config  *BusChatConfig
	root    string
}

func newBusChat(root string) *busChat {
	cfg := LoadBusChatConfig(root)
	return &busChat{
		manager: newBusSessionManager(root),
		config:  cfg,
		root:    root,
	}
}

// ChatRequestOpts carries context for a chat.request bus event.
// Call sites populate what they have; emit includes only non-zero values.
type ChatRequestOpts struct {
	SessionID string
	Content   string
	Origin    string

	// Model context
	Model  string
	Stream bool

	// Identity
	UserID    string
	UserName  string
	AgentName string

	// OTEL bridge
	TraceID string
	SpanID  string

	// Context pipeline
	HasTAA     bool
	TAAProfile string
}

// ChatResponseOpts carries context for a chat.response bus event.
type ChatResponseOpts struct {
	BusID      string
	RequestSeq int
	Content    string
	Model      string
	DurationMs int64

	// Token breakdown
	PromptTokens     int
	CompletionTokens int
	TokensUsed       int // kept for backward compat (sum)

	// Cache/cost
	CacheReadTokens   int
	CacheCreateTokens int
	CostUSD           float64

	// Outcome
	FinishReason string
	Stream       bool

	// Correlation
	RequestHash string
	TraceID     string
	SpanID      string

	// TAA context metrics
	TAATokens    int
	TAACoherence float64

	// Partial failure
	ErrorType    string
	ErrorMessage string
}

// ChatErrorOpts carries context for a chat.error bus event.
type ChatErrorOpts struct {
	BusID      string
	RequestSeq int

	// Correlation
	RequestHash string

	// Error detail
	ErrorMessage string
	ErrorType    string // rate_limit, context_overflow, auth, transient, fatal

	// Context
	DurationMs int64
	Model      string
	Stream     bool

	// Tracing
	TraceID string
	SpanID  string

	// Identity
	UserID    string
	AgentName string
}

// emitRequest writes a chat.request event to the session's bus.
// Returns the bus ID and event (for later pairing with emitResponse).
func (bc *busChat) emitRequest(opts ChatRequestOpts) (string, *CogBlock, error) {
	if opts.SessionID == "" {
		return "", nil, nil // no session = no bus tracking
	}

	origin := opts.Origin
	if origin == "" {
		origin = "http"
	}

	busID := bc.ensureBus(opts.SessionID, origin)

	payload := map[string]interface{}{
		"content":  opts.Content,
		"origin":   origin,
		"platform": origin,
	}
	if opts.Model != "" {
		payload["model"] = opts.Model
	}
	if opts.Stream {
		payload["stream"] = true
	}
	if opts.UserID != "" {
		payload["user_id"] = opts.UserID
	}
	if opts.UserName != "" {
		payload["user_name"] = opts.UserName
	}
	if opts.AgentName != "" {
		payload["agent"] = opts.AgentName
	}
	if opts.TraceID != "" {
		payload["trace_id"] = opts.TraceID
	}
	if opts.SpanID != "" {
		payload["span_id"] = opts.SpanID
	}
	if opts.HasTAA {
		payload["taa_enabled"] = true
	}
	if opts.TAAProfile != "" {
		payload["taa_profile"] = opts.TAAProfile
	}

	evt, err := bc.manager.appendBusEvent(busID, BlockChatRequest,
		fmt.Sprintf("%s:user", origin), payload)
	if err != nil {
		return busID, nil, fmt.Errorf("emit chat.request: %w", err)
	}

	log.Printf("[bus-chat] bus=%s seq=%d type=chat.request from=%s", busID, evt.Seq, origin)
	return busID, evt, nil
}

// emitResponse writes a chat.response event to the session's bus.
func (bc *busChat) emitResponse(opts ChatResponseOpts) {
	if opts.BusID == "" {
		return
	}

	payload := map[string]interface{}{
		"content":     opts.Content,
		"model":       opts.Model,
		"duration_ms": opts.DurationMs,
		"request_seq": opts.RequestSeq,
	}

	// Backward compat: tokens_used as sum
	tokensUsed := opts.TokensUsed
	if tokensUsed == 0 && (opts.PromptTokens > 0 || opts.CompletionTokens > 0) {
		tokensUsed = opts.PromptTokens + opts.CompletionTokens
	}
	if tokensUsed > 0 {
		payload["tokens_used"] = tokensUsed
	}

	// Token breakdown
	if opts.PromptTokens > 0 {
		payload["prompt_tokens"] = opts.PromptTokens
	}
	if opts.CompletionTokens > 0 {
		payload["completion_tokens"] = opts.CompletionTokens
	}

	// Cache/cost
	if opts.CacheReadTokens > 0 {
		payload["cache_read_tokens"] = opts.CacheReadTokens
	}
	if opts.CacheCreateTokens > 0 {
		payload["cache_create_tokens"] = opts.CacheCreateTokens
	}
	if opts.CostUSD > 0 {
		payload["cost_usd"] = opts.CostUSD
	}

	// Outcome
	if opts.FinishReason != "" {
		payload["finish_reason"] = opts.FinishReason
	}
	if opts.Stream {
		payload["stream"] = true
	}

	// Correlation
	if opts.RequestHash != "" {
		payload["request_hash"] = opts.RequestHash
	}
	if opts.TraceID != "" {
		payload["trace_id"] = opts.TraceID
	}
	if opts.SpanID != "" {
		payload["span_id"] = opts.SpanID
	}

	// TAA context metrics
	if opts.TAATokens > 0 {
		payload["taa_tokens"] = opts.TAATokens
	}
	if opts.TAACoherence > 0 {
		payload["taa_coherence"] = opts.TAACoherence
	}

	// Partial failure
	if opts.ErrorType != "" {
		payload["error_type"] = opts.ErrorType
	}
	if opts.ErrorMessage != "" {
		payload["error"] = opts.ErrorMessage
	}

	evt, err := bc.manager.appendBusEvent(opts.BusID, BlockChatResponse, "kernel:cogos", payload)
	if err != nil {
		log.Printf("[bus-chat] failed to emit chat.response on bus=%s: %v", opts.BusID, err)
		return
	}

	log.Printf("[bus-chat] bus=%s seq=%d type=chat.response duration=%dms", opts.BusID, evt.Seq, opts.DurationMs)
}

// emitError writes a chat.error event to the session's bus.
func (bc *busChat) emitError(opts ChatErrorOpts) {
	if opts.BusID == "" {
		return
	}

	payload := map[string]interface{}{
		"error":       opts.ErrorMessage,
		"error_type":  opts.ErrorType,
		"request_seq": opts.RequestSeq,
	}

	// Correlation
	if opts.RequestHash != "" {
		payload["request_hash"] = opts.RequestHash
	}

	// Context
	if opts.DurationMs > 0 {
		payload["duration_ms"] = opts.DurationMs
	}
	if opts.Model != "" {
		payload["model"] = opts.Model
	}
	if opts.Stream {
		payload["stream"] = true
	}

	// Tracing
	if opts.TraceID != "" {
		payload["trace_id"] = opts.TraceID
	}
	if opts.SpanID != "" {
		payload["span_id"] = opts.SpanID
	}

	// Identity
	if opts.UserID != "" {
		payload["user_id"] = opts.UserID
	}
	if opts.AgentName != "" {
		payload["agent"] = opts.AgentName
	}

	_, err := bc.manager.appendBusEvent(opts.BusID, BlockChatError, "kernel:cogos", payload)
	if err != nil {
		log.Printf("[bus-chat] failed to emit chat.error on bus=%s: %v", opts.BusID, err)
	}
}

// buildContextFromBus reads the bus event chain and constructs a ContextState for TAA.
func (bc *busChat) buildContextFromBus(busID, currentPrompt string) *ContextState {
	if !bc.config.Features.TAAEnabled || !bc.config.Features.ContextFromBus {
		return nil
	}

	messages, err := bc.manager.busEventsToMessages(busID, bc.config.MaxHistory)
	if err != nil {
		log.Printf("[bus-chat] failed to read bus history: %v", err)
		return nil
	}

	// Add current message
	contentBytes, _ := json.Marshal(currentPrompt)
	messages = append(messages, ChatMessage{
		Role:    "user",
		Content: contentBytes,
	})

	profile := bc.config.TAAProfile
	if profile == "" || profile == "none" || profile == "passthrough" {
		return nil
	}

	contextState, err := ConstructContextStateWithProfile(messages, busID, bc.root, profile)
	if err != nil {
		log.Printf("[bus-chat] TAA context construction warning: %v", err)
		return nil
	}

	if contextState != nil {
		tierTokens := make([]string, 0, 4)
		if contextState.Tier1Identity != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("1:%d", contextState.Tier1Identity.Tokens))
		}
		if contextState.Tier2Temporal != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("2:%d", contextState.Tier2Temporal.Tokens))
		}
		if contextState.Tier3Present != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("3:%d", contextState.Tier3Present.Tokens))
		}
		if contextState.Tier4Semantic != nil {
			tierTokens = append(tierTokens, fmt.Sprintf("4:%d", contextState.Tier4Semantic.Tokens))
		}
		log.Printf("[bus-chat] bus=%s context_tokens=%d tiers=[%s]",
			busID, contextState.TotalTokens, strings.Join(tierTokens, " "))
	}

	return contextState
}

// ensureBus returns (or creates) a bus for the given session ID.
func (bc *busChat) ensureBus(sessionID, origin string) string {
	busID := fmt.Sprintf("bus_chat_%s", sessionID)

	// Try to create — idempotent if bus already exists
	if _, err := bc.manager.createChatBus(sessionID, origin); err != nil {
		log.Printf("[bus-chat] warning: createChatBus: %v", err)
	}

	return busID
}
