// stream.go defines streaming types and Claude CLI wire format types.
//
// StreamChunkInference is the harness's unified streaming event. It carries
// text deltas, tool calls, tool results, usage data, and session metadata
// through a single channel from RunInferenceStream to the caller.
//
// The Claude* types (ClaudeStreamMessage, ClaudeMessage, ClaudeContent, etc.)
// represent the JSON wire format emitted by `claude --output-format stream-json`.
// The OpenAI* types represent the wire format for HTTP provider communication.
package harness

import "encoding/json"

// StreamChunkInference represents a single chunk in a streaming response.
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

// ClaudeStreamMessage represents a message from the Claude CLI stream-json output
type ClaudeStreamMessage struct {
	Type             string           `json:"type"`
	Subtype          string           `json:"subtype,omitempty"`
	Message          *ClaudeMessage   `json:"message,omitempty"`
	Result           string           `json:"result,omitempty"`
	StructuredOutput json.RawMessage  `json:"structured_output,omitempty"`
	Usage            *ClaudeUsage     `json:"usage,omitempty"`
	ToolUseResult    *ToolUseResultEx `json:"tool_use_result,omitempty"`
}

// ToolUseResultEx contains extended tool result info from Claude CLI
type ToolUseResultEx struct {
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	IsImage     bool   `json:"isImage,omitempty"`
}

// ClaudeMessage represents the nested message in assistant responses
type ClaudeMessage struct {
	Content    []ClaudeContent `json:"content,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      *ClaudeUsage    `json:"usage,omitempty"`
}

// ClaudeContent represents a content block in the message
type ClaudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // For tool_use blocks
	Name      string          `json:"name,omitempty"`        // For tool_use blocks
	Input     json.RawMessage `json:"input,omitempty"`       // For tool_use blocks
	ToolUseID string          `json:"tool_use_id,omitempty"` // For tool_result blocks
	Content   string          `json:"content,omitempty"`     // For tool_result blocks
	IsError   bool            `json:"is_error,omitempty"`    // For tool_result blocks
}

// ClaudeUsage represents token usage info from Claude CLI output.
// The cache fields are populated in both result messages and message_start events.
type ClaudeUsage struct {
	InputTokens       int     `json:"input_tokens,omitempty"`
	OutputTokens      int     `json:"output_tokens,omitempty"`
	CacheReadTokens   int     `json:"cache_read_input_tokens,omitempty"`
	CacheCreateTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
}

// OpenAI-compatible wire types for HTTP provider communication

// OpenAIChatRequest is the request format for OpenAI-compatible APIs
type OpenAIChatRequest struct {
	Model         string              `json:"model"`
	Messages      []OpenAIChatMessage `json:"messages"`
	MaxTokens     *int                `json:"max_tokens,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	Stream        bool                `json:"stream"`
	StreamOptions *StreamOptions      `json:"stream_options,omitempty"`
}

// StreamOptions controls streaming behavior (OpenAI extension).
// Setting IncludeUsage causes the final chunk to contain token usage.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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
	Usage *OpenAIUsage `json:"usage,omitempty"`
}

// OpenAIUsage represents the usage object in an OpenAI streaming chunk.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
