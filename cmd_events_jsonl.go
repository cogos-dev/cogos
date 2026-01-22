// .cog/cmd_events_jsonl.go
// JSONL-based event query commands (migrated from events.sh)

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

// Simple event struct for JSONL queries (different from timeline.Event)
type JSONLEvent struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Seq          int                    `json:"seq"`
	Ts           string                 `json:"ts"`
	SessionID    string                 `json:"session_id"`
	ParentID     *string                `json:"parent_id,omitempty"`
	Data         map[string]interface{} `json:"data"`
	TraceID      string                 `json:"trace_id"`
	SpanID       string                 `json:"span_id"`
	ParentSpanID *string                `json:"parent_span_id,omitempty"`
	CascadeID    string                 `json:"cascade_id"`
	CascadeDepth int                    `json:"cascade_depth"`
}

// findEventFiles returns all event files sorted by modification time (newest first)
func findEventFiles(eventsDir string) ([]string, error) {
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var files []struct {
		path    string
		modTime time.Time
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(eventsDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, struct {
			path    string
			modTime time.Time
		}{path, info.ModTime()})
	}

	// Sort by modification time (newest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	result := make([]string, len(files))
	for i, f := range files {
		result[i] = f.path
	}

	return result, nil
}

// readJSONLEvents reads events from a file with optional filtering
func readJSONLEvents(path string, limit int, eventTypes []string) ([]JSONLEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []JSONLEvent
	scanner := bufio.NewScanner(file)

	// Increase buffer size for large events
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	typeFilter := make(map[string]bool)
	for _, t := range eventTypes {
		typeFilter[t] = true
	}

	for scanner.Scan() {
		var evt JSONLEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			// Skip malformed events
			continue
		}

		// Apply type filter if specified
		if len(typeFilter) > 0 && !typeFilter[evt.Type] {
			continue
		}

		events = append(events, evt)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Apply limit (tail semantics - last N events)
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}

	return events, nil
}

// cmdEventsTail shows the last N events
func cmdEventsTail(args []string) error {
	n := 10
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &n)
	}

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return fmt.Errorf("no event files found")
	}

	// Read from most recent file
	events, err := readJSONLEvents(files[0], n, nil)
	if err != nil {
		return err
	}

	// Output as JSONL
	for _, evt := range events {
		data, _ := json.Marshal(evt)
		fmt.Println(string(data))
	}

	return nil
}

// cmdEventsCount counts events by type
func cmdEventsCount(args []string) error {
	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("{}")
		return nil
	}

	counts := make(map[string]int)

	// Count from most recent file
	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	for _, evt := range events {
		counts[evt.Type]++
	}

	data, _ := json.MarshalIndent(counts, "", "  ")
	fmt.Println(string(data))

	return nil
}

// cmdEventsSearch searches events by content
func cmdEventsSearch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("query required")
	}
	query := strings.ToLower(args[0])

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return nil
	}

	// Search in most recent file
	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	for _, evt := range events {
		// Search in event type and data
		eventJSON, _ := json.Marshal(evt)
		if strings.Contains(strings.ToLower(string(eventJSON)), query) {
			fmt.Println(string(eventJSON))
		}
	}

	return nil
}

// cmdEventsSession shows current session info
func cmdEventsSession(args []string) error {
	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("{}")
		return nil
	}

	// Get session from most recent file
	events, err := readJSONLEvents(files[0], 1, nil)
	if err != nil {
		return err
	}

	if len(events) == 0 {
		fmt.Println("{}")
		return nil
	}

	info := map[string]interface{}{
		"session_id": events[0].SessionID,
		"file":       filepath.Base(files[0]),
	}

	data, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(data))

	return nil
}

// cmdEventsTimeline shows formatted timeline of recent events
func cmdEventsTimeline(args []string) error {
	n := 10
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &n)
	}

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return fmt.Errorf("no event files found")
	}

	events, err := readJSONLEvents(files[0], n, nil)
	if err != nil {
		return err
	}

	// ANSI color codes
	colors := map[string]string{
		"USER_MESSAGE":   "\033[0;34m", // Blue
		"TOOL_CALL":      "\033[0;33m", // Yellow
		"TOOL_RESULT":    "\033[0;32m", // Green
		"ASSISTANT_TURN": "\033[0;36m", // Cyan
		"ERROR":          "\033[0;31m", // Red
	}
	reset := "\033[0m"

	for _, evt := range events {
		// Extract time from timestamp
		ts := "??:??:??"
		if len(evt.Ts) > 19 {
			ts = evt.Ts[11:19]
		}

		color := colors[evt.Type]
		if color == "" {
			color = reset
		}

		fmt.Printf("%s %s%-15s%s\n", ts, color, evt.Type, reset)
	}

	return nil
}

