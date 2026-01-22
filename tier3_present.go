// Tier 3: Present Context Formatter
//
// Formats recent conversation history with coherence check meta-prompt.
// This tier implements the "present moment" context for TAA (Temporal Attention Architecture).
//
// Budget: ~33k tokens (~150k characters)
// Strategy: Window backward from most recent message to fit budget
// Output: Formatted conversation + coherence check prompt

package main

import (
	"fmt"
	"strings"
)

// Token counting constants
const (
	// Approximate token-to-character ratio (conservative estimate)
	CharsPerToken = 4

	// Tier 3 token budget (33% of total 100k token budget)
	Tier3MaxTokens = 33000

	// Tier 3 character budget
	Tier3MaxChars = Tier3MaxTokens * CharsPerToken // ~132k chars
)

// FormatPresentContext formats recent conversation history within token budget.
//
// Algorithm:
// 1. Work backward from most recent message
// 2. Format each message with role label
// 3. Stop when budget would be exceeded
// 4. Add coherence check meta-prompt at the end
//
// Parameters:
// - messages: Full conversation history
// - anchor: Current conversation topic (from Tier 2 analysis)
// - goal: User's active goal (from Tier 2 analysis)
// - maxTokens: Token budget (0 = use default Tier3MaxTokens)
//
// Returns:
// - Formatted context string
// - Error if any
func FormatPresentContext(messages []ChatMessage, anchor string, goal string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = Tier3MaxTokens
	}
	maxChars := maxTokens * CharsPerToken

	// Build coherence check meta-prompt (always included)
	coherenceCheck := buildCoherenceCheck(anchor, goal)
	coherenceChars := len(coherenceCheck)

	// Reserve space for coherence check
	availableChars := maxChars - coherenceChars

	if availableChars < 1000 {
		return "", fmt.Errorf("insufficient budget for coherence check (need at least %d chars)", coherenceChars+1000)
	}

	// Window messages backward from most recent
	windowedMessages := windowMessagesBackward(messages, availableChars)

	// Format messages
	var formatted strings.Builder
	formatted.WriteString("# Recent Conversation\n\n")

	for i, msg := range windowedMessages {
		// Format role label
		roleLabel := formatRoleLabel(msg.Role)

		// Write message
		formatted.WriteString(fmt.Sprintf("[%s]: %s\n", roleLabel, msg.Content))

		// Add spacing between messages (except last)
		if i < len(windowedMessages)-1 {
			formatted.WriteString("\n")
		}
	}

	// Add coherence check at the end
	formatted.WriteString("\n")
	formatted.WriteString(coherenceCheck)

	return formatted.String(), nil
}

// windowMessagesBackward selects messages working backward from most recent
// that fit within the character budget.
func windowMessagesBackward(messages []ChatMessage, maxChars int) []ChatMessage {
	if len(messages) == 0 {
		return []ChatMessage{}
	}

	var selected []ChatMessage
	currentChars := 0

	// Work backward from most recent
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		// Estimate formatted size
		roleLabel := formatRoleLabel(msg.Role)
		msgSize := len(fmt.Sprintf("[%s]: %s\n\n", roleLabel, msg.Content))

		// Check if adding this message would exceed budget
		if currentChars+msgSize > maxChars && len(selected) > 0 {
			// Stop here - we have at least one message
			break
		}

		// Add message to front of selected list (we're going backward)
		selected = append([]ChatMessage{msg}, selected...)
		currentChars += msgSize

		// Safety check: if we have 100+ messages in window, that's probably enough
		if len(selected) >= 100 {
			break
		}
	}

	return selected
}

// formatRoleLabel converts message role to display label
func formatRoleLabel(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	default:
		// Capitalize first letter of unknown roles
		if len(role) > 0 {
			return strings.ToUpper(role[:1]) + role[1:]
		}
		return "Unknown"
	}
}

// buildCoherenceCheck constructs the coherence meta-prompt
func buildCoherenceCheck(anchor string, goal string) string {
	var check strings.Builder

	check.WriteString("---\n")
	check.WriteString("[Coherence Check]\n")

	// Include anchor if available
	if anchor != "" {
		check.WriteString(fmt.Sprintf("Current topic: %s\n", anchor))
	} else {
		check.WriteString("Current topic: (analyzing...)\n")
	}

	// Include goal if available
	if goal != "" {
		check.WriteString(fmt.Sprintf("User's goal: %s\n", goal))
	} else {
		check.WriteString("User's goal: (analyzing...)\n")
	}

	// Add instruction
	check.WriteString("\nInstruction: Stay focused on this topic and goal. Avoid drifting into tangential exploration.\n")
	check.WriteString("---\n")

	return check.String()
}

// EstimatePresentContextTokens estimates token count for given messages
// This is a utility function for testing and debugging.
func EstimatePresentContextTokens(messages []ChatMessage) int {
	totalChars := 0

	for _, msg := range messages {
		roleLabel := formatRoleLabel(msg.Role)
		msgSize := len(fmt.Sprintf("[%s]: %s\n\n", roleLabel, msg.Content))
		totalChars += msgSize
	}

	// Add coherence check overhead (estimate ~500 chars)
	totalChars += 500

	return totalChars / CharsPerToken
}
