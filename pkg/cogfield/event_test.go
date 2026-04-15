package cogfield

import "testing"

func TestExtractTimestamp(t *testing.T) {
	// Hashed payload format
	evt := SessionJSONLEvent{
		HashedPayload: &struct {
			Type      string                 `json:"type"`
			Timestamp string                 `json:"timestamp"`
			SessionID string                 `json:"session_id"`
			Data      map[string]interface{} `json:"data"`
		}{
			Timestamp: "2026-04-14T12:00:00Z",
		},
	}
	if got := ExtractTimestamp(evt); got != "2026-04-14T12:00:00Z" {
		t.Errorf("hashed payload: got %q", got)
	}

	// Flat format
	evt = SessionJSONLEvent{
		Ts: "2026-04-14T13:00:00Z",
	}
	if got := ExtractTimestamp(evt); got != "2026-04-14T13:00:00Z" {
		t.Errorf("flat format: got %q", got)
	}

	// Empty
	evt = SessionJSONLEvent{}
	if got := ExtractTimestamp(evt); got != "" {
		t.Errorf("empty: got %q", got)
	}
}
