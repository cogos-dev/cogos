package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// --- Test: Tool dispatch table ---

func TestAgentHarness_ToolDispatch(t *testing.T) {
	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: "http://unused",
		Model:     "test",
	})

	var called atomic.Bool
	var gotArgs json.RawMessage

	h.RegisterTool(ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "test_tool",
			Description: "A test tool",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		},
	}, func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
		called.Store(true)
		gotArgs = args
		return json.Marshal(map[string]string{"result": "ok"})
	})

	// Verify tool is registered.
	if len(h.tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(h.tools))
	}
	if h.tools[0].Function.Name != "test_tool" {
		t.Fatalf("expected tool name 'test_tool', got %q", h.tools[0].Function.Name)
	}

	// Dispatch it.
	tc := agentToolCall{
		ID:   "call_1",
		Type: "function",
	}
	tc.Function.Name = "test_tool"
	tc.Function.Arguments = json.RawMessage(`{"x":"hello"}`)

	result, err := h.dispatchTool(context.Background(), tc)
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !called.Load() {
		t.Fatal("tool function was not called")
	}

	// Verify args were passed through.
	var parsedArgs struct {
		X string `json:"x"`
	}
	if err := json.Unmarshal(gotArgs, &parsedArgs); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if parsedArgs.X != "hello" {
		t.Fatalf("expected arg x='hello', got %q", parsedArgs.X)
	}

	// Verify result.
	var parsedResult map[string]string
	if err := json.Unmarshal(result, &parsedResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsedResult["result"] != "ok" {
		t.Fatalf("expected result 'ok', got %q", parsedResult["result"])
	}
}

func TestAgentHarness_ToolDispatch_UnknownTool(t *testing.T) {
	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: "http://unused",
		Model:     "test",
	})

	tc := agentToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "nonexistent"
	tc.Function.Arguments = json.RawMessage(`{}`)

	_, err := h.dispatchTool(context.Background(), tc)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

// --- Test: Message building (observations → chat messages) ---

func TestAgentHarness_Assess_MessageBuilding(t *testing.T) {
	var receivedReq agentChatRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		resp := agentChatResponse{
			Model: "test-model",
			Done:  true,
			DoneReason: "stop",
		}
		resp.Message.Role = "assistant"
		resp.Message.Content = `{"action":"sleep","reason":"all quiet","urgency":0.1,"target":""}`
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test-model",
	})

	_, err := h.Assess(context.Background(), "You are a kernel guardian.", "Workspace is quiet.")
	if err != nil {
		t.Fatalf("assess error: %v", err)
	}

	// Verify the request was built correctly.
	if receivedReq.Model != "test-model" {
		t.Errorf("expected model 'test-model', got %q", receivedReq.Model)
	}
	if len(receivedReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(receivedReq.Messages))
	}
	if receivedReq.Messages[0].Role != "system" {
		t.Errorf("expected system role, got %q", receivedReq.Messages[0].Role)
	}
	if receivedReq.Messages[0].Content != "You are a kernel guardian." {
		t.Errorf("unexpected system content: %q", receivedReq.Messages[0].Content)
	}
	if receivedReq.Messages[1].Role != "user" {
		t.Errorf("expected user role, got %q", receivedReq.Messages[1].Role)
	}
	if receivedReq.Messages[1].Content != "Workspace is quiet." {
		t.Errorf("unexpected user content: %q", receivedReq.Messages[1].Content)
	}
	if receivedReq.Format != "json" {
		t.Error("expected format 'json' for assess")
	}
}

// --- Test: Assessment parsing ---

func TestAgentHarness_Assess_Parsing(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected Assessment
	}{
		{
			name: "sleep",
			json: `{"action":"sleep","reason":"nothing to do","urgency":0.0,"target":""}`,
			expected: Assessment{
				Action:  "sleep",
				Reason:  "nothing to do",
				Urgency: 0.0,
				Target:  "",
			},
		},
		{
			name: "consolidate",
			json: `{"action":"consolidate","reason":"journal needs rollover","urgency":0.7,"target":"cog://mem/episodic/journal/2026-04-13.cog.md"}`,
			expected: Assessment{
				Action:  "consolidate",
				Reason:  "journal needs rollover",
				Urgency: 0.7,
				Target:  "cog://mem/episodic/journal/2026-04-13.cog.md",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newCannedServer(t, tc.json, nil)
			defer server.Close()

			h := NewAgentHarness(AgentHarnessConfig{
				OllamaURL: server.URL,
				Model:     "test",
			})

			got, err := h.Assess(context.Background(), "system", "observe")
			if err != nil {
				t.Fatalf("assess: %v", err)
			}
			if got.Action != tc.expected.Action {
				t.Errorf("action: got %q, want %q", got.Action, tc.expected.Action)
			}
			if got.Reason != tc.expected.Reason {
				t.Errorf("reason: got %q, want %q", got.Reason, tc.expected.Reason)
			}
			if got.Urgency != tc.expected.Urgency {
				t.Errorf("urgency: got %f, want %f", got.Urgency, tc.expected.Urgency)
			}
			if got.Target != tc.expected.Target {
				t.Errorf("target: got %q, want %q", got.Target, tc.expected.Target)
			}
		})
	}
}

