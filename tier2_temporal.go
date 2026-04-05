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
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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

	// Load TAA config for extraction parameters
	cfg := LoadTAAConfig(workspaceRoot)

	// Extract anchor (current topic) from recent user messages
	anchor = extractAnchor(messages, cfg.Temporal.RecencyWindow, cfg.Temporal.AnchorKeywords)

	// Extract goal from recent user messages
	goal = extractGoal(messages, cfg.Temporal.GoalKeywords)

	// Load working memory (session-scoped)
	workingMemory := loadWorkingMemory(workspaceRoot, sessionID)

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

	// Recent messages — momentary awareness buffer
	// Echoes the last few messages so continuity is preserved in Tier 2
	// regardless of Tier 3 budget pressure or context compression.
	if recentN := cfg.Temporal.RecentMessages; recentN > 0 && len(messages) > 0 {
		recent := messages
		if len(recent) > recentN {
			recent = recent[len(recent)-recentN:]
		}
		builder.WriteString("## Recent Context\n")
		for _, msg := range recent {
			label := "**user:**"
			if msg.Role == "assistant" {
				label = "**you:**"
			} else if msg.Role == "system" {
				label = "**system:**"
			}
			content := strings.Join(strings.Fields(msg.GetContent()), " ")
			// Truncate very long messages to keep the buffer compact
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			builder.WriteString(fmt.Sprintf("%s %s\n", label, content))
		}
		builder.WriteString("\n")
	}

	// Working memory (session state)
	if workingMemory != "" {
		builder.WriteString("## Session State\n")
		builder.WriteString(workingMemory)
		builder.WriteString("\n")
	}

	// Cross-session peripheral awareness
	peripheral := loadPeripheralContext(workspaceRoot, sessionID)
	if peripheral != "" {
		builder.WriteString(peripheral)
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
// recencyWindow controls how many recent user messages to analyze.
// topN controls the maximum number of anchor keywords to return.
func extractAnchor(messages []ChatMessage, recencyWindow, topN int) string {
	if recencyWindow <= 0 {
		recencyWindow = 10
	}
	if topN <= 0 {
		topN = 5
	}
	// Get recent user messages
	userMessages := filterUserMessages(messages, recencyWindow)

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

	// Take top terms that appear more than once
	var topTerms []string
	for _, wf := range freqs {
		if wf.count < 2 && len(topTerms) > 0 {
			break // Stop at single-occurrence words if we have some terms
		}
		if len(topTerms) >= topN {
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
// goalWindow controls how many recent user messages to scan for goals.
func extractGoal(messages []ChatMessage, goalWindow int) string {
	if goalWindow <= 0 {
		goalWindow = 5
	}
	// Get recent user messages
	userMessages := filterUserMessages(messages, goalWindow)

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

// loadWorkingMemory loads the session-scoped working memory file if it exists.
// Falls back to the global path during transition period.
func loadWorkingMemory(workspaceRoot, sessionID string) string {
	if workspaceRoot == "" {
		return ""
	}

	// Session-scoped path (primary)
	if sessionID == "" {
		sessionID = "_default"
	}
	workingPath := filepath.Join(workspaceRoot, ".cog", "mem", "episodic", "sessions", sessionID, "working.cog.md")

	// Legacy global path removed — session-scoped is now canonical.

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

// Per-section character cap for working memory. Any single section exceeding
// this limit is truncated to prevent one section from dominating the budget.
const workingMemorySectionCap = 2000

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
		"Current Focus":              true,
		"Active Artifacts":           true,
		"Open Questions":             true,
		"Key Decisions":              true,
		"Next Actions":               true,
		"Key Decisions This Session": true,
	}

	// flushSection writes the accumulated section content to the builder,
	// applying the per-section character cap.
	flushSection := func(name string, lines []string) {
		if name == "" || len(lines) == 0 {
			return
		}
		builder.WriteString("### ")
		builder.WriteString(name)
		builder.WriteString("\n")

		charCount := 0
		for _, l := range lines {
			lineLen := len(l) + 1 // +1 for the newline we append
			if charCount+lineLen > workingMemorySectionCap {
				// Write as much of this line as fits
				remaining := workingMemorySectionCap - charCount
				if remaining > 0 {
					builder.WriteString(l[:remaining])
				}
				builder.WriteString("\n[... truncated]\n")
				return
			}
			builder.WriteString(l)
			builder.WriteString("\n")
			charCount += lineLen
		}
		builder.WriteString("\n")
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check if this is a section header
		if strings.HasPrefix(trimmed, "# ") {
			// Flush previous section if it was included
			if includeSections[currentSection] {
				flushSection(currentSection, sectionContent)
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
	if includeSections[currentSection] {
		flushSection(currentSection, sectionContent)
	}

	return builder.String()
}

// === Cross-Session Peripheral Awareness ===
//
// Reads the bus registry and tails recent events from other active conversations.
// This gives the agent awareness of what's happening on other interfaces
// (Discord, HTTP, CLI) without losing local focus on the current session.
//
// Filesystem-based: works in both serve and CLI modes.

// busCandidate holds a filtered registry entry with parsed timestamp for sorting.
type busCandidate struct {
	busID        string
	state        string
	participants []string
	lastEventAt  time.Time
	eventCount   int
}

// loadPeripheralContext reads recent activity from all active buses
// except the current session, formatted as deep conversation excerpts.
// Uses budget-aware allocation with recency decay.
func loadPeripheralContext(workspaceRoot, currentSessionID string) string {
	if workspaceRoot == "" {
		return ""
	}

	cfg := LoadTAAConfig(workspaceRoot)
	pcfg := cfg.Temporal.Peripheral

	busesDir := filepath.Join(workspaceRoot, ".cog", ".state", "buses")
	registryPath := filepath.Join(busesDir, "registry.json")

	data, err := os.ReadFile(registryPath)
	if err != nil {
		return "" // No bus registry — fresh workspace or buses not initialized
	}

	var entries []struct {
		BusID        string   `json:"bus_id"`
		State        string   `json:"state"`
		Participants []string `json:"participants"`
		LastEventAt  string   `json:"last_event_at"`
		EventCount   int      `json:"event_count"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return ""
	}

	// Derive the current bus ID from session ID
	currentBusID := ""
	if currentSessionID != "" {
		if strings.HasPrefix(currentSessionID, "bus_chat_") {
			currentBusID = currentSessionID
		} else {
			currentBusID = "bus_chat_" + currentSessionID
		}
	}

	now := time.Now()
	maxAge := time.Duration(pcfg.MaxAgeHours) * time.Hour

	// Filter and parse candidates
	var candidates []busCandidate
	for _, entry := range entries {
		if entry.BusID == currentBusID {
			continue
		}
		if entry.State != "active" || entry.EventCount == 0 {
			continue
		}
		// TTL check: skip buses whose last activity is older than maxAge (default 24h).
		// Also skip buses with missing or unparseable timestamps — treat as stale.
		if entry.LastEventAt == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, entry.LastEventAt)
		if err != nil {
			continue // Unparseable timestamp — treat as stale
		}
		if now.Sub(parsed) > maxAge {
			continue
		}
		lastAt := parsed
		candidates = append(candidates, busCandidate{
			busID:        entry.BusID,
			participants: entry.Participants,
			lastEventAt:  lastAt,
			eventCount:   entry.EventCount,
		})
	}

	// Sort by recency (most recent first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastEventAt.After(candidates[j].lastEventAt)
	})

	// Cap at MaxBuses
	if len(candidates) > pcfg.MaxBuses {
		candidates = candidates[:pcfg.MaxBuses]
	}

	if len(candidates) == 0 {
		return ""
	}

	// Calculate total char budget for peripheral context
	totalBudget := Tier2MaxChars * pcfg.BudgetPct / 100

	// Allocate budget across buses using recency decay
	budgets := allocateBusBudgets(len(candidates), totalBudget, pcfg.RecencyDecay)

	// Read exchanges and build output
	type busDigest struct {
		origin    string
		lastEvent string // stable ISO timestamp (not relative age — avoids cache busting)
		exchanges []busExchange
	}

	var digests []busDigest
	for i, cand := range candidates {
		exchanges := readRecentExchanges(busesDir, cand.busID, budgets[i], pcfg.MaxMsgChars)
		if len(exchanges) == 0 {
			continue
		}

		// Check minimum content threshold
		totalContent := 0
		for _, ex := range exchanges {
			totalContent += len(ex.Content)
		}
		if totalContent < pcfg.MinBusChars {
			continue
		}

		origin := inferOrigin(cand.participants, cand.busID)

		// Use a stable timestamp instead of volatile relative age ("47m ago").
		// Relative age changes every turn and busts the KV cache prefix.
		// The model can compute recency from the timestamp itself.
		lastEvent := "unknown"
		if !cand.lastEventAt.IsZero() {
			lastEvent = cand.lastEventAt.Format("2006-01-02T15:04Z07:00") // minute-precision, stable
		}

		digests = append(digests, busDigest{
			origin:    origin,
			lastEvent: lastEvent,
			exchanges: exchanges,
		})
	}

	if len(digests) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Active Conversations\n")
	sb.WriteString("Other ongoing sessions you are participating in:\n\n")

	for _, d := range digests {
		sb.WriteString(fmt.Sprintf("### [%s] last_event:%s (%d exchanges)\n", d.origin, d.lastEvent, len(d.exchanges)))
		for _, ex := range d.exchanges {
			label := "**user:**"
			if ex.Role == "assistant" {
				label = "**you:**"
			}
			// Collapse whitespace for display
			content := strings.Join(strings.Fields(ex.Content), " ")
			sb.WriteString(fmt.Sprintf("%s %s\n", label, content))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// allocateBusBudgets distributes a total char budget across n buses using recency decay.
// Bus 0 (most recent) gets the largest share; each subsequent bus gets decay * previous.
// Returns a slice of char budgets, one per bus.
func allocateBusBudgets(n, totalBudget int, decay float64) []int {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []int{totalBudget}
	}

	// Calculate raw weights: 1.0, decay, decay^2, ...
	weights := make([]float64, n)
	w := 1.0
	sum := 0.0
	for i := 0; i < n; i++ {
		weights[i] = w
		sum += w
		w *= decay
	}

	// Normalize and convert to char budgets
	budgets := make([]int, n)
	for i := 0; i < n; i++ {
		budgets[i] = int(weights[i] / sum * float64(totalBudget))
	}

	return budgets
}

// busExchange represents a single message in a bus conversation.
type busExchange struct {
	Role    string // "user" or "assistant"
	Content string
}

// readRecentExchanges reads the most recent exchanges from a bus, up to charBudget.
// Returns exchanges in chronological order (oldest first).
func readRecentExchanges(busesDir, busID string, charBudget, maxMsgChars int) []busExchange {
	eventsPath := filepath.Join(busesDir, busID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Collect all chat events
	var allExchanges []busExchange
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var evt struct {
			Type    string                 `json:"type"`
			Payload map[string]interface{} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		content, _ := evt.Payload["content"].(string)
		if content == "" {
			continue
		}

		var role string
		switch evt.Type {
		case BlockChatRequest:
			role = "user"
		case BlockChatResponse:
			role = "assistant"
		default:
			continue
		}

		// Truncate individual messages
		if maxMsgChars > 0 && len(content) > maxMsgChars {
			content = content[:maxMsgChars] + "..."
		}

		allExchanges = append(allExchanges, busExchange{Role: role, Content: content})
	}

	if len(allExchanges) == 0 {
		return nil
	}

	// Walk backward from the end, accumulating content length
	totalChars := 0
	startIdx := len(allExchanges)
	for i := len(allExchanges) - 1; i >= 0; i-- {
		msgLen := len(allExchanges[i].Content)
		if totalChars+msgLen > charBudget && startIdx < len(allExchanges) {
			break // would exceed budget and we already have at least one message
		}
		totalChars += msgLen
		startIdx = i
	}

	return allExchanges[startIdx:]
}

// tailBusExchange reads the last user message and last bot response from a bus's events file.
// Returns (lastUserContent, lastBotContent). Either may be empty.
// Kept for backward compatibility — wraps readRecentExchanges.
func tailBusExchange(busesDir, busID string) (string, string) {
	exchanges := readRecentExchanges(busesDir, busID, 1<<30, 0) // large budget, no per-msg truncation
	var lastUser, lastBot string
	for _, ex := range exchanges {
		switch ex.Role {
		case "user":
			lastUser = ex.Content
		case "assistant":
			lastBot = ex.Content
		}
	}
	return lastUser, lastBot
}

// inferOrigin determines the interface/origin from bus participants and ID.
func inferOrigin(participants []string, busID string) string {
	for _, p := range participants {
		switch {
		case strings.Contains(p, "discord"):
			return "discord"
		case strings.Contains(p, "cli"):
			return "cli"
		case strings.Contains(p, "http"):
			return "http"
		case strings.Contains(p, "hook"):
			return "hook"
		case strings.Contains(p, "fleet"):
			return "fleet"
		}
	}
	// Fall back to bus ID heuristics
	if strings.Contains(busID, "discord") {
		return "discord"
	}
	return "http"
}

// truncateContent truncates a string to maxLen, adding ellipsis if needed.
// Also collapses newlines to spaces for single-line display.
func truncateContent(s string, maxLen int) string {
	// Collapse whitespace
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
