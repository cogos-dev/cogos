package modality_test

import (
	"context"
	"testing"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestBus_RegisterAndPerceive(t *testing.T) {
	bus := modality.NewBus()

	// Register text module.
	tm := modality.NewTextModule()
	if err := bus.Register(tm); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Start the bus.
	if err := bus.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer bus.Stop(context.Background())

	// Perceive text input.
	event, err := bus.Perceive([]byte("hello world"), modality.Text, "test-channel")
	if err != nil {
		t.Fatalf("Perceive: %v", err)
	}
	if event == nil {
		t.Fatal("Perceive returned nil event")
	}
	if event.Content != "hello world" {
		t.Errorf("Content = %q, want %q", event.Content, "hello world")
	}
	if event.Channel != "test-channel" {
		t.Errorf("Channel = %q, want %q", event.Channel, "test-channel")
	}
	if event.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", event.Confidence)
	}
}

func TestBus_RegisterDuplicate(t *testing.T) {
	bus := modality.NewBus()
	tm := modality.NewTextModule()
	if err := bus.Register(tm); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := bus.Register(modality.NewTextModule()); err == nil {
		t.Fatal("expected error on duplicate registration")
	}
}

func TestBus_PerceiveUnknownModality(t *testing.T) {
	bus := modality.NewBus()
	_, err := bus.Perceive([]byte("test"), modality.Voice, "ch")
	if err == nil {
		t.Fatal("expected error for unregistered modality")
	}
}

func TestBus_Act(t *testing.T) {
	bus := modality.NewBus()
	tm := modality.NewTextModule()
	bus.Register(tm)
	tm.Start(context.Background())
	defer tm.Stop(context.Background())

	intent := &modality.CognitiveIntent{
		Modality: modality.Text,
		Content:  "hello",
		Channel:  "test",
	}
	output, err := bus.Act(intent)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if string(output.Data) != "hello" {
		t.Errorf("Data = %q, want %q", output.Data, "hello")
	}
	if output.MimeType != "text/plain" {
		t.Errorf("MimeType = %q, want %q", output.MimeType, "text/plain")
	}
}

func TestBus_HUD(t *testing.T) {
	bus := modality.NewBus()
	bus.Register(modality.NewTextModule())
	bus.Start(context.Background())
	defer bus.Stop(context.Background())

	hud := bus.HUD()
	modules, ok := hud["modules"].(map[string]any)
	if !ok {
		t.Fatal("HUD missing modules")
	}
	if _, ok := modules["text"]; !ok {
		t.Error("HUD missing text module")
	}
}

func TestBus_Health(t *testing.T) {
	bus := modality.NewBus()
	tm := modality.NewTextModule()
	bus.Register(tm)

	// Before start: stopped.
	health := bus.Health()
	if health[modality.Text] != modality.StatusStopped {
		t.Errorf("Health before start = %s, want stopped", health[modality.Text])
	}

	// After start: healthy.
	bus.Start(context.Background())
	health = bus.Health()
	if health[modality.Text] != modality.StatusHealthy {
		t.Errorf("Health after start = %s, want healthy", health[modality.Text])
	}

	// After stop: stopped.
	bus.Stop(context.Background())
	health = bus.Health()
	if health[modality.Text] != modality.StatusStopped {
		t.Errorf("Health after stop = %s, want stopped", health[modality.Text])
	}
}

func TestBus_Order(t *testing.T) {
	bus := modality.NewBus()
	bus.Register(modality.NewTextModule())
	order := bus.Order()
	if len(order) != 1 || order[0] != modality.Text {
		t.Errorf("Order = %v, want [text]", order)
	}
}

func TestBus_LogEvent(t *testing.T) {
	bus := modality.NewBus()
	bus.Register(modality.NewTextModule())
	bus.Start(context.Background())

	// Trigger events via perceive.
	bus.Perceive([]byte("hello"), modality.Text, "ch1")

	hud := bus.HUD()
	events, ok := hud["recent_events"].([]map[string]any)
	if !ok {
		t.Fatal("HUD missing recent_events")
	}
	if len(events) == 0 {
		t.Error("expected at least one event after Perceive")
	}
}
