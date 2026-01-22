package sdk

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cogos-dev/cogos/sdk/types"
)

// --- Phase 2 Mutation Tests ---

func TestMutationWithMetadata(t *testing.T) {
	m := NewSetMutation([]byte("content"))
	m.WithMetadata("validate", false)
	m.WithMetadata("source", "test")

	if val, ok := m.Metadata["validate"].(bool); !ok || val {
		t.Errorf("Metadata[validate] = %v, want false", m.Metadata["validate"])
	}

	if val, ok := m.Metadata["source"].(string); !ok || val != "test" {
		t.Errorf("Metadata[source] = %v, want 'test'", m.Metadata["source"])
	}
}

func TestNewSetJSONMutation(t *testing.T) {
	data := map[string]any{
		"key":   "value",
		"count": 42,
	}

	m, err := NewSetJSONMutation(data)
	if err != nil {
		t.Fatalf("NewSetJSONMutation error: %v", err)
	}

	if !m.IsSet() {
		t.Error("Should be a Set mutation")
	}

	// Verify content is valid JSON
	var result map[string]any
	if err := json.Unmarshal(m.Content, &result); err != nil {
		t.Errorf("Content is not valid JSON: %v", err)
	}

	if result["key"] != "value" {
		t.Errorf("Content[key] = %v, want 'value'", result["key"])
	}
}

// --- Signal Type Tests ---

func TestSignalRelevance(t *testing.T) {
	now := time.Now()
	signal := &types.Signal{
		Location:    "test",
		Type:        "ACTIVE",
		DepositedAt: now,
		HalfLife:    4.0, // 4 hours
		Strength:    1.0,
	}

	// Fresh signal should have high relevance
	rel := signal.Relevance(now)
	if rel < 0.8 {
		t.Errorf("Fresh signal relevance = %f, want > 0.8", rel)
	}

	// After one half-life, relevance should be lower
	relAfter := signal.Relevance(now.Add(4 * time.Hour))
	if relAfter >= rel {
		t.Errorf("Decayed signal relevance %f should be less than fresh %f", relAfter, rel)
	}
}

func TestSignalIsActive(t *testing.T) {
	now := time.Now()
	signal := &types.Signal{
		Location:    "test",
		Type:        "ACTIVE",
		DepositedAt: now,
		HalfLife:    4.0,
		Strength:    1.0,
	}

	// Fresh signal should be active
	if !signal.IsActive(now) {
		t.Error("Fresh signal should be active")
	}

	// Very old signal should not be active
	if signal.IsActive(now.Add(100 * time.Hour)) {
		t.Error("Very old signal should not be active")
	}
}

func TestSignalSetActive(t *testing.T) {
	now := time.Now()
	ss := &types.SignalSet{
		Location: "test",
		Signals: []*types.Signal{
			{Location: "test", Type: "ACTIVE", DepositedAt: now, HalfLife: 4.0, Strength: 1.0},
			{Location: "test", Type: "OLD", DepositedAt: now.Add(-100 * time.Hour), HalfLife: 4.0, Strength: 1.0},
		},
		Timestamp: now,
	}

	active := ss.Active()
	if len(active) != 1 {
		t.Errorf("len(Active()) = %d, want 1", len(active))
	}
	if active[0].Type != "ACTIVE" {
		t.Errorf("Active signal type = %q, want 'ACTIVE'", active[0].Type)
	}
}

func TestSignalSetByType(t *testing.T) {
	now := time.Now()
	ss := &types.SignalSet{
		Location: "test",
		Signals: []*types.Signal{
			{Location: "test", Type: "ACTIVE", DepositedAt: now, HalfLife: 4.0, Strength: 1.0},
			{Location: "test", Type: "PENDING", DepositedAt: now, HalfLife: 4.0, Strength: 1.0},
			{Location: "test", Type: "ACTIVE", DepositedAt: now, HalfLife: 4.0, Strength: 0.5},
		},
		Timestamp: now,
	}

	active := ss.ByType("ACTIVE")
	if len(active) != 2 {
		t.Errorf("len(ByType('ACTIVE')) = %d, want 2", len(active))
	}

	pending := ss.ByType("PENDING")
	if len(pending) != 1 {
		t.Errorf("len(ByType('PENDING')) = %d, want 1", len(pending))
	}
}

// --- Thread Type Tests ---

func TestNewThread(t *testing.T) {
	thread := types.NewThread("test-thread")

	if thread.ID != "test-thread" {
		t.Errorf("ID = %q, want 'test-thread'", thread.ID)
	}

	if thread.Status != "active" {
		t.Errorf("Status = %q, want 'active'", thread.Status)
	}

	if thread.MessageCount() != 0 {
		t.Errorf("MessageCount = %d, want 0", thread.MessageCount())
	}

	if !thread.IsActive() {
		t.Error("New thread should be active")
	}
}

func TestThreadAppendMessage(t *testing.T) {
	thread := types.NewThread("test")
	msg := types.NewUserMessage("Hello")

	thread.AppendMessage(msg)

	if thread.MessageCount() != 1 {
		t.Errorf("MessageCount = %d, want 1", thread.MessageCount())
	}

	last := thread.LastMessage()
	if last.Content != "Hello" {
		t.Errorf("LastMessage().Content = %q, want 'Hello'", last.Content)
	}
}

