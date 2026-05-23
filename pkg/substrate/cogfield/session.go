package cogfield

// SessionMessage represents a single event in a session conversation.
type SessionMessage struct {
	Seq       int                    `json:"seq"`
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	Meta      map[string]interface{} `json:"meta,omitempty"`
}

// SessionDetail is the response for GET /api/cogfield/sessions/{id}.
type SessionDetail struct {
	ID           string           `json:"id"`
	Created      string           `json:"created"`
	Modified     string           `json:"modified"`
	EventCount   int              `json:"event_count"`
	MessageCount int              `json:"message_count"`
	Messages     []SessionMessage `json:"messages"`
	Source       string           `json:"source,omitempty"`
}
