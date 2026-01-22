package types

import (
	"time"
)

// MessageRole defines who sent a message in a thread.
type MessageRole string

const (
	// MessageRoleUser is a message from the user.
	MessageRoleUser MessageRole = "user"

	// MessageRoleAssistant is a message from the assistant.
	MessageRoleAssistant MessageRole = "assistant"

	// MessageRoleSystem is a system message.
	MessageRoleSystem MessageRole = "system"

	// MessageRoleTool is a message from tool execution.
	MessageRoleTool MessageRole = "tool"
)

// Message represents a single message in a conversation thread.
type Message struct {
	// ID is the unique message identifier.
	ID string `json:"id,omitempty"`

	// Role is who sent the message (user, assistant, system, tool).
	Role MessageRole `json:"role"`

	// Content is the message text content.
	Content string `json:"content"`

	// Timestamp is when the message was created.
	Timestamp time.Time `json:"timestamp"`

	// ParentID is the ID of the parent message (for threading).
	ParentID string `json:"parent_id,omitempty"`

	// Metadata contains additional message data.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewMessage creates a new message with the current timestamp.
func NewMessage(role MessageRole, content string) *Message {
	return &Message{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	}
}

// NewUserMessage creates a new user message.
func NewUserMessage(content string) *Message {
	return NewMessage(MessageRoleUser, content)
}

// NewAssistantMessage creates a new assistant message.
func NewAssistantMessage(content string) *Message {
	return NewMessage(MessageRoleAssistant, content)
}

// NewSystemMessage creates a new system message.
func NewSystemMessage(content string) *Message {
	return NewMessage(MessageRoleSystem, content)
}

// WithMetadata adds metadata to the message.
func (m *Message) WithMetadata(key string, value any) *Message {
	if m.Metadata == nil {
		m.Metadata = make(map[string]any)
	}
	m.Metadata[key] = value
	return m
}

// Thread represents a conversation thread with messages.
type Thread struct {
	// ID is the unique thread identifier.
	ID string `json:"id"`

	// Title is an optional thread title.
	Title string `json:"title,omitempty"`

	// CreatedAt is when the thread was created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the thread was last updated.
	UpdatedAt time.Time `json:"updated_at"`

	// Messages is the list of messages in the thread.
	Messages []*Message `json:"messages"`

	// Status is the thread status (active, archived, etc.).
	Status string `json:"status,omitempty"`

	// Metadata contains additional thread data.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewThread creates a new empty thread.
func NewThread(id string) *Thread {
	now := time.Now()
	return &Thread{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  make([]*Message, 0),
		Status:    "active",
	}
}

// AppendMessage adds a message to the thread.
func (t *Thread) AppendMessage(msg *Message) {
	t.Messages = append(t.Messages, msg)
	t.UpdatedAt = time.Now()
}

// MessageCount returns the number of messages in the thread.
func (t *Thread) MessageCount() int {
	return len(t.Messages)
}

// LastMessage returns the most recent message, or nil if empty.
func (t *Thread) LastMessage() *Message {
	if len(t.Messages) == 0 {
		return nil
	}
	return t.Messages[len(t.Messages)-1]
}

// LastN returns the last N messages from the thread.
func (t *Thread) LastN(n int) []*Message {
	if n <= 0 || len(t.Messages) == 0 {
		return nil
	}
	if n >= len(t.Messages) {
		return t.Messages
	}
	return t.Messages[len(t.Messages)-n:]
}

// IsActive returns true if the thread is active.
func (t *Thread) IsActive() bool {
	return t.Status == "active" || t.Status == ""
}

// IsArchived returns true if the thread is archived.
func (t *Thread) IsArchived() bool {
	return t.Status == "archived"
}

// WithMetadata adds metadata to the thread.
func (t *Thread) WithMetadata(key string, value any) *Thread {
	if t.Metadata == nil {
		t.Metadata = make(map[string]any)
	}
	t.Metadata[key] = value
	return t
}

// ThreadSummary is a condensed view of a thread.
type ThreadSummary struct {
	// ID is the thread identifier.
	ID string `json:"id"`

	// Title is the thread title.
	Title string `json:"title,omitempty"`

	// MessageCount is the total number of messages.
	MessageCount int `json:"message_count"`

	// LastActivity is when the thread was last updated.
	LastActivity time.Time `json:"last_activity"`

	// Status is the thread status.
	Status string `json:"status"`
}

// ToSummary creates a summary of the thread.
func (t *Thread) ToSummary() *ThreadSummary {
	return &ThreadSummary{
		ID:           t.ID,
		Title:        t.Title,
		MessageCount: len(t.Messages),
		LastActivity: t.UpdatedAt,
		Status:       t.Status,
	}
}

// ThreadList is a collection of threads.
type ThreadList struct {
	// Threads is the list of thread summaries.
	Threads []*ThreadSummary `json:"threads"`

	// Total is the total count before pagination.
	Total int `json:"total"`

	// ActiveID is the currently active thread ID.
	ActiveID string `json:"active_id,omitempty"`
}
