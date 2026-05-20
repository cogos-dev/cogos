package acp

import "encoding/json"

// Claude Code's --output-format stream-json emits one JSON object per line.
// The top-level discriminator is the "type" field; "subtype" further refines
// for system/result frames. Only the frame shapes we actually consume are
// modeled here — unknown frames decode into UnknownEvent so the translator
// can skip them without erroring.

// EventType is the top-level "type" discriminator on a stream-json line.
type EventType string

const (
	EventSystem    EventType = "system"
	EventAssistant EventType = "assistant"
	EventUser      EventType = "user"
	EventResult    EventType = "result"
	// stream_event carries Anthropic's raw SSE deltas mid-turn.
	EventStream EventType = "stream_event"
)

// rawEvent is the wire envelope used for initial dispatch — every line is
// first decoded into this shape, then the "type" tag selects the concrete
// frame parser below.
type rawEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// SystemEvent is emitted at session start with init metadata, and at other
// substrate transitions. We currently only care about the init payload to
// learn the actual session_id claude is using.
type SystemEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id,omitempty"`
	// Other fields exist (cwd, model, tools, mcp_servers, ...). We don't
	// model them here — promote as needs surface.
}

// AssistantEvent carries a complete assistant message block at end-of-turn.
// stream-json typically emits incremental deltas via stream_event AND a
// final assistant frame summarising the turn. We treat the assistant frame
// as the authoritative end-of-turn signal.
type AssistantEvent struct {
	Type    string `json:"type"`
	Message struct {
		Role    string        `json:"role"`
		Content []ContentItem `json:"content"`
		// model, stop_reason, usage all present too — promote later.
	} `json:"message"`
	SessionID string `json:"session_id,omitempty"`
}

// ContentItem is one entry inside an assistant message's content array.
// We model text and tool_use here; thinking/redacted_thinking can be added
// when we wire thought-streaming.
type ContentItem struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use
}

// StreamEvent is the mid-turn delta carrier — Anthropic's SSE event shape
// pulled out of the streaming API and re-emitted as one NDJSON line per
// event.
//
// The .Event payload's "type" field tells you which delta kind it is
// (content_block_start, content_block_delta, message_stop, ...). For the
// spike we just expose the raw payload and let the translator filter.
type StreamEvent struct {
	Type      string          `json:"type"` // always "stream_event"
	Event     json.RawMessage `json:"event"`
	SessionID string          `json:"session_id,omitempty"`
}

// StreamDelta is the inner Anthropic SSE shape we care about for token-by-
// token rendering. Only content_block_delta carries usable text deltas at
// the rate we want to surface to the dashboard.
type StreamDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index,omitempty"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"delta"`
}

// ResultEvent terminates a turn. It carries the final text, usage, and any
// per-turn cost. We use this as the "end of turn" signal that closes the
// per-prompt receive loop.
type ResultEvent struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	IsError    bool   `json:"is_error,omitempty"`
	Result     string `json:"result,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	NumTurns   int    `json:"num_turns,omitempty"`
}

// UnknownEvent is the catch-all for frames we don't model yet. The
// translator skips these silently with a debug log; promoting one to a
// concrete type is purely additive.
type UnknownEvent struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// Event is the type-safe union the translator hands upstream. Exactly one
// of the pointer fields will be non-nil.
type Event struct {
	System    *SystemEvent
	Assistant *AssistantEvent
	Stream    *StreamEvent
	Result    *ResultEvent
	Unknown   *UnknownEvent
}

// ParseLine decodes one NDJSON line into the typed Event union. Returns an
// error only for malformed JSON; unknown frame types resolve to
// Event{Unknown: ...} so the caller can choose to ignore them.
func ParseLine(line []byte) (Event, error) {
	var probe rawEvent
	if err := json.Unmarshal(line, &probe); err != nil {
		return Event{}, err
	}
	switch EventType(probe.Type) {
	case EventSystem:
		var ev SystemEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Event{}, err
		}
		return Event{System: &ev}, nil
	case EventAssistant:
		var ev AssistantEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Event{}, err
		}
		return Event{Assistant: &ev}, nil
	case EventStream:
		var ev StreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Event{}, err
		}
		return Event{Stream: &ev}, nil
	case EventResult:
		var ev ResultEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Event{}, err
		}
		return Event{Result: &ev}, nil
	default:
		return Event{Unknown: &UnknownEvent{Type: probe.Type, Raw: append([]byte(nil), line...)}}, nil
	}
}

// PromptInput is one user message sent to claude via --input-format stream-json.
// The CLI expects the same shape as the Anthropic Messages API user message
// (one JSON object per line on stdin). We construct it here.
type PromptInput struct {
	Type    string    `json:"type"` // always "user"
	Message PromptMsg `json:"message"`
	Session string    `json:"session_id,omitempty"`
}

type PromptMsg struct {
	Role    string        `json:"role"`    // always "user"
	Content []ContentItem `json:"content"` // typically a single TextContent
}

// NewTextPrompt is a convenience constructor for the common case: one user
// text message, no images, no tool results. session_id is optional —
// claude infers it from --resume but stream-json input also accepts an
// explicit override.
func NewTextPrompt(text string) PromptInput {
	return PromptInput{
		Type: "user",
		Message: PromptMsg{
			Role:    "user",
			Content: []ContentItem{{Type: "text", Text: text}},
		},
	}
}
