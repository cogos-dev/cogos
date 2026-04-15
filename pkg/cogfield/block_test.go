package cogfield

import (
	"encoding/json"
	"testing"
)

func TestBlockJSONRoundTrip(t *testing.T) {
	block := Block{
		V:        2,
		ID:       "block-1",
		BusID:    "bus-abc",
		Seq:      1,
		Ts:       "2026-04-14T12:00:00Z",
		From:     "agent-a",
		To:       "agent-b",
		Type:     "bus.message",
		Payload:  map[string]interface{}{"content": "hello"},
		Prev:     []string{"hash-0"},
		PrevHash: "hash-0",
		Hash:     "hash-1",
		Merkle:   "merkle-1",
		Size:     42,
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.V != 2 {
		t.Errorf("V = %d, want 2", decoded.V)
	}
	if decoded.BusID != "bus-abc" {
		t.Errorf("BusID = %q, want %q", decoded.BusID, "bus-abc")
	}
	if decoded.Hash != "hash-1" {
		t.Errorf("Hash = %q, want %q", decoded.Hash, "hash-1")
	}
	if len(decoded.Prev) != 1 || decoded.Prev[0] != "hash-0" {
		t.Errorf("Prev = %v, want [hash-0]", decoded.Prev)
	}
}

func TestBlockOmitsEmptyFields(t *testing.T) {
	block := Block{
		Ts:      "2026-04-14T12:00:00Z",
		From:    "agent-a",
		Type:    "bus.message",
		Payload: map[string]interface{}{},
		Hash:    "hash-1",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// These fields should be omitted when empty
	for _, field := range []string{"id", "bus_id", "to", "prev", "prev_hash", "merkle", "sig"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted when empty", field)
		}
	}
}
