// context_assembly_tool_pairing_test.go — unit tests for repairToolPairing
package engine

import (
	"testing"
)

// -- helpers ------------------------------------------------------------------

func toolMsg(id string) ProviderMessage {
	return ProviderMessage{Role: "tool", Content: "result", ToolCallID: id}
}

func assistantWithCalls(content string, ids ...string) ProviderMessage {
	calls := make([]ToolCall, len(ids))
	for i, id := range ids {
		calls[i] = ToolCall{ID: id, Name: "fn_" + id, Arguments: "{}"}
	}
	return ProviderMessage{Role: "assistant", Content: content, ToolCalls: calls}
}

func userMsg(text string) ProviderMessage {
	return ProviderMessage{Role: "user", Content: text}
}

// checkNoPairingViolation walks msgs and asserts Anthropic pairing invariants hold:
//   1. Every role=="tool" message must follow an assistant that listed its ID.
//   2. Every assistant ToolCall.ID must have a downstream role=="tool" result.
func checkNoPairingViolation(t *testing.T, msgs []ProviderMessage) {
	t.Helper()

	// Build map: assistant-position -> set of ToolCall IDs.
	// Also collect all result IDs and their positions.
	type assistantEntry struct {
		pos int
		ids map[string]bool
	}
	var assistants []assistantEntry
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			ids := make(map[string]bool)
			for _, tc := range m.ToolCalls {
				ids[tc.ID] = true
			}
			assistants = append(assistants, assistantEntry{pos: i, ids: ids})
		}
	}

	// Invariant 1: every tool_result must have a preceding assistant with its ID.
	for i, m := range msgs {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		found := false
		for _, a := range assistants {
			if a.pos < i && a.ids[m.ToolCallID] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("invariant 1 violated at index %d: tool_result ToolCallID=%q has no preceding assistant tool_use", i, m.ToolCallID)
		}
	}

	// Invariant 2: every assistant ToolCall must have a downstream tool_result.
	for _, a := range assistants {
		for id := range a.ids {
			found := false
			for i := a.pos + 1; i < len(msgs); i++ {
				if msgs[i].Role == "tool" && msgs[i].ToolCallID == id {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("invariant 2 violated: assistant at index %d has ToolCall.ID=%q with no downstream tool_result", a.pos, id)
			}
		}
	}
}

// -- tests --------------------------------------------------------------------

// TestRepairToolPairing_NoTools verifies that a no-tool conversation is unchanged.
func TestRepairToolPairing_NoTools(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{
		userMsg("hello"),
		{Role: "assistant", Content: "world"},
		userMsg("again"),
	}
	out, n := repairToolPairing(msgs)
	if n != 0 {
		t.Errorf("expected 0 repairs, got %d", n)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected %d messages, got %d", len(msgs), len(out))
	}
	for i, m := range out {
		if m.Role != msgs[i].Role || m.Content != msgs[i].Content {
			t.Errorf("message[%d] changed: got %+v, want %+v", i, m, msgs[i])
		}
	}
}

// TestRepairToolPairing_ValidPairPreserved verifies a valid tool_use+tool_result
// pair passes through unchanged.
func TestRepairToolPairing_ValidPairPreserved(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{
		userMsg("do something"),
		assistantWithCalls("", "id-001"),
		toolMsg("id-001"),
		userMsg("done"),
	}
	out, n := repairToolPairing(msgs)
	if n != 0 {
		t.Errorf("expected 0 repairs on valid pair, got %d", n)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	checkNoPairingViolation(t, out)
}

// TestRepairToolPairing_OrphanToolResult verifies that a leading orphan tool_result
// (no preceding assistant tool_use) is dropped. Mirrors Case B from the reproducer.
func TestRepairToolPairing_OrphanToolResult(t *testing.T) {
	t.Parallel()
	// [user, role:tool, user] — preceding assistant+tool_use absent.
	msgs := []ProviderMessage{
		userMsg("start"),
		toolMsg("toolu_01BA2XORPHAN002"),
		userMsg("continue"),
	}
	out, n := repairToolPairing(msgs)
	if n == 0 {
		t.Error("expected at least 1 repair for orphan tool_result, got 0")
	}
	// The orphan tool_result should be gone.
	for _, m := range out {
		if m.Role == "tool" {
			t.Errorf("orphan tool_result survived repair: %+v", m)
		}
	}
	checkNoPairingViolation(t, out)
}

// TestRepairToolPairing_OrphanToolUse verifies that an assistant with tool_uses
// but no downstream tool_result is repaired. Mirrors Case A from the reproducer.
func TestRepairToolPairing_OrphanToolUse(t *testing.T) {
	t.Parallel()
	// [user, assistant+tool_calls, user] — tool_result absent.
	msgs := []ProviderMessage{
		userMsg("start"),
		assistantWithCalls("", "toolu_018mw2ORPHAN001"),
		userMsg("continue"),
	}
	out, n := repairToolPairing(msgs)
	if n == 0 {
		t.Error("expected at least 1 repair for orphan tool_use, got 0")
	}
	// The assistant must not carry an unmatched tool_use.
	for _, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			t.Errorf("assistant with orphan ToolCalls survived repair: %+v", m)
		}
	}
	checkNoPairingViolation(t, out)
}

