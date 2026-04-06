package main

import "testing"

func TestNormalizeAnthropicRequestProducesCogBlock(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-sonnet","system":[{"type":"text","text":"system context"}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]},{"role":"user","content":[{"type":"text","text":"what context do you see?"}]}],"max_tokens":256}`)

	block := NormalizeAnthropicRequest(body, "http")
	if block.ID == "" {
		t.Fatal("block ID should not be empty")
	}
	if block.Kind != BlockMessage {
		t.Fatalf("Kind = %q; want %q", block.Kind, BlockMessage)
	}
	if block.SourceTransport != "anthropic" {
		t.Fatalf("SourceTransport = %q; want anthropic", block.SourceTransport)
	}
	if block.Provenance.NormalizedBy != "http-anthropic" {
		t.Fatalf("NormalizedBy = %q; want http-anthropic", block.Provenance.NormalizedBy)
	}
	if len(block.Messages) != 4 {
		t.Fatalf("messages len = %d; want 4", len(block.Messages))
	}
	if block.Messages[0].Role != "system" || block.Messages[0].Content != "system context" {
		t.Fatalf("system message = %+v; want normalized system context", block.Messages[0])
	}
	if block.Messages[3].Content != "what context do you see?" {
		t.Fatalf("last normalized message = %q; want final user message", block.Messages[3].Content)
	}
}
