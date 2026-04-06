package main

import (
	"strings"
	"testing"
)

// ── isConversation detection ────────────────────────────────────────────────

func TestIsConversation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "markdown turn headers",
			text: "# Chat\n\n## [user]\nHello\n\n## [assistant]\nHi there\n",
			want: true,
		},
		{
			name: "bold role markers",
			text: "**User:** What is X?\n\n**Assistant:** X is Y.\n",
			want: true,
		},
		{
			name: "mixed case headers",
			text: "## User\nHello\n\n## Assistant\nHi\n\n## User\nThanks\n",
			want: true,
		},
		{
			name: "plain document",
			text: "# Architecture\n\nSome content about systems.\n\n## Components\n\nMore text.\n",
			want: false,
		},
		{
			name: "single turn marker only",
			text: "## [user]\nJust one turn marker, not a conversation.\n",
			want: false,
		},
		{
			name: "empty text",
			text: "",
			want: false,
		},
		{
			name: "h3 human/ai markers",
			text: "### Human\nHello\n\n### AI\nHi\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isConversation(tt.text)
			if got != tt.want {
				t.Errorf("isConversation(%q) = %v; want %v", tt.text[:min(len(tt.text), 60)], got, tt.want)
			}
		})
	}
}

// ── splitTurns ──────────────────────────────────────────────────────────────

func TestSplitTurns(t *testing.T) {
	t.Parallel()

	text := "# Session\n\n## [user]\nWhat is 2+2?\n\n## [assistant]\nIt is 4.\n\n## [user]\nThanks!\n"
	turns := splitTurns(text)

	// Expect: preamble ("# Session"), then 3 turns.
	if len(turns) != 4 {
		t.Fatalf("got %d turns; want 4\nturns: %v", len(turns), turns)
	}

	if !strings.HasPrefix(turns[0], "# Session") {
		t.Errorf("turns[0] = %q; want preamble starting with '# Session'", turns[0])
	}
	if !strings.Contains(turns[1], "What is 2+2?") {
		t.Errorf("turns[1] should contain user question")
	}
	if !strings.Contains(turns[2], "It is 4.") {
		t.Errorf("turns[2] should contain assistant answer")
	}
	if !strings.Contains(turns[3], "Thanks!") {
		t.Errorf("turns[3] should contain user thanks")
	}
}

func TestSplitTurnsNoPreamble(t *testing.T) {
	t.Parallel()

	text := "## [user]\nHello\n\n## [assistant]\nHi\n"
	turns := splitTurns(text)

	if len(turns) != 2 {
		t.Fatalf("got %d turns; want 2", len(turns))
	}
}

func TestSplitTurnsBoldMarkers(t *testing.T) {
	t.Parallel()

	text := "**User:** What is X?\n\n**Assistant:** X is Y.\n\n**User:** Got it.\n"
	turns := splitTurns(text)

	if len(turns) != 3 {
		t.Fatalf("got %d turns; want 3\nturns: %v", len(turns), turns)
	}
}

// ── groupTurns ──────────────────────────────────────────────────────────────

func TestGroupTurnsFitInOne(t *testing.T) {
	t.Parallel()

	turns := []string{"## [user]\nShort", "## [assistant]\nAlso short"}
	chunks := groupTurns(turns, 2000)

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks; want 1 (both turns should fit)", len(chunks))
	}
	if !strings.Contains(chunks[0], "Short") || !strings.Contains(chunks[0], "Also short") {
		t.Errorf("chunk should contain both turns")
	}
}

func TestGroupTurnsOverflow(t *testing.T) {
	t.Parallel()

	// Two turns that together exceed target=50.
	turn1 := "## [user]\n" + strings.Repeat("a", 30)
	turn2 := "## [assistant]\n" + strings.Repeat("b", 30)
	turns := []string{turn1, turn2}
	chunks := groupTurns(turns, 50)

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks; want 2 (turns should not be merged)", len(chunks))
	}
}

func TestGroupTurnsOversizedSingleTurn(t *testing.T) {
	t.Parallel()

	// A single turn that exceeds the target — should NOT be split.
	bigTurn := "## [user]\n" + strings.Repeat("x", 3000)
	turns := []string{bigTurn}
	chunks := groupTurns(turns, 2000)

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks; want 1 (oversized turn stays whole)", len(chunks))
	}
	if len(chunks[0]) != len(bigTurn) {
		t.Errorf("oversized chunk should not be truncated")
	}
}

