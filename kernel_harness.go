// kernel_harness.go is the integration layer between the CogOS kernel (package main)
// and the inference harness (package harness).
//
// It provides three things:
//
//  1. kernelServicesAdapter — implements harness.KernelServices by wrapping
//     existing kernel functions (dispatch, emitEvent, depositSignal, etc.)
//
//  2. Type conversion functions — the kernel and harness have parallel type
//     hierarchies (both define InferenceRequest, ContextState, etc.). These
//     converters bridge them at the boundary.
//
//  3. Shim functions (HarnessRunInference, HarnessRunInferenceStream) — called
//     by serve.go and inference.go where the old RunInference/RunInferenceStream
//     used to be called. They convert kernel→harness, delegate, then convert back.
//
// Call flow:
//
//	serve.go:handleChatCompletions
//	  → HarnessRunInference(kernelReq)          ← this file
//	    → toHarnessRequest(kernelReq)            ← type conversion
//	    → harness.Harness.RunInference(hReq)     ← harness package
//	    → fromHarnessResponse(hResp)             ← type conversion
//	  ← kernelResp
package main

import (
	"encoding/json"
	"os"

	"github.com/cogos-dev/cogos/harness"
)

// kernelServicesAdapter implements harness.KernelServices by wrapping the
// existing package-level functions in the kernel. Each method delegates to
// a kernel function defined elsewhere in package main:
//
//	WorkspaceRoot       → ResolveWorkspace()           (cog.go)
//	DispatchHook        → dispatch()                   (cog.go)
//	EmitEvent           → emitEvent()                  (inference.go)
//	ReadContinuation    → readContinuationState()      (inference.go)
//	DepositSignal       → depositSignal()              (inference.go)
//	RemoveSignal        → removeSignal()               (inference.go)
//	ResolveWorkDir      → env + ResolveWorkspace()     (inline)
//	ConvertOpenAITools  → convertOpenAIToolsToMCP()    (mcp.go)
type kernelServicesAdapter struct{}

// Compile-time check: kernelServicesAdapter implements harness.KernelServices
var _ harness.KernelServices = (*kernelServicesAdapter)(nil)

func (k *kernelServicesAdapter) WorkspaceRoot() string {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return ""
	}
	return root
}

func (k *kernelServicesAdapter) DispatchHook(event string, data map[string]any) *harness.HookResult {
	result := dispatch(event, "", data)
	if result == nil {
		return nil
	}
	return &harness.HookResult{
		Decision:          result.Decision,
		Reason:            result.Reason,
		Message:           result.Message,
		Fallback:          result.Fallback,
		AdditionalContext: result.AdditionalContext,
	}
}

func (k *kernelServicesAdapter) EmitEvent(eventType string, data map[string]any) error {
	return emitEvent(eventType, data)
}

func (k *kernelServicesAdapter) ReadContinuationState() (*harness.ContinuationState, error) {
	state, err := readContinuationState()
	if err != nil {
		return nil, err
	}
	return &harness.ContinuationState{
		SessionID:          state.SessionID,
		Timestamp:          state.Timestamp,
		Trigger:            state.Trigger,
		Focus:              state.Focus,
		ContinuationPrompt: state.ContinuationPrompt,
	}, nil
}

func (k *kernelServicesAdapter) DepositSignal(location, signalType, agentID string, halfLife float64, meta map[string]any) error {
	// Convert map[string]any to map[string]interface{} (same type, but match old signature)
	return depositSignal(location, signalType, agentID, halfLife, meta)
}

func (k *kernelServicesAdapter) RemoveSignal(location, signalType string) error {
	return removeSignal(location, signalType)
}

func (k *kernelServicesAdapter) ResolveWorkDir(requestWorkspace string) string {
	if requestWorkspace != "" {
		return requestWorkspace
	}
	if defaultWs := os.Getenv("DEFAULT_CLIENT_WORKSPACE"); defaultWs != "" {
		return defaultWs
	}
	if wsRoot, _, wsErr := ResolveWorkspace(); wsErr == nil {
		return wsRoot
	}
	return ""
}

