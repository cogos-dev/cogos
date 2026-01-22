// Tier 2: Temporal Context Retriever
//
// Analyzes conversation history to extract temporal context:
// - Conversation anchor (current topic)
// - Active goal (what user is trying to accomplish)
// - Working memory (session state from eidolon)
//
// Budget: ~25k tokens (~100k characters)
// Strategy: Extract signals from recent messages, load working memory

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Token counting constants for Tier 2
const (
	// Tier 2 token budget (25% of total 100k token budget)
	Tier2MaxTokens = 25000

	// Tier 2 character budget
	Tier2MaxChars = Tier2MaxTokens * CharsPerToken // ~100k chars
)

// Common words to filter out when extracting topics
var stopWords = map[string]bool{
	// Articles
	"a": true, "an": true, "the": true,
	// Pronouns
	"i": true, "you": true, "he": true, "she": true, "it": true, "we": true, "they": true,
	"me": true, "him": true, "her": true, "us": true, "them": true,
	"my": true, "your": true, "his": true, "its": true, "our": true, "their": true,
	"this": true, "that": true, "these": true, "those": true,
	"what": true, "which": true, "who": true, "whom": true, "whose": true,
	// Common verbs
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "having": true,
	"do": true, "does": true, "did": true, "doing": true,
	"will": true, "would": true, "could": true, "should": true, "may": true, "might": true, "must": true,
	"can": true, "shall": true,
	"get": true, "got": true, "make": true, "made": true, "let": true,
	// Prepositions
	"in": true, "on": true, "at": true, "to": true, "for": true, "with": true, "by": true,
	"from": true, "of": true, "about": true, "into": true, "through": true, "during": true,
	"before": true, "after": true, "above": true, "below": true, "between": true, "under": true,
	// Conjunctions
	"and": true, "or": true, "but": true, "if": true, "because": true, "as": true, "while": true,
	"although": true, "though": true, "unless": true, "until": true, "when": true, "where": true,
	// Common words
	"just": true, "also": true, "so": true, "then": true, "than": true, "now": true,
	"here": true, "there": true, "very": true, "too": true, "not": true, "no": true, "yes": true,
	"please": true, "thanks": true, "thank": true, "okay": true, "ok": true,
	"want": true, "need": true, "know": true, "think": true, "see": true, "look": true,
	"like": true, "use": true, "way": true, "thing": true, "something": true, "anything": true,
	"some": true, "any": true, "all": true, "each": true, "every": true, "both": true,
	"more": true, "most": true, "other": true, "same": true, "different": true,
	"first": true, "last": true, "next": true, "new": true, "old": true,
	"good": true, "bad": true, "right": true, "wrong": true,
	"how": true, "why": true,
}

// Goal detection patterns (compiled once)
var goalPatterns = []*regexp.Regexp{
	// Direct requests
	regexp.MustCompile(`(?i)\bhelp\s+(?:me\s+)?(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bi\s+want\s+to\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bi\s+need\s+to\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bi(?:'m|\s+am)\s+trying\s+to\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bcan\s+you\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bcould\s+you\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bplease\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\blet(?:'s|s)\s+(\w+(?:\s+\w+){0,5})`),
	// Questions
	regexp.MustCompile(`(?i)\bhow\s+(?:do|can|should|would)\s+(?:i|we)\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bhow\s+to\s+(\w+(?:\s+\w+){0,5})`),
	regexp.MustCompile(`(?i)\bwhat(?:'s|s|\s+is)\s+the\s+(?:best\s+)?way\s+to\s+(\w+(?:\s+\w+){0,5})`),
}

