// provider_ollama_test.go — OllamaProvider unit tests
//
// Unit tests use httptest.NewServer to mock Ollama responses.
// Integration tests (//go:build integration) hit a real Ollama at localhost:11434.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ── buildOllamaRequest ────────────────────────────────────────────────────────

func TestBuildOllamaRequestSystemPrompt(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		SystemPrompt: "You are helpful.",
		Messages: []ProviderMessage{
			{Role: "user", Content: "hello"},
		},
	}
	r := buildOllamaRequest("qwen2.5:9b", req, false, 0)

	if r.Model != "qwen2.5:9b" {
		t.Errorf("model = %q; want qwen2.5:9b", r.Model)
	}
	if r.Think {
		t.Error("Think should be false to suppress thinking mode")
	}
	if r.Stream {
		t.Error("Stream should be false for non-streaming request")
	}
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

func TestBuildOllamaRequestNoSystemPrompt(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "hi"},
		},
	}
	r := buildOllamaRequest("model", req, true, 0)
	if len(r.Messages) != 1 {
		t.Errorf("messages len = %d; want 1 (no system prepended)", len(r.Messages))
	}
	if !r.Stream {
		t.Error("Stream should be true")
	}
}

func TestBuildOllamaRequestOptions(t *testing.T) {
	t.Parallel()
	temp := 0.7
	req := &CompletionRequest{
		Temperature: &temp,
		MaxTokens:   512,
	}
	r := buildOllamaRequest("m", req, false, 0)
	if r.Options["temperature"] != 0.7 {
		t.Errorf("temperature = %v; want 0.7", r.Options["temperature"])
	}
	if r.Options["num_predict"] != 512 {
		t.Errorf("num_predict = %v; want 512", r.Options["num_predict"])
	}
}

func TestBuildOllamaRequestNumCtx(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hello"}},
	}

	// With context window set, num_ctx should appear in options.
	r := buildOllamaRequest("m", req, false, 32768)
	if r.Options["num_ctx"] != 32768 {
		t.Errorf("num_ctx = %v; want 32768", r.Options["num_ctx"])
	}

	// With context window = 0, num_ctx should be absent.
	r2 := buildOllamaRequest("m", req, false, 0)
	if _, ok := r2.Options["num_ctx"]; ok {
		t.Errorf("num_ctx should be absent when contextWindow=0, got %v", r2.Options["num_ctx"])
	}
}