// TestRepairToolPairing_SplitPair mirrors the live bug: an assistant has 8
// tool_use blocks but budget eviction dropped 6 of the 8 tool_results.
// After repair: only the 2 surviving results (and their paired tool_uses) remain.
func TestRepairToolPairing_SplitPair(t *testing.T) {
	t.Parallel()
	// Build 8 tool_call IDs.
	allIDs := []string{
		"toolu_018mw2SPLIT001",
		"toolu_01BA2XSPLIT002",
		"toolu_019XXSPLIT003",
		"toolu_01CCCSPLIT004",
		"toulu_01DDDSPLIT005",
		"toolu_01EEESPLIT006",
		"toolu_01FFFSPLIT007",
		"toulu_01GGGSPLIT008",
	}
	// Only 2 tool_results survived eviction.
	survivingIDs := []string{allIDs[0], allIDs[1]}

	msgs := []ProviderMessage{
		userMsg("do lots"),
		assistantWithCalls("", allIDs...),
		toolMsg(survivingIDs[0]),
		toolMsg(survivingIDs[1]),
		userMsg("next"),
	}
	out, n := repairToolPairing(msgs)
	if n == 0 {
		t.Error("expected repairs for 6 orphan tool_uses, got 0")
	}
	// The surviving assistant message must have exactly 2 ToolCalls.
	for _, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if len(m.ToolCalls) != 2 {
				t.Errorf("repaired assistant has %d ToolCalls, want 2", len(m.ToolCalls))
			}
			for _, tc := range m.ToolCalls {
				found := false
				for _, sid := range survivingIDs {
					if tc.ID == sid {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("unexpected ToolCall.ID=%q in repaired assistant", tc.ID)
				}
			}
		}
	}
	checkNoPairingViolation(t, out)
}

// TestRepairToolPairing_AssistantWithTextAndAllResultsEvicted verifies that when
// all tool results are evicted but the assistant has non-empty text content, the
// assistant is kept as plain assistant text (ToolCalls nil), not dropped.
func TestRepairToolPairing_AssistantWithTextAndAllResultsEvicted(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{
		userMsg("start"),
		{
			Role:     "assistant",
			Content:  "Let me check that for you.",
			ToolCalls: []ToolCall{{ID: "id-text-001", Name: "lookup", Arguments: "{}"}},
		},
		// No tool_result — evicted.
		userMsg("thanks"),
	}
	out, _ := repairToolPairing(msgs)
	// The assistant should survive as plain text.
	found := false
	for _, m := range out {
		if m.Role == "assistant" {
			found = true
			if len(m.ToolCalls) != 0 {
				t.Errorf("assistant should have nil ToolCalls after repair, got %+v", m.ToolCalls)
			}
			if m.Content != "Let me check that for you." {
				t.Errorf("assistant Content changed: %q", m.Content)
			}
		}
	}
	if !found {
		t.Error("assistant message was dropped but it has non-empty Content — should be kept as plain text")
	}
	checkNoPairingViolation(t, out)
}

// TestRepairToolPairing_Idempotent verifies that running repair twice on a slice
// is a no-op on the second pass.
func TestRepairToolPairing_Idempotent(t *testing.T) {
	t.Parallel()
	// Start with an already-broken history.
	msgs := []ProviderMessage{
		userMsg("start"),
		assistantWithCalls("", "id-idem-001"),
		// tool_result intentionally absent.
		userMsg("end"),
	}
	pass1, n1 := repairToolPairing(msgs)
	pass2, n2 := repairToolPairing(pass1)
	if n2 != 0 {
		t.Errorf("second pass should be a no-op (0 repairs) but got %d repairs (first pass: %d)", n2, n1)
	}
	if len(pass2) != len(pass1) {
		t.Errorf("second pass changed length: %d -> %d", len(pass1), len(pass2))
	}
	checkNoPairingViolation(t, pass2)
}

// TestRepairToolPairing_MultipleValidPairs verifies that a history with multiple
// consecutive valid tool turns all survive unchanged.
func TestRepairToolPairing_MultipleValidPairs(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{
		userMsg("do two things"),
		assistantWithCalls("", "id-A", "id-B"),
		toolMsg("id-A"),
		toolMsg("id-B"),
		{Role: "assistant", Content: "done"},
		userMsg("great"),
		assistantWithCalls("", "id-C"),
		toolMsg("id-C"),
		userMsg("finished"),
	}
	out, n := repairToolPairing(msgs)
	if n != 0 {
		t.Errorf("expected 0 repairs for all-valid history, got %d", n)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected %d messages, got %d", len(msgs), len(out))
	}
	checkNoPairingViolation(t, out)
}

// TestBuildAnthropicRequest_NoOrphanAfterRepair verifies that calling
// buildAnthropicRequest on a broken history (orphan tool_use) does not produce
// an anthropicMessage with an unmatched tool_use block — i.e., the defensive
// repair inside buildAnthropicRequest fires correctly.
func TestBuildAnthropicRequest_NoOrphanAfterRepair(t *testing.T) {
	t.Parallel()
	// Broken input: assistant with a tool_use but no downstream tool_result.
	req := &CompletionRequest{
		Messages: []ProviderMessage{
			userMsg("do it"),
			assistantWithCalls("", "orphan-id-001"),
			// No tool_result message — evicted.
			userMsg("continue"),
		},
	}
	ar := buildAnthropicRequest("claude-sonnet-4-20250514", req, false, 8192)

	// Walk the resulting anthropic messages; no tool_use block should appear
	// without a corresponding tool_result block in the immediately-following
	// user message.
	for i, am := range ar.Messages {
		if am.Role != "assistant" {
			continue
		}
		blocks, ok := am.Content.([]anthropicContentBlock)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			// Look for a matching tool_result in the next message.
			matched := false
			if i+1 < len(ar.Messages) {
				nextBlocks, ok2 := ar.Messages[i+1].Content.([]anthropicContentBlock)
				if ok2 {
					for _, nb := range nextBlocks {
						if nb.Type == "tool_result" && nb.ToolUseID == b.ID {
							matched = true
							break
						}
					}
				}
			}
			if !matched {
				t.Errorf("buildAnthropicRequest produced orphan tool_use block ID=%q at message index %d", b.ID, i)
			}
		}
	}
}
