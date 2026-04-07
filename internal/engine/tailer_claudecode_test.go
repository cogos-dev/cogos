package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeCodeTailerNormalizesJSONL(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "conversation.jsonl")
	fixture := "" +
		`{"role":"user","content":"hello from user","timestamp":"2026-01-02T03:04:05Z"}` + "\n" +
		`{"role":"assistant","content":"calling tool","tool_use":{"name":"shell"},"timestamp":"2026-01-02T03:04:06Z"}` + "\n" +
		`{"role":"tool","content":"tool output","tool_result":{"status":"ok"},"timestamp":"2026-01-02T03:04:07Z"}` + "\n"
	if err := os.WriteFile(path, []byte(fixture), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer := &ClaudeCodeTailer{
		Watcher: NewFileWatcher(10 * time.Millisecond),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan CogBlock, 3)
	errCh := make(chan error, 1)
	go func() {
		errCh <- tailer.Tail(ctx, path, out)
	}()

	first := waitForBlock(t, out)
	second := waitForBlock(t, out)
	third := waitForBlock(t, out)

	assertClaudeCodeBlock(t, first, BlockMessage, "user", "hello from user")
	assertClaudeCodeBlock(t, second, BlockToolCall, "assistant", "calling tool")
	assertClaudeCodeBlock(t, third, BlockToolResult, "tool", "tool output")

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Tail returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tailer did not stop after cancellation")
	}
}

func TestNormalizeClaudeCodeLineToolUseKind(t *testing.T) {
	t.Parallel()

	block, err := normalizeClaudeCodeLine([]byte(`{"role":"assistant","content":"calling","tool_use":{"name":"shell"},"timestamp":"2026-01-02T03:04:06Z"}`))
	if err != nil {
		t.Fatalf("normalizeClaudeCodeLine: %v", err)
	}
	if block.Kind != BlockToolCall {
		t.Fatalf("Kind = %q; want %q", block.Kind, BlockToolCall)
	}
}

func TestNormalizeClaudeCodeLineToolResultKind(t *testing.T) {
	t.Parallel()

	block, err := normalizeClaudeCodeLine([]byte(`{"role":"tool","content":"ok","tool_result":{"status":"ok"},"timestamp":"2026-01-02T03:04:07Z"}`))
	if err != nil {
		t.Fatalf("normalizeClaudeCodeLine: %v", err)
	}
	if block.Kind != BlockToolResult {
		t.Fatalf("Kind = %q; want %q", block.Kind, BlockToolResult)
	}
}

func TestNormalizeClaudeCodeLineMalformedJSONReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := normalizeClaudeCodeLine([]byte(`{"role":"assistant","content":"oops"`)); err == nil {
		t.Fatal("normalizeClaudeCodeLine error = nil; want parse error")
	}
}

func assertClaudeCodeBlock(t *testing.T, block CogBlock, kind CogBlockKind, role, content string) {
	t.Helper()

	if block.Kind != kind {
		t.Fatalf("Kind = %q; want %q", block.Kind, kind)
	}
	if block.SourceChannel != "claude-code" {
		t.Fatalf("SourceChannel = %q; want claude-code", block.SourceChannel)
	}
	if block.SourceIdentity != role {
		t.Fatalf("SourceIdentity = %q; want %q", block.SourceIdentity, role)
	}
	if block.Provenance.OriginChannel != "claude-code" {
		t.Fatalf("Provenance.OriginChannel = %q; want claude-code", block.Provenance.OriginChannel)
	}
	if block.Provenance.NormalizedBy != "tailer-claude-code" {
		t.Fatalf("Provenance.NormalizedBy = %q; want tailer-claude-code", block.Provenance.NormalizedBy)
	}
	if len(block.Messages) != 1 {
		t.Fatalf("len(Messages) = %d; want 1", len(block.Messages))
	}
	if block.Messages[0].Role != role {
		t.Fatalf("Messages[0].Role = %q; want %q", block.Messages[0].Role, role)
	}
	if block.Messages[0].Content != content {
		t.Fatalf("Messages[0].Content = %q; want %q", block.Messages[0].Content, content)
	}
}

func waitForBlock(t *testing.T, blocks <-chan CogBlock) CogBlock {
	t.Helper()

	select {
	case block := <-blocks:
		return block
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for block")
		return CogBlock{}
	}
}
