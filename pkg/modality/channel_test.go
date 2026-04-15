package modality_test

import (
	"testing"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestChannelRegistry_RegisterAndGet(t *testing.T) {
	reg := modality.NewChannelRegistry()

	desc := &modality.ChannelDescriptor{
		ID:        "discord-text",
		Transport: "openclaw-gateway",
		Input:     []modality.ModalityType{modality.Text},
		Output:    []modality.ModalityType{modality.Text, modality.Voice},
	}

	if err := reg.Register(desc); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := reg.Get("discord-text")
	if !ok {
		t.Fatal("Get returned false")
	}
	if got.Transport != "openclaw-gateway" {
		t.Errorf("Transport = %q, want %q", got.Transport, "openclaw-gateway")
	}
}

func TestChannelRegistry_RegisterDuplicate(t *testing.T) {
	reg := modality.NewChannelRegistry()
	desc := &modality.ChannelDescriptor{ID: "ch1"}
	reg.Register(desc)
	if err := reg.Register(&modality.ChannelDescriptor{ID: "ch1"}); err == nil {
		t.Fatal("expected error on duplicate registration")
	}
}

func TestChannelRegistry_SessionBinding(t *testing.T) {
	reg := modality.NewChannelRegistry()

	desc := &modality.ChannelDescriptor{
		ID:     "ch-voice",
		Output: []modality.ModalityType{modality.Voice},
	}
	reg.Register(desc)

	// Bind to session.
	if err := reg.BindToSession("ch-voice", "session-1"); err != nil {
		t.Fatalf("BindToSession: %v", err)
	}

	// Check SupportsModality.
	matches := reg.SupportsModality("session-1", modality.Voice)
	if len(matches) != 1 {
		t.Fatalf("SupportsModality returned %d, want 1", len(matches))
	}

	// Text should not match.
	textMatches := reg.SupportsModality("session-1", modality.Text)
	if len(textMatches) != 0 {
		t.Errorf("SupportsModality(text) returned %d, want 0", len(textMatches))
	}

	// Unbind.
	if err := reg.UnbindFromSession("ch-voice", "session-1"); err != nil {
		t.Fatalf("UnbindFromSession: %v", err)
	}
	matches = reg.SupportsModality("session-1", modality.Voice)
	if len(matches) != 0 {
		t.Errorf("SupportsModality after unbind returned %d, want 0", len(matches))
	}
}

func TestChannelRegistry_Unregister(t *testing.T) {
	reg := modality.NewChannelRegistry()
	desc := &modality.ChannelDescriptor{ID: "ch1"}
	reg.Register(desc)
	reg.BindToSession("ch1", "s1")

	if err := reg.Unregister("ch1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	if _, ok := reg.Get("ch1"); ok {
		t.Error("channel should be removed after Unregister")
	}
}

func TestChannelDescriptor_SupportsOutput(t *testing.T) {
	desc := &modality.ChannelDescriptor{
		Output: []modality.ModalityType{modality.Text, modality.Voice},
	}
	if !desc.SupportsOutput(modality.Voice) {
		t.Error("should support voice")
	}
	if desc.SupportsOutput(modality.Vision) {
		t.Error("should not support vision")
	}
}

func TestChannelRegistry_Snapshot(t *testing.T) {
	reg := modality.NewChannelRegistry()
	reg.Register(&modality.ChannelDescriptor{ID: "a"})
	reg.Register(&modality.ChannelDescriptor{ID: "b"})

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Errorf("Snapshot has %d entries, want 2", len(snap))
	}
}

func TestChannelRegistry_ChannelsForSession(t *testing.T) {
	reg := modality.NewChannelRegistry()
	reg.Register(&modality.ChannelDescriptor{ID: "a"})
	reg.Register(&modality.ChannelDescriptor{ID: "b"})
	reg.BindToSession("a", "s1")
	reg.BindToSession("b", "s1")

	chs := reg.ChannelsForSession("s1")
	if len(chs) != 2 {
		t.Errorf("ChannelsForSession returned %d, want 2", len(chs))
	}
}
