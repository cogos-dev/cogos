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

func TestExtractAnchor(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "I want to implement the reconciliation loop"),
		makeMsg("assistant", "Sure, I can help with that."),
		makeMsg("user", "The reconciliation loop should handle drift detection"),
		makeMsg("user", "Make the reconciliation loop configurable"),
	}

	anchor := extractAnchor(messages, 10, 5)

	if anchor == "" {
		t.Fatal("extractAnchor returned empty string")
	}

	// "reconciliation" and "loop" appear in all 3 user messages
	if !(containsWord(anchor, "reconciliation") || containsWord(anchor, "loop")) {
		t.Errorf("anchor=%q, expected to contain 'reconciliation' or 'loop'", anchor)
	}
}

func TestExtractAnchorEmpty(t *testing.T) {
	anchor := extractAnchor(nil, 10, 5)
	if anchor != "" {
		t.Errorf("extractAnchor(nil) = %q, expected empty", anchor)
	}

	anchor = extractAnchor([]ChatMessage{}, 10, 5)
	if anchor != "" {
		t.Errorf("extractAnchor([]) = %q, expected empty", anchor)
	}
}

func TestExtractAnchorOnlyAssistantMessages(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("assistant", "Here is the plan"),
	}

	anchor := extractAnchor(messages, 10, 5)
	if anchor != "" {
		t.Errorf("anchor=%q, expected empty for assistant-only messages", anchor)
	}
}

func TestExtractAnchorTopN(t *testing.T) {
	// Create messages with many repeated words
	messages := []ChatMessage{
		makeMsg("user", "alpha alpha beta beta gamma gamma delta delta epsilon epsilon"),
		makeMsg("user", "alpha alpha beta beta gamma gamma delta delta epsilon epsilon"),
	}

	// With topN=2, should only get 2 terms
	anchor := extractAnchor(messages, 10, 2)
	terms := splitTerms(anchor)
	if len(terms) > 2 {
		t.Errorf("topN=2 but got %d terms: %q", len(terms), anchor)
	}
}

func TestExtractGoal(t *testing.T) {
	tests := []struct {
		name    string
		message string
		expect  string // substring expected in result
	}{
		{"help request", "help me implement the cache layer", "implement"},
		{"want to", "I want to build a REST API", "build"},
		{"need to", "I need to fix the authentication bug", "fix"},
		{"trying to", "I'm trying to optimize the query", "optimize"},
		{"can you", "Can you refactor the handler", "refactor"},
		{"how to", "How to deploy the application", "deploy"},
		{"lets", "Let's create a new module", "create"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := []ChatMessage{makeMsg("user", tt.message)}
			goal := extractGoal(messages, 5)
			if goal == "" {
				t.Fatalf("extractGoal returned empty for %q", tt.message)
			}
			if !containsCI(goal, tt.expect) {
				t.Errorf("goal=%q, expected to contain %q", goal, tt.expect)
			}
		})
	}
}

func TestExtractGoalQuestion(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "What is the difference between channels and mutexes?"),
	}

	goal := extractGoal(messages, 5)

	if goal == "" {
		t.Fatal("extractGoal returned empty for question")
	}
	if !containsCI(goal, "understand") {
		t.Errorf("goal=%q, expected 'understand:' prefix for questions", goal)
	}
}

func TestExtractGoalEmpty(t *testing.T) {
	goal := extractGoal(nil, 5)
	if goal != "" {
		t.Errorf("extractGoal(nil) = %q, expected empty", goal)
	}
}

func TestLoadWorkingMemory(t *testing.T) {
	root := t.TempDir()

	// Create working memory at the correct path
	memDir := filepath.Join(root, ".cog", "mem")
	os.MkdirAll(memDir, 0o755)
	os.WriteFile(filepath.Join(memDir, "working.cog.md"), []byte(`---
title: Working Memory
---
# Current Focus
Testing the pipeline

# Next Actions
- Write more tests
`), 0o644)

	result := loadWorkingMemory(root)

	if result == "" {
		t.Fatal("loadWorkingMemory returned empty")
	}
	if !containsCI(result, "Testing the pipeline") {
		t.Errorf("loadWorkingMemory missing expected content, got: %s", result)
	}
}