// Word extraction pattern
var wordPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9_-]*`)

// RetrieveTemporalContext analyzes message history to extract temporal signals.
//
// Parameters:
// - messages: Full conversation history
// - sessionID: Current session identifier
// - workspaceRoot: Root path of the workspace
// - maxTokens: Token budget (0 = use default Tier2MaxTokens)
//
// Returns:
// - context: Formatted temporal context string
// - anchor: Extracted conversation topic
// - goal: Inferred user goal
// - error: Any error encountered
func RetrieveTemporalContext(messages []ChatMessage, sessionID string, workspaceRoot string, maxTokens int) (context string, anchor string, goal string, err error) {
	if maxTokens <= 0 {
		maxTokens = Tier2MaxTokens
	}
	maxChars := maxTokens * CharsPerToken

	// Extract anchor (current topic) from recent user messages
	anchor = extractAnchor(messages)

	// Extract goal from recent user messages
	goal = extractGoal(messages)

	// Load working memory
	workingMemory := loadWorkingMemory(workspaceRoot)

	// Build temporal context
	var builder strings.Builder

	builder.WriteString("# Temporal Context\n\n")

	// Conversation anchor
	builder.WriteString("## Current Focus\n")
	if anchor != "" {
		builder.WriteString("Topic: ")
		builder.WriteString(anchor)
		builder.WriteString("\n")
	} else {
		builder.WriteString("Topic: (new conversation)\n")
	}
	builder.WriteString("\n")

	// Active goal
	builder.WriteString("## User Intent\n")
	if goal != "" {
		builder.WriteString("Goal: ")
		builder.WriteString(goal)
		builder.WriteString("\n")
	} else {
		builder.WriteString("Goal: (exploring/discussing)\n")
	}
	builder.WriteString("\n")

	// Working memory (session state)
	if workingMemory != "" {
		builder.WriteString("## Session State\n")
		builder.WriteString(workingMemory)
		builder.WriteString("\n")
	}

	context = builder.String()

	// Truncate if over budget
	if len(context) > maxChars {
		context = context[:maxChars]
	}

	return context, anchor, goal, nil
}

// extractAnchor analyzes recent user messages to identify the current topic.
// Uses frequency analysis of nouns/terms.
func extractAnchor(messages []ChatMessage) string {
	// Get recent user messages (last 10)
	userMessages := filterUserMessages(messages, 10)

	if len(userMessages) == 0 {
		return ""
	}

	// Count word frequencies
	wordCounts := make(map[string]int)

	for _, msg := range userMessages {
		words := extractWords(msg.GetContent())
		for _, word := range words {
			wordCounts[word]++
		}
	}

	// Sort by frequency
	type wordFreq struct {
		word  string
		count int
	}

	var freqs []wordFreq
	for word, count := range wordCounts {
		freqs = append(freqs, wordFreq{word, count})
	}

	sort.Slice(freqs, func(i, j int) bool {
		// Sort by count descending, then alphabetically for ties
		if freqs[i].count != freqs[j].count {
			return freqs[i].count > freqs[j].count
		}
		return freqs[i].word < freqs[j].word
	})

	// Take top 3-5 terms that appear more than once
	var topTerms []string
	for _, wf := range freqs {
		if wf.count < 2 && len(topTerms) > 0 {
			break // Stop at single-occurrence words if we have some terms
		}
		if len(topTerms) >= 5 {
			break
		}
		topTerms = append(topTerms, wf.word)
	}

	if len(topTerms) == 0 {
		// Fall back to most recent message's first significant word
		if len(userMessages) > 0 {
			words := extractWords(userMessages[len(userMessages)-1].GetContent())
			if len(words) > 0 {
				return words[0]
			}
		}
		return ""
	}

	return strings.Join(topTerms, ", ")
}

// extractGoal looks for goal-indicating patterns in recent user messages.
func extractGoal(messages []ChatMessage) string {
	// Get recent user messages (last 5)
	userMessages := filterUserMessages(messages, 5)

	if len(userMessages) == 0 {
		return ""
	}

	// Check most recent messages first (reverse order)
	for i := len(userMessages) - 1; i >= 0; i-- {
		msg := userMessages[i]
		content := msg.GetContent()

		// Try each goal pattern
		for _, pattern := range goalPatterns {
			matches := pattern.FindStringSubmatch(content)
			if len(matches) > 1 {
				goal := strings.TrimSpace(matches[1])
				// Clean up the goal text
				goal = cleanGoalText(goal)
				if goal != "" {
					return goal
				}
			}
		}

		// Check if it's a question (ends with ?)
		trimmed := strings.TrimSpace(content)
		if strings.HasSuffix(trimmed, "?") {
			// Extract the question as the goal
			goal := extractQuestionGoal(trimmed)
			if goal != "" {
				return goal
			}
		}
	}

	return ""
}

// filterUserMessages returns the last n user messages from the history.
func filterUserMessages(messages []ChatMessage, n int) []ChatMessage {
	var userMsgs []ChatMessage

	for _, msg := range messages {
		if msg.Role == "user" {
			userMsgs = append(userMsgs, msg)
		}
	}

	// Return last n
	if len(userMsgs) > n {
		return userMsgs[len(userMsgs)-n:]
	}
	return userMsgs
}

// extractWords extracts meaningful words from text, filtering stop words.
func extractWords(text string) []string {
	// Convert to lowercase for comparison
	lower := strings.ToLower(text)

	// Find all words
	matches := wordPattern.FindAllString(lower, -1)

	// Filter stop words and short words
	var words []string
	for _, word := range matches {
		if len(word) < 3 {
			continue
		}
		if stopWords[word] {
			continue
		}
		words = append(words, word)
	}

	return words
}

// cleanGoalText cleans up extracted goal text.
func cleanGoalText(goal string) string {
	// Remove trailing punctuation
	goal = strings.TrimRight(goal, ".,;:!?")

	// Remove common filler at the end
	suffixes := []string{" please", " thanks", " thank you", " if you can", " for me"}
	for _, suffix := range suffixes {
		goal = strings.TrimSuffix(strings.ToLower(goal), suffix)
	}

	// Minimum length check
	if len(goal) < 3 {
		return ""
	}

	return goal
}

// extractQuestionGoal extracts a goal from a question.
func extractQuestionGoal(question string) string {
	// Remove question mark
	question = strings.TrimSuffix(question, "?")
	question = strings.TrimSpace(question)

	// If short enough, use the whole question
	if len(question) < 80 {
		return "understand: " + question
	}

	// Truncate long questions
	words := strings.Fields(question)
	if len(words) > 10 {
		return "understand: " + strings.Join(words[:10], " ") + "..."
	}

	return "understand: " + question
}

// loadWorkingMemory loads the current working memory file if it exists.
func loadWorkingMemory(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}

	// Try primary location
	workingPath := filepath.Join(workspaceRoot, ".cog", "memory", "working.cog.md")

	data, err := os.ReadFile(workingPath)
	if err != nil {
		return ""
	}

	content := string(data)

	// Parse the markdown to extract key sections
	// Skip frontmatter and extract main content
	sections := parseWorkingMemory(content)

	return sections
}

// parseWorkingMemory extracts relevant sections from working memory markdown.
func parseWorkingMemory(content string) string {
	var builder strings.Builder

	// Skip frontmatter if present
	if strings.HasPrefix(content, "---") {
		endIdx := strings.Index(content[3:], "---")
		if endIdx > 0 {
			content = content[endIdx+6:]
		}
	}

	// Look for key sections
	lines := strings.Split(content, "\n")

	currentSection := ""
	sectionContent := []string{}

	includeSections := map[string]bool{
		"Current Focus":           true,
		"Active Artifacts":        true,
		"Open Questions":          true,
		"Key Decisions":           true,
		"Next Actions":            true,
		"Key Decisions This Session": true,
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check if this is a section header
		if strings.HasPrefix(trimmed, "# ") {
			// Flush previous section if it was included
			if currentSection != "" && len(sectionContent) > 0 {
				builder.WriteString("### ")
				builder.WriteString(currentSection)
				builder.WriteString("\n")
				for _, l := range sectionContent {
					builder.WriteString(l)
					builder.WriteString("\n")
				}
				builder.WriteString("\n")
			}

			currentSection = strings.TrimPrefix(trimmed, "# ")
			sectionContent = []string{}
			continue
		}

		// Add content to current section if it's one we want
		if includeSections[currentSection] && trimmed != "" && !strings.HasPrefix(trimmed, "(") {
			sectionContent = append(sectionContent, line)
		}
	}

	// Flush final section
	if currentSection != "" && len(sectionContent) > 0 && includeSections[currentSection] {
		builder.WriteString("### ")
		builder.WriteString(currentSection)
		builder.WriteString("\n")
		for _, l := range sectionContent {
			builder.WriteString(l)
			builder.WriteString("\n")
		}
	}

	return builder.String()
}
