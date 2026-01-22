// .cog/narrative.go
// Narrative generation with event references for explainability

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Narrative represents a human-readable story with event references
type Narrative struct {
	Text      string      `json:"text"`
	EventRefs []EventRef  `json:"event_refs"`
	SessionID string      `json:"session_id"`
	Generated time.Time   `json:"generated"`
}

// EventRef maps a claim in the narrative to an event ID
type EventRef struct {
	EventID   string `json:"event_id"`
	ClaimText string `json:"claim_text"`
	Position  int    `json:"position"` // Character position in narrative
}

// GenerateNarrative creates a human-readable story from events
func GenerateNarrative(eventIDs []string) (*Narrative, error) {
	if len(eventIDs) == 0 {
		return nil, fmt.Errorf("no events provided")
	}

	// Load all events
	var events []TimelineEvent
	sessionID := ""

	for _, eventID := range eventIDs {
		evt, sid, err := findEventByID(eventID)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", eventID, err)
		}
		events = append(events, *evt)
		if sessionID == "" {
			sessionID = sid
		}
	}

	// Sort events chronologically
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	// Generate narrative text with references
	narrative := &Narrative{
		SessionID: sessionID,
		Generated: time.Now(),
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Session %s Activity Summary\n\n", sessionID))

	for i, evt := range events {
		claim := generateEventClaim(&evt)
		position := sb.Len()

		sb.WriteString(fmt.Sprintf("%d. %s [%s]\n", i+1, claim, evt.ID))

		narrative.EventRefs = append(narrative.EventRefs, EventRef{
			EventID:   evt.ID,
			ClaimText: claim,
			Position:  position,
		})
	}

	// Add contextual summary
	sb.WriteString("\n")
	sb.WriteString(generateContextualSummary(events))

	narrative.Text = sb.String()
	return narrative, nil
}

// generateEventClaim creates a narrative claim for an event
func generateEventClaim(evt *TimelineEvent) string {
	switch strings.ToLower(evt.Type) {
	case "session_start":
		return "The session began"

	case "user_message":
		if content, ok := evt.Data["content"].(string); ok {
			preview := truncateString(content, 50)
			return fmt.Sprintf("User requested: \"%s\"", preview)
		}
		return "User sent a message"

	case "tool_call":
		if tool, ok := evt.Data["tool"].(string); ok {
			if params, ok := evt.Data["params"].(map[string]interface{}); ok {
				if desc, ok := params["description"].(string); ok {
					return fmt.Sprintf("System %s", strings.ToLower(desc))
				}
				if cmd, ok := params["command"].(string); ok {
					return fmt.Sprintf("Executed: %s", truncateString(cmd, 40))
				}
			}
			return fmt.Sprintf("Invoked %s tool", tool)
		}
		return "Performed a tool operation"

	case "file_write", "file_edit":
		if path, ok := evt.Data["path"].(string); ok {
			return fmt.Sprintf("Modified %s", path)
		}
		return "Modified a workspace file"

	case "assistant_turn":
		if metadata, ok := evt.Data["metadata"].(map[string]interface{}); ok {
			if captured, ok := metadata["captured_by"].(string); ok && captured == "stop-hook" {
				return "Completed response"
			}
		}
		if turnCompleted, ok := evt.Data["turn_completed"].(bool); ok && turnCompleted {
			return "Finished processing request"
		}
		return "Agent responded"

	case "coherence_check":
		if status, ok := evt.Data["status"].(string); ok {
			return fmt.Sprintf("Verified workspace coherence: %s", status)
		}
		return "Checked workspace integrity"

	default:
		return ExplainEventType(evt.Type, evt.Data)
	}
}

