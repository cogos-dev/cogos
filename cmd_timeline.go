// .cog/cmd_timeline.go
// CLI commands for timeline and narrative operations

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmdEvents handles the `cog events` command
func cmdEvents(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog events {list|show|explain|query|narrative|tail|count|search|session|timeline|tools|stats|dead-rays|friction|cascade}")
	}

	subcommand := args[0]

	switch subcommand {
	// Timeline/narrative commands (existing)
	case "list":
		return cmdEventsList(args[1:])
	case "show":
		return cmdEventsShow(args[1:])
	case "explain":
		return cmdEventsExplain(args[1:])
	case "query":
		return cmdEventsQuery(args[1:])
	case "narrative":
		return cmdEventsNarrative(args[1:])
	// Shell-compatible JSONL query commands (new)
	case "tail":
		return cmdEventsTail(args[1:])
	case "count":
		return cmdEventsCount(args[1:])
	case "search":
		return cmdEventsSearch(args[1:])
	case "session":
		return cmdEventsSession(args[1:])
	case "timeline":
		return cmdEventsTimeline(args[1:])
	case "tools":
		return cmdEventsTools(args[1:])
	case "stats":
		return cmdEventsStats(args[1:])
	case "dead-rays":
		return cmdEventsDeadRays(args[1:])
	case "friction":
		return cmdEventsFriction(args[1:])
	case "cascade":
		return cmdEventsCascade(args[1:])
	default:
		return fmt.Errorf("unknown subcommand: %s", subcommand)
	}
}

// cmdEventsList lists events with optional filters
// Usage: cog events list [--session=X] [--type=Y] [--limit=N]
func cmdEventsList(args []string) error {
	filters := &TimelineFilters{
		Limit: 50, // Default limit
	}
	var sessionID string

	// Parse flags
	for _, arg := range args {
		if strings.HasPrefix(arg, "--session=") {
			sessionID = strings.TrimPrefix(arg, "--session=")
			filters.SessionID = sessionID
		} else if strings.HasPrefix(arg, "--type=") {
			filters.EventType = strings.TrimPrefix(arg, "--type=")
		} else if strings.HasPrefix(arg, "--limit=") {
			limitStr := strings.TrimPrefix(arg, "--limit=")
			var limit int
			if _, err := fmt.Sscanf(limitStr, "%d", &limit); err == nil {
				filters.Limit = limit
			}
		} else if strings.HasPrefix(arg, "--artifact=") {
			filters.Artifact = strings.TrimPrefix(arg, "--artifact=")
		}
	}

	// If no session specified, show recent events across all sessions
	var events []TimelineEvent
	var err error

	if sessionID == "" {
		events, err = searchAllSessions(filters, filters.Limit)
		if err != nil {
			return fmt.Errorf("search sessions: %w", err)
		}
		fmt.Printf("Recent events (limit %d):\n\n", filters.Limit)
	} else {
		events, err = LoadEvents(sessionID, filters)
		if err != nil {
			return fmt.Errorf("load events: %w", err)
		}
		fmt.Printf("Events for session %s:\n\n", sessionID)
	}

	if len(events) == 0 {
		fmt.Println("No events found")
		return nil
	}

	// Format as table
	fmt.Printf("%-20s %-20s %-30s %s\n", "TIMESTAMP", "ID", "TYPE", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 120))

	for _, evt := range events {
		timeStr := evt.Timestamp.Format("2006-01-02 15:04:05")
		idShort := truncateString(evt.ID, 20)
		typeStr := truncateString(evt.Type, 30)
		desc := truncateString(ExplainEventType(evt.Type, evt.Data), 50)

		fmt.Printf("%-20s %-20s %-30s %s\n", timeStr, idShort, typeStr, desc)
	}

	fmt.Printf("\nTotal: %d events\n", len(events))

	return nil
}

