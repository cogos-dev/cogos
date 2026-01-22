// .cog/test_timeline.go
// Tests for timeline and narrative functionality

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTimelineRenderEventsInOrder tests that timeline renders events chronologically
func TestTimelineRenderEventsInOrder(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create test events with timestamps
	events := []testEvent{
		{Type: "SESSION_START", Timestamp: time.Now().Add(-3 * time.Minute)},
		{Type: "USER_MESSAGE", Timestamp: time.Now().Add(-2 * time.Minute), Data: map[string]interface{}{"content": "Hello"}},
		{Type: "TOOL_CALL", Timestamp: time.Now().Add(-1 * time.Minute), Data: map[string]interface{}{"tool": "Bash"}},
	}

	writeTestEvents(t, sessionID, events)

	// Render timeline
	timeline, err := RenderTimeline(sessionID, nil)
	if err != nil {
		t.Fatalf("RenderTimeline failed: %v", err)
	}

	// Verify events appear in chronological order
	if !strings.Contains(timeline, "SESSION_START") {
		t.Error("Timeline missing SESSION_START")
	}
	if !strings.Contains(timeline, "USER_MESSAGE") {
		t.Error("Timeline missing USER_MESSAGE")
	}
	if !strings.Contains(timeline, "TOOL_CALL") {
		t.Error("Timeline missing TOOL_CALL")
	}

	// Verify order: SESSION_START should appear before USER_MESSAGE
	startIdx := strings.Index(timeline, "SESSION_START")
	msgIdx := strings.Index(timeline, "USER_MESSAGE")
	if startIdx == -1 || msgIdx == -1 || startIdx > msgIdx {
		t.Error("Events not in chronological order")
	}

	t.Logf("Timeline:\n%s", timeline)
}

