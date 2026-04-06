package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeOpenAIRequestProducesCogBlock(t *testing.T) {
	t.Parallel()

	req := &oaiChatRequest{
		Model: "claude",
		Messages: []oaiMessage{
			{Role: "system", Content: mustMarshalString("system context")},
			{Role: "user", Content: mustMarshalString("hello")},
			{Role: "assistant", Content: mustMarshalString("hi")},
			{Role: "user", Content: mustMarshalString("what context do you see?")},
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	block := NormalizeOpenAIRequest(req, raw, "http")
	if block.ID == "" {
		t.Fatal("block ID should not be empty")
	}
	if block.Timestamp.IsZero() {
		t.Fatal("block timestamp should be set")
	}
	if block.Kind != BlockMessage {
		t.Fatalf("block.Kind = %q; want %q", block.Kind, BlockMessage)
	}
	if block.SourceChannel != "http" {
		t.Fatalf("SourceChannel = %q; want http", block.SourceChannel)
	}
	if block.SourceTransport != "openai-compat" {
		t.Fatalf("SourceTransport = %q; want openai-compat", block.SourceTransport)
	}
	if len(block.RawPayload) == 0 {
		t.Fatal("raw payload should be preserved")
	}
	if len(block.Messages) != 4 {
		t.Fatalf("messages len = %d; want 4", len(block.Messages))
	}
	if block.Messages[3].Content != "what context do you see?" {
		t.Fatalf("last normalized message = %q", block.Messages[3].Content)
	}
	if !block.TrustContext.Authenticated || block.TrustContext.TrustScore != 1.0 {
		t.Fatalf("unexpected trust context: %+v", block.TrustContext)
	}
	if block.Provenance.NormalizedBy != "http-openai" {
		t.Fatalf("NormalizedBy = %q; want http-openai", block.Provenance.NormalizedBy)
	}
}

func TestRecordBlockReturnsLedgerRef(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := &Process{
		cfg:       cfg,
		sessionID: "session-record-block",
	}

	block := &CogBlock{
		ID:              "block-1",
		SourceChannel:   "http",
		SourceTransport: "openai-compat",
		TargetIdentity:  "Cog",
		WorkspaceID:     filepath.Base(root),
		Kind:            BlockMessage,
		Timestamp:       mustTime(t, "2026-04-02T12:00:00Z"),
	}

	ref := p.RecordBlock(block)
	if ref == "" {
		t.Fatal("RecordBlock should return a ledger ref")
	}
	if block.LedgerRef == "" {
		t.Fatal("block should be annotated with ledger ref")
	}

	eventsPath := filepath.Join(root, ".cog", "ledger", p.SessionID(), "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("ledger should contain a recorded event")
	}
}

func mustTime(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", ts, err)
	}
	return parsed
}
