// provider_vllm_test.go — vLLM provider dispatch parity tests.
//
// vLLM is registered as a first-class provider type in router.go's
// makeProvider switch (case "vllm"). It resolves to OpenAICompatProvider
// because vLLM speaks the OpenAI /v1/chat/completions and /v1/models
// contract.
//
// These tests do not require a live vLLM server. They use httptest.Server
// to emit vLLM-shaped responses and verify the OpenAICompatProvider parses
// them correctly. vLLM's distinguishing wire-level features — the standard
// "usage" block with prompt_tokens / completion_tokens / total_tokens, and
// the /v1/models listing — are confirmed to round-trip through the
// existing OpenAICompatProvider without any vLLM-specific code path.
//
// Phase 0 of the Ollama → vLLM migration plan; see docs/inference/vllm.md
// and the migration cogdoc for the broader phased approach.
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

// newVLLMTestServer returns an httptest server that emits vLLM-shaped
// /v1/models and /v1/chat/completions responses for the configured model.
func newVLLMTestServer(t *testing.T, modelID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			// vLLM's /v1/models response shape matches OpenAI's: a "data"
			// array of objects with an "id" field plus implementation-
			// specific extras the OpenAI client ignores.
			_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON(modelID))
		case "/v1/chat/completions":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var payload openaiChatRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if payload.Stream {
				// Minimal SSE response with one delta and a usage chunk.
				w.Header().Set("Content-Type", "text/event-stream")
				stop := "stop"
				chunks := []openaiStreamChunk{
					{
						ID:      "vllm-chatcmpl-1",
						Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Role: "assistant"}}},
					},
					{
						ID:      "vllm-chatcmpl-1",
						Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Content: "pong"}}},
					},
					{
						ID:      "vllm-chatcmpl-1",
						Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{}, FinishReason: &stop}},
						Usage:   &openaiUsageResponse{PromptTokens: 8, CompletionTokens: 1, TotalTokens: 9},
					},
				}
				for _, c := range chunks {
					b, _ := json.Marshal(c)
					fmt.Fprintf(w, "data: %s\n\n", b)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			// Non-streaming: emit a vLLM-shaped chat completion with a
			// canonical usage block (vLLM always populates usage).
			_ = json.NewEncoder(w).Encode(openaiChatResponse{
				ID: "vllm-chatcmpl-1",
				Choices: []openaiChoice{
					{
						Index:        0,
						Message:      openaiMessage{Role: "assistant", Content: "pong"},
						FinishReason: "stop",
					},
				},
				Usage: &openaiUsageResponse{
					PromptTokens:     8,
					CompletionTokens: 1,
					TotalTokens:      9,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestVLLMProviderTypeResolvesToOpenAICompat asserts that the "vllm"
// provider type label produces an OpenAICompatProvider — i.e., vLLM
// reuses the shared dispatch path. This is the load-bearing claim for
// Phase 0: no new provider implementation is required.
//
// #556 wraps every local openai-compat-family provider (including vllm) in
// a queuedProvider at construction time, so makeProvider's direct return
// type is now *queuedProvider; unwrap one layer to reach the
// *OpenAICompatProvider this test is actually about. Name()/Model()/
// Capabilities() are asserted through the outer queuedProvider, exercising
// the embedded-interface promotion queuedProvider relies on for everything
// it doesn't explicitly override.
func TestVLLMProviderTypeResolvesToOpenAICompat(t *testing.T) {
	t.Parallel()

	p, err := makeProvider("vllm", ProviderConfig{
		Type:     "vllm",
		Endpoint: "http://localhost:8000",
		Model:    "gemma4:e4b",
		Timeout:  30,
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider(vllm): %v", err)
	}
	qp, ok := p.(*queuedProvider)
	if !ok {
		t.Fatalf("vllm should resolve to *queuedProvider (wrapping OpenAICompatProvider per #556), got %T", p)
	}
	oa, ok := qp.Provider.(*OpenAICompatProvider)
	if !ok {
		t.Fatalf("vllm's queuedProvider should wrap *OpenAICompatProvider, got %T", qp.Provider)
	}
	if p.Name() != "vllm" {
		t.Errorf("Name() = %q; want vllm", p.Name())
	}
	if p.Model() != "gemma4:e4b" {
		t.Errorf("Model() = %q; want gemma4:e4b", p.Model())
	}
	caps := oa.Capabilities()
	if !caps.IsLocal {
		t.Error("IsLocal should be true for vllm (local-tier provider)")
	}
}

// TestVLLMAvailableHitsModelsEndpoint verifies the /v1/models probe used
// by the router's availability check works against a vLLM-shaped server.
func TestVLLMAvailableHitsModelsEndpoint(t *testing.T) {
	t.Parallel()

	srv := newVLLMTestServer(t, "gemma4:e4b")
	defer srv.Close()

	p, err := makeProvider("vllm-local", ProviderConfig{
		Type:     "vllm",
		Endpoint: srv.URL,
		Model:    "gemma4:e4b",
		Timeout:  5,
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	if !p.Available(context.Background()) {
		t.Error("Available() = false; want true when model is listed at /v1/models")
	}
}

// TestVLLMCompleteParsesUsageBlock confirms that vLLM's standard usage
// block round-trips through the shared OpenAI-compat parsing logic. This
// is the dispatch-parity assertion against Ollama / lmstudio.
func TestVLLMCompleteParsesUsageBlock(t *testing.T) {
	t.Parallel()

	srv := newVLLMTestServer(t, "gemma4:e4b")
	defer srv.Close()

	p, err := makeProvider("vllm-local", ProviderConfig{
		Type:     "vllm",
		Endpoint: srv.URL,
		Model:    "gemma4:e4b",
		Timeout:  5,
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "reply with: pong"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "pong" {
		t.Errorf("Content = %q; want pong", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q; want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v; want {InputTokens:8 OutputTokens:1}", resp.Usage)
	}
	if resp.ProviderMeta.Provider != "vllm-local" {
		t.Errorf("ProviderMeta.Provider = %q; want vllm-local", resp.ProviderMeta.Provider)
	}
}

// TestVLLMStreamEmitsSSEDeltas confirms SSE streaming through the shared
// parseOpenAISSE path against a vLLM-shaped event stream.
func TestVLLMStreamEmitsSSEDeltas(t *testing.T) {
	t.Parallel()

	srv := newVLLMTestServer(t, "gemma4:e4b")
	defer srv.Close()

	p, err := makeProvider("vllm-local", ProviderConfig{
		Type:     "vllm",
		Endpoint: srv.URL,
		Model:    "gemma4:e4b",
		Timeout:  5,
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{{Role: "user", Content: "reply pong"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var content strings.Builder
	var done bool
	var usage *TokenUsage
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		if sc.Delta != "" {
			content.WriteString(sc.Delta)
		}
		if sc.Done {
			done = true
			usage = sc.Usage
		}
	}
	if content.String() != "pong" {
		t.Errorf("streamed content = %q; want pong", content.String())
	}
	if !done {
		t.Error("expected a Done chunk at end of vllm SSE stream")
	}
	if usage == nil {
		t.Fatal("expected usage on final stream chunk")
	}
	if usage.InputTokens != 8 || usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v; want {8, 1}", usage)
	}
}
