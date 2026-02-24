// interfaces.go defines the bridge contract between the harness and the kernel.
//
// The harness never imports package main. All kernel dependencies flow through
// [KernelServices], which the kernel implements in kernel_harness.go via
// kernelServicesAdapter.
//
// To add a new kernel capability to the harness:
//  1. Add a method to KernelServices here
//  2. Implement it on kernelServicesAdapter in kernel_harness.go
//  3. Call it from harness code via h.kernel.NewMethod()
package harness

import "encoding/json"

// KernelServices is the single interface the harness uses to call back into the
// kernel. The kernel implements this with kernelServicesAdapter (kernel_harness.go).
//
// Methods are grouped by purpose:
//   - Workspace: WorkspaceRoot, ResolveWorkDir
//   - Lifecycle: DispatchHook (PreInference/PostInference)
//   - Observability: EmitEvent (INFERENCE_START/COMPLETE/ERROR)
//   - Signal field: DepositSignal, RemoveSignal
//   - State: ReadContinuationState (eigenfield persistence)
//   - Tools: ConvertOpenAIToolsToMCP (bridge config generation)
type KernelServices interface {
	// WorkspaceRoot returns the resolved workspace root path.
	// Used for MCP config generation and default working directory.
	WorkspaceRoot() string

	// DispatchHook dispatches a lifecycle hook event (e.g., "PreInference",
	// "PostInference"). Returns nil if no hook matched or the hook allowed
	// the action. A "block" decision aborts inference.
	DispatchHook(event string, data map[string]any) *HookResult

	// EmitEvent writes a timestamped event to .cog/run/events/.
	// Event types: INFERENCE_START, INFERENCE_COMPLETE, INFERENCE_ERROR.
	EmitEvent(eventType string, data map[string]any) error

	// ReadContinuationState reads .cog/run/continuation.json for eigenfield
	// persistence across context compaction. Returns (nil, err) if no state.
	ReadContinuationState() (*ContinuationState, error)

	// DepositSignal places a signal in the signal field at the given location.
	// Used to mark inference as active (location="inference", type="active").
	DepositSignal(location, signalType, agentID string, halfLife float64, meta map[string]any) error

	// RemoveSignal removes a signal. Used to clear the inference-active signal
	// when a request completes.
	RemoveSignal(location, signalType string) error

	// ResolveWorkDir determines the working directory for Claude CLI execution.
	// Priority: requestWorkspace → DEFAULT_CLIENT_WORKSPACE env → kernel workspace.
	ResolveWorkDir(requestWorkspace string) string

	// ConvertOpenAIToolsToMCP converts OpenAI-format tool definitions to MCP
	// format for the bridge subprocess. Delegates to mcp.go in the kernel.
	ConvertOpenAIToolsToMCP(tools []json.RawMessage) []MCPTool
}

// HookResult represents the result of a hook dispatch
type HookResult struct {
	Decision          string `json:"decision"`                    // "allow" or "block"
	Reason            string `json:"reason,omitempty"`            // Why blocked
	Message           string `json:"message,omitempty"`           // Human-readable message
	Fallback          bool   `json:"fallback,omitempty"`          // Used default behavior
	AdditionalContext string `json:"additionalContext,omitempty"` // Context to inject (for PreInference)
}

// ContinuationState represents the eigenfield continuation state
type ContinuationState struct {
	SessionID          string `json:"session_id"`
	Timestamp          string `json:"timestamp"`
	Trigger            string `json:"trigger"`
	Focus              string `json:"focus"`
	ContinuationPrompt string `json:"continuation_prompt"`
}

// MCPTool represents a tool in MCP format (mirrors kernel's definition)
type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
}