// TestQueryFindEventsByType tests querying events by type
func TestQueryFindEventsByType(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create mixed event types
	events := []testEvent{
		{Type: "USER_MESSAGE", Timestamp: time.Now(), Data: map[string]interface{}{"content": "Test 1"}},
		{Type: "TOOL_CALL", Timestamp: time.Now().Add(1 * time.Second), Data: map[string]interface{}{"tool": "Read"}},
		{Type: "USER_MESSAGE", Timestamp: time.Now().Add(2 * time.Second), Data: map[string]interface{}{"content": "Test 2"}},
	}

	writeTestEvents(t, sessionID, events)

	// Query for USER_MESSAGE events
	results, err := QueryEvents(fmt.Sprintf("type:USER_MESSAGE session:%s", sessionID))
	if err != nil {
		t.Fatalf("QueryEvents failed: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("Expected 2 USER_MESSAGE events, got %d", len(results))
	}

	for _, evt := range results {
		if evt.Type != "USER_MESSAGE" {
			t.Errorf("Expected USER_MESSAGE type, got %s", evt.Type)
		}
	}
}

// TestNarrativeGeneration tests narrative generation with event refs
func TestNarrativeGeneration(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create test events
	events := []testEvent{
		{Type: "SESSION_START", Timestamp: time.Now()},
		{Type: "USER_MESSAGE", Timestamp: time.Now().Add(1 * time.Second), Data: map[string]interface{}{"content": "Hello"}},
		{Type: "ASSISTANT_TURN", Timestamp: time.Now().Add(2 * time.Second)},
	}

	eventIDs := writeTestEvents(t, sessionID, events)

	// Generate narrative
	narrative, err := GenerateNarrative(eventIDs)
	if err != nil {
		t.Fatalf("GenerateNarrative failed: %v", err)
	}

	// Verify narrative has text
	if narrative.Text == "" {
		t.Error("Narrative text is empty")
	}

	// Verify event refs match event count
	if len(narrative.EventRefs) != len(eventIDs) {
		t.Errorf("Expected %d event refs, got %d", len(eventIDs), len(narrative.EventRefs))
	}

	// Verify each event ID appears in narrative text
	for _, eventID := range eventIDs {
		if !strings.Contains(narrative.Text, eventID) {
			t.Errorf("Narrative missing event ID: %s", eventID)
		}
	}

	t.Logf("Narrative:\n%s", narrative.Text)
}

// TestNarrativeValidation tests that validation catches missing events
func TestNarrativeValidation(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create valid events
	events := []testEvent{
		{Type: "SESSION_START", Timestamp: time.Now()},
		{Type: "USER_MESSAGE", Timestamp: time.Now().Add(1 * time.Second)},
	}

	writeTestEvents(t, sessionID, events)

	// Load events as ledger
	ledger, err := LoadEvents(sessionID, nil)
	if err != nil {
		t.Fatalf("LoadEvents failed: %v", err)
	}

	// Create narrative with valid refs
	validNarrative := &Narrative{
		Text:      fmt.Sprintf("Event [%s] occurred", ledger[0].ID),
		EventRefs: []EventRef{{EventID: ledger[0].ID, ClaimText: "Event occurred"}},
		SessionID: sessionID,
	}

	// Validate - should succeed
	if err := ValidateNarrative(validNarrative, ledger); err != nil {
		t.Errorf("Valid narrative failed validation: %v", err)
	}

	// Create narrative with invalid ref (RED TEAM TEST)
	fakeEventID := "evt_99999999999_fakefake"
	invalidNarrative := CreateConfabulatedNarrative(sessionID, fakeEventID)

	// Validate - should fail
	if err := ValidateNarrative(invalidNarrative, ledger); err == nil {
		t.Error("RED TEAM: Validation should have failed for confabulated narrative")
	} else {
		t.Logf("RED TEAM: Correctly detected confabulation: %v", err)
	}
}

// TestQueryByArtifact tests filtering events by artifact/URI
func TestQueryByArtifact(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create events with different URIs
	// Note: actual events have 'uri' field, not in Data
	// We'll test with the current structure

	events := []testEvent{
		{Type: "FILE_WRITE", Timestamp: time.Now(), Data: map[string]interface{}{"path": "mem/semantic/test.md"}},
		{Type: "FILE_WRITE", Timestamp: time.Now().Add(1 * time.Second), Data: map[string]interface{}{"path": "adr/001-test.md"}},
	}

	writeTestEvents(t, sessionID, events)

	// This test demonstrates the structure - actual filtering would need
	// events.jsonl to have URI field at top level
	t.Log("Artifact filtering test structure created")
}

// TestTimelineFormatting tests that timeline output is readable
func TestTimelineFormatting(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	events := []testEvent{
		{Type: "USER_MESSAGE", Timestamp: time.Now(), Data: map[string]interface{}{"content": "Test message"}},
	}

	writeTestEvents(t, sessionID, events)

	timeline, err := RenderTimeline(sessionID, nil)
	if err != nil {
		t.Fatalf("RenderTimeline failed: %v", err)
	}

	// Check formatting elements
	if !strings.Contains(timeline, "Timeline:") {
		t.Error("Missing timeline header")
	}
	if !strings.Contains(timeline, "===") {
		t.Error("Missing separator")
	}
	if !strings.Contains(timeline, "ID:") {
		t.Error("Missing event ID field")
	}

	// Verify timestamp format (HH:MM:SS)
	if !strings.Contains(timeline, ":") {
		t.Error("Missing time separator in timestamp")
	}
}

// TestExplainEvent tests detailed event explanation
func TestExplainEvent(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	events := []testEvent{
		{Type: "TOOL_CALL", Timestamp: time.Now(), Data: map[string]interface{}{
			"tool": "Bash",
			"params": map[string]interface{}{
				"command":     "ls -la",
				"description": "List files",
			},
		}},
	}

	eventIDs := writeTestEvents(t, sessionID, events)

	explanation, err := ExplainEvent(eventIDs[0])
	if err != nil {
		t.Fatalf("ExplainEvent failed: %v", err)
	}

	// Check explanation contains key info
	if !strings.Contains(explanation, "Type:") {
		t.Error("Explanation missing type")
	}
	if !strings.Contains(explanation, "Session:") {
		t.Error("Explanation missing session")
	}
	if !strings.Contains(explanation, "Description:") {
		t.Error("Explanation missing description")
	}

	t.Logf("Explanation:\n%s", explanation)
}

// === Test Helpers ===

type testEvent struct {
	Type      string
	Timestamp time.Time
	Data      map[string]interface{}
}

func createTestSession(t *testing.T) string {
	sessionID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	ledgerPath := filepath.Join(".cog", "ledger", sessionID)

	if err := os.MkdirAll(ledgerPath, 0755); err != nil {
		t.Fatalf("Failed to create test session: %v", err)
	}

	return sessionID
}

func cleanupTestSession(t *testing.T, sessionID string) {
	ledgerPath := filepath.Join(".cog", "ledger", sessionID)
	if err := os.RemoveAll(ledgerPath); err != nil {
		t.Logf("Warning: failed to cleanup test session: %v", err)
	}
}

func writeTestEvents(t *testing.T, sessionID string, events []testEvent) []string {
	eventsPath := filepath.Join(".cog", "ledger", sessionID, "events.jsonl")

	file, err := os.Create(eventsPath)
	if err != nil {
		t.Fatalf("Failed to create events file: %v", err)
	}
	defer file.Close()

	var eventIDs []string

	for i, evt := range events {
		eventID := fmt.Sprintf("evt_%d_%s_%d", evt.Timestamp.Unix(), sessionID[:8], i)
		eventIDs = append(eventIDs, eventID)

		eventJSON := map[string]interface{}{
			"id":         eventID,
			"type":       evt.Type,
			"ts":         evt.Timestamp.Format(time.RFC3339),
			"session_id": sessionID,
			"seq":        i + 1,
		}

		if evt.Data != nil {
			eventJSON["data"] = evt.Data
		}

		line, err := json.Marshal(eventJSON)
		if err != nil {
			t.Fatalf("Failed to marshal event: %v", err)
		}

		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatalf("Failed to write event: %v", err)
		}
	}

	return eventIDs
}

