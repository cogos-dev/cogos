//go:build integration

// provider_ollama_integration_test.go — integration tests against a real Ollama
// Run with: go test -tags integration -run TestOllamaIntegration ./...
package main

import (
	"context"
	"testing"
)

func TestOllamaIntegrationPing(t *testing.T) {
	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: "http://localhost:11434",
		Model:    "qwen2.5:9b",
	})
	lat, err := p.Ping(context.Background())
	if err != nil {
		t.Skipf("Ollama not available: %v", err)
	}
	t.Logf("Ollama ping latency: %v", lat)
}

func TestOllamaIntegrationComplete(t *testing.T) {
	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: "http://localhost:11434",
		Model:    "qwen2.5:9b",
		Timeout:  120,
	})
	if !p.Available(context.Background()) {
		t.Skip("qwen2.5:9b not available in Ollama")
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
		t.Error("empty response from Ollama")
	}
}

func TestOllamaIntegrationStream(t *testing.T) {
	p := NewOllamaProvider("ollama", ProviderConfig{
		Endpoint: "http://localhost:11434",
		Model:    "qwen2.5:9b",
		Timeout:  120,
	})
	if !p.Available(context.Background()) {
		t.Skip("qwen2.5:9b not available in Ollama")
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
