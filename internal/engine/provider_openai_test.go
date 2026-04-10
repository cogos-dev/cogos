// provider_openai_test.go — OpenAICompatProvider unit tests
//
// All tests use httptest.NewServer to mock OpenAI-compatible API responses.
// No real API calls are made.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestOpenAIProvider creates an OpenAICompatProvider pointed at the given
// test server URL.
func newTestOpenAIProvider(t *testing.T, endpoint, model string) *OpenAICompatProvider {
	t.Helper()
	return NewOpenAICompatProvider("openai-compat", ProviderConfig{
		Endpoint:  endpoint,
		Model:     model,
		MaxTokens: 4096,
		Timeout:   5,
	})
}

// openaiModelsResponseJSON returns a /v1/models response body.
func openaiModelsResponseJSON(ids ...string) openaiModelsResponse {
	var models []openaiModel
	for _, id := range ids {
		models = append(models, openaiModel{ID: id})
	}
	return openaiModelsResponse{Data: models}
}

// openaiChatResponseJSON returns a minimal non-streaming chat completion response.
func openaiChatResponseJSON(content, finishReason string) openaiChatResponse {
	return openaiChatResponse{
		ID: "chatcmpl-test",
		Choices: []openaiChoice{
			{
				Index:        0,
				Message:      openaiMessage{Role: "assistant", Content: content},
				FinishReason: finishReason,
			},
		},
		Usage: &openaiUsageResponse{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}
}

// ── buildOpenAIRequest ───────────────────────────────────────────────────────

func TestBuildOpenAIRequestSystemPrompt(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		SystemPrompt: "You are helpful.",
		Messages: []ProviderMessage{
			{Role: "user", Content: "hello"},
		},
	}
	r := buildOpenAIRequest("test-model", req, false, 4096)

	if r.Model != "test-model" {
		t.Errorf("model = %q; want test-model", r.Model)
	}
	if r.Stream {
		t.Error("Stream should be false for non-streaming request")
	}
	// System prompt + user message.
	if len(r.Messages) != 2 {
		t.Fatalf("messages len = %d; want 2", len(r.Messages))
	}
	if r.Messages[0].Role != "system" || r.Messages[0].Content != "You are helpful." {
		t.Errorf("first message = %+v; want system/helpful", r.Messages[0])
	}
	if r.Messages[1].Role != "user" || r.Messages[1].Content != "hello" {
		t.Errorf("second message = %+v; want user/hello", r.Messages[1])
	}
}

func TestBuildOpenAIRequestNoSystemPrompt(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	}
	r := buildOpenAIRequest("m", req, true, 0)
	if len(r.Messages) != 1 {
		t.Errorf("messages len = %d; want 1 (no system prepended)", len(r.Messages))
	}
	if !r.Stream {
		t.Error("Stream should be true")
	}
}

func TestBuildOpenAIRequestOptions(t *testing.T) {
	t.Parallel()
	temp := 0.7
	req := &CompletionRequest{
		Temperature: &temp,
		MaxTokens:   512,
	}
	r := buildOpenAIRequest("m", req, false, 4096)
	if r.Temperature == nil || *r.Temperature != 0.7 {
		t.Errorf("temperature = %v; want 0.7", r.Temperature)
	}
	if r.MaxTokens != 512 {
		t.Errorf("max_tokens = %v; want 512 (request override)", r.MaxTokens)
	}
}

func TestBuildOpenAIRequestContextItems(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		SystemPrompt: "Identity block.",
		Context: []ContextItem{
			{ID: "cog://mem/note", Zone: ZoneFoveal, Salience: 0.9, Content: "relevant note"},
		},
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	}
	r := buildOpenAIRequest("m", req, false, 4096)

	// System prompt + context item + user message = 3.
	if len(r.Messages) != 3 {
		t.Fatalf("messages len = %d; want 3", len(r.Messages))
	}
	if r.Messages[0].Role != "system" {
		t.Errorf("first message role = %q; want system", r.Messages[0].Role)
	}
	if !strings.Contains(r.Messages[1].Content, "relevant note") {
		t.Error("context item content not found in messages")
	}
}

func TestBuildOpenAIRequestTools(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "search"}},
		Tools: []ToolDefinition{
			{
				Name:        "web_search",
				Description: "Search the web",
				InputSchema: map[string]interface{}{"type": "object"},
			},
		},
		ToolChoice: "auto",
	}
	r := buildOpenAIRequest("m", req, false, 4096)

	if len(r.Tools) != 1 {
		t.Fatalf("tools len = %d; want 1", len(r.Tools))
	}
	if r.Tools[0].Function.Name != "web_search" {
		t.Errorf("tool name = %q; want web_search", r.Tools[0].Function.Name)
	}
	if r.Tools[0].Type != "function" {
		t.Errorf("tool type = %q; want function", r.Tools[0].Type)
	}
	if r.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v; want auto", r.ToolChoice)
	}
}

