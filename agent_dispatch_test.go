// agent_dispatch_test.go — Unit coverage for the Phase 2 dispatch transport.
//
// Tests use httptest.Server stand-ins for both the Ollama native /api/chat
// endpoint and the LM Studio /v1/chat/completions endpoint. No live Gemma is
// required; the harness's chatCompletionTo is exercised against the stand-in.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
)

// ollamaServer returns a stand-in /api/chat endpoint that responds with the
// canned agentChatResponse on every call. The handler increments callCount
// so tests can assert per-slot fan-out behaviour.
func ollamaServer(t *testing.T, callCount *atomic.Int32, responder func(callIdx int) agentChatResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("expected /api/chat, got %s", r.URL.Path)
		}
		idx := int(callCount.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responder(idx))
	}))
}

// openaiServer returns a stand-in /v1/chat/completions endpoint that translates
// the canned agentChatResponse into the OpenAI choices[].message envelope.
func openaiServer(t *testing.T, callCount *atomic.Int32, responder func(callIdx int) agentChatResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gemma-3-27b-it"}]}`))
			return
		case "/v1/chat/completions":
			idx := int(callCount.Add(1)) - 1
			canned := responder(idx)
			out := map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role":       canned.Message.Role,
							"content":    canned.Message.Content,
							"tool_calls": canned.Message.ToolCalls,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
}

