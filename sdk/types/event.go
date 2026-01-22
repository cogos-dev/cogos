package types

import (
	"encoding/json"
	"time"
)

// EventType defines the kind of event being logged.
type EventType string

// Core event types for the ledger.
const (
	// EventTypeMessage is a conversation message.
	EventTypeMessage EventType = "message"

	// EventTypeMutation is a workspace mutation.
	EventTypeMutation EventType = "mutation"

	// EventTypeSignal is a signal field change.
	EventTypeSignal EventType = "signal"

	// EventTypeSession is a session lifecycle event.
	EventTypeSession EventType = "session"

	// EventTypeCoherence is a coherence state change.
	EventTypeCoherence EventType = "coherence"

	// EventTypeError is an error event.
	EventTypeError EventType = "error"

	// EventTypeCustom is a user-defined event type.
	EventTypeCustom EventType = "custom"
)

// Event represents a single event in the ledger.
// Events are append-only and immutable once written.
type Event struct {
	// Seq is the monotonically increasing sequence number.
	Seq int64 `json:"seq"`

	// Type is the event type.
	Type EventType `json:"type"`

	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`

	// SessionID is the session that created this event.
	SessionID string `json:"session_id"`

	// Source is where the event originated (e.g., "cog-chat", "kernel").
	Source string `json:"source,omitempty"`

	// URI is the cog:// URI affected, if applicable.
	URI string `json:"uri,omitempty"`

	// Data contains type-specific event payload.
	Data json.RawMessage `json:"data,omitempty"`

	// Hash is the content hash of this event for verification.
	Hash string `json:"hash,omitempty"`

	// PrevHash is the hash of the previous event (for chaining).
	PrevHash string `json:"prev_hash,omitempty"`
}

// NewEvent creates a new event with the current timestamp.
func NewEvent(eventType EventType, sessionID string) *Event {
	return &Event{
		Type:      eventType,
		Timestamp: time.Now(),
		SessionID: sessionID,
	}
}

// WithSource sets the event source.
func (e *Event) WithSource(source string) *Event {
	e.Source = source
	return e
}

// WithURI sets the affected URI.
func (e *Event) WithURI(uri string) *Event {
	e.URI = uri
	return e
}

// WithData sets the event data payload.
func (e *Event) WithData(data any) *Event {
	if data == nil {
		e.Data = nil
		return e
	}
	jsonData, err := json.Marshal(data)
	if err == nil {
		e.Data = jsonData
	}
	return e
}

// GetData unmarshals the event data into the given value.
func (e *Event) GetData(v any) error {
	if e.Data == nil {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// MarshalJSON implements custom JSON marshaling.
func (e *Event) MarshalJSON() ([]byte, error) {
	type eventAlias Event
	return json.Marshal((*eventAlias)(e))
}

// ToJSONLine returns the event as a JSONL line (with newline).
func (e *Event) ToJSONLine() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// EventBatch is a collection of events.
type EventBatch struct {
	// Events is the list of events.
	Events []*Event `json:"events"`

	// FirstSeq is the sequence number of the first event.
	FirstSeq int64 `json:"first_seq"`

	// LastSeq is the sequence number of the last event.
	LastSeq int64 `json:"last_seq"`

	// SessionID is the session these events belong to.
	SessionID string `json:"session_id"`
}

// Count returns the number of events in the batch.
func (eb *EventBatch) Count() int {
	return len(eb.Events)
}

// EventFilter specifies criteria for querying events.
type EventFilter struct {
	// Types filters by event type.
	Types []EventType `json:"types,omitempty"`

	// SessionID filters by session.
	SessionID string `json:"session_id,omitempty"`

	// Source filters by event source.
	Source string `json:"source,omitempty"`

	// After filters events after this time.
	After time.Time `json:"after,omitempty"`

	// Before filters events before this time.
	Before time.Time `json:"before,omitempty"`

	// AfterSeq filters events after this sequence number.
	AfterSeq int64 `json:"after_seq,omitempty"`

	// Limit is the maximum number of events to return.
	Limit int `json:"limit,omitempty"`
}

// MessageEventData is the data payload for message events.
type MessageEventData struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
}

// MutationEventData is the data payload for mutation events.
type MutationEventData struct {
	Op       string `json:"op"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	BytesLen int    `json:"bytes_len,omitempty"`
}

// SignalEventData is the data payload for signal events.
type SignalEventData struct {
	Location string  `json:"location"`
	Type     string  `json:"type"`
	Strength float64 `json:"strength"`
	Action   string  `json:"action"` // "deposit" or "remove"
}

// SessionEventData is the data payload for session events.
type SessionEventData struct {
	Action string `json:"action"` // "start", "end", "checkpoint"
	Status string `json:"status,omitempty"`
}

// Crystal represents the Merkle root of a session's events.
type Crystal struct {
	// SessionID is the session this crystal belongs to.
	SessionID string `json:"session_id"`

	// MerkleRoot is the computed Merkle root hash.
	MerkleRoot string `json:"merkle_root"`

	// EventCount is the number of events in the session.
	EventCount int `json:"event_count"`

	// FirstSeq is the first event sequence number.
	FirstSeq int64 `json:"first_seq"`

	// LastSeq is the last event sequence number.
	LastSeq int64 `json:"last_seq"`

	// CreatedAt is when this crystal was computed.
	CreatedAt time.Time `json:"created_at"`

	// Verified is true if the crystal has been verified.
	Verified bool `json:"verified"`
}
