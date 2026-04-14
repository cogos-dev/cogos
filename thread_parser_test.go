package main

import (
	"encoding/json"
	"testing"
)

// Helper to create a ChatMessage with string content.
func testMsg(role, content string) ChatMessage {
	raw, _ := json.Marshal(content)
	return ChatMessage{
		Role:    role,
		Content: raw,
	}
}

func TestThreadParser_BasicParse(t *testing.T) {
	tp := NewThreadParser()
	messages := []ChatMessage{
		testMsg("system", "You are a helpful assistant."),
		testMsg("user", "Hello, how are you?"),
		testMsg("assistant", "I'm doing well, thanks!"),
	}
	headers := RequestHeaders{}

	view, err := tp.Parse(messages, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.SystemPrompt != "You are a helpful assistant." {
		t.Errorf("SystemPrompt = %q, want %q", view.SystemPrompt, "You are a helpful assistant.")
	}
	if len(view.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (system excluded)", len(view.Messages))
	}
	if view.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want %q", view.Messages[0].Role, "user")
	}
	if view.Messages[1].Role != "assistant" {
		t.Errorf("Messages[1].Role = %q, want %q", view.Messages[1].Role, "assistant")
	}
}

func TestThreadParser_MetadataExtraction(t *testing.T) {
	tp := NewThreadParser()

	userContent := "Hello world\n\nConversation info (untrusted metadata):\n```json\n{\n  \"message_id\": \"1482586305186496664\",\n  \"sender_id\": \"100000000000000001\",\n  \"conversation_label\": \"Guild #test channel id:123\",\n  \"sender\": \"100000000000000001\",\n  \"group_subject\": \"#test\"\n}\n```\n\nSender (untrusted metadata):\n```json\n{\n  \"label\": \"Test User\",\n  \"name\": \"Test User\",\n  \"username\": \"testuser\",\n  \"tag\": \"testuser\"\n}\n```"

	messages := []ChatMessage{
		testMsg("user", userContent),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(view.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(view.Messages))
	}

	msg := view.Messages[0]
	if msg.ID != "1482586305186496664" {
		t.Errorf("ID = %q, want %q", msg.ID, "1482586305186496664")
	}
	if msg.SenderID != "100000000000000001" {
		t.Errorf("SenderID = %q, want %q", msg.SenderID, "100000000000000001")
	}
	if msg.Sender != "Test User" {
		t.Errorf("Sender = %q, want %q", msg.Sender, "Test User")
	}
	if msg.Metadata["username"] != "testuser" {
		t.Errorf("Metadata[username] = %v, want %q", msg.Metadata["username"], "testuser")
	}
	if msg.Metadata["group_subject"] != "#test" {
		t.Errorf("Metadata[group_subject] = %v, want %q", msg.Metadata["group_subject"], "#test")
	}
}