// TestDispatch_SinglePath_ContentReturned verifies the simplest happy path:
// n=1, no tool calls, the agent emits content, the dispatcher surfaces it
// as DispatchResult.Content with Success=true.
func TestDispatch_SinglePath_ContentReturned(t *testing.T) {
	var calls atomic.Int32
	server := ollamaServer(t, &calls, func(i int) agentChatResponse {
		return makeContentResponse(fmt.Sprintf("hello slot %d", i))
	})
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{Task: "say hello", N: 1}
	if err := req.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	res, err := d.DispatchToHarness(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := len(res.Results); got != 1 {
		t.Fatalf("expected 1 result, got %d", got)
	}
	r0 := res.Results[0]
	if !r0.Success {
		t.Fatalf("expected success, got error=%q", r0.Error)
	}
	if r0.Content != "hello slot 0" {
		t.Fatalf("expected content 'hello slot 0', got %q", r0.Content)
	}
	if r0.ModelUsed != engine.DispatchModelE4B {
		t.Fatalf("expected ModelUsed=e4b, got %q", r0.ModelUsed)
	}
	if r0.Turns != 1 {
		t.Fatalf("expected 1 turn, got %d", r0.Turns)
	}
}

// TestDispatch_RespondToolContent verifies that when the agent invokes the
// respond tool (rather than emitting plain text), Content is populated with
// the respond tool's "text" argument — the contract for Phase 2'.
func TestDispatch_RespondToolContent(t *testing.T) {
	var calls atomic.Int32
	server := ollamaServer(t, &calls, func(i int) agentChatResponse {
		// Turn 0: respond tool. Turn 1: empty content (loop terminates).
		if i == 0 {
			return makeToolCallResponse("call_resp", "respond", `{"text":"local-model says hi"}`)
		}
		return makeContentResponse("")
	})
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	RegisterRespondTool(h)
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{Task: "greet the user", N: 1}
	_ = req.Normalize()
	res, err := d.DispatchToHarness(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	r0 := res.Results[0]
	if !r0.Success {
		t.Fatalf("expected success, got error=%q", r0.Error)
	}
	if r0.Content != "local-model says hi" {
		t.Fatalf("expected respond text, got %q", r0.Content)
	}
	// Tool-call summary should record the respond invocation.
	if len(r0.ToolCalls) != 1 || r0.ToolCalls[0].Name != "respond" {
		t.Fatalf("expected one respond tool-call summary, got %+v", r0.ToolCalls)
	}
}

// TestDispatch_ConcurrentN3 dispatches 3 slots in one batch and asserts that
// (a) all three return distinct results, (b) no slot's failure is visible in
// the others, (c) total elapsed time is closer to one slot's latency than
// three (concurrency, not serialization).
func TestDispatch_ConcurrentN3(t *testing.T) {
	var calls atomic.Int32
	const slotDelay = 80 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		// Inject a deterministic per-call latency so the wall-clock
		// concurrency win is observable regardless of host load.
		time.Sleep(slotDelay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(makeContentResponse(fmt.Sprintf("reply-%d", idx)))
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{Task: "echo", N: 3}
	_ = req.Normalize()
	start := time.Now()
	res, err := d.DispatchToHarness(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := len(res.Results); got != 3 {
		t.Fatalf("expected 3 results, got %d", got)
	}

	// Distinct results: index 0..2 each visible.
	seen := make(map[string]bool)
	for _, r := range res.Results {
		if !r.Success {
			t.Errorf("slot %d not successful: %q", r.Index, r.Error)
		}
		if r.Content == "" {
			t.Errorf("slot %d had empty content", r.Index)
		}
		seen[r.Content] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct contents, got %d (%v)", len(seen), seen)
	}

	// Concurrency check: serialized would be ~3*slotDelay (240ms) plus
	// margin; concurrent should be closer to 1*slotDelay. Allow 2.5x as
	// a generous threshold for CI noise.
	if elapsed > slotDelay*5/2 {
		t.Errorf("expected concurrent dispatch (elapsed ~%v), got %v", slotDelay, elapsed)
	}
}

// TestDispatch_ToolScopeNarrowing verifies AllowedTools restricts the
// dispatch to the named subset and that an unknown name surfaces as an
// invalid-input error before any HTTP call is made.
func TestDispatch_ToolScopeNarrowing(t *testing.T) {
	var calls atomic.Int32
	var seenTools []ToolDefinition
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body agentChatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if seenTools == nil {
			seenTools = body.Tools
		}
		_ = calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(makeContentResponse("ok"))
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	// Register two tools so we can scope to one.
	RegisterRespondTool(h)
	RegisterWaitTool(h, "/tmp/unused")
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	// Happy path: scope to just `wait`, expect only wait sent to the model.
	req := engine.DispatchRequest{Task: "scoped run", Tools: []string{"wait"}}
	_ = req.Normalize()
	if _, err := d.DispatchToHarness(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(seenTools) != 1 || seenTools[0].Function.Name != "wait" {
		t.Errorf("expected only 'wait' in tools, got %+v", seenTools)
	}

	// Error path: unknown tool name surfaces before the HTTP call.
	calls.Store(0)
	seenTools = nil
	req2 := engine.DispatchRequest{Task: "bogus", Tools: []string{"not_a_tool"}}
	_ = req2.Normalize()
	_, err := d.DispatchToHarness(context.Background(), req2)
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	if !strings.Contains(err.Error(), "not_a_tool") {
		t.Errorf("error should name the missing tool, got %q", err.Error())
	}
	if calls.Load() != 0 {
		t.Errorf("expected zero HTTP calls before validation failed, got %d", calls.Load())
	}
}

// TestDispatch_TimeoutSlot verifies a slot whose context exceeds the per-slot
// budget returns Success=false Error="timeout" while sibling slots continue.
//
// The slow handler picks up cancellation via its request context (so the
// httptest.Server can drain on Close) but it sleeps long enough that the
// dispatcher's per-slot deadline fires first.
func TestDispatch_TimeoutSlot(t *testing.T) {
	var calls atomic.Int32
	stopHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		// Slot 0 sleeps until the dispatcher cancels (via r.Context()) or
		// the test signals shutdown via stopHandler, whichever comes first.
		// Slot 1 returns instantly.
		if idx == 0 {
			select {
			case <-r.Context().Done():
			case <-stopHandler:
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(makeContentResponse("fast"))
	}))
	// Order matters: signal handlers to bail BEFORE the server tries to
	// drain in-flight connections, otherwise Close() blocks waiting for
	// the still-sleeping handler.
	defer server.Close()
	defer close(stopHandler)

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{Task: "race", N: 2, TimeoutSeconds: 1}
	_ = req.Normalize()
	res, err := d.DispatchToHarness(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(res.Results))
	}

	// Find the slot that timed out and the one that succeeded by content.
	var timedOut, succeeded *engine.DispatchResult
	for i := range res.Results {
		if res.Results[i].Success {
			succeeded = &res.Results[i]
		} else if res.Results[i].Error == "timeout" {
			timedOut = &res.Results[i]
		}
	}
	if timedOut == nil {
		t.Fatalf("expected one timeout, got %+v", res.Results)
	}
	if succeeded == nil {
		t.Fatalf("expected one success, got %+v", res.Results)
	}
	if succeeded.Content != "fast" {
		t.Errorf("succeeded slot content = %q, want 'fast'", succeeded.Content)
	}
}

// TestDispatch_LMStudioReachableRoutes26B exercises the 26B path with a
// stand-in OpenAI-compatible server. Verifies the dispatcher (a) probes
// /v1/models, (b) routes /v1/chat/completions when reachable, (c) records
// ModelUsed=26b on the result.
func TestDispatch_LMStudioReachableRoutes26B(t *testing.T) {
	var calls atomic.Int32
	server := openaiServer(t, &calls, func(i int) agentChatResponse {
		return makeContentResponse("from twentysix-b")
	})
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: "http://localhost:0/unused", Model: "gemma-e4b"})
	d := &HarnessDispatcher{
		AgentID:         engine.DefaultAgentID,
		Harness:         h,
		LMStudioBaseURL: server.URL,
		LMStudioModel:   "gemma-3-27b-it",
	}

	req := engine.DispatchRequest{Task: "ask the big one", Model: engine.DispatchModel26B, N: 1}
	_ = req.Normalize()
	res, err := d.DispatchToHarness(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Results[0].Success {
		t.Fatalf("expected success, got error=%q", res.Results[0].Error)
	}
	if res.Results[0].Content != "from twentysix-b" {
		t.Errorf("unexpected content: %q", res.Results[0].Content)
	}
	if res.Results[0].ModelUsed != engine.DispatchModel26B {
		t.Errorf("expected ModelUsed=26b, got %q", res.Results[0].ModelUsed)
	}
	if len(res.Notes) != 0 {
		t.Errorf("expected no batch notes when 26B reachable, got %v", res.Notes)
	}
}

// TestDispatch_LMStudioUnreachableDegradesToE4B verifies the degrade-to-e4b
// fallback fires when the 26B endpoint isn't reachable. The result should
// carry ModelUsed=e4b and a warning note.
func TestDispatch_LMStudioUnreachableDegradesToE4B(t *testing.T) {
	var calls atomic.Int32
	ollama := ollamaServer(t, &calls, func(i int) agentChatResponse {
		return makeContentResponse("e4b fallback")
	})
	defer ollama.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: ollama.URL, Model: "gemma-e4b"})
	d := &HarnessDispatcher{
		AgentID:         engine.DefaultAgentID,
		Harness:         h,
		LMStudioBaseURL: "http://127.0.0.1:1/unreachable", // port 1 closed by convention
	}

	req := engine.DispatchRequest{Task: "26b please", Model: engine.DispatchModel26B, N: 1}
	_ = req.Normalize()
	res, err := d.DispatchToHarness(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Results[0].Success {
		t.Fatalf("expected success after degrade, got error=%q", res.Results[0].Error)
	}
	if res.Results[0].ModelUsed != engine.DispatchModelE4B {
		t.Errorf("expected ModelUsed=e4b after degrade, got %q", res.Results[0].ModelUsed)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "degraded") {
		t.Errorf("expected degrade note, got %v", res.Notes)
	}
	// Per-slot Error should mirror the batch note so callers inspecting
	// individual slots see the degradation context.
	if !strings.Contains(res.Results[0].Error, "degraded") {
		t.Errorf("expected per-slot Error to carry degrade note, got %q", res.Results[0].Error)
	}
}

// TestEndpointReachable_CallerCancelDoesNotPoisonCache verifies that a probe
// failure caused by the CALLER's context being cancelled (e.g. an HTTP client
// disconnecting mid-dispatch, per serve_agents.go's r.Context() plumbing)
// does NOT write a negative result into the shared reachByURL cache. Without
// this guard, one caller's disconnect poisons endpointReachable for every
// other in-flight dispatch against the same URL for reachabilityCacheTTL.
func TestEndpointReachable_CallerCancelDoesNotPoisonCache(t *testing.T) {
	// A blocking handler so the probe is still in-flight when we cancel.
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(block)

	d := &HarnessDispatcher{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- d.endpointReachable(ctx, server.URL)
	}()
	// Give the probe goroutine time to start the request, then cancel the
	// CALLER's context (not the probe's own timeout).
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatalf("expected endpointReachable to return false on caller-cancel, got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("endpointReachable did not return after caller cancel")
	}

	d.mu.Lock()
	_, cached := d.reachByURL[server.URL]
	d.mu.Unlock()
	if cached {
		t.Errorf("expected no cache entry after caller-context cancel, but one was written")
	}
}

// TestEndpointReachable_GenuineRefusalWritesCache verifies the companion
// case: a real probe failure (connection refused, not a caller cancel) must
// still populate reachByURL, or a genuinely-down endpoint would be re-probed
// on every dispatch instead of being cached negative for reachabilityCacheTTL.
func TestEndpointReachable_GenuineRefusalWritesCache(t *testing.T) {
	d := &HarnessDispatcher{}
	const unreachable = "http://127.0.0.1:1" // port 1 closed by convention

	ok := d.endpointReachable(context.Background(), unreachable)
	if ok {
		t.Fatalf("expected endpointReachable=false for a closed port")
	}

	d.mu.Lock()
	entry, cached := d.reachByURL[unreachable]
	d.mu.Unlock()
	if !cached {
		t.Fatalf("expected a genuine refusal to write the reachability cache")
	}
	if entry.ok {
		t.Errorf("expected cached entry to be negative, got ok=true")
	}
}

// TestDispatch_SystemPromptOverride confirms the per-call SystemPrompt
// reaches the model in the system message slot.
func TestDispatch_SystemPromptOverride(t *testing.T) {
	var seenSystem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body agentChatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			seenSystem = body.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(makeContentResponse("k"))
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{Task: "go", SystemPrompt: "you are a validator"}
	_ = req.Normalize()
	if _, err := d.DispatchToHarness(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seenSystem != "you are a validator" {
		t.Errorf("system prompt not threaded through; got %q", seenSystem)
	}
}

// TestDispatch_QueryUnavailable confirms the QueryDispatchToHarness helper
// surfaces ErrAgentUnavailable when the controller is nil and a clean
// invalid-input error when normalization fails (empty task).
func TestDispatch_QueryUnavailable(t *testing.T) {
	if _, err := engine.QueryDispatchToHarness(context.Background(), nil, engine.DispatchRequest{Task: "x"}); err == nil {
		t.Fatal("expected error when controller is nil")
	}
}

// TestDispatch_QueryEmptyTask verifies the empty-task branch of normalize.
func TestDispatch_QueryEmptyTask(t *testing.T) {
	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: "http://unused", Model: "x"})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}
	_, err := engine.QueryDispatchToHarness(context.Background(), &dispatchControllerStub{disp: d}, engine.DispatchRequest{Task: "  "})
	if err == nil {
		t.Fatal("expected error for empty task")
	}
	if !strings.Contains(err.Error(), "task is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDispatch_IdentityPropagation confirms identity claims pass through to
// the harness's tool context via DispatchIdentityFromContext.
func TestDispatch_IdentityPropagation(t *testing.T) {
	var capturedIdentity engine.DispatchIdentity
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(makeToolCallResponse("c1", "ident_probe", `{}`))
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: server.URL, Model: "gemma-test"})
	// Register a probe tool that captures the dispatch identity from ctx.
	h.RegisterTool(ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "ident_probe",
			Description: "probe",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}, func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		capturedIdentity = DispatchIdentityFromContext(ctx)
		return json.Marshal(map[string]string{"ok": "1"})
	})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{
		Task: "probe",
		Identity: engine.DispatchIdentity{
			Iss: "anthropic.claude-code",
			Sub: "session-xyz",
			Aud: "cogos.kernel",
		},
		// Bound the loop — once the probe tool returns, the model would
		// be asked for another turn; the canned server keeps returning
		// the same tool call, so cap turns to 1 by triggering wait next
		// turn. Easier path: timeout tight so we only do one turn.
		TimeoutSeconds: 1,
	}
	_ = req.Normalize()
	_, _ = d.DispatchToHarness(context.Background(), req)

	if capturedIdentity.Iss != "anthropic.claude-code" {
		t.Errorf("identity Iss not propagated, got %+v", capturedIdentity)
	}
	if capturedIdentity.Sub != "session-xyz" {
		t.Errorf("identity Sub not propagated, got %+v", capturedIdentity)
	}
}

// dispatchControllerStub is a minimal AgentController + AgentDispatcher
// composition used by the QueryDispatchToHarness tests to bypass the rest
// of the controller surface.
type dispatchControllerStub struct{ disp *HarnessDispatcher }

func (s *dispatchControllerStub) ListAgents(_ context.Context, _ bool) ([]engine.AgentSummary, error) {
	return nil, nil
}
func (s *dispatchControllerStub) GetAgent(_ context.Context, _ string, _ bool, _ int) (*engine.AgentSnapshot, error) {
	return nil, nil
}
func (s *dispatchControllerStub) TriggerAgent(_ context.Context, _ string, _ string, _ bool) (*engine.AgentTriggerResult, error) {
	return nil, nil
}
func (s *dispatchControllerStub) DispatchToHarness(ctx context.Context, req engine.DispatchRequest) (*engine.DispatchBatchResult, error) {
	return s.disp.DispatchToHarness(ctx, req)
}
