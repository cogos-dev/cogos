package modality_test

import (
	"encoding/json"
	"testing"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestWireMessage_RoundTrip(t *testing.T) {
	msg := &modality.WireMessage{
		ID:        "test-1",
		Type:      "request",
		Module:    "tts",
		Operation: "synthesize",
		Data: map[string]any{
			"text":  "hello",
			"voice": "bm_lewis",
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded modality.WireMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ID != msg.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, msg.ID)
	}
	if decoded.Type != msg.Type {
		t.Errorf("Type = %q, want %q", decoded.Type, msg.Type)
	}
	if decoded.Module != msg.Module {
		t.Errorf("Module = %q, want %q", decoded.Module, msg.Module)
	}
	if decoded.Operation != msg.Operation {
		t.Errorf("Operation = %q, want %q", decoded.Operation, msg.Operation)
	}
	text, _ := decoded.Data["text"].(string)
	if text != "hello" {
		t.Errorf("Data.text = %q, want %q", text, "hello")
	}
}

func TestWireMessage_ErrorFields(t *testing.T) {
	msg := &modality.WireMessage{
		ID:          "err-1",
		Type:        "error",
		Error:       "model not found",
		ErrorType:   "not_found",
		Recoverable: true,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded modality.WireMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !decoded.Recoverable {
		t.Error("Recoverable should be true")
	}
	if decoded.ErrorType != "not_found" {
		t.Errorf("ErrorType = %q, want %q", decoded.ErrorType, "not_found")
	}
}

func TestWireMessage_StreamingFields(t *testing.T) {
	msg := &modality.WireMessage{
		ID:    "stream-1",
		Type:  "response",
		Chunk: 3,
		Done:  true,
		Result: map[string]any{
			"audio_b64": "AAAA",
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded modality.WireMessage
	json.Unmarshal(data, &decoded)

	if decoded.Chunk != 3 {
		t.Errorf("Chunk = %d, want 3", decoded.Chunk)
	}
	if !decoded.Done {
		t.Error("Done should be true")
	}
}
