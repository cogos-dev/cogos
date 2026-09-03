// serve_anthropic_tools_test.go — tool support on the Anthropic-shape
// POST /v1/messages surface: request translation (tools, tool_choice,
// tool_use/tool_result history), non-stream tool_use rendering, streaming
// tool_use event sequence, and kernel-ownership enforcement.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Request translation ──────────────────────────────────────────────────────

func TestAnthropicToOpenAIRequest_Tools(t *testing.T) {
	t.Parallel()
	req := &anthropicMessagesRequest{
		Model: "claude",
		Tools: []anthropicTool{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
		Messages: []anthropicInputMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out := anthropicToOpenAIRequest(req)
	if len(out.Tools) != 1 {
		t.Fatalf("Tools len = %d; want 1", len(out.Tools))
	}
	tl := out.Tools[0]
	if tl.Type != "function" || tl.Function.Name != "get_weather" || tl.Function.Description != "Get weather" {
		t.Fatalf("tool = %+v", tl)
	}
	if tl.Function.Parameters["type"] != "object" {
		t.Fatalf("parameters = %v", tl.Function.Parameters)
	}
}

func TestAnthropicToolChoiceString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   *anthropicToolChoice
		want string
	}{
		{nil, ""},
		{&anthropicToolChoice{Type: "auto"}, "auto"},
		{&anthropicToolChoice{Type: "none"}, "none"},
		{&anthropicToolChoice{Type: "any"}, "required"},
		{&anthropicToolChoice{Type: "tool", Name: "get_weather"}, "get_weather"},
		{&anthropicToolChoice{Type: "bogus"}, ""},
	}
	for _, c := range cases {
		if got := anthropicToolChoiceString(c.in); got != c.want {
			t.Errorf("anthropicToolChoiceString(%+v) = %q; want %q", c.in, got, c.want)
		}
	}
}

// Tool-loop history round-trip: assistant tool_use → assistant.tool_calls,
// user tool_result → role:"tool" with tool_call_id.
func TestAnthropicToOpenAIRequest_ToolUseToolResultHistory(t *testing.T) {
	t.Parallel()
	req := &anthropicMessagesRequest{
		Model: "claude",
		Messages: []anthropicInputMessage{
			{Role: "user", Content: json.RawMessage(`"what's the weather?"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny, 20C"}
			]`)},
		},
	}
	out := anthropicToOpenAIRequest(req)
	// Note: an empty system field still yields a leading system message
	// (pre-existing normalizeAnthropicContent behavior), so expect 4.
	if len(out.Messages) != 4 {
		t.Fatalf("messages len = %d; want 4: %+v", len(out.Messages), out.Messages)
	}
	asst := out.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) == 0 {
		t.Fatalf("assistant msg = %+v; want tool_calls", asst)
	}
	var calls []oaiToolCall
	if err := json.Unmarshal(asst.ToolCalls, &calls); err != nil {
		t.Fatalf("decode tool_calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ID != "toolu_1" || calls[0].Function.Name != "get_weather" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil || args["city"] != "SF" {
		t.Fatalf("arguments = %q err=%v", calls[0].Function.Arguments, err)
	}
	toolMsg := out.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "toolu_1" {
		t.Fatalf("tool msg = %+v; want role=tool tool_call_id=toolu_1", toolMsg)
	}
	if got := extractContent(toolMsg.Content); got != "sunny, 20C" {
		t.Fatalf("tool result content = %q", got)
	}
}

// tool_result content may also be an array of blocks.
func TestAnthropicToolResultText_BlockArray(t *testing.T) {
	t.Parallel()
	got := anthropicToolResultText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`))
	if got != "a\nb" {
		t.Fatalf("got %q; want a\\nb", got)
	}
	if got := anthropicToolResultText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatalf("got %q; want plain", got)
	}
}

// ── Non-stream tool_use rendering ────────────────────────────────────────────

func TestHandleAnthropicMessagesNonStreaming_ToolUse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	stub := NewStubProvider("stub", "")
	stub.toolCalls = []ToolCall{{
		ID:        "toolu_abc",
		Name:      "get_weather",
		Arguments: `{"city":"SF"}`,
	}}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"claude","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto"},"max_tokens":64,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}

	// Provider must have received the tool definitions + tool_choice.
	if stub.lastRequest == nil || len(stub.lastRequest.Tools) != 1 || stub.lastRequest.Tools[0].Name != "get_weather" {
		t.Fatalf("provider did not receive tools: %+v", stub.lastRequest)
	}
	if len(stub.lastRequest.ExternalTools) != 1 {
		t.Fatalf("get_weather should be external; ExternalTools=%+v", stub.lastRequest.ExternalTools)
	}
	if stub.lastRequest.ToolChoice != "auto" {
		t.Fatalf("ToolChoice = %q; want auto", stub.lastRequest.ToolChoice)
	}

	var resp struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var tu *struct {
		Type  string         `json:"type"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	for i := range resp.Content {
		if resp.Content[i].Type == "tool_use" {
			tu = &resp.Content[i]
		}
	}
	if tu == nil {
		t.Fatalf("no tool_use content block: %+v", resp.Content)
	}
	if tu.ID != "toolu_abc" || tu.Name != "get_weather" {
		t.Fatalf("tool_use = %+v", tu)
	}
	if tu.Input["city"] != "SF" {
		t.Fatalf("input not parsed as object: %+v", tu.Input)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q; want tool_use", resp.StopReason)
	}
}

