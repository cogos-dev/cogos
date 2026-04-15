package modality_test

import (
	"context"
	"testing"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestTextModule_Type(t *testing.T) {
	tm := modality.NewTextModule()
	if tm.Type() != modality.Text {
		t.Errorf("Type = %s, want text", tm.Type())
	}
}

func TestTextModule_Gate(t *testing.T) {
	tm := modality.NewTextModule()
	if tm.Gate() != nil {
		t.Error("text module should have nil gate")
	}
}

func TestTextModule_Lifecycle(t *testing.T) {
	tm := modality.NewTextModule()

	if tm.Health() != modality.StatusStopped {
		t.Errorf("initial health = %s, want stopped", tm.Health())
	}

	if err := tm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tm.Health() != modality.StatusHealthy {
		t.Errorf("health after start = %s, want healthy", tm.Health())
	}

	state := tm.State()
	if state.Modality != modality.Text {
		t.Errorf("State.Modality = %s, want text", state.Modality)
	}

	if err := tm.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if tm.Health() != modality.StatusStopped {
		t.Errorf("health after stop = %s, want stopped", tm.Health())
	}
}

func TestTextModule_Decode(t *testing.T) {
	tm := modality.NewTextModule()
	decoder := tm.Decoder()

	event, err := decoder.Decode([]byte("hello world"), modality.Text, "test")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.Content != "hello world" {
		t.Errorf("Content = %q, want %q", event.Content, "hello world")
	}
	if event.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", event.Confidence)
	}
}

func TestTextModule_Encode(t *testing.T) {
	tm := modality.NewTextModule()
	encoder := tm.Encoder()

	intent := &modality.CognitiveIntent{
		Modality: modality.Text,
		Content:  "response text",
	}
	output, err := encoder.Encode(intent)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(output.Data) != "response text" {
		t.Errorf("Data = %q, want %q", output.Data, "response text")
	}
	if output.MimeType != "text/plain" {
		t.Errorf("MimeType = %q, want %q", output.MimeType, "text/plain")
	}
}