func TestLoadWorkingMemoryMissing(t *testing.T) {
	result := loadWorkingMemory("/nonexistent/workspace")
	if result != "" {
		t.Errorf("loadWorkingMemory for missing file should return empty, got: %q", result)
	}
}

func TestLoadWorkingMemoryEmptyRoot(t *testing.T) {
	result := loadWorkingMemory("")
	if result != "" {
		t.Errorf("loadWorkingMemory('') should return empty, got: %q", result)
	}
}

func TestFilterUserMessages(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "msg1"),
		makeMsg("assistant", "reply1"),
		makeMsg("user", "msg2"),
		makeMsg("assistant", "reply2"),
		makeMsg("user", "msg3"),
		makeMsg("user", "msg4"),
		makeMsg("user", "msg5"),
	}

	// Filter last 3
	result := filterUserMessages(messages, 3)
	if len(result) != 3 {
		t.Fatalf("filterUserMessages(n=3) returned %d messages, expected 3", len(result))
	}
	if result[0].GetContent() != "msg3" {
		t.Errorf("first message should be msg3, got %q", result[0].GetContent())
	}
	if result[2].GetContent() != "msg5" {
		t.Errorf("last message should be msg5, got %q", result[2].GetContent())
	}
}

func TestFilterUserMessagesMoreThanAvailable(t *testing.T) {
	messages := []ChatMessage{
		makeMsg("user", "only"),
	}

	result := filterUserMessages(messages, 100)
	if len(result) != 1 {
		t.Fatalf("filterUserMessages returned %d, expected 1", len(result))
	}
}

func TestExtractWords(t *testing.T) {
	words := extractWords("I want to implement the reconciliation loop for CogOS")

	// Should filter stop words
	for _, w := range words {
		if stopWords[w] {
			t.Errorf("extractWords returned stop word: %q", w)
		}
	}

	// Should include meaningful words
	found := map[string]bool{}
	for _, w := range words {
		found[w] = true
	}
	if !found["implement"] {
		t.Error("extractWords should include 'implement'")
	}
	if !found["reconciliation"] {
		t.Error("extractWords should include 'reconciliation'")
	}
}

// === Recent Context (Momentary Awareness) Tests ===

func TestRetrieveTemporalContextIncludesRecentMessages(t *testing.T) {
	resetTAACache()
	root := t.TempDir()

	messages := []ChatMessage{
		makeMsg("user", "First message about reconciliation"),
		makeMsg("assistant", "I can help with reconciliation."),
		makeMsg("user", "Second message about the loop"),
		makeMsg("assistant", "The loop handles drift detection."),
		makeMsg("user", "Third message about configuration"),
	}

	ctx, _, _, err := RetrieveTemporalContext(messages, "test-session", root, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain the Recent Context section
	if !strings.Contains(ctx, "## Recent Context") {
		t.Fatal("missing ## Recent Context section in temporal context")
	}
	// Should contain actual message content
	if !strings.Contains(ctx, "reconciliation") {
		t.Error("recent context should contain 'reconciliation' from messages")
	}
	if !strings.Contains(ctx, "drift detection") {
		t.Error("recent context should contain 'drift detection' from messages")
	}
	// Should have role labels
	if !strings.Contains(ctx, "**user:**") {
		t.Error("recent context should contain **user:** labels")
	}
	if !strings.Contains(ctx, "**you:**") {
		t.Error("recent context should contain **you:** labels")
	}
}

func TestRetrieveTemporalContextRecentMessagesWindowed(t *testing.T) {
	resetTAACache()
	root := t.TempDir()

	// Create more messages than the default window (6)
	var messages []ChatMessage
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, makeMsg(role, fmt.Sprintf("Message number %d", i)))
	}

	ctx, _, _, err := RetrieveTemporalContext(messages, "test-session", root, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should include the most recent messages, not the oldest
	if !strings.Contains(ctx, "Message number 9") {
		t.Error("should include most recent message (9)")
	}
	if !strings.Contains(ctx, "Message number 4") {
		t.Error("should include message 4 (within last 6)")
	}
	// Should NOT include very old messages
	if strings.Contains(ctx, "Message number 0") {
		t.Error("should not include message 0 (outside window of 6)")
	}
	if strings.Contains(ctx, "Message number 3") {
		t.Error("should not include message 3 (outside window of 6)")
	}
}

