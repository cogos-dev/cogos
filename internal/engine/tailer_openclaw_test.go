package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenClawTailerTailsJSONLFile(t *testing.T) {
	t.Parallel()

	fixturePath := filepath.Join("testdata", "openclaw_session.jsonl")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}

	root := t.TempDir()
	logPath := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(logPath, fixture, 0644); err != nil {
		t.Fatalf("WriteFile log: %v", err)
	}

	tailer := &OpenClawTailer{Watcher: NewFileWatcher(10 * time.Millisecond)}
	if got := tailer.Name(); got != "openclaw" {
		t.Fatalf("Name() = %q; want %q", got, "openclaw")
	}

	out := make(chan CogBlock, 3)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- tailer.Tail(ctx, logPath, out)
	}()

	got := make([]CogBlock, 0, 3)
	for len(got) < 3 {
		select {
		case block := <-out:
			got = append(got, block)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for openclaw blocks")
		}
	}

	check := func(index int, kind CogBlockKind, role, content string) {
		if got[index].Kind != kind {
			t.Fatalf("block[%d].Kind = %q; want %q", index, got[index].Kind, kind)
		}
		if got[index].SourceChannel != "openclaw" {
			t.Fatalf("block[%d].SourceChannel = %q; want openclaw", index, got[index].SourceChannel)
		}
		if got[index].Provenance.OriginChannel != "openclaw" {
			t.Fatalf("block[%d].Provenance.OriginChannel = %q; want openclaw", index, got[index].Provenance.OriginChannel)
		}
		if got[index].Provenance.NormalizedBy != "tailer-openclaw" {
			t.Fatalf("block[%d].Provenance.NormalizedBy = %q; want tailer-openclaw", index, got[index].Provenance.NormalizedBy)
		}
		if got[index].SessionID != "sess-123" {
			t.Fatalf("block[%d].SessionID = %q; want sess-123", index, got[index].SessionID)
		}
		if len(got[index].Messages) != 1 {
			t.Fatalf("block[%d].Messages len = %d; want 1", index, len(got[index].Messages))
		}
		if got[index].Messages[0].Role != role {
			t.Fatalf("block[%d].Messages[0].Role = %q; want %q", index, got[index].Messages[0].Role, role)
		}
		if got[index].Messages[0].Content != content {
			t.Fatalf("block[%d].Messages[0].Content = %q; want %q", index, got[index].Messages[0].Content, content)
		}
		if got[index].Timestamp.IsZero() {
			t.Fatalf("block[%d].Timestamp should be set", index)
		}
	}

	check(0, BlockMessage, "user", "hello from openclaw")
	check(1, BlockToolCall, "assistant", `{"name":"search","arguments":{"q":"weather"}}`)
	check(2, BlockToolResult, "tool", "sunny")

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

func TestOpenClawTailerDirectoryModeDiscoversNewJSONLFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tailer := &OpenClawTailer{
		Watcher:      NewFileWatcher(10 * time.Millisecond),
		ScanInterval: 10 * time.Millisecond,
	}

	out := make(chan CogBlock, 2)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- tailer.Tail(ctx, root, out)
	}()

	time.Sleep(30 * time.Millisecond)
	logPath := filepath.Join(root, "new-session.jsonl")
	line := `{"id":"evt-1","type":"message","role":"user","content":"hello","timestamp":"2026-01-02T03:04:05Z","session_id":"sess-1"}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	block := waitForBlock(t, out)
	if block.Kind != BlockMessage {
		t.Fatalf("Kind = %q; want %q", block.Kind, BlockMessage)
	}
	if block.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q; want %q", block.SessionID, "sess-1")
	}

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

func TestOpenClawTailerSkipsMalformedJSONLines(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	logPath := filepath.Join(root, "session.jsonl")
	fixture := "" +
		`{"id":"evt-bad","type":"message","role":"user","content":"unterminated"` + "\n" +
		`{"id":"evt-good","type":"message","role":"user","content":"hello","timestamp":"2026-01-02T03:04:05Z","session_id":"sess-2"}` + "\n"
	if err := os.WriteFile(logPath, []byte(fixture), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer := &OpenClawTailer{Watcher: NewFileWatcher(10 * time.Millisecond)}
	out := make(chan CogBlock, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- tailer.Tail(ctx, logPath, out)
	}()

	block := waitForBlock(t, out)
	if block.ID != "evt-good" {
		t.Fatalf("ID = %q; want evt-good", block.ID)
	}

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