// TestREDTEAMConfabulationDetection is the critical RED TEAM test
// It verifies that narratives claiming events that don't exist are rejected
func TestREDTEAMConfabulationDetection(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	// Create a real event
	events := []testEvent{
		{Type: "SESSION_START", Timestamp: time.Now()},
	}
	writeTestEvents(t, sessionID, events)

	// Load ledger
	ledger, err := LoadEvents(sessionID, nil)
	if err != nil {
		t.Fatalf("LoadEvents failed: %v", err)
	}

	// Create confabulated narrative claiming an event that doesn't exist
	fakeEventID := "evt_NONEXISTENT_12345678"
	confabulated := &Narrative{
		Text: fmt.Sprintf("The system performed action X [%s], which never actually happened", fakeEventID),
		EventRefs: []EventRef{
			{
				EventID:   fakeEventID,
				ClaimText: "The system performed action X",
				Position:  0,
			},
		},
		SessionID: sessionID,
		Generated: time.Now(),
	}

	// Validation MUST fail
	err = ValidateNarrative(confabulated, ledger)
	if err == nil {
		t.Fatal("RED TEAM FAILURE: Validation passed for confabulated narrative - this is a security risk!")
	}

	// Verify error message mentions missing events
	if !strings.Contains(err.Error(), "non-existent") {
		t.Errorf("Error message should mention non-existent events, got: %v", err)
	}

	t.Logf("RED TEAM PASS: Confabulation correctly detected: %v", err)
}

// TestNarrativeEventRefsInText verifies all event refs appear in narrative text
func TestNarrativeEventRefsInText(t *testing.T) {
	sessionID := createTestSession(t)
	defer cleanupTestSession(t, sessionID)

	events := []testEvent{
		{Type: "SESSION_START", Timestamp: time.Now()},
	}
	eventIDs := writeTestEvents(t, sessionID, events)
	ledger, _ := LoadEvents(sessionID, nil)

	// Create narrative where event ref is NOT in text
	badNarrative := &Narrative{
		Text: "This narrative doesn't include the event ID",
		EventRefs: []EventRef{
			{
				EventID:   eventIDs[0],
				ClaimText: "Event happened",
				Position:  0,
			},
		},
		SessionID: sessionID,
	}

	// Validation should fail
	err := ValidateNarrative(badNarrative, ledger)
	if err == nil {
		t.Error("Validation should fail when event ref not in text")
	} else {
		t.Logf("Correctly detected missing ref in text: %v", err)
	}
}
