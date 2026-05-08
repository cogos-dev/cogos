package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fixture lines are captured Anthropic stream-json NDJSON events.
// Each string is one NDJSON line as emitted by `claude --output-format stream-json`.

// toolUseFixture exercises the full tool_use event sequence:
//
//	content_block_start (tool_use) → content_block_delta (input_json_delta) × N → content_block_stop → result
var toolUseFixture = []string{
	// system init
	`{"type":"system","subtype":"init","session_id":"test-session","tools":[],"mcp_servers":[]}`,
	// content_block_start for a tool_use block at index 0
	`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"calculator"}}}`,
	// first input_json_delta chunk
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"op\""}}}`,
	// second input_json_delta chunk
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"+\",\"a\":2,\"b\":2}"}}}`,
	// content_block_stop finalises the tool call
	`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	// result message
	`{"type":"result","subtype":"success","result":"4","is_error":false,"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`,
}

// textOnlyFixture exercises normal text streaming with no tool calls.
var textOnlyFixture = []string{
	`{"type":"system","subtype":"init","session_id":"test-session","tools":[],"mcp_servers":[]}`,
	`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"","name":""}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world!"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	`{"type":"result","subtype":"success","result":"Hello, world!","is_error":false,"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":3}}`,
}

// multiToolFixture exercises two tool calls at different content block indices.
var multiToolFixture = []string{
	`{"type":"system","subtype":"init","session_id":"test-session","tools":[],"mcp_servers":[]}`,
	// first tool call at index 0
	`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read_file"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/foo\"}"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	// second tool call at index 1
	`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_02","name":"write_file"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/bar\"}"}}}`,
	`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
	`{"type":"result","subtype":"success","result":"done","is_error":false,"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":10}}`,
}

func fixtureReader(lines []string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n"))
}

// ── drainStreamJSON tests ─────────────────────────────────────────────────────

func TestDrainStreamJSON_TextOnly(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	content, toolCalls, usage, stopReason, err := p.drainStreamJSON(fixtureReader(textOnlyFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "Hello, world!" {
		t.Errorf("expected content %q, got %q", "Hello, world!", content)
	}
	if len(toolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(toolCalls))
	}
	if usage.InputTokens != 8 {
		t.Errorf("expected 8 input tokens, got %d", usage.InputTokens)
	}
	if stopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %q", stopReason)
	}
}

func TestDrainStreamJSON_SingleToolCall(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	content, toolCalls, usage, stopReason, err := p.drainStreamJSON(fixtureReader(toolUseFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No text content in this fixture.
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "toolu_01" {
		t.Errorf("expected tool ID %q, got %q", "toolu_01", tc.ID)
	}
	if tc.Name != "calculator" {
		t.Errorf("expected tool name %q, got %q", "calculator", tc.Name)
	}
	want := `{"op":"+","a":2,"b":2}`
	if tc.Arguments != want {
		t.Errorf("expected arguments %q, got %q", want, tc.Arguments)
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", usage.InputTokens)
	}
	if stopReason != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %q", stopReason)
	}
}

func TestDrainStreamJSON_MultipleToolCalls(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	_, toolCalls, _, _, err := p.drainStreamJSON(fixtureReader(multiToolFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(toolCalls))
	}

	names := map[string]bool{}
	for _, tc := range toolCalls {
		names[tc.Name] = true
	}
	if !names["read_file"] {
		t.Error("expected read_file tool call")
	}
	if !names["write_file"] {
		t.Error("expected write_file tool call")
	}

	ids := map[string]bool{}
	for _, tc := range toolCalls {
		ids[tc.ID] = true
	}
	if !ids["toolu_01"] {
		t.Error("expected toolu_01 ID")
	}
	if !ids["toolu_02"] {
		t.Error("expected toolu_02 ID")
	}
}

// ── parseStreamLine unit tests ────────────────────────────────────────────────

func TestParseStreamLine_TextDelta(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("text_delta should not be a done signal")
	}
	if chunk == nil {
		t.Fatal("expected a chunk")
	}
	if chunk.Delta != "hi" {
		t.Errorf("expected delta %q, got %q", "hi", chunk.Delta)
	}
	if chunk.ToolCallDelta != nil {
		t.Error("text_delta should not produce a ToolCallDelta")
	}
}

func TestParseStreamLine_ContentBlockStart_ToolUse(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_99","name":"bash"}}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("content_block_start should not be a done signal")
	}
	if chunk == nil {
		t.Fatal("expected a chunk for tool_use start")
	}
	if chunk.ToolCallDelta == nil {
		t.Fatal("expected ToolCallDelta")
	}
	if chunk.ToolCallDelta.ID != "toolu_99" {
		t.Errorf("expected ID %q, got %q", "toolu_99", chunk.ToolCallDelta.ID)
	}
	if chunk.ToolCallDelta.Name != "bash" {
		t.Errorf("expected Name %q, got %q", "bash", chunk.ToolCallDelta.Name)
	}
	if chunk.ToolCallDelta.Index != 2 {
		t.Errorf("expected Index 2, got %d", chunk.ToolCallDelta.Index)
	}
	// Pending entry should be registered.
	if _, ok := state.pending[2]; !ok {
		t.Error("state.pending should have entry at index 2")
	}
}

func TestParseStreamLine_ContentBlockStart_Text_NoChunk(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","id":"","name":""}}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("content_block_start for text should not be a done signal")
	}
	// text content_block_start produces no chunk — delta events carry the text.
	if chunk != nil {
		t.Errorf("expected nil chunk for text content_block_start, got %+v", chunk)
	}
}

func TestParseStreamLine_InputJSONDelta_AppendsToState(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	// Seed a pending tool call so delta has somewhere to write.
	state.pending[0] = &partialToolCall{id: "toolu_01", name: "calc"}

	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"n\":1}"}}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("input_json_delta should not be done")
	}
	if chunk == nil || chunk.ToolCallDelta == nil {
		t.Fatal("expected ToolCallDelta chunk")
	}
	if chunk.ToolCallDelta.ArgsDelta != `{"n":1}` {
		t.Errorf("expected ArgsDelta %q, got %q", `{"n":1}`, chunk.ToolCallDelta.ArgsDelta)
	}
	if state.pending[0].args.String() != `{"n":1}` {
		t.Errorf("state args buffer wrong: %q", state.pending[0].args.String())
	}
}

func TestParseStreamLine_ContentBlockStop_FinalizesToolCall(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	ptc := &partialToolCall{id: "toolu_01", name: "calc"}
	ptc.args.WriteString(`{"n":42}`)
	state.pending[0] = ptc

	line := []byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("content_block_stop should not be a done signal")
	}
	if chunk != nil {
		t.Errorf("content_block_stop should produce nil chunk, got %+v", chunk)
	}
	// pending entry should be removed and added to done.
	if _, ok := state.pending[0]; ok {
		t.Error("pending entry should be removed after stop")
	}
	if len(state.done) != 1 {
		t.Fatalf("expected 1 finalized tool call, got %d", len(state.done))
	}
	tc := state.done[0]
	if tc.ID != "toolu_01" || tc.Name != "calc" || tc.Arguments != `{"n":42}` {
		t.Errorf("finalized tool call wrong: %+v", tc)
	}
}

func TestParseStreamLine_ContentBlockStop_TextBlock_NoOp(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	// No pending entry at index 0 — should be a no-op.
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
	chunk, done := p.parseStreamLine(line, state)
	if done {
		t.Fatal("content_block_stop should not be done")
	}
	if chunk != nil {
		t.Errorf("no-op stop should produce nil chunk")
	}
	if len(state.done) != 0 {
		t.Error("no finalized calls expected")
	}
}

func TestParseStreamLine_Result_IsDone(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	line := []byte(`{"type":"result","subtype":"success","result":"4","is_error":false,"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
	chunk, done := p.parseStreamLine(line, state)
	if !done {
		t.Fatal("result message should signal done")
	}
	if chunk == nil {
		t.Fatal("result message should produce a chunk")
	}
	if !chunk.Done {
		t.Error("chunk.Done should be true")
	}
	if chunk.StopReason != "tool_use" {
		t.Errorf("expected stop_reason %q, got %q", "tool_use", chunk.StopReason)
	}
	if chunk.Usage == nil || chunk.Usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens in usage, got %+v", chunk.Usage)
	}
}

