// provider_ollama_test.go — OllamaProvider unit tests
//
// Unit tests use httptest.NewServer to mock Ollama responses.
// Integration tests (//go:build integration) hit a real Ollama at localhost:11434.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

