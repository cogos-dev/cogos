// .cog/timeline.go
// Timeline operations for human-readable event streams

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TimelineFilters defines criteria for filtering timeline events
type TimelineFilters struct {
	SessionID string
	EventType string
	Artifact  string // Filter by URI/artifact
	After     time.Time
	Before    time.Time
	Limit     int
}

// TimelineEvent wraps an Event with human-readable context
type TimelineEvent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"ts"`
	SessionID   string    `json:"session_id"`
	Description string    `json:"description"` // Human-readable summary
	Data        map[string]interface{} `json:"data,omitempty"`

	// For narrative references
	Seq         int64     `json:"seq"`
}

// RenderTimeline formats events in chronological order with readable formatting
func RenderTimeline(sessionID string, filters *TimelineFilters) (string, error) {
	events, err := LoadEvents(sessionID, filters)
	if err != nil {
		return "", fmt.Errorf("load events: %w", err)
	}

	if len(events) == 0 {
		return "No events found matching criteria\n", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Timeline: %s (%d events)\n", sessionID, len(events)))
	sb.WriteString(strings.Repeat("=", 80))
	sb.WriteString("\n\n")

	for _, evt := range events {
		sb.WriteString(FormatTimelineEvent(&evt))
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// FormatTimelineEvent formats a single event for display
func FormatTimelineEvent(evt *TimelineEvent) string {
	timeStr := evt.Timestamp.Format("15:04:05")

	// Build description
	desc := evt.Description
	if desc == "" {
		desc = ExplainEventType(evt.Type, evt.Data)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[%s] %s\n", timeStr, evt.Type))
	sb.WriteString(fmt.Sprintf("  ID: %s\n", evt.ID))
	sb.WriteString(fmt.Sprintf("  %s\n", desc))

	// Add key data fields
	if evt.Data != nil {
		for key, value := range evt.Data {
			if shouldShowField(key) {
				sb.WriteString(fmt.Sprintf("  - %s: %v\n", key, truncateValue(value)))
			}
		}
	}

	return sb.String()
}

// ExplainEventType generates a human-readable explanation for an event type
func ExplainEventType(eventType string, data map[string]interface{}) string {
	switch strings.ToLower(eventType) {
	case "session_start":
		return "Session started"
	case "user_message":
		if content, ok := data["content"].(string); ok {
			return fmt.Sprintf("User: %s", truncateString(content, 60))
		}
		return "User sent a message"
	case "tool_call":
		if tool, ok := data["tool"].(string); ok {
			return fmt.Sprintf("Tool called: %s", tool)
		}
		return "Tool invoked"
	case "assistant_turn":
		return "Assistant completed turn"
	case "file_write", "file_edit":
		if path, ok := data["path"].(string); ok {
			return fmt.Sprintf("Modified file: %s", filepath.Base(path))
		}
		return "Modified workspace file"
	case "coherence_check":
		if status, ok := data["status"].(string); ok {
			return fmt.Sprintf("Coherence check: %s", status)
		}
		return "Checked workspace coherence"
	default:
		return fmt.Sprintf("Event: %s", eventType)
	}
}

// LoadEvents loads events from ledger with optional filters
func LoadEvents(sessionID string, filters *TimelineFilters) ([]TimelineEvent, error) {
	ledgerPath := filepath.Join(".cog", "ledger", sessionID, "events.jsonl")

	file, err := os.Open(ledgerPath)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer file.Close()

	var events []TimelineEvent
	scanner := bufio.NewScanner(file)
	seq := int64(1)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			// Skip malformed lines
			continue
		}

		evt := parseTimelineEvent(raw, seq)
		seq++

		// Apply filters
		if filters != nil {
			if filters.EventType != "" && evt.Type != filters.EventType {
				continue
			}
			if filters.Artifact != "" {
				// Check if event references the artifact
				if uri, ok := raw["uri"].(string); ok {
					if !strings.Contains(uri, filters.Artifact) {
						continue
					}
				} else {
					continue
				}
			}
			if !filters.After.IsZero() && evt.Timestamp.Before(filters.After) {
				continue
			}
			if !filters.Before.IsZero() && evt.Timestamp.After(filters.Before) {
				continue
			}
		}

		events = append(events, evt)

		if filters != nil && filters.Limit > 0 && len(events) >= filters.Limit {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ledger: %w", err)
	}

	// Sort by timestamp
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	return events, nil
}

// parseTimelineEvent converts raw JSON to TimelineEvent
func parseTimelineEvent(raw map[string]interface{}, seq int64) TimelineEvent {
	evt := TimelineEvent{
		Seq: seq,
	}

	if id, ok := raw["id"].(string); ok {
		evt.ID = id
	}
	if typ, ok := raw["type"].(string); ok {
		evt.Type = typ
	}
	if sid, ok := raw["session_id"].(string); ok {
		evt.SessionID = sid
	}

	// Parse timestamp
	if tsStr, ok := raw["ts"].(string); ok {
		if ts, err := time.Parse(time.RFC3339, tsStr); err == nil {
			evt.Timestamp = ts
		}
	}

	// Extract data
	if data, ok := raw["data"].(map[string]interface{}); ok {
		evt.Data = data
	}

	return evt
}

// QueryEvents searches events by various criteria
func QueryEvents(query string) ([]TimelineEvent, error) {
	// Parse query string
	// Supported formats:
	//   "type:USER_MESSAGE" - filter by type
	//   "session:abc123" - filter by session
	//   "artifact:mem/semantic" - filter by URI/artifact
	//   "what changed file X?" - semantic search (simplified)

	parts := strings.Fields(query)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty query")
	}

	filters := &TimelineFilters{}
	var sessionID string

	for _, part := range parts {
		if strings.Contains(part, ":") {
			kv := strings.SplitN(part, ":", 2)
			switch strings.ToLower(kv[0]) {
			case "type":
				filters.EventType = kv[1]
			case "session":
				sessionID = kv[1]
				filters.SessionID = kv[1]
			case "artifact":
				filters.Artifact = kv[1]
			}
		}
	}

	// If no session specified, search all recent sessions
	if sessionID == "" {
		return searchAllSessions(filters, 100)
	}

	return LoadEvents(sessionID, filters)
}

// searchAllSessions searches across all sessions (limited)
func searchAllSessions(filters *TimelineFilters, limit int) ([]TimelineEvent, error) {
	ledgerDir := filepath.Join(".cog", "ledger")

	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		return nil, fmt.Errorf("read ledger dir: %w", err)
	}

	var allEvents []TimelineEvent

	// Sort sessions by modification time (most recent first)
	type sessionInfo struct {
		id      string
		modTime time.Time
	}
	var sessions []sessionInfo

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		eventsPath := filepath.Join(ledgerDir, entry.Name(), "events.jsonl")
		info, err := os.Stat(eventsPath)
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionInfo{
			id:      entry.Name(),
			modTime: info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})

	// Search recent sessions
	for i := 0; i < len(sessions) && i < 10; i++ {
		events, err := LoadEvents(sessions[i].id, filters)
		if err != nil {
			continue
		}
		allEvents = append(allEvents, events...)

		if len(allEvents) >= limit {
			break
		}
	}

	// Sort all by timestamp
	sort.Slice(allEvents, func(i, j int) bool {
		return allEvents[i].Timestamp.After(allEvents[j].Timestamp)
	})

	// Limit results
	if len(allEvents) > limit {
		allEvents = allEvents[:limit]
	}

	return allEvents, nil
}

// ExplainEvent generates a detailed human-readable explanation
func ExplainEvent(eventID string) (string, error) {
	// Find event across all sessions
	evt, sessionID, err := findEventByID(eventID)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Event: %s\n", evt.ID))
	sb.WriteString(strings.Repeat("=", 80))
	sb.WriteString("\n\n")

	sb.WriteString(fmt.Sprintf("Type: %s\n", evt.Type))
	sb.WriteString(fmt.Sprintf("Session: %s\n", sessionID))
	sb.WriteString(fmt.Sprintf("Timestamp: %s\n", evt.Timestamp.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Sequence: %d\n\n", evt.Seq))

	sb.WriteString("Description:\n")
	sb.WriteString(ExplainEventType(evt.Type, evt.Data))
	sb.WriteString("\n\n")

	if evt.Data != nil && len(evt.Data) > 0 {
		sb.WriteString("Data:\n")
		dataJSON, _ := json.MarshalIndent(evt.Data, "  ", "  ")
		sb.WriteString("  ")
		sb.WriteString(string(dataJSON))
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// findEventByID searches for an event across all sessions
func findEventByID(eventID string) (*TimelineEvent, string, error) {
	ledgerDir := filepath.Join(".cog", "ledger")

	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		return nil, "", fmt.Errorf("read ledger dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		events, err := LoadEvents(entry.Name(), nil)
		if err != nil {
			continue
		}

		for _, evt := range events {
			if evt.ID == eventID {
				return &evt, entry.Name(), nil
			}
		}
	}

	return nil, "", fmt.Errorf("event not found: %s", eventID)
}

// Helper functions

func shouldShowField(key string) bool {
	// Skip internal fields
	skip := map[string]bool{
		"trace_id":       true,
		"span_id":        true,
		"parent_span_id": true,
		"cascade_id":     true,
		"cascade_depth":  true,
	}
	return !skip[key]
}

func truncateValue(value interface{}) string {
	str := fmt.Sprintf("%v", value)
	return truncateString(str, 80)
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