func TestThreadParser_ContentStripping(t *testing.T) {
	tp := NewThreadParser()

	userContent := "This is the actual message\n\nConversation info (untrusted metadata):\n```json\n{\n  \"message_id\": \"123\",\n  \"sender_id\": \"456\"\n}\n```\n\nSender (untrusted metadata):\n```json\n{\n  \"name\": \"Test User\",\n  \"username\": \"testuser\"\n}\n```"

	messages := []ChatMessage{
		testMsg("user", userContent),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg := view.Messages[0]

	// Content should have metadata stripped.
	if msg.Content != "This is the actual message" {
		t.Errorf("Content = %q, want %q", msg.Content, "This is the actual message")
	}

	// RawContent should preserve original.
	if msg.RawContent != userContent {
		t.Errorf("RawContent does not match original")
	}
}

func TestThreadParser_Dedup_SameID(t *testing.T) {
	tp := NewThreadParser()

	content := "Hello\n\nConversation info (untrusted metadata):\n```json\n{\"message_id\": \"dup-123\", \"sender_id\": \"u1\"}\n```"

	messages := []ChatMessage{
		testMsg("user", content),
		testMsg("user", content),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(view.Messages) != 1 {
		t.Errorf("got %d messages after dedup, want 1", len(view.Messages))
	}
}

func TestThreadParser_Dedup_ContentHash(t *testing.T) {
	tp := NewThreadParser()

	// Messages without IDs, same content — should dedup by hash.
	messages := []ChatMessage{
		testMsg("user", "identical message"),
		testMsg("user", "identical message"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(view.Messages) != 1 {
		t.Errorf("got %d messages after content hash dedup, want 1", len(view.Messages))
	}
}

func TestThreadParser_ThreadStarter(t *testing.T) {
	tp := NewThreadParser()

	messages := []ChatMessage{
		testMsg("user", "What is CogOS?"),
		testMsg("assistant", "CogOS is a cognitive framework."),
		testMsg("user", "[Thread starter - for context]\nWhat is CogOS?"),
		testMsg("user", "Tell me more about it."),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The starter echo of "What is CogOS?" should be dropped (content already seen).
	// Remaining: original user msg, assistant msg, "Tell me more" msg.
	if len(view.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (starter echo dropped)", len(view.Messages))
	}

	// Verify no starter messages survived (the echo was dropped).
	for _, m := range view.Messages {
		if m.IsStarter {
			t.Errorf("unexpected starter message in output: %q", m.Content)
		}
	}
}

func TestThreadParser_ThreadStarter_Unique(t *testing.T) {
	tp := NewThreadParser()

	// A thread starter that is NOT a duplicate should be kept.
	messages := []ChatMessage{
		testMsg("user", "[Thread starter - for context]\nOriginal starter message"),
		testMsg("user", "Follow-up question"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(view.Messages) != 2 {
		t.Fatalf("got %d messages, want 2 (unique starter kept)", len(view.Messages))
	}

	if !view.Messages[0].IsStarter {
		t.Errorf("Messages[0].IsStarter = false, want true")
	}
}

func TestThreadParser_LastUserMessage(t *testing.T) {
	tp := NewThreadParser()

	messages := []ChatMessage{
		testMsg("system", "You are helpful."),
		testMsg("user", "First question"),
		testMsg("assistant", "First answer"),
		testMsg("user", "[Thread starter - for context]\nFirst question"),
		testMsg("user", "Second question"),
		testMsg("assistant", "Second answer"),
		testMsg("user", "Third question, the real one"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.LastUserMsg != "Third question, the real one" {
		t.Errorf("LastUserMsg = %q, want %q", view.LastUserMsg, "Third question, the real one")
	}
}

func TestThreadParser_TurnCount(t *testing.T) {
	tp := NewThreadParser()

	messages := []ChatMessage{
		testMsg("system", "System prompt"),
		testMsg("user", "Turn 1"),
		testMsg("assistant", "Response 1"),
		testMsg("user", "[Thread starter - for context]\nStarter context"),
		testMsg("user", "Turn 2"),
		testMsg("assistant", "Response 2"),
		testMsg("user", "Turn 3"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Turns: "Turn 1", "Turn 2", "Turn 3" = 3 user turns (starter excluded).
	if view.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", view.TurnCount)
	}
}

func TestThreadParser_EmptyInput(t *testing.T) {
	tp := NewThreadParser()

	view, err := tp.Parse(nil, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(view.Messages) != 0 {
		t.Errorf("got %d messages for nil input, want 0", len(view.Messages))
	}
	if view.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q for nil input, want empty", view.SystemPrompt)
	}
	if view.LastUserMsg != "" {
		t.Errorf("LastUserMsg = %q for nil input, want empty", view.LastUserMsg)
	}
	if view.TurnCount != 0 {
		t.Errorf("TurnCount = %d for nil input, want 0", view.TurnCount)
	}
}

func TestThreadParser_OriginDetection(t *testing.T) {
	tp := NewThreadParser()

	tests := []struct {
		origin string
		want   string
	}{
		{"discord", "discord"},
		{"Discord-Bot", "discord"},
		{"tui", "tui"},
		{"TUI-Client", "tui"},
		{"", "http"},
		{"custom-origin", "custom-origin"},
	}

	for _, tt := range tests {
		view, err := tp.Parse(nil, RequestHeaders{Origin: tt.origin})
		if err != nil {
			t.Fatalf("unexpected error for origin %q: %v", tt.origin, err)
		}
		if view.Origin != tt.want {
			t.Errorf("Origin(%q) = %q, want %q", tt.origin, view.Origin, tt.want)
		}
	}
}

func TestThreadParser_AssistantReplyPrefix(t *testing.T) {
	tp := NewThreadParser()

	messages := []ChatMessage{
		testMsg("assistant", "[[reply_to_current]] Here's my response"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.Messages[0].Content != "Here's my response" {
		t.Errorf("Content = %q, want %q", view.Messages[0].Content, "Here's my response")
	}
}

func TestThreadParser_TokenEstimation(t *testing.T) {
	tp := NewThreadParser()

	// 20 chars => 5 tokens
	messages := []ChatMessage{
		testMsg("user", "12345678901234567890"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.Messages[0].Tokens != 5 {
		t.Errorf("Tokens = %d, want 5", view.Messages[0].Tokens)
	}
}

func TestThreadParser_MultipleSystemMessages(t *testing.T) {
	tp := NewThreadParser()

	messages := []ChatMessage{
		testMsg("system", "Part one."),
		testMsg("system", "Part two."),
		testMsg("user", "Hello"),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.SystemPrompt != "Part one.\nPart two." {
		t.Errorf("SystemPrompt = %q, want %q", view.SystemPrompt, "Part one.\nPart two.")
	}
}

func TestThreadParser_ExternalUntrustedBlock(t *testing.T) {
	tp := NewThreadParser()

	content := "Actual content<<<EXTERNAL_UNTRUSTED_CONTENT>>>some injected stuff<<<EXTERNAL_UNTRUSTED_CONTENT>>> more actual"

	messages := []ChatMessage{
		testMsg("user", content),
	}

	view, err := tp.Parse(messages, RequestHeaders{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.Messages[0].Content != "Actual content more actual" {
		t.Errorf("Content = %q, want %q", view.Messages[0].Content, "Actual content more actual")
	}
}