// cmdEventsTools shows tool usage summary
func cmdEventsTools(args []string) error {
	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return nil
	}

	// Read last 1000 events
	events, err := readJSONLEvents(files[0], 1000, []string{"TOOL_CALL"})
	if err != nil {
		return err
	}

	counts := make(map[string]int)
	for _, evt := range events {
		if tool, ok := evt.Data["tool"].(string); ok {
			counts[tool]++
		}
	}

	// Sort by count
	type toolCount struct {
		tool  string
		count int
	}
	var sorted []toolCount
	for tool, count := range counts {
		sorted = append(sorted, toolCount{tool, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	for _, tc := range sorted {
		fmt.Printf("%4d  %s\n", tc.count, tc.tool)
	}

	return nil
}

// cmdEventsStats shows event statistics
func cmdEventsStats(args []string) error {
	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("No events found")
		return nil
	}

	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	counts := make(map[string]int)
	total := 0
	for _, evt := range events {
		counts[evt.Type]++
		total++
	}

	fmt.Println("Event Statistics")
	fmt.Println("================")
	fmt.Println()
	fmt.Printf("Total events: %d\n", total)
	fmt.Println()

	// Sort by count
	type stat struct {
		typ   string
		count int
	}
	var sorted []stat
	for typ, count := range counts {
		sorted = append(sorted, stat{typ, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	for _, s := range sorted {
		pct := 0.0
		if total > 0 {
			pct = float64(s.count) / float64(total) * 100
		}
		fmt.Printf("  %-20s %5d (%5.1f%%)\n", s.typ, s.count, pct)
	}

	return nil
}

// cmdEventsDeadRays shows dead rays (cascade_depth=1 events that triggered nothing)
func cmdEventsDeadRays(args []string) error {
	hours := 24
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &hours)
	}

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return nil
	}

	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	// Filter for dead rays (cascade_depth=1) in time window
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)

	for _, evt := range events {
		if evt.CascadeDepth != 1 {
			continue
		}

		// Parse timestamp
		ts, err := time.Parse(time.RFC3339, evt.Ts)
		if err != nil || ts.Before(cutoff) {
			continue
		}

		data, _ := json.Marshal(evt)
		fmt.Println(string(data))
	}

	return nil
}

// cmdEventsFriction shows friction report (dead rays grouped by type)
func cmdEventsFriction(args []string) error {
	hours := 24
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &hours)
	}

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("No friction detected")
		return nil
	}

	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	// Filter for dead rays in time window
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	counts := make(map[string]int)
	total := 0

	for _, evt := range events {
		if evt.CascadeDepth != 1 {
			continue
		}

		ts, err := time.Parse(time.RFC3339, evt.Ts)
		if err != nil || ts.Before(cutoff) {
			continue
		}

		counts[evt.Type]++
		total++
	}

	if total == 0 {
		fmt.Println("No friction detected")
		return nil
	}

	fmt.Printf("Friction Report: %d dead rays\n", total)
	fmt.Println(strings.Repeat("=", 50))

	// Sort by count
	type stat struct {
		typ   string
		count int
	}
	var sorted []stat
	for typ, count := range counts {
		sorted = append(sorted, stat{typ, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	for _, s := range sorted {
		pct := float64(s.count) / float64(total) * 100
		fmt.Printf("  %-30s %4d (%5.1f%%)\n", s.typ, s.count, pct)
	}

	return nil
}

// cmdEventsCascade traces a specific cascade by ID
func cmdEventsCascade(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("cascade ID required")
	}
	cascadeID := args[0]

	eventsDir, err := getEventsDir()
	if err != nil {
		return err
	}

	files, err := findEventFiles(eventsDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return nil
	}

	events, err := readJSONLEvents(files[0], 0, nil)
	if err != nil {
		return err
	}

	// Filter by cascade ID
	for _, evt := range events {
		if evt.CascadeID == cascadeID {
			data, _ := json.Marshal(evt)
			fmt.Println(string(data))
		}
	}

	return nil
}
