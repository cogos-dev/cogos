//go:build integration

// provider_openai_integration_test.go — integration tests against a real OpenAI-compatible server
// Run with: go test -tags integration -run TestOpenAIIntegration ./internal/engine/ -v
package engine

import (
	"context"
	"testing"
)

func TestOpenAIIntegrationPing(t *testing.T) {
	p := NewOpenAICompatProvider("lmstudio", ProviderConfig{
		Endpoint: "http://localhost:1234",
		Timeout:  5,
	})
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Skipf("OpenAI-compat server not available: %v", err)
	}
	t.Logf("OpenAI-compat ping latency: %v", lat)
}

func TestOpenAIIntegrationListModels(t *testing.T) {
	p := NewOpenAICompatProvider("lmstudio", ProviderConfig{
		Endpoint: "http://localhost:1234",
		Timeout:  5,
	})
	models, err := p.listModels(context.Background())
	if err != nil {
		t.Skipf("OpenAI-compat server not available: %v", err)
	}
	if len(models) == 0 {
		t.Error("no models returned from /v1/models")
	}
	for _, m := range models {
		t.Logf("Model: %s", m)
	}
}

func TestOpenAIIntegrationComplete(t *testing.T) {
	p := NewOpenAICompatProvider("lmstudio", ProviderConfig{
		Endpoint: "http://localhost:1234",
		Timeout:  120,
	})
	if !p.Available(context.Background()) {
		t.Skip("OpenAI-compat server not available or has no models")
	}

	// Get the first available model.
	models, _ := p.listModels(context.Background())
	if len(models) > 0 {
		p.model = models[0]
	}

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "Reply with exactly the word: pong"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	t.Logf("Response: %q (in=%d out=%d lat=%v)",
		resp.Content, resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.ProviderMeta.Latency)
	if resp.Content == "" {
		t.Error("empty response from OpenAI-compat server")
	}
}

func TestOpenAIIntegrationStream(t *testing.T) {
	p := NewOpenAICompatProvider("lmstudio", ProviderConfig{
		Endpoint: "http://localhost:1234",
		Timeout:  120,
	})
	if !p.Available(context.Background()) {
		t.Skip("OpenAI-compat server not available or has no models")
	}

	// Get the first available model.
	models, _ := p.listModels(context.Background())
	if len(models) > 0 {
		p.model = models[0]
	}

	ch, err := p.Stream(context.Background(), &CompletionRequest{
		Messages: []ProviderMessage{
			{Role: "user", Content: "Count to 3, one number per line."},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var chunks int
	for sc := range ch {
		if sc.Error != nil {
			t.Fatalf("stream error: %v", sc.Error)
		}
		chunks++
		if sc.Done {
			if sc.Usage != nil {
				t.Logf("Token usage: in=%d out=%d", sc.Usage.InputTokens, sc.Usage.OutputTokens)
			}
			break
		}
	}
	if chunks == 0 {
		t.Error("no chunks received")
	}
	t.Logf("Received %d chunks", chunks)
}