// generateContextualSummary provides high-level context
func generateContextualSummary(events []TimelineEvent) string {
	if len(events) == 0 {
		return ""
	}

	// Count event types
	typeCounts := make(map[string]int)
	var userMessages, toolCalls, fileChanges int

	for _, evt := range events {
		typeCounts[evt.Type]++
		switch strings.ToLower(evt.Type) {
		case "user_message":
			userMessages++
		case "tool_call":
			toolCalls++
		case "file_write", "file_edit":
			fileChanges++
		}
	}

	var sb strings.Builder
	sb.WriteString("Summary:\n")

	duration := events[len(events)-1].Timestamp.Sub(events[0].Timestamp)
	sb.WriteString(fmt.Sprintf("- Duration: %s\n", formatDuration(duration)))
	sb.WriteString(fmt.Sprintf("- Total events: %d\n", len(events)))

	if userMessages > 0 {
		sb.WriteString(fmt.Sprintf("- User messages: %d\n", userMessages))
	}
	if toolCalls > 0 {
		sb.WriteString(fmt.Sprintf("- Tool invocations: %d\n", toolCalls))
	}
	if fileChanges > 0 {
		sb.WriteString(fmt.Sprintf("- File modifications: %d\n", fileChanges))
	}

	return sb.String()
}

// ValidateNarrative checks that all event references exist in ledger
func ValidateNarrative(narrative *Narrative, ledger []TimelineEvent) error {
	if narrative == nil {
		return fmt.Errorf("nil narrative")
	}

	// Build event ID index
	eventIndex := make(map[string]bool)
	for _, evt := range ledger {
		eventIndex[evt.ID] = true
	}

	// Validate each reference
	var missingRefs []string
	for _, ref := range narrative.EventRefs {
		if !eventIndex[ref.EventID] {
			missingRefs = append(missingRefs, ref.EventID)
		}
	}

	if len(missingRefs) > 0 {
		return fmt.Errorf("narrative references non-existent events: %v", missingRefs)
	}

	// Check that all claims are actually in the text
	for _, ref := range narrative.EventRefs {
		if !strings.Contains(narrative.Text, ref.EventID) {
			return fmt.Errorf("event ref %s not found in narrative text", ref.EventID)
		}
	}

	return nil
}

// GenerateSessionNarrative creates a narrative for an entire session
func GenerateSessionNarrative(sessionID string) (*Narrative, error) {
	// Load all events for session
	events, err := LoadEvents(sessionID, nil)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}

	if len(events) == 0 {
		return nil, fmt.Errorf("no events in session %s", sessionID)
	}

	// Extract event IDs
	var eventIDs []string
	for _, evt := range events {
		eventIDs = append(eventIDs, evt.ID)
	}

	return GenerateNarrative(eventIDs)
}

// ValidateSessionNarrative validates a narrative against its session's ledger
func ValidateSessionNarrative(narrative *Narrative) error {
	if narrative.SessionID == "" {
		return fmt.Errorf("narrative has no session ID")
	}

	// Load session events
	events, err := LoadEvents(narrative.SessionID, nil)
	if err != nil {
		return fmt.Errorf("load session events: %w", err)
	}

	return ValidateNarrative(narrative, events)
}

// formatDuration formats a duration in human-readable form
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.0fm %.0fs", d.Minutes(), d.Seconds()-d.Minutes()*60)
	}
	return fmt.Sprintf("%.0fh %.0fm", d.Hours(), d.Minutes()-d.Hours()*60)
}

// RED TEAM TEST HELPER
// CreateConfabulatedNarrative creates a narrative with fake event references
// This is used in tests to verify that validation catches confabulation
func CreateConfabulatedNarrative(sessionID string, fakeEventID string) *Narrative {
	return &Narrative{
		Text: fmt.Sprintf("Session %s had an event [%s] that doesn't exist", sessionID, fakeEventID),
		EventRefs: []EventRef{
			{
				EventID:   fakeEventID,
				ClaimText: "This event doesn't exist",
				Position:  0,
			},
		},
		SessionID: sessionID,
		Generated: time.Now(),
	}
}