// --- Test: Tool loop termination (content without tool_calls → done) ---

func TestAgentHarness_Execute_NoTools(t *testing.T) {
	server := newCannedServer(t, "Task complete. Memory consolidated.", nil)
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test",
	})

	result, err := h.Execute(context.Background(), "system", "consolidate journals")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result != "Task complete. Memory consolidated." {
		t.Errorf("unexpected result: %q", result)
	}
}

// --- Test: Tool loop with tool calls ---

func TestAgentHarness_Execute_WithToolCalls(t *testing.T) {
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")

		if callCount == 1 {
			// First call: model requests a tool call.
			resp := makeToolCallResponse("call_1", "test_tool", `{"query":"test"}`)
			json.NewEncoder(w).Encode(resp)
		} else {
			// Second call: model returns final content.
			resp := makeContentResponse("Done. Found 3 results.")
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test",
	})

	var toolCalled bool
	h.RegisterTool(ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "test_tool",
			Description: "test",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}, func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		toolCalled = true
		return json.Marshal(map[string]int{"count": 3})
	})

	result, err := h.Execute(context.Background(), "system", "search for things")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !toolCalled {
		t.Error("tool was not called")
	}
	if result != "Done. Found 3 results." {
		t.Errorf("unexpected result: %q", result)
	}
}

// --- Test: MaxTurns safety limit ---

func TestAgentHarness_Execute_MaxTurns(t *testing.T) {
	// Server always returns tool calls — never finishes.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := makeToolCallResponse("call_inf", "loop_tool", `{}`)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test",
		MaxTurns:  3,
	})

	h.RegisterTool(ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "loop_tool",
			Description: "always loops",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}, func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"status": "still going"})
	})

	_, err := h.Execute(context.Background(), "system", "do something")
	if err == nil {
		t.Fatal("expected max turns error")
	}
	if expected := fmt.Sprintf("execute: hit max turns (%d) without completion", 3); err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
}

// --- Test: Context cancellation ---

func TestAgentHarness_Execute_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until context is done.
		<-r.Context().Done()
	}))
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := h.Execute(ctx, "system", "task")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// --- Test: RunCycle with sleep assessment ---

func TestAgentHarness_RunCycle_Sleep(t *testing.T) {
	server := newCannedServer(t, `{"action":"sleep","reason":"quiet","urgency":0.0,"target":""}`, nil)
	defer server.Close()

	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: server.URL,
		Model:     "test",
	})

	assessment, result, err := h.RunCycle(context.Background(), "system", "nothing happening")
	if err != nil {
		t.Fatalf("run cycle: %v", err)
	}
	if assessment.Action != "sleep" {
		t.Errorf("expected sleep, got %q", assessment.Action)
	}
	if result != "" {
		t.Errorf("expected empty result for sleep, got %q", result)
	}
}

// --- Test: Default MaxTurns ---

func TestAgentHarness_DefaultMaxTurns(t *testing.T) {
	h := NewAgentHarness(AgentHarnessConfig{
		OllamaURL: "http://unused",
		Model:     "test",
	})
	if h.maxTurns != 10 {
		t.Errorf("expected default maxTurns=10, got %d", h.maxTurns)
	}
}

// --- Helpers ---

// newCannedServer returns an httptest.Server that always responds with the given content.
// If toolCalls is non-nil, they are included in the response.
func newCannedServer(t *testing.T, content string, toolCalls []agentToolCall) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := agentChatResponse{
			Model: "test",
			Done:  true,
			DoneReason: "stop",
		}
		resp.Message.Role = "assistant"
		resp.Message.Content = content
		resp.Message.ToolCalls = toolCalls
		json.NewEncoder(w).Encode(resp)
	}))
}

// makeToolCallResponse builds a response with a single tool call.
func makeToolCallResponse(id, name, args string) agentChatResponse {
	tc := agentToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = json.RawMessage(args)
	resp := agentChatResponse{
		Model: "test",
		Done:  true,
		DoneReason: "tool_calls",
	}
	resp.Message.Role = "assistant"
	resp.Message.ToolCalls = []agentToolCall{tc}
	return resp
}

// makeContentResponse builds a response with content and no tool calls.
func makeContentResponse(content string) agentChatResponse {
	resp := agentChatResponse{
		Model: "test",
		Done:  true,
		DoneReason: "stop",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = content
	return resp
}