// ── Streaming tool_use event sequence ────────────────────────────────────────

// parseAnthropicSSEEvents decodes an SSE body into an ordered list of (event, data)
// pairs — same structural approach as TestHandleAnthropicMessagesStreaming_SDKShape.
func parseAnthropicSSEEvents(t *testing.T, body string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		var ev, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" || data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event %s: not JSON: %v", ev, err)
		}
		out = append(out, struct {
			Event string
			Data  map[string]any
		}{ev, m})
	}
	return out
}

func TestHandleAnthropicMessagesStreaming_ToolUse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	stub := NewStubProvider("stub", "")
	stub.chunks = []string{"let me check"}
	stub.toolCalls = []ToolCall{{
		ID:        "toolu_str",
		Name:      "get_weather",
		Arguments: `{"city":"SF"}`,
	}}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"claude","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}],
		"max_tokens":64,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	events := parseAnthropicSSEEvents(t, w.Body.String())

	// Find the tool_use content_block_start; it must be at a distinct
	// (non-zero) index with id/name/empty-object input.
	var toolIdx float64 = -1
	var sawInputDelta, sawToolStop bool
	var partialJSON string
	var finalStop string
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			cb, _ := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				toolIdx = e.Data["index"].(float64)
				if cb["id"] != "toolu_str" || cb["name"] != "get_weather" {
					t.Fatalf("tool_use start = %v", cb)
				}
				if _, ok := cb["input"].(map[string]any); !ok {
					t.Fatalf("tool_use start input not an object: %v", cb["input"])
				}
			}
		case "content_block_delta":
			d, _ := e.Data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				if e.Data["index"].(float64) != toolIdx {
					t.Fatalf("input_json_delta at index %v; want %v", e.Data["index"], toolIdx)
				}
				partialJSON += d["partial_json"].(string)
				sawInputDelta = true
			}
		case "content_block_stop":
			if e.Data["index"].(float64) == toolIdx && toolIdx >= 0 {
				sawToolStop = true
			}
		case "message_delta":
			d, _ := e.Data["delta"].(map[string]any)
			if sr, ok := d["stop_reason"].(string); ok {
				finalStop = sr
			}
			if _, ok := e.Data["usage"].(map[string]any); !ok {
				t.Fatalf("message_delta.usage missing: %v", e.Data)
			}
		}
	}
	if toolIdx <= 0 {
		t.Fatalf("tool_use block index = %v; want distinct index > 0", toolIdx)
	}
	if !sawInputDelta || !sawToolStop {
		t.Fatalf("sawInputDelta=%v sawToolStop=%v", sawInputDelta, sawToolStop)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(partialJSON), &parsed); err != nil || parsed["city"] != "SF" {
		t.Fatalf("assembled partial_json = %q err=%v", partialJSON, err)
	}
	if finalStop != "tool_use" {
		t.Fatalf("final stop_reason = %q; want tool_use", finalStop)
	}
	// #599 regression guard: message_start.message.usage must be present.
	msgStart := events[0]
	if msgStart.Event != "message_start" {
		t.Fatalf("first event = %s; want message_start", msgStart.Event)
	}
	msg, _ := msgStart.Data["message"].(map[string]any)
	if _, ok := msg["usage"].(map[string]any); !ok {
		t.Fatalf("message_start.message.usage missing")
	}
}

// ── Kernel-ownership enforcement ─────────────────────────────────────────────

// A kernel-owned (MCP-internal) tool call must be executed in-process and
// never forwarded to the client as a tool_use block.
func TestHandleAnthropicMessagesNonStreaming_InternalToolNotForwarded(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer wired")
	}
	if !srv.mcpServer.IsInternalTool("cog_read_cogdoc") {
		t.Fatal("mcpServer snapshot missing cog_read_cogdoc")
	}

	scripted := []*CompletionResponse{
		{
			StopReason: "tool_use",
			ToolCalls: []ToolCall{{
				ID:        "call_int_1",
				Name:      "cog_read_cogdoc",
				Arguments: `{"uri":"cog://mem/does-not-exist.md"}`,
			}},
			ProviderMeta: ProviderMeta{Provider: "scripted", Model: "scripted"},
		},
		{
			Content:      "done after internal tool",
			StopReason:   "end_turn",
			ProviderMeta: ProviderMeta{Provider: "scripted", Model: "scripted"},
		},
	}
	prov := newScriptedToolUseProvider("scripted", scripted...)
	router := NewSimpleRouter(RoutingConfig{Default: "scripted"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"claude","messages":[{"role":"user","content":"go"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if got := len(prov.requests); got != 2 {
		t.Fatalf("provider calls = %d; want 2 (tool hop + final)", got)
	}
	// Second provider request must carry the role=tool result message.
	sawToolMsg := false
	for _, m := range prov.requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_int_1" {
			sawToolMsg = true
		}
	}
	if !sawToolMsg {
		t.Fatalf("second request lacks role=tool result: %+v", prov.requests[1].Messages)
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range resp.Content {
		if c.Type == "tool_use" {
			t.Fatalf("kernel-owned tool_use leaked to client: %+v", resp.Content)
		}
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q; want end_turn", resp.StopReason)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "done after internal tool" {
		t.Fatalf("content = %+v", resp.Content)
	}
}