// cmdEventsShow shows detailed information about a specific event
// Usage: cog events show <event_id>
func cmdEventsShow(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog events show <event_id>")
	}

	eventID := args[0]

	evt, sessionID, err := findEventByID(eventID)
	if err != nil {
		return fmt.Errorf("find event: %w", err)
	}

	fmt.Printf("Event Details\n")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("\nID:        %s\n", evt.ID)
	fmt.Printf("Type:      %s\n", evt.Type)
	fmt.Printf("Session:   %s\n", sessionID)
	fmt.Printf("Timestamp: %s\n", evt.Timestamp.Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("Sequence:  %d\n", evt.Seq)

	if evt.Data != nil && len(evt.Data) > 0 {
		fmt.Printf("\nData:\n")
		dataJSON, err := json.MarshalIndent(evt.Data, "  ", "  ")
		if err == nil {
			fmt.Printf("  %s\n", string(dataJSON))
		}
	}

	return nil
}

// cmdEventsExplain provides human-readable explanation of an event
// Usage: cog events explain <event_id>
func cmdEventsExplain(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog events explain <event_id>")
	}

	eventID := args[0]
	explanation, err := ExplainEvent(eventID)
	if err != nil {
		return fmt.Errorf("explain event: %w", err)
	}

	fmt.Print(explanation)
	return nil
}

// cmdEventsQuery searches for events matching a query
// Usage: cog events query "type:USER_MESSAGE"
// Usage: cog events query "artifact:mem/semantic"
// Usage: cog events query "what changed file X?"
func cmdEventsQuery(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog events query <query>")
	}

	query := strings.Join(args, " ")
	events, err := QueryEvents(query)
	if err != nil {
		return fmt.Errorf("query events: %w", err)
	}

	if len(events) == 0 {
		fmt.Printf("No events found matching query: %s\n", query)
		return nil
	}

	fmt.Printf("Query: %s\n", query)
	fmt.Printf("Results: %d events\n\n", len(events))

	// Format as table
	fmt.Printf("%-20s %-20s %-30s %s\n", "TIMESTAMP", "ID", "TYPE", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 120))

	for _, evt := range events {
		timeStr := evt.Timestamp.Format("2006-01-02 15:04:05")
		idShort := truncateString(evt.ID, 20)
		typeStr := truncateString(evt.Type, 30)
		desc := truncateString(ExplainEventType(evt.Type, evt.Data), 50)

		fmt.Printf("%-20s %-20s %-30s %s\n", timeStr, idShort, typeStr, desc)
	}

	return nil
}

// cmdEventsNarrative generates a human-readable narrative
// Usage: cog events narrative <session_id>
// Usage: cog events narrative <event_id1> <event_id2> ...
func cmdEventsNarrative(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog events narrative <session_id_or_event_ids...>")
	}

	var narrative *Narrative
	var err error

	// Check if first arg looks like a session ID (UUID format)
	if len(args) == 1 && isSessionID(args[0]) {
		narrative, err = GenerateSessionNarrative(args[0])
		if err != nil {
			return fmt.Errorf("generate narrative: %w", err)
		}
	} else {
		// Treat args as event IDs
		narrative, err = GenerateNarrative(args)
		if err != nil {
			return fmt.Errorf("generate narrative: %w", err)
		}
	}

	// Display narrative
	fmt.Print(narrative.Text)
	fmt.Printf("\n--- Generated: %s ---\n", narrative.Generated.Format("2006-01-02 15:04:05"))
	fmt.Printf("Event References: %d\n", len(narrative.EventRefs))

	// Validate narrative
	if err := ValidateSessionNarrative(narrative); err != nil {
		fmt.Printf("\nWARNING: Narrative validation failed: %v\n", err)
	} else {
		fmt.Printf("\nNarrative validated successfully (all events exist)\n")
	}

	return nil
}

// isSessionID checks if a string looks like a session ID (UUID)
func isSessionID(s string) bool {
	// Simple heuristic: contains dashes and is about the right length
	return len(s) >= 32 && strings.Contains(s, "-")
}

// registerEventsCommands registers events commands in the main command dispatcher
func registerEventsCommands() {
	// This would be called from main() to register the events command
	// For now, we'll handle it directly in the command router
}