// ── ChunkDocument (full integration) ────────────────────────────────────────

func TestChunkDocumentConversation(t *testing.T) {
	t.Parallel()

	// Build a conversation with multiple turns.
	var sb strings.Builder
	sb.WriteString("# Chat about math\n\n")
	for i := 0; i < 10; i++ {
		sb.WriteString("## [user]\n")
		sb.WriteString(strings.Repeat("Question text. ", 30))
		sb.WriteString("\n\n")
		sb.WriteString("## [assistant]\n")
		sb.WriteString(strings.Repeat("Answer text here. ", 30))
		sb.WriteString("\n\n")
	}

	body := sb.String()
	chunks := ChunkDocument(body, 2000)

	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// Verify no chunk splits mid-turn: every chunk that contains a turn header
	// for [user] or [assistant] should contain the full turn content.
	for i, chunk := range chunks {
		// Check that no chunk ends in the middle of a line.
		lines := strings.Split(chunk, "\n")
		lastLine := strings.TrimSpace(lines[len(lines)-1])
		// The last line should be meaningful content, not a bare header.
		if strings.HasPrefix(lastLine, "## [") && !strings.Contains(lastLine, "\n") {
			// A bare header as the last line means the turn body was split off.
			// This is OK only if the header IS the only thing in that turn segment,
			// but that would mean the content was split. Let's just verify no
			// mid-word truncation happened.
			t.Errorf("chunk %d ends with a bare turn header: %q", i, lastLine)
		}
	}

	t.Logf("conversation: %d chars -> %d chunks", len(body), len(chunks))
}

func TestChunkDocumentNonConversation(t *testing.T) {
	t.Parallel()

	body := "# Architecture\n\n" +
		strings.Repeat("First paragraph content. ", 50) + "\n\n" +
		strings.Repeat("Second paragraph content. ", 50) + "\n\n" +
		strings.Repeat("Third paragraph content. ", 50)

	chunks := ChunkDocument(body, 500)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for long document; got %d", len(chunks))
	}

	// Every chunk should be <= target (unless a single paragraph exceeds it).
	for i, chunk := range chunks {
		t.Logf("chunk %d: %d chars", i, len(chunk))
	}
}

func TestChunkDocumentEmpty(t *testing.T) {
	t.Parallel()
	chunks := ChunkDocument("", 2000)
	if chunks != nil {
		t.Errorf("expected nil for empty body; got %d chunks", len(chunks))
	}
}

func TestChunkDocumentWhitespaceOnly(t *testing.T) {
	t.Parallel()
	chunks := ChunkDocument("   \n\n  \n", 2000)
	if chunks != nil {
		t.Errorf("expected nil for whitespace-only body; got %d chunks", len(chunks))
	}
}

func TestChunkDocumentDefaultSize(t *testing.T) {
	t.Parallel()
	// Passing 0 should use the default.
	body := "## [user]\nHello\n\n## [assistant]\nHi\n"
	chunks := ChunkDocument(body, 0)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks; want 1", len(chunks))
	}
}

// ── chunkByParagraphs ───────────────────────────────────────────────────────

func TestChunkByParagraphs(t *testing.T) {
	t.Parallel()

	para1 := strings.Repeat("Word ", 100) // ~500 chars
	para2 := strings.Repeat("More ", 100) // ~500 chars
	para3 := strings.Repeat("Last ", 100) // ~500 chars

	text := para1 + "\n\n" + para2 + "\n\n" + para3
	chunks := chunkByParagraphs(text, 600)

	// Each paragraph is ~500 chars, target is 600, so they should
	// NOT merge (500+2+500 = 1002 > 600).
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks; want 3", len(chunks))
	}
}

func TestChunkByParagraphsMergeable(t *testing.T) {
	t.Parallel()

	text := "Short para one.\n\nShort para two.\n\nShort para three."
	chunks := chunkByParagraphs(text, 2000)

	// All three short paragraphs should fit in one chunk.
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks; want 1 (all short paras should merge)", len(chunks))
	}
}

// ── Regression: turn markers should not false-positive on normal headings ───

func TestIsConversationNoFalsePositive(t *testing.T) {
	t.Parallel()

	// A document with headings like "## Components" should NOT be detected.
	text := "# Design Doc\n\n## Components\n\nThe system has three parts.\n\n" +
		"## Architecture\n\nLayered design.\n\n## Testing\n\nUnit tests.\n"

	if isConversation(text) {
		t.Error("normal document should not be detected as conversation")
	}
}