func TestThreadLastN(t *testing.T) {
	thread := types.NewThread("test")
	for i := 0; i < 5; i++ {
		thread.AppendMessage(types.NewUserMessage("message"))
	}

	last3 := thread.LastN(3)
	if len(last3) != 3 {
		t.Errorf("len(LastN(3)) = %d, want 3", len(last3))
	}

	// Request more than available
	all := thread.LastN(10)
	if len(all) != 5 {
		t.Errorf("len(LastN(10)) = %d, want 5 (all messages)", len(all))
	}

	// Edge cases
	empty := thread.LastN(0)
	if len(empty) != 0 {
		t.Errorf("len(LastN(0)) = %d, want 0", len(empty))
	}
}

func TestThreadSummary(t *testing.T) {
	thread := types.NewThread("test")
	thread.Title = "Test Thread"
	thread.AppendMessage(types.NewUserMessage("Hello"))
	thread.AppendMessage(types.NewAssistantMessage("Hi"))

	summary := thread.ToSummary()

	if summary.ID != "test" {
		t.Errorf("ID = %q, want 'test'", summary.ID)
	}
	if summary.Title != "Test Thread" {
		t.Errorf("Title = %q, want 'Test Thread'", summary.Title)
	}
	if summary.MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2", summary.MessageCount)
	}
}

func TestMessageRoles(t *testing.T) {
	user := types.NewUserMessage("user message")
	if user.Role != types.MessageRoleUser {
		t.Errorf("User role = %q, want %q", user.Role, types.MessageRoleUser)
	}

	assistant := types.NewAssistantMessage("assistant message")
	if assistant.Role != types.MessageRoleAssistant {
		t.Errorf("Assistant role = %q, want %q", assistant.Role, types.MessageRoleAssistant)
	}

	system := types.NewSystemMessage("system message")
	if system.Role != types.MessageRoleSystem {
		t.Errorf("System role = %q, want %q", system.Role, types.MessageRoleSystem)
	}
}

func TestMessageWithMetadata(t *testing.T) {
	msg := types.NewUserMessage("test").
		WithMetadata("model", "opus-4.5").
		WithMetadata("tokens", 100)

	if msg.Metadata["model"] != "opus-4.5" {
		t.Errorf("Metadata[model] = %v, want 'opus-4.5'", msg.Metadata["model"])
	}
	if msg.Metadata["tokens"] != 100 {
		t.Errorf("Metadata[tokens] = %v, want 100", msg.Metadata["tokens"])
	}
}

// --- Event Type Tests ---

func TestNewEvent(t *testing.T) {
	event := types.NewEvent(types.EventTypeMessage, "session-123")

	if event.Type != types.EventTypeMessage {
		t.Errorf("Type = %q, want %q", event.Type, types.EventTypeMessage)
	}
	if event.SessionID != "session-123" {
		t.Errorf("SessionID = %q, want 'session-123'", event.SessionID)
	}
	if event.Timestamp.IsZero() {
		t.Error("Timestamp should be set")
	}
}

func TestEventWithData(t *testing.T) {
	event := types.NewEvent(types.EventTypeMessage, "session").
		WithSource("cog-chat").
		WithURI("cog://thread/current").
		WithData(types.MessageEventData{
			Role:    "user",
			Content: "Hello",
			Model:   "opus-4.5",
		})

	if event.Source != "cog-chat" {
		t.Errorf("Source = %q, want 'cog-chat'", event.Source)
	}
	if event.URI != "cog://thread/current" {
		t.Errorf("URI = %q, want 'cog://thread/current'", event.URI)
	}

	var data types.MessageEventData
	if err := event.GetData(&data); err != nil {
		t.Errorf("GetData error: %v", err)
	}
	if data.Role != "user" {
		t.Errorf("Data.Role = %q, want 'user'", data.Role)
	}
	if data.Content != "Hello" {
		t.Errorf("Data.Content = %q, want 'Hello'", data.Content)
	}
}

func TestEventToJSONLine(t *testing.T) {
	event := types.NewEvent(types.EventTypeMessage, "session")
	event.Seq = 1

	line, err := event.ToJSONLine()
	if err != nil {
		t.Fatalf("ToJSONLine error: %v", err)
	}

	// Should end with newline
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Error("JSONL line should end with newline")
	}

	// Should be valid JSON (without newline)
	var parsed types.Event
	if err := json.Unmarshal(line[:len(line)-1], &parsed); err != nil {
		t.Errorf("Line is not valid JSON: %v", err)
	}
	if parsed.Seq != 1 {
		t.Errorf("Seq = %d, want 1", parsed.Seq)
	}
}

func TestEventTypes(t *testing.T) {
	eventTypes := []types.EventType{
		types.EventTypeMessage,
		types.EventTypeMutation,
		types.EventTypeSignal,
		types.EventTypeSession,
		types.EventTypeCoherence,
		types.EventTypeError,
		types.EventTypeCustom,
	}

	for _, et := range eventTypes {
		if et == "" {
			t.Error("Event type should not be empty")
		}
	}
}

func TestEventBatchCount(t *testing.T) {
	batch := &types.EventBatch{
		Events: []*types.Event{
			types.NewEvent(types.EventTypeMessage, "s1"),
			types.NewEvent(types.EventTypeMessage, "s1"),
			types.NewEvent(types.EventTypeMessage, "s1"),
		},
		FirstSeq:  1,
		LastSeq:   3,
		SessionID: "s1",
	}

	if batch.Count() != 3 {
		t.Errorf("Count() = %d, want 3", batch.Count())
	}
}