func TestParseStreamLine_ErrorResult(t *testing.T) {
	t.Parallel()

	p := &ClaudeCodeProvider{name: "test", model: "sonnet"}
	state := newCCStreamState()
	line := []byte(`{"type":"result","is_error":true,"result":"something went wrong","stop_reason":""}`)
	chunk, done := p.parseStreamLine(line, state)
	if !done {
		t.Fatal("error result should signal done")
	}
	if chunk.Error == nil {
		t.Fatal("expected an error in chunk")
	}
	if !strings.Contains(chunk.Error.Error(), "something went wrong") {
		t.Errorf("error message wrong: %v", chunk.Error)
	}
}

// ── Context-cancel / subprocess-kill tests ───────────────────────────────────

// TestComplete_ContextCancel_KillsSubprocess verifies that cancelling the
// context actually kills the subprocess instead of leaving cmd.Wait() stuck in
// waitpid(2). This is the regression test for myrgic/cogos#79.
//
// Strategy: point cliBinary at a tiny shell wrapper that ignores all its
// arguments and just sleeps for 30 s. Cancel the context after 50 ms and
// assert that Complete() returns well within the 10 s WaitDelay grace period
// (we allow 12 s total for headroom). The test would time out or take ≥30 s
// without the fix.
func TestComplete_ContextCancel_KillsSubprocess(t *testing.T) {
	// Write a shell wrapper to a temp file. The wrapper ignores all arguments
	// (the claude flags injected by buildArgs) and just sleeps so we can observe
	// the kill behaviour.
	script, err := os.CreateTemp(t.TempDir(), "fake-claude-*.sh")
	if err != nil {
		t.Fatalf("create temp script: %v", err)
	}
	fmt.Fprintln(script, "#!/bin/sh")
	fmt.Fprintln(script, "sleep 30")
	script.Close()
	if err := os.Chmod(script.Name(), 0o755); err != nil {
		t.Fatalf("chmod temp script: %v", err)
	}

	procMgr := NewProcessManager(ProcessManagerConfig{})
	p := &ClaudeCodeProvider{
		name:      "test",
		model:     "sonnet",
		cliBinary: script.Name(),
		procMgr:   procMgr,
	}

	req := &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "hello"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context after a short delay to let the subprocess start.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, callErr := p.Complete(ctx, req)
	elapsed := time.Since(start)

	// Complete() must return — this is the essential assertion.
	// Without the fix, cmd.Wait() sits in waitpid(2) indefinitely.

	// Must return in time: well under the 30 s sleep but allowing for the 10 s
	// WaitDelay escalation window plus scheduling slack.
	const deadline = 12 * time.Second
	if elapsed > deadline {
		t.Errorf("Complete() took %v; expected < %v — subprocess was not killed on cancel", elapsed, deadline)
	}

	// Must report a cancellation error.
	if callErr == nil {
		t.Error("Complete() returned nil error; expected cancellation error")
	} else if !strings.Contains(callErr.Error(), "cancel") && !strings.Contains(callErr.Error(), "context") {
		t.Errorf("Complete() error %q does not mention cancellation", callErr)
	}
}
