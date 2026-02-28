package main

// CogBlock type constants — canonical event types for the bus protocol.
const (
	// Tool-as-RPC
	BlockToolInvoke = "tool.invoke"
	BlockToolResult = "tool.result"

	// Chat lifecycle
	BlockChatRequest  = "chat.request"
	BlockChatResponse = "chat.response"
	BlockChatError    = "chat.error"
	BlockChatReset    = "chat.reset"
	BlockMessage      = "message"

	// Channel messages (bridged from OpenClaw via cogbus-message-bridge hook)
	// Actual type is "{channelId}.message" (e.g. "discord.message"), matched by suffix.
	BlockChannelMessageSuffix = ".message"

	// System lifecycle
	BlockSystemStartup  = "system.startup"
	BlockSystemShutdown = "system.shutdown"
	BlockSystemHealth   = "system.health"
)

// ToolInvokePayload is the bus event payload for tool invocation requests.
type ToolInvokePayload struct {
	RequestID   string         `json:"requestId"`
	Tool        string         `json:"tool"`
	Args        map[string]any `json:"args,omitempty"`
	CallerAgent string         `json:"callerAgent"`
	TargetAgent string         `json:"targetAgent,omitempty"` // empty = any capable agent
}

// ToolResultPayload is the bus event payload for tool invocation results.
type ToolResultPayload struct {
	RequestID  string `json:"requestId"`
	Result     any    `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	ExecutedBy string `json:"executedBy"`
	DurationMs int64  `json:"durationMs"`

	// Enrichment fields (V2) — echoed from tool.invoke for correlation
	Tool       string `json:"tool,omitempty"`     // tool name (echoed from invoke)
	Dispatch   string `json:"dispatch,omitempty"` // "builtin" or "bridge"
	ResultSize int    `json:"resultSize,omitempty"` // byte size of serialized result
}