func TestRetrieveTemporalContextRecentTruncatesLongMessages(t *testing.T) {
	resetTAACache()
	root := t.TempDir()

	longMsg := strings.Repeat("word ", 200) // 1000 chars
	messages := []ChatMessage{
		makeMsg("user", longMsg),
	}

	ctx, _, _, err := RetrieveTemporalContext(messages, "test-session", root, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The recent context should truncate messages > 500 chars
	if !strings.Contains(ctx, "## Recent Context") {
		t.Fatal("missing Recent Context section")
	}
	if !strings.Contains(ctx, "...") {
		t.Error("long messages in recent context should be truncated with ...")
	}
}

func TestRetrieveTemporalContextEmptyMessages(t *testing.T) {
	resetTAACache()
	root := t.TempDir()

	ctx, _, _, err := RetrieveTemporalContext(nil, "test-session", root, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not crash, and should not have Recent Context section
	if strings.Contains(ctx, "## Recent Context") {
		t.Error("should not include Recent Context for empty messages")
	}
}

// === Cross-Session Peripheral Awareness Tests ===

// resetTAACache clears the global TAA config cache so tests get fresh defaults.
func resetTAACache() {
	taaConfigMutex.Lock()
	cachedTAAConfig = nil
	taaConfigMutex.Unlock()
}

func TestLoadPeripheralContext(t *testing.T) {
	resetTAACache()
	root := t.TempDir()

	// Create bus infrastructure
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	os.MkdirAll(busesDir, 0o755)

	// Create a registry with two buses
	registry := []map[string]interface{}{
		{
			"bus_id":        "bus_chat_session1",
			"state":         "active",
			"participants":  []string{"discord:user"},
			"last_event_at": time.Now().Add(-5 * time.Minute).Format(time.RFC3339Nano),
			"event_count":   3,
		},
		{
			"bus_id":        "bus_chat_current",
			"state":         "active",
			"participants":  []string{"http:user"},
			"last_event_at": time.Now().Format(time.RFC3339Nano),
			"event_count":   2,
		},
	}
	regData, _ := json.Marshal(registry)
	os.WriteFile(filepath.Join(busesDir, "registry.json"), regData, 0o644)

	// Create events for session1 — content must exceed MinBusChars (200)
	os.MkdirAll(filepath.Join(busesDir, "bus_chat_session1"), 0o755)
	events := []string{
		`{"v":1,"bus_id":"bus_chat_session1","seq":1,"type":"chat.request","from":"discord:user","payload":{"content":"How do I configure the agent provider? I've been looking at the config files but I'm not sure which one controls the provider settings for multi-agent orchestration."}}`,
		`{"v":1,"bus_id":"bus_chat_session1","seq":2,"type":"chat.response","from":"kernel:cogos","payload":{"content":"The agent provider is configured via .cog/config/agents/agents.hcl. You can define multiple providers in the providers block, each with their own model and endpoint settings."}}`,
	}
	os.WriteFile(
		filepath.Join(busesDir, "bus_chat_session1", "events.jsonl"),
		[]byte(strings.Join(events, "\n")+"\n"),
		0o644,
	)

	// Load peripheral context excluding the "current" session
	result := loadPeripheralContext(root, "current")

	if result == "" {
		t.Fatal("loadPeripheralContext returned empty — expected session1 to appear")
	}
	if !strings.Contains(result, "Active Conversations") {
		t.Error("missing Active Conversations header")
	}
	if !strings.Contains(result, "discord") {
		t.Error("missing discord origin label")
	}
	if !strings.Contains(result, "agent provider") {
		t.Error("missing user message content")
	}
	// Verify new deep format: should have **user:** and **you:** labels
	if !strings.Contains(result, "**user:**") {
		t.Error("missing **user:** label in deep format")
	}
	if !strings.Contains(result, "**you:**") {
		t.Error("missing **you:** label in deep format")
	}
	// Should use ### header per bus, not bullet list
	if !strings.Contains(result, "### [discord]") {
		t.Error("missing ### [discord] header in deep format")
	}
	// Should include exchange count
	if !strings.Contains(result, "exchanges)") {
		t.Error("missing exchange count in header")
	}
}

func TestLoadPeripheralContextExcludesCurrent(t *testing.T) {
	resetTAACache()
	root := t.TempDir()
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	os.MkdirAll(busesDir, 0o755)

	// Only one bus, and it IS the current session
	registry := []map[string]interface{}{
		{
			"bus_id":        "bus_chat_mysession",
			"state":         "active",
			"participants":  []string{"http:user"},
			"last_event_at": time.Now().Format(time.RFC3339Nano),
			"event_count":   5,
		},
	}
	regData, _ := json.Marshal(registry)
	os.WriteFile(filepath.Join(busesDir, "registry.json"), regData, 0o644)

	result := loadPeripheralContext(root, "mysession")
	if result != "" {
		t.Errorf("expected empty (only bus is current session), got: %q", result)
	}
}

func TestLoadPeripheralContextNoRegistry(t *testing.T) {
	resetTAACache()
	result := loadPeripheralContext(t.TempDir(), "session1")
	if result != "" {
		t.Errorf("expected empty for missing registry, got: %q", result)
	}
}

func TestLoadPeripheralContextSkipsOldBuses(t *testing.T) {
	resetTAACache()
	root := t.TempDir()
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	os.MkdirAll(busesDir, 0o755)

	// Bus with event >24h ago
	registry := []map[string]interface{}{
		{
			"bus_id":        "bus_chat_old",
			"state":         "active",
			"participants":  []string{"http:user"},
			"last_event_at": time.Now().Add(-48 * time.Hour).Format(time.RFC3339Nano),
			"event_count":   5,
		},
	}
	regData, _ := json.Marshal(registry)
	os.WriteFile(filepath.Join(busesDir, "registry.json"), regData, 0o644)

	result := loadPeripheralContext(root, "current")
	if result != "" {
		t.Errorf("expected empty for old bus, got: %q", result)
	}
}

func TestLoadPeripheralContextDeepFormat(t *testing.T) {
	resetTAACache()
	root := t.TempDir()
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	os.MkdirAll(busesDir, 0o755)

	registry := []map[string]interface{}{
		{
			"bus_id":        "bus_chat_deep1",
			"state":         "active",
			"participants":  []string{"http:user"},
			"last_event_at": time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano),
			"event_count":   6,
		},
	}
	regData, _ := json.Marshal(registry)
	os.WriteFile(filepath.Join(busesDir, "registry.json"), regData, 0o644)

	// Create a multi-turn conversation
	os.MkdirAll(filepath.Join(busesDir, "bus_chat_deep1"), 0o755)
	events := []string{
		`{"type":"chat.request","payload":{"content":"What is the TAA pipeline and how does it work with the context constructor to assemble the system prompt?"}}`,
		`{"type":"chat.response","payload":{"content":"The TAA pipeline has four tiers: identity, temporal, present, and semantic. Each tier retrieves context within its token budget."}}`,
		`{"type":"chat.request","payload":{"content":"How does the budget allocation work across tiers? Is it percentage-based or fixed tokens for each tier?"}}`,
		`{"type":"chat.response","payload":{"content":"It is percentage-based. The config specifies percentages for each tier that sum to less than 100 percent of the total budget."}}`,
	}
	os.WriteFile(
		filepath.Join(busesDir, "bus_chat_deep1", "events.jsonl"),
		[]byte(strings.Join(events, "\n")+"\n"),
		0o644,
	)

	result := loadPeripheralContext(root, "other_session")

	if result == "" {
		t.Fatal("expected non-empty deep format output")
	}

	// Should contain multiple exchanges, not just one
	userCount := strings.Count(result, "**user:**")
	botCount := strings.Count(result, "**you:**")
	if userCount < 2 {
		t.Errorf("expected at least 2 user turns, got %d", userCount)
	}
	if botCount < 2 {
		t.Errorf("expected at least 2 bot turns, got %d", botCount)
	}

	// Should mention exchange count in header
	if !strings.Contains(result, "4 exchanges") {
		t.Errorf("expected '4 exchanges' in header, got:\n%s", result)
	}
}

func TestLoadPeripheralContextSortByRecency(t *testing.T) {
	resetTAACache()
	root := t.TempDir()
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	os.MkdirAll(busesDir, 0o755)

	// Two buses: older one listed first in registry, but newer one should appear first in output
	registry := []map[string]interface{}{
		{
			"bus_id":        "bus_chat_older",
			"state":         "active",
			"participants":  []string{"cli:user"},
			"last_event_at": time.Now().Add(-30 * time.Minute).Format(time.RFC3339Nano),
			"event_count":   2,
		},
		{
			"bus_id":        "bus_chat_newer",
			"state":         "active",
			"participants":  []string{"discord:user"},
			"last_event_at": time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano),
			"event_count":   2,
		},
	}
	regData, _ := json.Marshal(registry)
	os.WriteFile(filepath.Join(busesDir, "registry.json"), regData, 0o644)

	// Create events for both (content long enough to pass MinBusChars)
	for _, busID := range []string{"bus_chat_older", "bus_chat_newer"} {
		os.MkdirAll(filepath.Join(busesDir, busID), 0o755)
		events := []string{
			`{"type":"chat.request","payload":{"content":"This is a test message that is long enough to pass the minimum bus chars threshold for inclusion in peripheral context output."}}`,
			`{"type":"chat.response","payload":{"content":"This is a response that also needs to be long enough. The peripheral context system requires a minimum amount of content per bus."}}`,
		}
		os.WriteFile(
			filepath.Join(busesDir, busID, "events.jsonl"),
			[]byte(strings.Join(events, "\n")+"\n"),
			0o644,
		)
	}

	result := loadPeripheralContext(root, "other_session")

	// discord (newer) should appear before cli (older)
	discordIdx := strings.Index(result, "[discord]")
	cliIdx := strings.Index(result, "[cli]")
	if discordIdx < 0 || cliIdx < 0 {
		t.Fatalf("expected both discord and cli in output, got:\n%s", result)
	}
	if discordIdx > cliIdx {
		t.Errorf("expected discord (newer) before cli (older), discord at %d, cli at %d", discordIdx, cliIdx)
	}
}