func TestBuildOllamaRequestKeepAlivePinsModel(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	}
	r := buildOllamaRequest("m", req, false, 0)
	// Compare via JSON shape so the test is robust to any/int interface boxing.
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"keep_alive":-1`)) {
		t.Errorf("request body should pin model with keep_alive=-1; got %s", body)
	}
}

func TestBuildOllamaRequestKeepAliveOverride(t *testing.T) {
	t.Parallel()

	// Zero override: forces model eviction after the request (cold-start simulation).
	zeroVal := any(0)
	reqZero := &CompletionRequest{
		Messages:  []ProviderMessage{{Role: "user", Content: "hi"}},
		KeepAlive: &zeroVal,
	}
	rZero := buildOllamaRequest("m", reqZero, false, 0)
	bodyZero, err := json.Marshal(rZero)
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if !bytes.Contains(bodyZero, []byte(`"keep_alive":0`)) {
		t.Errorf("zero override should produce keep_alive=0; got %s", bodyZero)
	}

	// Duration string override: "5m" passes through to Ollama as-is.
	durVal := any("5m")
	reqDur := &CompletionRequest{
		Messages:  []ProviderMessage{{Role: "user", Content: "hi"}},
		KeepAlive: &durVal,
	}
	rDur := buildOllamaRequest("m", reqDur, false, 0)
	bodyDur, err := json.Marshal(rDur)
	if err != nil {
		t.Fatalf("marshal dur: %v", err)
	}
	if !bytes.Contains(bodyDur, []byte(`"keep_alive":"5m"`)) {
		t.Errorf("duration string override should produce keep_alive=\"5m\"; got %s", bodyDur)
	}

	// Nil override: falls back to provider default (-1).
	reqNil := &CompletionRequest{
		Messages:  []ProviderMessage{{Role: "user", Content: "hi"}},
		KeepAlive: nil,
	}
	rNil := buildOllamaRequest("m", reqNil, false, 0)
	bodyNil, err := json.Marshal(rNil)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if !bytes.Contains(bodyNil, []byte(`"keep_alive":-1`)) {
		t.Errorf("nil override should fall back to keep_alive=-1; got %s", bodyNil)
	}
}

func TestOllamaCapabilitiesContextWindow(t *testing.T) {
	t.Parallel()
	p := NewOllamaProvider("ollama", ProviderConfig{Model: "m", ContextWindow: 32768})
	caps := p.Capabilities()
	if caps.MaxContextTokens != 32768 {
		t.Errorf("MaxContextTokens = %d; want 32768", caps.MaxContextTokens)
	}

	// Default when no context window configured.
	p2 := NewOllamaProvider("ollama", ProviderConfig{Model: "m"})
	caps2 := p2.Capabilities()
	if caps2.MaxContextTokens != 4096 {
		t.Errorf("MaxContextTokens = %d; want 4096 (default)", caps2.MaxContextTokens)
	}
}

// ── Available ─────────────────────────────────────────────────────────────────

// TestOllamaAvailableCachesWithinTTL guards #441 for the Ollama provider:
// repeated Available() calls within availCacheTTL hit the upstream /api/tags
// exactly once, not on every call.
func TestOllamaAvailableCachesWithinTTL(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "qwen2.5:9b"}},
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	for i := 0; i < 6; i++ {
		if !p.Available(context.Background()) {
			t.Fatalf("Available() = false on call %d; want true", i)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream /api/tags hit %d times; want 1 (calls within TTL should be cached)", got)
	}
}

func TestOllamaAvailableModelPresent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "qwen2.5:9b"},
				{"name": "llama3:8b"},
			},
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: srv.URL,
		Model:    "qwen2.5:9b",
	})
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true when model is present")
	}
}

func TestOllamaAvailableModelAbsent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "llama3:8b"}},
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: srv.URL,
		Model:    "qwen2.5:9b",
	})
	if p.Available(context.Background()) {
		t.Error("Available() = true; want false when model is absent")
	}
}

func TestOllamaAvailableServerDown(t *testing.T) {
	t.Parallel()
	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: "http://localhost:1", // nothing listening
		Model:    "any",
		Timeout:  1,
	})
	if p.Available(context.Background()) {
		t.Error("Available() = true; want false when server is down")
	}
}

// ── listModels ────────────────────────────────────────────────────────────────

// TestOllamaListModels verifies that listModels() correctly returns model names
// from /api/tags and filters out any entry with an empty name.
func TestOllamaListModels(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3:8b"},
				{"name": ""}, // should be filtered out
				{"name": "gemma4:e4b"},
			},
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "llama3:8b"})
	names, err := p.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("len(names) = %d; want 2 (empty entry filtered)", len(names))
	}
	if names[0] != "llama3:8b" {
		t.Errorf("names[0] = %q; want llama3:8b", names[0])
	}
	if names[1] != "gemma4:e4b" {
		t.Errorf("names[1] = %q; want gemma4:e4b", names[1])
	}
}

// ── Ping ──────────────────────────────────────────────────────────────────────

func TestOllamaPing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.5.0"})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "m"})
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if lat <= 0 {
		t.Errorf("latency = %v; want > 0", lat)
	}
}

// ── Complete ──────────────────────────────────────────────────────────────────

func TestOllamaComplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model:           "qwen2.5:9b",
			Message:         ollamaMessage{Role: "assistant", Content: "Hello!"},
			Done:            true,
			PromptEvalCount: 5,
			EvalCount:       3,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	req := &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("Content = %q; want Hello!", resp.Content)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v; want {5, 3}", resp.Usage)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn", resp.StopReason)
	}
}

func TestOllamaCompleteError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "m"})
	_, err := p.Complete(context.Background(), &CompletionRequest{})
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

// ── Stream ────────────────────────────────────────────────────────────────────

func TestOllamaStream(t *testing.T) {
	t.Parallel()

	// Ollama streams newline-delimited JSON chunks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		chunks := []ollamaChatResponse{
			{Message: ollamaMessage{Role: "assistant", Content: "Hi"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: " there"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: "!"}, Done: true,
				PromptEvalCount: 3, EvalCount: 3},
		}
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", b)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
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
		content.WriteString(sc.Delta)
		lastChunk = sc
	}
	if content.String() != "Hi there!" {
		t.Errorf("streamed content = %q; want Hi there!", content.String())
	}
	if !lastChunk.Done {
		t.Error("last chunk should have Done=true")
	}
	if lastChunk.Usage == nil || lastChunk.Usage.InputTokens != 3 {
		t.Errorf("last chunk usage = %+v; want InputTokens=3", lastChunk.Usage)
	}
}

// TestOllamaStreamCapturesEvalCount asserts the streamed response_tokens
// value lands in the final chunk's Usage. Regression: issue #93 — the
// snapshot reported 0 despite hundreds of tokens of real output.
func TestOllamaStreamCapturesEvalCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		chunks := []ollamaChatResponse{
			{Message: ollamaMessage{Role: "assistant", Content: "alpha "}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: "beta "}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: "gamma"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: ""}, Done: true,
				PromptEvalCount: 11, EvalCount: 42},
		}
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", b)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "gemma4:e4b"})
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var lastChunk StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		lastChunk = sc
	}
	if !lastChunk.Done {
		t.Fatal("last chunk should have Done=true")
	}
	if lastChunk.Usage == nil {
		t.Fatal("last chunk usage = nil; want populated")
	}
	if lastChunk.Usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d; want 42 (eval_count from final chunk)",
			lastChunk.Usage.OutputTokens)
	}
	if lastChunk.Usage.InputTokens != 11 {
		t.Errorf("InputTokens = %d; want 11", lastChunk.Usage.InputTokens)
	}
}

// TestOllamaStreamFallbackChunkCount asserts that when the Ollama stream
// completion record omits eval_count, we fall back to counting content
// chunks instead of reporting zero.
func TestOllamaStreamFallbackChunkCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		// Three content chunks then a final done chunk with no eval_count.
		chunks := []ollamaChatResponse{
			{Message: ollamaMessage{Role: "assistant", Content: "a"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: "b"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: "c"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: ""}, Done: true},
		}
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", b)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "m"})
	ch, err := p.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var lastChunk StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		lastChunk = sc
	}
	if lastChunk.Usage == nil {
		t.Fatal("usage should be populated even with no eval_count")
	}
	if lastChunk.Usage.OutputTokens != 3 {
		t.Errorf("fallback OutputTokens = %d; want 3 (content chunk count)",
			lastChunk.Usage.OutputTokens)
	}
}

// TestOllamaCompleteCapturesEvalCount asserts that the non-streaming path
// also surfaces eval_count as the response token count. Regression: issue #93.
func TestOllamaCompleteCapturesEvalCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model:           "gemma4:e4b",
			Message:         ollamaMessage{Role: "assistant", Content: "hello world"},
			Done:            true,
			PromptEvalCount: 4,
			EvalCount:       17,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "gemma4:e4b"})
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.OutputTokens != 17 {
		t.Errorf("OutputTokens = %d; want 17 (eval_count)", resp.Usage.OutputTokens)
	}
	if resp.Usage.InputTokens != 4 {
		t.Errorf("InputTokens = %d; want 4 (prompt_eval_count)", resp.Usage.InputTokens)
	}
}

func TestOllamaStreamContextCancelled(t *testing.T) {
	t.Parallel()
	// Server that streams slowly — we cancel before it finishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		// Send one chunk, then block until client disconnects.
		b, _ := json.Marshal(ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: "hello"},
		})
		fmt.Fprintf(w, "%s\n", b)
		flusher.Flush()
		// Block until context is done.
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Read first chunk then cancel.
	<-ch
	cancel()

	// Drain the channel — it should close cleanly.
	for range ch {
	}
}

// ── Capabilities ─────────────────────────────────────────────────────────────

func TestOllamaCapabilities(t *testing.T) {
	t.Parallel()
	p := NewOllamaProvider("ollama", ProviderConfig{Model: "qwen2.5:9b"})
	caps := p.Capabilities()
	if !caps.IsLocal {
		t.Error("IsLocal should be true for Ollama")
	}
	if !caps.HasCapability(CapStreaming) {
		t.Error("should support streaming")
	}
}

func TestLookupOllamaModelProfileGemma4E4B(t *testing.T) {
	t.Parallel()

	profile := lookupOllamaModelProfile("gemma4:e4b")

	if !containsCapability(profile.Capabilities, CapStreaming) {
		t.Error("gemma4:e4b should support streaming")
	}
	if !containsCapability(profile.Capabilities, CapJSON) {
		t.Error("gemma4:e4b should support json output")
	}
	if !containsCapability(profile.Capabilities, CapToolCallValidation) {
		t.Error("gemma4:e4b should require tool call validation")
	}
	if containsCapability(profile.Capabilities, CapToolUse) {
		t.Error("gemma4:e4b should not advertise reliable tool use")
	}
	if profile.MaxContextTokens != 128000 {
		t.Errorf("MaxContextTokens = %d; want 128000", profile.MaxContextTokens)
	}
}

func TestOllamaCapabilitiesUseKnownModelProfile(t *testing.T) {
	t.Parallel()

	p := NewOllamaProvider("ollama", ProviderConfig{Model: "gemma4:e4b"})
	caps := p.Capabilities()

	if !caps.HasCapability(CapToolCallValidation) {
		t.Error("gemma4:e4b should advertise tool_call_validation")
	}
	if caps.HasCapability(CapToolUse) {
		t.Error("gemma4:e4b should not advertise tool_use")
	}
	if caps.MaxContextTokens != 128000 {
		t.Errorf("MaxContextTokens = %d; want 128000", caps.MaxContextTokens)
	}
}

// ── decodeOllamaToolArguments ─────────────────────────────────────────────────

func TestDecodeOllamaToolArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantMap map[string]interface{} // non-nil means we expect a map with these keys/values
		wantStr string                 // non-empty means we expect a raw string back
	}{
		{
			name:    "object input",
			input:   `{"query":"hello","limit":5}`,
			wantMap: map[string]interface{}{"query": "hello", "limit": json.Number("5")},
		},
		{
			name:    "string input wrapping JSON object",
			input:   `"{\"query\":\"hello\",\"limit\":5}"`,
			wantMap: map[string]interface{}{"query": "hello", "limit": json.Number("5")},
		},
		{
			name:    "empty string",
			input:   "",
			wantMap: map[string]interface{}{},
		},
		{
			name:    "whitespace only",
			input:   "   ",
			wantMap: map[string]interface{}{},
		},
		{
			name:    "null literal",
			input:   "null",
			wantMap: nil, // null decodes to nil interface; encodeOllamaToolArguments must handle it
		},
		{
			name:    "malformed JSON",
			input:   `{not valid json`,
			wantStr: `{not valid json`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeOllamaToolArguments(tc.input)

			if tc.wantStr != "" {
				s, ok := got.(string)
				if !ok {
					t.Fatalf("expected string result, got %T: %v", got, got)
				}
				if s != tc.wantStr {
					t.Errorf("got %q; want %q", s, tc.wantStr)
				}
				return
			}

			if tc.wantMap == nil {
				// null input — result should be nil or an empty/null interface
				// encodeOllamaToolArguments should still produce valid JSON
				encoded := encodeOllamaToolArguments(got)
				if encoded != "null" && encoded != "{}" {
					t.Errorf("encodeOllamaToolArguments(nil) = %q; want null or {}", encoded)
				}
				return
			}

			gotMap, ok := got.(map[string]interface{})
			if !ok {
				t.Fatalf("expected map[string]interface{}, got %T: %v", got, got)
			}
			for k, wantV := range tc.wantMap {
				gotV, exists := gotMap[k]
				if !exists {
					t.Errorf("key %q missing from decoded map", k)
					continue
				}
				if fmt.Sprintf("%v", gotV) != fmt.Sprintf("%v", wantV) {
					t.Errorf("key %q: got %v (%T); want %v (%T)", k, gotV, gotV, wantV, wantV)
				}
			}
			// Also verify round-trip through encodeOllamaToolArguments produces valid JSON.
			encoded := encodeOllamaToolArguments(got)
			var roundtrip map[string]interface{}
			if err := json.Unmarshal([]byte(encoded), &roundtrip); err != nil {
				t.Errorf("encodeOllamaToolArguments round-trip not valid JSON: %v (got %q)", err, encoded)
			}
		})
	}
}

// TestEncodeOllamaToolArgumentsNil verifies nil input produces "{}".
func TestEncodeOllamaToolArgumentsNil(t *testing.T) {
	t.Parallel()
	got := encodeOllamaToolArguments(nil)
	if got != "{}" {
		t.Errorf("encodeOllamaToolArguments(nil) = %q; want {}", got)
	}
}

// TestDecodeEncodeRoundTrip verifies that the string-wrapped shape commonly
// produced by Ollama (e.g. llama3.1, qwen2.5 tool calls) normalizes to the
// same object shape as a direct JSON object input.
func TestDecodeEncodeRoundTrip(t *testing.T) {
	t.Parallel()
	objectInput := `{"query":"test","n":3}`
	// Simulate Ollama wrapping the JSON object in a string.
	stringInput := `"{\"query\":\"test\",\"n\":3}"`

	decodedObj := decodeOllamaToolArguments(objectInput)
	decodedStr := decodeOllamaToolArguments(stringInput)

	encodedObj := encodeOllamaToolArguments(decodedObj)
	encodedStr := encodeOllamaToolArguments(decodedStr)

	var mapObj, mapStr map[string]interface{}
	if err := json.Unmarshal([]byte(encodedObj), &mapObj); err != nil {
		t.Fatalf("encodedObj not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(encodedStr), &mapStr); err != nil {
		t.Fatalf("encodedStr not valid JSON: %v", err)
	}
	if fmt.Sprintf("%v", mapObj) != fmt.Sprintf("%v", mapStr) {
		t.Errorf("object path and string path produce different results:\n  obj: %v\n  str: %v", mapObj, mapStr)
	}
}

func containsCapability(caps []Capability, want Capability) bool {
	for _, cap := range caps {
		if cap == want {
			return true
		}
	}
	return false
}

func TestBuildOllamaRequestToolsAndToolReplies(t *testing.T) {
	t.Parallel()
	req := &CompletionRequest{
		Messages: []ProviderMessage{
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "search", Arguments: `{"query":"x"}`},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content:    `{"ok":true}`,
			},
		},
		Tools: []ToolDefinition{
			{
				Name:        "search",
				Description: "Search the memory index",
				InputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}
	r := buildOllamaRequest("m", req, false, 0)
	if len(r.Tools) != 1 || r.Tools[0].Function.Name != "search" {
		t.Fatalf("tools = %+v; want one search tool", r.Tools)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("messages len = %d; want 2", len(r.Messages))
	}
	if len(r.Messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls len = %d; want 1", len(r.Messages[0].ToolCalls))
	}
	if r.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %q; want call_1", r.Messages[1].ToolCallID)
	}
}

// TestStreamSetsToolCallsFinishReason asserts that when the Ollama stream
// contains tool_call chunks, the final StreamChunk has StopReason="tool_use",
// matching the non-streaming Complete path's vocabulary.
func TestStreamSetsToolCallsFinishReason(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		// Chunk 1: tool_call emitted mid-stream.
		chunk1 := ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				ToolCalls: []ollamaToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ollamaToolCallDetail{
							Name:      "search",
							Arguments: json.RawMessage(`{"query":"cogos"}`),
						},
					},
				},
			},
			Done: false,
		}
		// Chunk 2: final done chunk with token counts.
		chunk2 := ollamaChatResponse{
			Message:         ollamaMessage{Role: "assistant", Content: ""},
			Done:            true,
			PromptEvalCount: 8,
			EvalCount:       5,
		}
		for _, c := range []ollamaChatResponse{chunk1, chunk2} {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", b)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "use a tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var lastChunk StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		lastChunk = sc
	}
	if !lastChunk.Done {
		t.Fatal("last chunk should have Done=true")
	}
	if lastChunk.StopReason != "tool_use" {
		t.Errorf("StopReason = %q; want \"tool_use\" when tool calls were streamed", lastChunk.StopReason)
	}
}

// TestStreamNoToolCallsFinishReason asserts that when the Ollama stream
// contains no tool_calls, the final chunk's StopReason is empty (not
// overridden to "tool_use").
func TestStreamNoToolCallsFinishReason(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		chunks := []ollamaChatResponse{
			{Message: ollamaMessage{Role: "assistant", Content: "Hello"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: " world"}, Done: false},
			{Message: ollamaMessage{Role: "assistant", Content: ""}, Done: true,
				PromptEvalCount: 3, EvalCount: 4},
		}
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", b)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "say hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var lastChunk StreamChunk
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		lastChunk = sc
	}
	if !lastChunk.Done {
		t.Fatal("last chunk should have Done=true")
	}
	if lastChunk.StopReason == "tool_use" {
		t.Errorf("StopReason = %q; want non-tool_use when no tool calls were streamed", lastChunk.StopReason)
	}
}

// TestBuildOllamaRequestSuppressesToolsWhenNone verifies that the tools array
// is omitted from the wire body when tool_choice is "none", and that it is
// included for other values ("auto", "required", specific tool name).
func TestBuildOllamaRequestSuppressesToolsWhenNone(t *testing.T) {
	t.Parallel()

	tools := []ToolDefinition{
		{
			Name:        "search",
			Description: "Search the memory index",
			InputSchema: map[string]interface{}{"type": "object"},
		},
	}
	msgs := []ProviderMessage{{Role: "user", Content: "hi"}}

	// tool_choice == "none" → tools field must be absent from the wire body.
	t.Run("none suppresses tools", func(t *testing.T) {
		t.Parallel()
		req := &CompletionRequest{Messages: msgs, Tools: tools, ToolChoice: "none"}
		r := buildOllamaRequest("m", req, false, 0)
		if len(r.Tools) != 0 {
			t.Errorf("tools len = %d; want 0 when tool_choice is none", len(r.Tools))
		}
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := raw["tools"]; present {
			t.Errorf("wire body contains 'tools' key; want omitted when tool_choice is none")
		}
	})

	// Other tool_choice values must still send the tools schema.
	for _, tc := range []string{"auto", "required", "search"} {
		tc := tc
		t.Run("includes tools for "+tc, func(t *testing.T) {
			t.Parallel()
			req := &CompletionRequest{Messages: msgs, Tools: tools, ToolChoice: tc}
			r := buildOllamaRequest("m", req, false, 0)
			if len(r.Tools) != 1 {
				t.Errorf("tools len = %d; want 1 for tool_choice=%q", len(r.Tools), tc)
			}
		})
	}

	// No tools in the request → tools field absent regardless of tool_choice.
	t.Run("no tools always omits tools field", func(t *testing.T) {
		t.Parallel()
		req := &CompletionRequest{Messages: msgs, ToolChoice: "auto"}
		r := buildOllamaRequest("m", req, false, 0)
		if len(r.Tools) != 0 {
			t.Errorf("tools len = %d; want 0 when no tools defined", len(r.Tools))
		}
	})
}

func TestOllamaCompleteToolCalls(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model: "qwen2.5:9b",
			Message: ollamaMessage{
				Role: "assistant",
				ToolCalls: []ollamaToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ollamaToolCallDetail{
							Name:      "search",
							Arguments: json.RawMessage(`{"query":"hi"}`),
						},
					},
				},
			},
			Done:            true,
			PromptEvalCount: 5,
			EvalCount:       3,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "use a tool"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q; want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "search" {
		t.Fatalf("ToolCalls = %+v; want one search tool call", resp.ToolCalls)
	}
}

// ── ollamaToolCallID ─────────────────────────────────────────────────────────

// TestOllamaToolCallID covers the deterministic ID generator for tool calls
// that Ollama returns without an ID field.
func TestOllamaToolCallID(t *testing.T) {
	t.Parallel()

	t.Run("deterministic same input same output", func(t *testing.T) {
		t.Parallel()
		id1 := ollamaToolCallID("search", map[string]any{"query": "hello"}, 0)
		id2 := ollamaToolCallID("search", map[string]any{"query": "hello"}, 0)
		if id1 != id2 {
			t.Errorf("same inputs produced different IDs: %q vs %q", id1, id2)
		}
	})

	t.Run("distinct ID per distinct args", func(t *testing.T) {
		t.Parallel()
		id1 := ollamaToolCallID("search", map[string]any{"query": "foo"}, 0)
		id2 := ollamaToolCallID("search", map[string]any{"query": "bar"}, 0)
		if id1 == id2 {
			t.Errorf("different args produced the same ID: %q", id1)
		}
	})

	t.Run("distinct ID per distinct seq", func(t *testing.T) {
		t.Parallel()
		id1 := ollamaToolCallID("search", map[string]any{"query": "x"}, 0)
		id2 := ollamaToolCallID("search", map[string]any{"query": "x"}, 1)
		if id1 == id2 {
			t.Errorf("different seq produced the same ID: %q", id1)
		}
	})

	t.Run("format conforms to call_<hex> pattern", func(t *testing.T) {
		t.Parallel()
		id := ollamaToolCallID("get_weather", map[string]any{"city": "Denver"}, 2)
		if !strings.HasPrefix(id, "call_") {
			t.Errorf("ID %q does not start with call_", id)
		}
		suffix := strings.TrimPrefix(id, "call_")
		if len(suffix) != 12 {
			t.Errorf("ID suffix %q has len %d; want 12 hex chars", suffix, len(suffix))
		}
		for _, c := range suffix {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("ID suffix %q contains non-hex char %q", suffix, c)
			}
		}
	})
}

// TestOllamaCompleteToolCallMissingID asserts that when Ollama returns a tool
// call without an ID field, the returned CompletionResponse assigns a
// non-empty ID matching the call_<hex> format.
func TestOllamaCompleteToolCallMissingID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		// Intentionally omit the ID field to simulate an older Ollama model
		// that doesn't assign tool-call IDs.
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model: "gemma4:e4b",
			Message: ollamaMessage{
				Role: "assistant",
				ToolCalls: []ollamaToolCall{
					{
						// ID is deliberately omitted (zero value "").
						Type: "function",
						Function: ollamaToolCallDetail{
							Name:      "list_files",
							Arguments: json.RawMessage(`{"path":"/tmp"}`),
						},
					},
				},
			},
			Done:            true,
			PromptEvalCount: 8,
			EvalCount:       4,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "gemma4:e4b"})
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "list files"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q; want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d; want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID == "" {
		t.Fatal("ToolCall.ID is empty; want a generated call_<hex> ID")
	}
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("ToolCall.ID = %q; want prefix call_", tc.ID)
	}
	// Confirm the assigned ID is stable (same response shape → same ID).
	expected := ollamaToolCallID("list_files", json.RawMessage(`{"path":"/tmp"}`), 0)
	if tc.ID != expected {
		t.Errorf("ToolCall.ID = %q; want deterministic %q", tc.ID, expected)
	}
}

// TestOllamaCompleteToolCallExistingIDPreserved asserts that when Ollama does
// return an ID on a tool call, we keep it rather than overwriting it.
func TestOllamaCompleteToolCallExistingIDPreserved(t *testing.T) {
	t.Parallel()

	const wantID = "ollama-assigned-id-xyz"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Model: "qwen2.5:9b",
			Message: ollamaMessage{
				Role: "assistant",
				ToolCalls: []ollamaToolCall{
					{
						ID:   wantID,
						Type: "function",
						Function: ollamaToolCallDetail{
							Name:      "echo",
							Arguments: json.RawMessage(`{}`),
						},
					},
				},
			},
			Done: true,
		})
	}))
	defer srv.Close()

	p := NewOllamaProvider("ollama", ProviderConfig{Endpoint: srv.URL, Model: "qwen2.5:9b"})
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "echo"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d; want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != wantID {
		t.Errorf("ToolCall.ID = %q; want %q (Ollama-assigned ID should be preserved)", resp.ToolCalls[0].ID, wantID)
	}
}