// ── parseOpenAIResponse ──────────────────────────────────────────────────────

func TestParseOpenAIResponseBasic(t *testing.T) {
	t.Parallel()
	or := &openaiChatResponse{
		Choices: []openaiChoice{
			{
				Message:      openaiMessage{Role: "assistant", Content: "Hello!"},
				FinishReason: "stop",
			},
		},
		Usage: &openaiUsageResponse{PromptTokens: 10, CompletionTokens: 3},
	}
	resp := parseOpenAIResponse(or, "model", "test", 0)
	if resp.Content != "Hello!" {
		t.Errorf("Content = %q; want Hello!", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v; want {10, 3}", resp.Usage)
	}
}

func TestParseOpenAIResponseToolCalls(t *testing.T) {
	t.Parallel()
	or := &openaiChatResponse{
		Choices: []openaiChoice{
			{
				Message: openaiMessage{
					Role: "assistant",
					ToolCalls: []openaiToolCall{
						{
							ID:   "call_abc",
							Type: "function",
							Function: openaiToolCallDetail{
								Name:      "search",
								Arguments: `{"query":"test"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
		Usage: &openaiUsageResponse{PromptTokens: 15, CompletionTokens: 8},
	}
	resp := parseOpenAIResponse(or, "model", "test", 0)
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d; want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("ToolCall.ID = %q; want call_abc", tc.ID)
	}
	if tc.Name != "search" {
		t.Errorf("ToolCall.Name = %q; want search", tc.Name)
	}
	if tc.Arguments != `{"query":"test"}` {
		t.Errorf("ToolCall.Arguments = %q; want json", tc.Arguments)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q; want tool_use", resp.StopReason)
	}
}

func TestParseOpenAIResponseNoChoices(t *testing.T) {
	t.Parallel()
	or := &openaiChatResponse{Choices: nil}
	resp := parseOpenAIResponse(or, "model", "test", 0)
	if resp.Content != "" {
		t.Errorf("Content = %q; want empty", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn (default)", resp.StopReason)
	}
}

func TestParseOpenAIResponseNoUsage(t *testing.T) {
	t.Parallel()
	or := &openaiChatResponse{
		Choices: []openaiChoice{
			{Message: openaiMessage{Content: "hi"}, FinishReason: "stop"},
		},
		Usage: nil,
	}
	resp := parseOpenAIResponse(or, "model", "test", 0)
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
		t.Errorf("Usage should be zero when not provided, got %+v", resp.Usage)
	}
}

// ── mapOpenAIFinishReason ────────────────────────────────────────────────────

func TestMapOpenAIFinishReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"", ""},
		{"unknown", "unknown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := mapOpenAIFinishReason(tc.input)
			if got != tc.want {
				t.Errorf("mapOpenAIFinishReason(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ── Available ────────────────────────────────────────────────────────────────

func TestOpenAIAvailableModelPresent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("gemma-2-9b", "llama-3.1-8b"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "gemma-2-9b")
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true when model is present")
	}
}

func TestOpenAIAvailableModelAbsent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("llama-3.1-8b"))
	}))
	defer srv.Close()

	// Server has models, just not the one we want — but Available still returns
	// true because the server is reachable and has models.
	p := newTestOpenAIProvider(t, srv.URL, "nonexistent-model")
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true (server has models even if exact match missing)")
	}
}

func TestOpenAIAvailableNoModels(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON())
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "any")
	if p.Available(context.Background()) {
		t.Error("Available() = true; want false when server has no models")
	}
}

func TestOpenAIAvailableServerDown(t *testing.T) {
	t.Parallel()
	p := NewOpenAICompatProvider("openai-compat", ProviderConfig{
		Endpoint: "http://localhost:1", // nothing listening
		Model:    "any",
		Timeout:  1,
	})
	if p.Available(context.Background()) {
		t.Error("Available() = true; want false when server is down")
	}
}

// ── Capabilities ─────────────────────────────────────────────────────────────

func TestOpenAICapabilities(t *testing.T) {
	t.Parallel()
	p := NewOpenAICompatProvider("openai-compat", ProviderConfig{Model: "test-model", MaxTokens: 8192})
	caps := p.Capabilities()
	if !caps.IsLocal {
		t.Error("IsLocal should be true for OpenAI-compat (local server)")
	}
	if !caps.HasCapability(CapStreaming) {
		t.Error("should support streaming")
	}
	if !caps.HasCapability(CapJSON) {
		t.Error("should support JSON output")
	}
	if caps.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d; want 8192", caps.MaxOutputTokens)
	}
	if caps.CostPerInputToken != 0 || caps.CostPerOutputToken != 0 {
		t.Error("cost should be 0 for local provider")
	}
}

func TestOpenAICapabilitiesDefaultMaxTokens(t *testing.T) {
	t.Parallel()
	p := NewOpenAICompatProvider("openai-compat", ProviderConfig{Model: "m"})
	caps := p.Capabilities()
	if caps.MaxOutputTokens != openaiCompatDefaultMaxToks {
		t.Errorf("MaxOutputTokens = %d; want %d (default)", caps.MaxOutputTokens, openaiCompatDefaultMaxToks)
	}
}

// ── Ping ─────────────────────────────────────────────────────────────────────

func TestOpenAIPing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("m"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if lat <= 0 {
		t.Errorf("latency = %v; want > 0", lat)
	}
}

func TestOpenAIPingWithAuth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("m"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	p.apiKey = "test-key"
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if lat <= 0 {
		t.Errorf("latency = %v; want > 0", lat)
	}
}

// ── Complete ─────────────────────────────────────────────────────────────────

func TestOpenAIComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		// Verify the request was properly formed.
		var payload openaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.Stream {
			http.Error(w, "stream should be false", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(openaiChatResponseJSON("Hello!", "stop"))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "test-model")
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("Content = %q; want Hello!", resp.Content)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v; want {10, 5}", resp.Usage)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn", resp.StopReason)
	}
	if resp.ProviderMeta.Provider != "openai-compat" {
		t.Errorf("ProviderMeta.Provider = %q; want openai-compat", resp.ProviderMeta.Provider)
	}
}

func TestOpenAICompleteHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	_, err := p.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Error("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

func TestOpenAICompleteToolUseResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(openaiChatResponse{
			ID: "chatcmpl-test",
			Choices: []openaiChoice{
				{
					Message: openaiMessage{
						Role: "assistant",
						ToolCalls: []openaiToolCall{
							{
								ID:   "call_xyz",
								Type: "function",
								Function: openaiToolCallDetail{
									Name:      "web_search",
									Arguments: `{"query":"golang testing"}`,
								},
							},
						},
					},
					FinishReason: "tool_calls",
				},
			},
			Usage: &openaiUsageResponse{PromptTokens: 20, CompletionTokens: 15},
		})
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "search for me"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d; want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_xyz" {
		t.Errorf("ToolCall.ID = %q; want call_xyz", tc.ID)
	}
	if tc.Name != "web_search" {
		t.Errorf("ToolCall.Name = %q; want web_search", tc.Name)
	}
	if tc.Arguments != `{"query":"golang testing"}` {
		t.Errorf("ToolCall.Arguments = %q; want json", tc.Arguments)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q; want tool_use", resp.StopReason)
	}
}

// ── Stream ───────────────────────────────────────────────────────────────────

// sseOpenAILines formats SSE data lines in the OpenAI SSE format.
func sseOpenAILines(chunks []openaiStreamChunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		fmt.Fprintf(&sb, "data: %s\n\n", b)
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func TestOpenAIStream(t *testing.T) {
	t.Parallel()

	stop := "stop"
	chunks := []openaiStreamChunk{
		{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Role: "assistant"}}},
		},
		{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Content: "Hello"}}},
		},
		{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Content: " world"}}},
		},
		{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{}, FinishReason: &stop}},
			Usage:   &openaiUsageResponse{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseOpenAILines(chunks))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "test-model")
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var content strings.Builder
	var lastChunk StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		if sc.Delta != "" {
			content.WriteString(sc.Delta)
		}
		lastChunk = sc
	}

	if content.String() != "Hello world" {
		t.Errorf("streamed content = %q; want 'Hello world'", content.String())
	}
	if !lastChunk.Done {
		t.Error("last chunk should have Done=true")
	}
	if lastChunk.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn", lastChunk.StopReason)
	}
	if lastChunk.Usage == nil {
		t.Fatal("last chunk usage should not be nil")
	}
	if lastChunk.Usage.InputTokens != 7 {
		t.Errorf("InputTokens = %d; want 7", lastChunk.Usage.InputTokens)
	}
	if lastChunk.Usage.OutputTokens != 4 {
		t.Errorf("OutputTokens = %d; want 4", lastChunk.Usage.OutputTokens)
	}
	if lastChunk.ProviderMeta == nil || lastChunk.ProviderMeta.Provider != "openai-compat" {
		t.Errorf("ProviderMeta.Provider = %v; want openai-compat", lastChunk.ProviderMeta)
	}
}

func TestOpenAIStreamToolCall(t *testing.T) {
	t.Parallel()

	stop := "tool_calls"
	chunks := []openaiStreamChunk{
		{
			ID: "chatcmpl-1",
			Choices: []openaiStreamChoice{{
				Index: 0,
				Delta: openaiStreamDelta{
					Role: "assistant",
					ToolCalls: []openaiStreamToolCall{{
						Index: 0,
						ID:    "call_abc",
						Type:  "function",
						Function: openaiToolCallDetail{
							Name:      "search",
							Arguments: "",
						},
					}},
				},
			}},
		},
		{
			ID: "chatcmpl-1",
			Choices: []openaiStreamChoice{{
				Index: 0,
				Delta: openaiStreamDelta{
					ToolCalls: []openaiStreamToolCall{{
						Index:    0,
						Function: openaiToolCallDetail{Arguments: `{"q`},
					}},
				},
			}},
		},
		{
			ID: "chatcmpl-1",
			Choices: []openaiStreamChoice{{
				Index: 0,
				Delta: openaiStreamDelta{
					ToolCalls: []openaiStreamToolCall{{
						Index:    0,
						Function: openaiToolCallDetail{Arguments: `uery":"test"}`},
					}},
				},
			}},
		},
		{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{}, FinishReason: &stop}},
			Usage:   &openaiUsageResponse{PromptTokens: 12, CompletionTokens: 8},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseOpenAILines(chunks))
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "search"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var allChunks []StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		allChunks = append(allChunks, sc)
	}

	// Find tool start, arg deltas, and done chunks.
	var toolStart, argDelta1, argDelta2, done StreamChunk
	for _, sc := range allChunks {
		if sc.ToolCallDelta != nil && sc.ToolCallDelta.ID == "call_abc" {
			toolStart = sc
		}
		if sc.ToolCallDelta != nil && sc.ToolCallDelta.ArgsDelta == `{"q` {
			argDelta1 = sc
		}
		if sc.ToolCallDelta != nil && sc.ToolCallDelta.ArgsDelta == `uery":"test"}` {
			argDelta2 = sc
		}
		if sc.Done {
			done = sc
		}
	}

	if toolStart.ToolCallDelta == nil {
		t.Error("expected a ToolCallDelta chunk with tool ID and name")
	} else {
		if toolStart.ToolCallDelta.Name != "search" {
			t.Errorf("tool name = %q; want search", toolStart.ToolCallDelta.Name)
		}
		if toolStart.ToolCallDelta.Index != 0 {
			t.Errorf("tool index = %d; want 0", toolStart.ToolCallDelta.Index)
		}
	}
	if argDelta1.ToolCallDelta == nil {
		t.Error("expected first arg delta chunk")
	}
	if argDelta2.ToolCallDelta == nil {
		t.Error("expected second arg delta chunk")
	}
	if !done.Done {
		t.Error("expected a Done chunk at end of stream")
	}
	if done.StopReason != "tool_use" {
		t.Errorf("StopReason = %q; want tool_use", done.StopReason)
	}
	if done.Usage == nil || done.Usage.OutputTokens != 8 {
		t.Errorf("done chunk usage = %+v; want OutputTokens=8", done.Usage)
	}
}

