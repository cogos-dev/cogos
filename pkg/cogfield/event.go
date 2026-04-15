package cogfield

// SessionJSONLEvent is the union type for the two JSONL formats found in .cog/.state/events/.
type SessionJSONLEvent struct {
	// Flat format fields
	ID        string `json:"id,omitempty"`
	Seq       int    `json:"seq,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Ts        string `json:"ts,omitempty"`
	Type      string `json:"type,omitempty"`

	// Hashed payload envelope format
	HashedPayload *struct {
		Type      string                 `json:"type"`
		Timestamp string                 `json:"timestamp"`
		SessionID string                 `json:"session_id"`
		Data      map[string]interface{} `json:"data"`
	} `json:"hashed_payload,omitempty"`
	Metadata *struct {
		Seq int `json:"seq"`
	} `json:"metadata,omitempty"`

	// Reconciler/bridge format
	Data map[string]interface{} `json:"data,omitempty"`
}

// ExtractTimestamp pulls the timestamp from either JSONL format.
func ExtractTimestamp(evt SessionJSONLEvent) string {
	if evt.HashedPayload != nil && evt.HashedPayload.Timestamp != "" {
		return evt.HashedPayload.Timestamp
	}
	if evt.Ts != "" {
		return evt.Ts
	}
	return ""
}
