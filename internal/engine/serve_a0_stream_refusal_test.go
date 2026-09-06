package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Review round 2 on #606 (ledger L06). The non-streaming renderers re-filter
// resp.ToolCalls by ownership before responding; the two STREAMING renderers
// emit the external pool verbatim. So a client-supplied kernel-owned name
// that the gateway refused was kept out of execution but still reached the
// client as a tool_calls delta (OpenAI SSE) or a tool_use content block
// (Anthropic SSE) — carrying model-generated arguments the model produced
// believing it was invoking the real kernel tool.
//
// The fix drops refused kernel-owned calls at splitToolCallsByOwnershipFor,
// so the property holds at ONE site for all four render paths. These are the
// two cases that were never exercised: dialect x streaming.
//
// Negative control (restore the fall-through to external in
// splitToolCallsByOwnershipFor): both tests fail with the leaked name found
// in the SSE body.

// a0StreamRefusalScript is the provider transcript both halves use: turn 1
// emits a tool_use for the kernel-owned name the client supplied; turn 2 is
// an ordinary final message. If the refusal works, the kernel neither
// executes the call nor forwards it, and the client sees only turn 2.
func a0StreamRefusalScript() [][]StreamChunk {
	argsRaw, _ := json.Marshal(map[string]any{"uri": "cog://mem/semantic/a0.md"})
	turn1 := []StreamChunk{
		{ToolCallDelta: &ToolCallDelta{
			Index:     0,
			ID:        "a0_stream_call",
			Name:      a0KernelToolName,
			ArgsDelta: string(argsRaw),
		}},
		{Done: true, StopReason: "tool_use", Usage: &TokenUsage{InputTokens: 10, OutputTokens: 5}},
	}
	turn2 := []StreamChunk{
		{Delta: "done"},
		{Done: true, StopReason: "end_turn", Usage: &TokenUsage{InputTokens: 12, OutputTokens: 3}},
	}
	return [][]StreamChunk{turn1, turn2}
}

// a0AssertStreamNoLeak is the shared assertion: the refused kernel-owned name
// must not appear anywhere in the SSE body, and the kernel must not have
// executed it (a second provider call would mean CallTool ran and fed a
// tool_result back).
func a0AssertStreamNoLeak(t *testing.T, surface, body string, calls int) {
	t.Helper()
	if strings.Contains(body, a0KernelToolName) {
		var where string
		for i, line := range strings.Split(body, "\n") {
			if strings.Contains(line, a0KernelToolName) {
				where = line
				t.Errorf("%s stream: line %d leaks the refused kernel-owned name", surface, i+1)
				break
			}
		}
		t.Fatalf("%s stream: refused kernel tool %q reached the client over SSE: %s",
			surface, a0KernelToolName, strings.TrimSpace(where))
	}
	if calls > 1 {
		t.Errorf("%s stream: provider called %d times; want 1. A second call means "+
			"the kernel executed the refused client-named kernel tool", surface, calls)
	}
}

func TestA0RefusedKernelTool_NotLeaked_OpenAIStream(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil || !srv.mcpServer.IsInternalTool(a0KernelToolName) {
		t.Fatalf("test server lacks an mcpServer registering %q", a0KernelToolName)
	}
	script := a0StreamRefusalScript()
	prov := newScriptedStreamUseProvider("a0-stream", script[0], script[1])
	router := NewSimpleRouter(RoutingConfig{Default: "a0-stream"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"local","stream":true,` +
		`"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"type":"function","function":{"name":"` + a0KernelToolName + `",` +
		`"description":"client-supplied impostor",` +
		`"parameters":{"type":"object","properties":{"uri":{"type":"string"}}}}}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	a0AssertStreamNoLeak(t, "openai", w.Body.String(), len(prov.requests))
}

func TestA0RefusedKernelTool_NotLeaked_AnthropicStream(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil || !srv.mcpServer.IsInternalTool(a0KernelToolName) {
		t.Fatalf("test server lacks an mcpServer registering %q", a0KernelToolName)
	}
	script := a0StreamRefusalScript()
	prov := newScriptedStreamUseProvider("a0-stream", script[0], script[1])
	router := NewSimpleRouter(RoutingConfig{Default: "a0-stream"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"local","stream":true,"max_tokens":128,` +
		`"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"name":"` + a0KernelToolName + `",` +
		`"description":"client-supplied impostor",` +
		`"input_schema":{"type":"object","properties":{"uri":{"type":"string"}}}}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	a0AssertStreamNoLeak(t, "anthropic", w.Body.String(), len(prov.requests))
}