func TestOpenAIStreamMalformedSSE(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {not valid json}\n\n")
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	ch, err := p.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var gotError bool
	for sc := range ch {
		if sc.Error != nil {
			gotError = true
		}
	}
	if !gotError {
		t.Error("expected a StreamChunk with Error for malformed JSON SSE data")
	}
}

func TestOpenAIStreamContextCancelled(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Send one chunk then block.
		chunk := openaiStreamChunk{
			ID:      "chatcmpl-1",
			Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Content: "hello"}}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Read first chunk then cancel.
	<-ch
	cancel()

	// Channel should close cleanly.
	for range ch {
	}
}

func TestOpenAIStreamHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "m")
	_, err := p.Stream(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Error("expected error for 503 response")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

// ── effectiveModel ───────────────────────────────────────────────────────────

func TestOpenAIEffectiveModel(t *testing.T) {
	t.Parallel()
	p := NewOpenAICompatProvider("openai-compat", ProviderConfig{Model: "gemma-2-9b"})

	// No override: use configured model.
	if got := p.effectiveModel(&CompletionRequest{}); got != "gemma-2-9b" {
		t.Errorf("effectiveModel = %q; want gemma-2-9b", got)
	}
	// With override.
	req := &CompletionRequest{ModelOverride: "llama-3.1-70b"}
	if got := p.effectiveModel(req); got != "llama-3.1-70b" {
		t.Errorf("effectiveModel = %q; want llama-3.1-70b", got)
	}
}

// ── Name ─────────────────────────────────────────────────────────────────────

func TestOpenAIName(t *testing.T) {
	t.Parallel()
	p := NewOpenAICompatProvider("my-lmstudio", ProviderConfig{Model: "m"})
	if p.Name() != "my-lmstudio" {
		t.Errorf("Name() = %q; want my-lmstudio", p.Name())
	}
}
