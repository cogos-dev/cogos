// agent_dispatch_live_test.go — opt-in smoke test against the live Ollama
// instance at localhost:11434. Skipped by default to keep `go test ./...`
// hermetic; enable with:
//
//	go test -tags fts5,dispatchlive -run TestDispatchLive_ -v ./...
//
// The smoke is a sanity probe, not a correctness test: it confirms (a) the
// dispatcher can reach Ollama, (b) the structured return is populated when a
// real model responds, (c) failure modes (no model loaded) are surfaced as
// Error rather than panicking.

//go:build dispatchlive

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
)

func TestDispatchLive_HelloFromGemma(t *testing.T) {
	ollamaURL := os.Getenv("DISPATCH_LIVE_OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	model := os.Getenv("DISPATCH_LIVE_MODEL")
	if model == "" {
		model = "gemma3:e4b"
	}

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: ollamaURL, Model: model})
	RegisterRespondTool(h)
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{
		Task:           "Reply with exactly: hello from gemma",
		N:              1,
		TimeoutSeconds: 15,
	}
	if err := req.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := d.DispatchToHarness(ctx, req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res.Results))
	}
	r := res.Results[0]
	t.Logf("[live] success=%v error=%q content=%q tool_calls=%d turns=%d duration=%.2fs",
		r.Success, r.Error, r.Content, len(r.ToolCalls), r.Turns, r.DurationSec)
	if !r.Success {
		// Don't fail the test on no-model — this smoke is informational.
		// We just want the dispatcher to surface a clean error string.
		t.Logf("[live] dispatcher surfaced error gracefully: %s", r.Error)
	} else if r.Content == "" {
		t.Errorf("[live] expected non-empty content from gemma, got empty")
	}
}

func TestDispatchLive_ConcurrentN3(t *testing.T) {
	ollamaURL := os.Getenv("DISPATCH_LIVE_OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	model := os.Getenv("DISPATCH_LIVE_MODEL")
	if model == "" {
		model = "gemma3:e4b"
	}

	h := NewAgentHarness(AgentHarnessConfig{OllamaURL: ollamaURL, Model: model})
	d := &HarnessDispatcher{AgentID: engine.DefaultAgentID, Harness: h}

	req := engine.DispatchRequest{
		Task:           "Pick a fruit and reply with just its name (one word).",
		N:              3,
		TimeoutSeconds: 30,
	}
	if err := req.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	res, err := d.DispatchToHarness(ctx, req)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	t.Logf("[live] batch elapsed=%.2fs total_results=%d", time.Since(start).Seconds(), len(res.Results))
	for i, r := range res.Results {
		t.Logf("[live] slot[%d] success=%v error=%q content=%q duration=%.2fs",
			i, r.Success, r.Error, r.Content, r.DurationSec)
	}
	// Sanity: every slot should have a non-negative DurationSec, even on
	// error. Sub-microsecond errors can round to 0.0 on some platforms,
	// so we only flag actively-negative values.
	for _, r := range res.Results {
		if r.DurationSec < 0 {
			t.Errorf("slot %d had negative duration %.2fs", r.Index, r.DurationSec)
		}
	}
}
