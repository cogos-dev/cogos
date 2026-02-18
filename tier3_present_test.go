package main

import (
	"strings"
	"testing"
)

func TestFormatPresentContext(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "Hello, can you help me?"),
		makeMsg("assistant", "Of course! What do you need help with?"),
		makeMsg("user", "I need to fix a bug in the parser"),
	}

	result, err := FormatPresentContext(messages, "parser, bug", "fix the parser bug", 0)
	if err != nil {
		t.Fatalf("FormatPresentContext error: %v", err)
	}

	if result == "" {
		t.Fatal("FormatPresentContext returned empty")
	}

	// Should contain conversation header
	if !strings.Contains(result, "Recent Conversation") {
		t.Error("Missing conversation header")
	}

	// Should contain user messages
	if !strings.Contains(result, "Hello, can you help me?") {
		t.Error("Missing first user message")
	}

	// Should contain coherence check
	if !strings.Contains(result, "Coherence Check") {
		t.Error("Missing coherence check section")
	}

	// Should contain anchor in coherence check
	if !strings.Contains(result, "parser, bug") {
		t.Error("Coherence check missing anchor")
	}

	// Should contain goal in coherence check
	if !strings.Contains(result, "fix the parser bug") {
		t.Error("Coherence check missing goal")
	}
}

func TestFormatPresentContextEmpty(t *testing.T) {
	result, err := FormatPresentContext([]ChatMessage{}, "", "", 0)
	if err != nil {
		t.Fatalf("FormatPresentContext error: %v", err)
	}

	// Should still have coherence check section even with no messages
	if !strings.Contains(result, "Coherence Check") {
		t.Error("Missing coherence check for empty messages")
	}
}

func TestWindowMessagesBackward(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "first message"),
		makeMsg("assistant", "first reply"),
		makeMsg("user", "second message"),
		makeMsg("assistant", "second reply"),
		makeMsg("user", "third message"),
	}

	// Small budget: should only get recent messages
	result := windowMessagesBackward(messages, 100)

	if len(result) == 0 {
		t.Fatal("windowMessagesBackward returned empty")
	}

	// Last message should be the most recent
	last := result[len(result)-1]
	if last.GetContent() != "third message" {
		t.Errorf("last message should be 'third message', got %q", last.GetContent())
	}
}

func TestWindowMessagesBackwardBudget(t *testing.T) {
	// Create messages with known sizes
	messages := []ChatMessage{
		makeMsg("user", strings.Repeat("a", 500)),
		makeMsg("user", strings.Repeat("b", 500)),
		makeMsg("user", strings.Repeat("c", 500)),
	}

	// Budget that can fit ~1 message
	result := windowMessagesBackward(messages, 600)

	// Should have at least the most recent message
	if len(result) == 0 {
		t.Fatal("Should have at least one message")
	}

	// Should not have all messages (budget too small)
	if len(result) == len(messages) {
		t.Error("Budget should have limited messages")
	}
}

func TestWindowMessagesBackwardEmpty(t *testing.T) {
	result := windowMessagesBackward(nil, 10000)
	if len(result) != 0 {
		t.Errorf("windowMessagesBackward(nil) returned %d messages, expected 0", len(result))
	}
}

func TestCoherenceCheckFormat(t *testing.T) {
	check := buildCoherenceCheck("reconciliation, drift", "implement drift detection")

	if !strings.Contains(check, "reconciliation, drift") {
		t.Error("coherence check missing anchor")
	}
	if !strings.Contains(check, "implement drift detection") {
		t.Error("coherence check missing goal")
	}
	if !strings.Contains(check, "Coherence Check") {
		t.Error("coherence check missing header")
	}
}

func TestCoherenceCheckEmptyAnchorGoal(t *testing.T) {
	check := buildCoherenceCheck("", "")

	if !strings.Contains(check, "analyzing") {
		t.Error("coherence check should show 'analyzing' for empty anchor/goal")
	}
}

func TestBudgetTooSmall(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "test"),
	}

	// Very small budget that can't fit coherence check
	_, err := FormatPresentContext(messages, "topic", "goal", 10)
	if err == nil {
		t.Error("Expected error for tiny budget, got nil")
	}
}

func TestFormatRoleLabel(t *testing.T) {
	tests := []struct {
		role     string
		expected string
	}{
		{"user", "User"},
		{"assistant", "Assistant"},
		{"system", "System"},
		{"custom", "Custom"},
		{"", "Unknown"},
	}

	for _, tt := range tests {
		result := formatRoleLabel(tt.role)
		if result != tt.expected {
			t.Errorf("formatRoleLabel(%q) = %q, expected %q", tt.role, result, tt.expected)
		}
	}
}

func TestEstimatePresentContextTokens(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "hello world"),
		makeMsg("assistant", "hi there"),
	}

	tokens := EstimatePresentContextTokens(messages)
	if tokens <= 0 {
		t.Errorf("EstimatePresentContextTokens returned %d, expected positive", tokens)
	}
}
