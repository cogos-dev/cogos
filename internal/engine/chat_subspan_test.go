// chat_subspan_test.go — unit tests for completeChat sub-span emission.
//
// Verifies that handleChat emits kernel.chat.subspan.v1 events to bus_traces
// for all four phases (prompt_eval, thinking_generation, answer_generation,
// tool_call_resolution) under the non-streaming path, using a mock provider
// that returns synthetic responses with and without reasoning content.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// reasoningStubProvider wraps StubProvider to inject ReasoningContent into
// Complete() responses, simulating a non-streaming reasoning-model reply
// (e.g. an LM Studio-served 26b A4B model).
type reasoningStubProvider struct {
	StubProvider
	reasoningContent string
	outputTokens     int
}

func newReasoningStubProvider(name, reasoning, answer string, outputTokens int) *reasoningStubProvider {
	return &reasoningStubProvider{
		StubProvider:     *NewStubProvider(name, answer),
		reasoningContent: reasoning,
		outputTokens:     outputTokens,
	}
}

func (p *reasoningStubProvider) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	// Simulate a non-trivial round-trip so duration_ms > 0.
	time.Sleep(2 * time.Millisecond)
	resp, err := p.StubProvider.Complete(context.Background(), req)
	if err != nil {
		return nil, err
	}
	resp.ReasoningContent = p.reasoningContent
	if p.outputTokens > 0 {
		resp.Usage.OutputTokens = p.outputTokens
	}
	return resp, nil
}

// subspansByName reads bus_traces from the BusSessionManager and returns a map
// of span_name → payload for the most recent event of each span_name.
func subspansByName(t *testing.T, bus *BusSessionManager) map[string]map[string]interface{} {
	t.Helper()
	events, err := bus.ReadEvents(BusTraces)
	if err != nil {
		t.Fatalf("ReadEvents(bus_traces): %v", err)
	}
	result := make(map[string]map[string]interface{})
	for _, e := range events {
		if e.Type != "kernel.chat.subspan.v1" {
			continue
		}
		var p map[string]interface{}
		raw, _ := json.Marshal(e.Payload)
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		name, _ := p["span_name"].(string)
		result[name] = p
	}
	return result
}

// TestCompleteChatSubSpans_NoReasoning verifies that a non-streaming response
// WITHOUT reasoning content emits prompt_eval and answer_generation, but NOT
// thinking_generation or tool_call_resolution.
//
// It also verifies that answer_generation.duration_ms is non-zero — this is
// the core regression check: pre-fix, duration_ms was 0 because answerStart
// and answerEnd were set in consecutive time.Now() calls with no work between
// them.
func TestCompleteChatSubSpans_NoReasoning(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	stub := newReasoningStubProvider("stub", "", "hello world", 10)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}

	spans := subspansByName(t, srv.busSessions)

	// prompt_eval must always fire.
	if _, ok := spans["chat.prompt_eval"]; !ok {
		t.Error("chat.prompt_eval span not emitted")
	}

	// answer_generation must fire with non-zero token count and non-zero duration.
	ansSpan, ok := spans["chat.answer_generation"]
	if !ok {
		t.Fatal("chat.answer_generation span not emitted")
	}
	tokAnswer, _ := ansSpan["tokens_answer"].(float64)
	if tokAnswer <= 0 {
		t.Errorf("chat.answer_generation tokens_answer = %v; want > 0", tokAnswer)
	}
	// Regression check: duration_ms must reflect the actual round-trip, not 0.
	durationMS, _ := ansSpan["duration_ms"].(float64)
	if durationMS <= 0 {
		t.Errorf("chat.answer_generation duration_ms = %v; want > 0 "+
			"(non-streaming: answer duration should equal the round-trip, not 0)", durationMS)
	}

	// thinking_generation must NOT fire when there is no reasoning content.
	if _, ok := spans["chat.thinking_generation"]; ok {
		t.Error("chat.thinking_generation emitted unexpectedly (response has no reasoning content)")
	}

	// tool_call_resolution must NOT fire (no tool calls).
	if _, ok := spans["chat.tool_call_resolution"]; ok {
		t.Error("chat.tool_call_resolution emitted unexpectedly (no tool calls in response)")
	}
}

// TestCompleteChatSubSpans_WithReasoning verifies that a non-streaming response
// WITH ReasoningContent populated emits all three generation spans:
// prompt_eval, thinking_generation, and answer_generation.
//
// Key assertions:
//   - thinking_generation.tokens_think > 0
//   - answer_generation.tokens_answer matches provider usage (42 in the mock)
//   - thinking + answer duration_ms ≤ prompt_eval duration_ms + 1ms tolerance
func TestCompleteChatSubSpans_WithReasoning(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	// 100-rune thinking, 400-rune answer → ~20% / 80% proportional split.
	thinking := strings.Repeat("a", 100)
	answer := strings.Repeat("b", 400)
	stub := newReasoningStubProvider("stub", thinking, answer, 42)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"think hard"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}

	spans := subspansByName(t, srv.busSessions)

	// All three generation spans must fire.
	for _, name := range []string{"chat.prompt_eval", "chat.thinking_generation", "chat.answer_generation"} {
		if _, ok := spans[name]; !ok {
			t.Errorf("span %q not emitted", name)
		}
	}

	// thinking_generation token count should be non-zero (rune proxy for thinking).
	thinkSpan := spans["chat.thinking_generation"]
	tokThink, _ := thinkSpan["tokens_think"].(float64)
	if tokThink <= 0 {
		t.Errorf("chat.thinking_generation tokens_think = %v; want > 0", tokThink)
	}

	// answer_generation tokens_answer should reflect the provider usage field.
	ansSpan := spans["chat.answer_generation"]
	tokAnswer, _ := ansSpan["tokens_answer"].(float64)
	if tokAnswer != 42 {
		t.Errorf("chat.answer_generation tokens_answer = %v; want 42", tokAnswer)
	}

	// answer_generation duration_ms must be positive.
	ansMs, _ := ansSpan["duration_ms"].(float64)
	if ansMs <= 0 {
		t.Errorf("chat.answer_generation duration_ms = %v; want > 0", ansMs)
	}

	// The sum of thinking + answer durations must not exceed prompt_eval + 1ms
	// tolerance (they are proportional slices of the same round-trip window).
	promptSpan := spans["chat.prompt_eval"]
	promptMs, _ := promptSpan["duration_ms"].(float64)
	thinkMs, _ := thinkSpan["duration_ms"].(float64)
	if thinkMs+ansMs > promptMs+1 {
		t.Errorf("thinking_ms(%v) + answer_ms(%v) = %v > prompt_eval_ms(%v) + 1; "+
			"sub-span timings exceed round-trip budget",
			thinkMs, ansMs, thinkMs+ansMs, promptMs)
	}

	// tool_call_resolution must NOT fire.
	if _, ok := spans["chat.tool_call_resolution"]; ok {
		t.Error("chat.tool_call_resolution emitted unexpectedly (no tool calls)")
	}
}