func TestTailBusExchange(t *testing.T) {
	root := t.TempDir()
	busDir := filepath.Join(root, "bus_test")
	os.MkdirAll(busDir, 0o755)

	events := []string{
		`{"type":"chat.request","payload":{"content":"first question"}}`,
		`{"type":"chat.response","payload":{"content":"first answer"}}`,
		`{"type":"chat.request","payload":{"content":"second question"}}`,
		`{"type":"chat.response","payload":{"content":"second answer"}}`,
	}
	os.WriteFile(filepath.Join(busDir, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o644)

	lastUser, lastBot := tailBusExchange(root, "bus_test")

	if lastUser != "second question" {
		t.Errorf("lastUser=%q, expected 'second question'", lastUser)
	}
	if lastBot != "second answer" {
		t.Errorf("lastBot=%q, expected 'second answer'", lastBot)
	}
}

func TestReadRecentExchanges(t *testing.T) {
	root := t.TempDir()
	busDir := filepath.Join(root, "bus_multi")
	os.MkdirAll(busDir, 0o755)

	events := []string{
		`{"type":"chat.request","payload":{"content":"first question"}}`,
		`{"type":"chat.response","payload":{"content":"first answer"}}`,
		`{"type":"chat.request","payload":{"content":"second question"}}`,
		`{"type":"chat.response","payload":{"content":"second answer"}}`,
		`{"type":"chat.request","payload":{"content":"third question"}}`,
		`{"type":"chat.response","payload":{"content":"third answer"}}`,
	}
	os.WriteFile(filepath.Join(busDir, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o644)

	// Large budget — should get all exchanges
	exchanges := readRecentExchanges(root, "bus_multi", 10000, 0)
	if len(exchanges) != 6 {
		t.Fatalf("expected 6 exchanges with large budget, got %d", len(exchanges))
	}
	// Verify chronological order
	if exchanges[0].Content != "first question" {
		t.Errorf("first exchange should be 'first question', got %q", exchanges[0].Content)
	}
	if exchanges[0].Role != "user" {
		t.Errorf("first exchange role should be 'user', got %q", exchanges[0].Role)
	}
	if exchanges[5].Content != "third answer" {
		t.Errorf("last exchange should be 'third answer', got %q", exchanges[5].Content)
	}
	if exchanges[5].Role != "assistant" {
		t.Errorf("last exchange role should be 'assistant', got %q", exchanges[5].Role)
	}
}

func TestReadRecentExchangesBudgetLimit(t *testing.T) {
	root := t.TempDir()
	busDir := filepath.Join(root, "bus_budget")
	os.MkdirAll(busDir, 0o755)

	events := []string{
		`{"type":"chat.request","payload":{"content":"aaaaaaaaaa"}}`,
		`{"type":"chat.response","payload":{"content":"bbbbbbbbbb"}}`,
		`{"type":"chat.request","payload":{"content":"cccccccccc"}}`,
		`{"type":"chat.response","payload":{"content":"dddddddddd"}}`,
	}
	os.WriteFile(filepath.Join(busDir, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o644)

	// Budget of 25 chars — should fit ~2 messages (each is 10 chars)
	exchanges := readRecentExchanges(root, "bus_budget", 25, 0)
	if len(exchanges) > 3 {
		t.Errorf("expected at most 3 exchanges with 25 char budget, got %d", len(exchanges))
	}
	if len(exchanges) < 2 {
		t.Errorf("expected at least 2 exchanges with 25 char budget, got %d", len(exchanges))
	}
	// Should include the most recent messages
	last := exchanges[len(exchanges)-1]
	if last.Content != "dddddddddd" {
		t.Errorf("last exchange should be 'dddddddddd', got %q", last.Content)
	}
}

func TestReadRecentExchangesLongMessages(t *testing.T) {
	root := t.TempDir()
	busDir := filepath.Join(root, "bus_long")
	os.MkdirAll(busDir, 0o755)

	longContent := strings.Repeat("x", 5000)
	events := []string{
		`{"type":"chat.request","payload":{"content":"` + longContent + `"}}`,
		`{"type":"chat.response","payload":{"content":"short reply"}}`,
	}
	os.WriteFile(filepath.Join(busDir, "events.jsonl"), []byte(strings.Join(events, "\n")+"\n"), 0o644)

	// maxMsgChars=100 should truncate the long message
	exchanges := readRecentExchanges(root, "bus_long", 100000, 100)
	if len(exchanges) == 0 {
		t.Fatal("expected exchanges")
	}

	// First message should be truncated to ~103 chars (100 + "...")
	if len(exchanges[0].Content) > 110 {
		t.Errorf("expected truncated message <= ~103 chars, got %d", len(exchanges[0].Content))
	}
	if !strings.HasSuffix(exchanges[0].Content, "...") {
		t.Error("truncated message should end with ...")
	}

	// Short message should be untouched
	if exchanges[1].Content != "short reply" {
		t.Errorf("short message should be unchanged, got %q", exchanges[1].Content)
	}
}

func TestReadRecentExchangesMissingFile(t *testing.T) {
	exchanges := readRecentExchanges(t.TempDir(), "nonexistent", 10000, 0)
	if exchanges != nil {
		t.Errorf("expected nil for missing file, got %v", exchanges)
	}
}

func TestPeripheralBudgetAllocation(t *testing.T) {
	// Single bus gets the whole budget
	budgets := allocateBusBudgets(1, 10000, 0.7)
	if len(budgets) != 1 || budgets[0] != 10000 {
		t.Errorf("single bus should get full budget, got %v", budgets)
	}

	// Two buses with decay 0.7: weights are 1.0, 0.7 => normalized ~59%, ~41%
	budgets = allocateBusBudgets(2, 10000, 0.7)
	if len(budgets) != 2 {
		t.Fatalf("expected 2 budgets, got %d", len(budgets))
	}
	if budgets[0] <= budgets[1] {
		t.Errorf("bus[0] should get more than bus[1]: %d vs %d", budgets[0], budgets[1])
	}
	// bus[0] should be ~5882, bus[1] should be ~4117
	if budgets[0] < 5000 || budgets[0] > 7000 {
		t.Errorf("bus[0] budget out of expected range: %d", budgets[0])
	}

	// Three buses: weights 1.0, 0.7, 0.49 => each gets progressively less
	budgets = allocateBusBudgets(3, 10000, 0.7)
	if len(budgets) != 3 {
		t.Fatalf("expected 3 budgets, got %d", len(budgets))
	}
	if budgets[0] <= budgets[1] || budgets[1] <= budgets[2] {
		t.Errorf("budgets should be strictly decreasing: %v", budgets)
	}
	// Sum should be close to total (rounding may lose a few)
	sum := budgets[0] + budgets[1] + budgets[2]
	if sum < 9000 || sum > 10000 {
		t.Errorf("budget sum %d should be close to 10000", sum)
	}

	// Edge: zero buses
	budgets = allocateBusBudgets(0, 10000, 0.7)
	if budgets != nil {
		t.Errorf("expected nil for 0 buses, got %v", budgets)
	}
}

func TestInferOrigin(t *testing.T) {
	tests := []struct {
		participants []string
		busID        string
		expected     string
	}{
		{[]string{"discord:session:123"}, "bus_chat_123", "discord"},
		{[]string{"http:user"}, "bus_chat_456", "http"},
		{[]string{"cli:user"}, "bus_chat_789", "cli"},
		{[]string{"kernel:cogos"}, "bus_chat_discord_abc", "discord"},
		{[]string{}, "bus_chat_xyz", "http"}, // fallback
	}

	for _, tt := range tests {
		result := inferOrigin(tt.participants, tt.busID)
		if result != tt.expected {
			t.Errorf("inferOrigin(%v, %q) = %q, expected %q", tt.participants, tt.busID, result, tt.expected)
		}
	}
}

func TestTruncateContent(t *testing.T) {
	short := "hello world"
	if truncateContent(short, 50) != "hello world" {
		t.Error("short content should not be truncated")
	}

	long := strings.Repeat("word ", 100)
	result := truncateContent(long, 30)
	if len(result) > 35 { // 30 + "..."
		t.Errorf("truncated content too long: %d", len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Error("truncated content should end with ...")
	}

	multiline := "line one\nline two\nline three"
	result = truncateContent(multiline, 100)
	if strings.Contains(result, "\n") {
		t.Error("truncateContent should collapse newlines")
	}
}

func TestCleanGoalText(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"implement the cache.", "implement the cache"},
		{"fix the bug,", "fix the bug"},
		{"ab", ""}, // too short
	}

	for _, tt := range tests {
		result := cleanGoalText(tt.input)
		if result != tt.expected {
			t.Errorf("cleanGoalText(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

// helpers

func containsWord(s, word string) bool {
	for _, w := range splitTerms(s) {
		if w == word {
			return true
		}
	}
	return false
}

func splitTerms(s string) []string {
	var terms []string
	for _, part := range splitComma(s) {
		part = trimSpace(part)
		if part != "" {
			terms = append(terms, part)
		}
	}
	return terms
}

func splitComma(s string) []string {
	result := []string{}
	current := ""
	for _, c := range s {
		if c == ',' {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	result = append(result, current)
	return result
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func containsCI(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && len(substr) > 0 && findCI(s, substr))
}

func findCI(s, substr string) bool {
	sl := toLower(s)
	subl := toLower(substr)
	for i := 0; i <= len(sl)-len(subl); i++ {
		if sl[i:i+len(subl)] == subl {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
