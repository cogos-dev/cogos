package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncWatcherDetectsNewEnvelopeFile(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	inboxPath := mustMakeSyncInbox(t, workspaceRoot)
	blobStore := newTestBlobStore(t, workspaceRoot)
	watcher := NewSyncWatcher(blobStore, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan SyncEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Watch(ctx, inboxPath, out)
	}()

	envelopePath := filepath.Join(inboxPath, "envelope.json")
	writeSyncEnvelopeFile(t, envelopePath, SyncEnvelope{
		Version:      1,
		OriginNodeID: "node-a",
		TargetNodeID: "node-b",
		BlobHash:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Kind:         "block",
		Signature:    "signature",
	})

	event := waitForSyncEvent(t, out)
	if !event.Valid {
		t.Fatalf("Valid = false, want true (error: %s)", event.ValidationError)
	}
	if event.FilePath != envelopePath {
		t.Fatalf("FilePath = %q, want %q", event.FilePath, envelopePath)
	}
	if event.AlreadyHave {
		t.Fatal("AlreadyHave = true, want false")
	}
	if event.Envelope.OriginNodeID != "node-a" {
		t.Fatalf("OriginNodeID = %q, want %q", event.Envelope.OriginNodeID, "node-a")
	}

	cancel()
	assertSyncWatcherStopped(t, errCh)
}

func TestSyncWatcherFlagsInvalidEnvelope(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	inboxPath := mustMakeSyncInbox(t, workspaceRoot)
	blobStore := newTestBlobStore(t, workspaceRoot)
	watcher := NewSyncWatcher(blobStore, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan SyncEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Watch(ctx, inboxPath, out)
	}()

	writeSyncEnvelopeFile(t, filepath.Join(inboxPath, "invalid.json"), SyncEnvelope{
		Version:      1,
		OriginNodeID: "node-a",
		BlobHash:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Kind:         "block",
		Signature:    "signature",
	})

	event := waitForSyncEvent(t, out)
	if event.Valid {
		t.Fatal("Valid = true, want false")
	}
	if event.ValidationError == "" {
		t.Fatal("ValidationError = empty, want message")
	}
	if event.AlreadyHave {
		t.Fatal("AlreadyHave = true, want false")
	}

	cancel()
	assertSyncWatcherStopped(t, errCh)
}

func TestSyncWatcherMarksAlreadyPresentBlob(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	inboxPath := mustMakeSyncInbox(t, workspaceRoot)
	blobStore := newTestBlobStore(t, workspaceRoot)
	hash, err := blobStore.Store([]byte("hello, constellation"), "text/plain")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	watcher := NewSyncWatcher(blobStore, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan SyncEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Watch(ctx, inboxPath, out)
	}()

	writeSyncEnvelopeFile(t, filepath.Join(inboxPath, "existing-blob.json"), SyncEnvelope{
		Version:      1,
		OriginNodeID: "node-a",
		TargetNodeID: "node-b",
		BlobHash:     hash,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Kind:         "manifest",
		Signature:    "signature",
	})

	event := waitForSyncEvent(t, out)
	if !event.Valid {
		t.Fatalf("Valid = false, want true (error: %s)", event.ValidationError)
	}
	if !event.AlreadyHave {
		t.Fatal("AlreadyHave = false, want true")
	}

	cancel()
	assertSyncWatcherStopped(t, errCh)
}

func TestSyncWatcherGracefulShutdown(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	inboxPath := mustMakeSyncInbox(t, workspaceRoot)
	blobStore := newTestBlobStore(t, workspaceRoot)
	watcher := NewSyncWatcher(blobStore, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Watch(ctx, inboxPath, make(chan SyncEvent))
	}()

	cancel()
	assertSyncWatcherStopped(t, errCh)
}

func newTestBlobStore(t *testing.T, workspaceRoot string) *BlobStore {
	t.Helper()

	blobStore := NewBlobStore(workspaceRoot)
	if err := blobStore.Init(); err != nil {
		t.Fatalf("Init blob store: %v", err)
	}
	return blobStore
}

func mustMakeSyncInbox(t *testing.T, workspaceRoot string) string {
	t.Helper()

	inboxPath := filepath.Join(workspaceRoot, ".cog", "sync", "inbox")
	if err := os.MkdirAll(inboxPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return inboxPath
}

func writeSyncEnvelopeFile(t *testing.T, path string, envelope SyncEnvelope) {
	t.Helper()

	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func waitForSyncEvent(t *testing.T, out <-chan SyncEvent) SyncEvent {
	t.Helper()

	select {
	case event := <-out:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sync event")
		return SyncEvent{}
	}
}

func assertSyncWatcherStopped(t *testing.T, errCh <-chan error) {
	t.Helper()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Watch returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher did not stop after cancellation")
	}
}