func (k *kernelServicesAdapter) GetAgentToolPolicy(agentID string) (*harness.AgentToolPolicy, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return nil, nil // No workspace = no CRD lookup
	}
	result, err := GetAgentCRDToolPolicy(root, agentID)
	if err != nil || result == nil {
		return nil, err
	}
	return &harness.AgentToolPolicy{
		AllowedTools:               result.AllowedTools,
		DenyTools:                  result.DenyTools,
		DangerouslySkipPermissions: result.DangerouslySkipPermissions,
	}, nil
}

func (k *kernelServicesAdapter) ConvertOpenAIToolsToMCP(tools []json.RawMessage) []harness.MCPTool {
	mcpTools := convertOpenAIToolsToMCP(tools)
	// Convert from main.MCPTool to harness.MCPTool
	result := make([]harness.MCPTool, len(mcpTools))
	for i, t := range mcpTools {
		result[i] = harness.MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return result
}

// globalHarness is the shared harness instance, initialized at serve startup.
var globalHarness *harness.Harness

// initHarness creates and stores the global harness instance.
func initHarness() *harness.Harness {
	if globalHarness == nil {
		globalHarness = harness.New(&kernelServicesAdapter{})
	}
	return globalHarness
}

// === TYPE CONVERSION (kernel ↔ harness) ===
//
// The kernel and harness define parallel types (both have InferenceRequest,
// ContextState, StreamChunkInference, etc.) because the harness is a separate
// Go module that cannot import package main. These converters bridge the two
// type systems at the call boundary. They're mechanical field-by-field copies.

// toHarnessContextState converts kernel ContextState to harness ContextState.
func toHarnessContextState(cs *ContextState) *harness.ContextState {
	if cs == nil {
		return nil
	}
	hcs := &harness.ContextState{
		Model:          cs.Model,
		TotalTokens:    cs.TotalTokens,
		CoherenceScore: cs.CoherenceScore,
		ShouldRefresh:  cs.ShouldRefresh,
		Anchor:         cs.Anchor,
		Goal:           cs.Goal,
	}
	if cs.Tier1Identity != nil {
		hcs.Tier1Identity = &harness.ContextTier{Content: cs.Tier1Identity.Content, Tokens: cs.Tier1Identity.Tokens, Source: cs.Tier1Identity.Source}
	}
	if cs.Tier2Temporal != nil {
		hcs.Tier2Temporal = &harness.ContextTier{Content: cs.Tier2Temporal.Content, Tokens: cs.Tier2Temporal.Tokens, Source: cs.Tier2Temporal.Source}
	}
	if cs.Tier3Present != nil {
		hcs.Tier3Present = &harness.ContextTier{Content: cs.Tier3Present.Content, Tokens: cs.Tier3Present.Tokens, Source: cs.Tier3Present.Source}
	}
	if cs.Tier4Semantic != nil {
		hcs.Tier4Semantic = &harness.ContextTier{Content: cs.Tier4Semantic.Content, Tokens: cs.Tier4Semantic.Tokens, Source: cs.Tier4Semantic.Source}
	}
	return hcs
}

// toHarnessRequest converts kernel InferenceRequest to harness InferenceRequest
func toHarnessRequest(req *InferenceRequest) *harness.InferenceRequest {
	return &harness.InferenceRequest{
		ID:            req.ID,
		Prompt:        req.Prompt,
		SystemPrompt:  req.SystemPrompt,
		Model:         req.Model,
		Schema:        req.Schema,
		MaxTokens:     req.MaxTokens,
		Origin:        req.Origin,
		Stream:        req.Stream,
		Context:       req.Context,
		ContextState:  toHarnessContextState(req.ContextState),
		Tools:           req.Tools,
		AllowedTools:    req.AllowedTools,
		SkipPermissions: req.SkipPermissions,
		WorkspaceRoot: req.WorkspaceRoot,
		MCPConfig:     req.MCPConfig,
		OpenClawURL:   req.OpenClawURL,
		OpenClawToken: req.OpenClawToken,
		SessionID:     req.SessionID,
		MaxRetries:    req.MaxRetries,
		Timeout:       req.Timeout,
	}
}

// fromHarnessResponse converts harness InferenceResponse to kernel InferenceResponse
func fromHarnessResponse(resp *harness.InferenceResponse) *InferenceResponse {
	if resp == nil {
		return nil
	}
	kr := &InferenceResponse{
		ID:                resp.ID,
		Content:           resp.Content,
		PromptTokens:      resp.PromptTokens,
		CompletionTokens:  resp.CompletionTokens,
		CacheReadTokens:   resp.CacheReadTokens,
		CacheCreateTokens: resp.CacheCreateTokens,
		CostUSD:           resp.CostUSD,
		FinishReason:      resp.FinishReason,
		Error:             resp.Error,
		ErrorMessage:      resp.ErrorMessage,
		ErrorType:         ErrorType(resp.ErrorType),
	}
	if resp.ContextMetrics != nil {
		kr.ContextMetrics = &ContextMetrics{
			TotalTokens:     resp.ContextMetrics.TotalTokens,
			TierBreakdown:   resp.ContextMetrics.TierBreakdown,
			CoherenceScore:  resp.ContextMetrics.CoherenceScore,
			CompressionUsed: resp.ContextMetrics.CompressionUsed,
		}
	}
	return kr
}

// fromHarnessStreamChunk converts harness StreamChunkInference to kernel StreamChunkInference
func fromHarnessStreamChunk(chunk harness.StreamChunkInference) StreamChunkInference {
	sc := StreamChunkInference{
		ID:           chunk.ID,
		Content:      chunk.Content,
		Done:         chunk.Done,
		FinishReason: chunk.FinishReason,
		Error:        chunk.Error,
		EventType:    chunk.EventType,
	}
	if chunk.ToolCall != nil {
		sc.ToolCall = &ToolCallData{
			ID:        chunk.ToolCall.ID,
			Name:      chunk.ToolCall.Name,
			Arguments: chunk.ToolCall.Arguments,
		}
	}
	if chunk.ToolResult != nil {
		sc.ToolResult = &ToolResultData{
			ToolCallID: chunk.ToolResult.ToolCallID,
			Content:    chunk.ToolResult.Content,
			IsError:    chunk.ToolResult.IsError,
		}
	}
	if chunk.Usage != nil {
		sc.Usage = &UsageData{
			InputTokens:       chunk.Usage.InputTokens,
			OutputTokens:      chunk.Usage.OutputTokens,
			CacheReadTokens:   chunk.Usage.CacheReadTokens,
			CacheCreateTokens: chunk.Usage.CacheCreateTokens,
			CostUSD:           chunk.Usage.CostUSD,
		}
	}
	if chunk.SessionInfo != nil {
		sc.SessionInfo = &SessionInfo{
			SessionID: chunk.SessionInfo.SessionID,
			Model:     chunk.SessionInfo.Model,
			Tools:     chunk.SessionInfo.Tools,
		}
	}
	return sc
}

// HarnessRunInference is the kernel entry point for non-streaming inference.
// Called by serve.go:handleNonStreamingResponse and inference.go:cmdInfer.
//
// It replaces the old direct RunInference() call with a harness-delegated path:
// kernel types → harness types → harness.RunInference → kernel types.
//
// The registry parameter is accepted for signature compatibility but the harness
// uses its own internal registry.
func HarnessRunInference(req *InferenceRequest, registry *RequestRegistry) (*InferenceResponse, error) {
	h := initHarness()
	hReq := toHarnessRequest(req)
	hResp, err := h.RunInference(hReq)
	// Copy the generated ID back to the kernel request
	req.ID = hReq.ID
	return fromHarnessResponse(hResp), err
}

// HarnessRunInferenceStream is the kernel entry point for streaming inference.
// Called by serve.go:handleStreamingResponse.
//
// Returns a channel of kernel-typed StreamChunkInference. Internally spawns a
// goroutine that reads from the harness channel and converts each chunk.
func HarnessRunInferenceStream(req *InferenceRequest, registry *RequestRegistry) (<-chan StreamChunkInference, error) {
	h := initHarness()
	hReq := toHarnessRequest(req)
	hChunks, err := h.RunInferenceStream(hReq)
	if err != nil {
		return nil, err
	}

	// Convert harness chunks to kernel chunks
	kernelChunks := make(chan StreamChunkInference, 100)
	go func() {
		defer close(kernelChunks)
		for hChunk := range hChunks {
			kernelChunks <- fromHarnessStreamChunk(hChunk)
		}
	}()
	return kernelChunks, nil
}
