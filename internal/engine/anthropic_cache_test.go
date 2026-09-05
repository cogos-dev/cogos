package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func cacheReq(msgs ...anthropicMessage) *anthropicRequest {
	return &anthropicRequest{
		Model:    "m",
		System:   []anthropicSystemBlock{{Type: "text", Text: "canonical"}, {Type: "text", Text: "nucleus"}},
		Tools:    []anthropicTool{{Name: "a"}, {Name: "b"}},
		Messages: msgs,
	}
}

func countBreakpoints(t *testing.T, r *anthropicRequest) int {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), `"cache_control":{"type":"ephemeral"}`)
}

func TestCacheBreakpoints_PlacedOnStableRegionEnds(t *testing.T) {
	t.Parallel()
	r := cacheReq(
		anthropicMessage{Role: "user", Content: "hi"},
		anthropicMessage{Role: "assistant", Content: []anthropicContentBlock{{Type: "tool_use", ID: "c1", Name: "a", Input: json.RawMessage(`{}`)}}},
		anthropicMessage{Role: "user", Content: []anthropicContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: "ok"}}},
	)
	applyAnthropicCacheBreakpoints(r)
	sys := r.System.([]anthropicSystemBlock)
	if sys[0].CacheControl != nil || sys[1].CacheControl == nil {
		t.Fatalf("system: only the LAST block must carry cache_control; got %+v", sys)
	}
	if r.Tools[0].CacheControl != nil || r.Tools[1].CacheControl == nil {
		t.Fatalf("tools: only the LAST tool must carry cache_control")
	}
	// second-to-last message (the assistant tool_use) is marked; the final
	// tool_result — the only cold region — is not.
	if blocks := r.Messages[1].Content.([]anthropicContentBlock); blocks[0].CacheControl == nil {
		t.Fatal("second-to-last message must carry the history breakpoint")
	}
	if blocks := r.Messages[2].Content.([]anthropicContentBlock); blocks[0].CacheControl != nil {
		t.Fatal("final message must NOT carry cache_control (it is the cold region)")
	}
	if n := countBreakpoints(t, r); n != 3 {
		t.Fatalf("breakpoints = %d; want 3 (Anthropic max is 4)", n)
	}
}

func TestCacheBreakpoints_StringContentConvertedToBlock(t *testing.T) {
	t.Parallel()
	r := cacheReq(anthropicMessage{Role: "user", Content: "hello"}, anthropicMessage{Role: "assistant", Content: "hi"})
	applyAnthropicCacheBreakpoints(r)
	blocks, ok := r.Messages[0].Content.([]anthropicContentBlock)
	if !ok || len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "hello" || blocks[0].CacheControl == nil {
		t.Fatalf("string content must become one marked text block; got %+v", r.Messages[0].Content)
	}
	if _, isStr := r.Messages[1].Content.(string); !isStr {
		t.Fatal("final message content must be left untouched")
	}
}

func TestCacheBreakpoints_SingleMessageNoHistoryMark(t *testing.T) {
	t.Parallel()
	r := cacheReq(anthropicMessage{Role: "user", Content: "first turn"})
	applyAnthropicCacheBreakpoints(r)
	if n := countBreakpoints(t, r); n != 2 {
		t.Fatalf("with one message only system+tools are marked; breakpoints = %d", n)
	}
}

func TestCacheBreakpoints_Idempotent(t *testing.T) {
	t.Parallel()
	r := cacheReq(anthropicMessage{Role: "user", Content: "a"}, anthropicMessage{Role: "assistant", Content: "b"}, anthropicMessage{Role: "user", Content: "c"})
	applyAnthropicCacheBreakpoints(r)
	applyAnthropicCacheBreakpoints(r)
	if n := countBreakpoints(t, r); n != 3 {
		t.Fatalf("double application must not add breakpoints; got %d", n)
	}
}

// The property that makes caching WORK: Anthropic's cache matches on the
// content of the three regions in spec order (system, tools, messages) up to
// each breakpoint — not on raw JSON bytes. So across consecutive agentic
// steps: system and tools must be unchanged, and the messages up to step N's
// history breakpoint must be a prefix of step N+1's messages. If the
// breakpoint ever landed on content that changes per step, every step would
// be a cold read — the bug this fixes.
func TestCacheBreakpoints_PrefixStableAcrossAgenticSteps(t *testing.T) {
	t.Parallel()
	mustJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	turn := []anthropicMessage{{Role: "user", Content: "list files"}}
	hop1 := []anthropicMessage{
		{Role: "assistant", Content: []anthropicContentBlock{{Type: "tool_use", ID: "c1", Name: "a", Input: json.RawMessage(`{}`)}}},
		{Role: "user", Content: []anthropicContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: "x.go"}}},
	}
	hop2 := []anthropicMessage{
		{Role: "assistant", Content: []anthropicContentBlock{{Type: "tool_use", ID: "c2", Name: "b", Input: json.RawMessage(`{}`)}}},
		{Role: "user", Content: []anthropicContentBlock{{Type: "tool_result", ToolUseID: "c2", Content: "y.go"}}},
	}
	cat := func(parts ...[]anthropicMessage) []anthropicMessage {
		var out []anthropicMessage
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	stepN := cacheReq(cat(turn, hop1)...)
	stepN1 := cacheReq(cat(turn, hop1, hop2)...)
	applyAnthropicCacheBreakpoints(stepN)
	applyAnthropicCacheBreakpoints(stepN1)

	// stable regions identical
	if mustJSON(stepN.System) != mustJSON(stepN1.System) {
		t.Fatal("system region changed between steps")
	}
	if mustJSON(stepN.Tools) != mustJSON(stepN1.Tools) {
		t.Fatal("tools region changed between steps")
	}
	// The CONTENT of step N's messages (markers stripped) must be a prefix of
	// step N+1's content. Anthropic keys the cache on content hashes of the
	// prefix at each breakpoint and looks prior breakpoints up automatically,
	// so N+1 hits N's cached span even though N+1's marker has moved forward.
	strip := func(s string) string { return strings.ReplaceAll(s, `,"cache_control":{"type":"ephemeral"}`, "") }
	cN := strings.TrimSuffix(strip(mustJSON(stepN.Messages)), "]")
	cN1 := strip(mustJSON(stepN1.Messages))
	if !strings.HasPrefix(cN1, cN) {
		t.Fatalf("step N's message content is not a prefix of step N+1\n N : %s\n N1: %s", cN[:min(len(cN), 240)], cN1[:min(len(cN1), 240)])
	}
	// and N+1's breakpoint advanced strictly past N's
	if len(stepN1.Messages)-1 <= len(stepN.Messages)-1 {
		t.Fatal("step N+1's breakpoint must advance past step N's")
	}
}
