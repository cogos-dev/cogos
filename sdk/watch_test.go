package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseWatchPattern(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		wantNS    string
		wantPath  string
		wantWild  bool
		wantRecur bool
		wantErr   bool
	}{
		{
			name:    "simple namespace",
			pattern: "cog://mem",
			wantNS:  "mem",
		},
		{
			name:     "namespace with path",
			pattern:  "cog://mem/semantic/insights",
			wantNS:   "mem",
			wantPath: "semantic/insights",
		},
		{
			name:     "wildcard path",
			pattern:  "cog://mem/semantic/*",
			wantNS:   "mem",
			wantPath: "semantic",
			wantWild: true,
		},
		{
			name:      "recursive wildcard",
			pattern:   "cog://mem/semantic/**",
			wantNS:    "mem",
			wantPath:  "semantic",
			wantWild:  true,
			wantRecur: true,
		},
		{
			name:    "signals namespace",
			pattern: "cog://signals/*",
			wantNS:  "signals",
			wantPath: "",
			wantWild: true,
		},
		{
			name:    "invalid scheme",
			pattern: "http://mem",
			wantErr: true,
		},
		{
			name:    "unknown namespace",
			pattern: "cog://unknown/*",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWatchPattern(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseWatchPattern() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.namespace != tt.wantNS {
				t.Errorf("namespace = %q, want %q", got.namespace, tt.wantNS)
			}
			if got.path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.path, tt.wantPath)
			}
			if got.wildcard != tt.wantWild {
				t.Errorf("wildcard = %v, want %v", got.wildcard, tt.wantWild)
			}
			if got.recursive != tt.wantRecur {
				t.Errorf("recursive = %v, want %v", got.recursive, tt.wantRecur)
			}
		})
	}
}

func TestWatchEventTypes(t *testing.T) {
	if WatchCreated != "created" {
		t.Errorf("WatchCreated = %q, want %q", WatchCreated, "created")
	}
	if WatchModified != "modified" {
		t.Errorf("WatchModified = %q, want %q", WatchModified, "modified")
	}
	if WatchDeleted != "deleted" {
		t.Errorf("WatchDeleted = %q, want %q", WatchDeleted, "deleted")
	}
}

func TestWatcher_Close(t *testing.T) {
	// Create test workspace
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	memDir := filepath.Join(cogDir, "mem", "semantic")

	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create id.cog
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\n---\n# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.Close()

	ctx := context.Background()
	watcher, err := kernel.WatchURI(ctx, "cog://mem/semantic/*")
	if err != nil {
		t.Fatal(err)
	}

	// Close should not error
	if err := watcher.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Double close should be safe
	if err := watcher.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
}

func TestWatcher_DetectsFileChanges(t *testing.T) {
	// Create test workspace
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	memDir := filepath.Join(cogDir, "mem", "semantic")

	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create id.cog
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\n---\n# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	watcher, err := kernel.WatchURI(ctx, "cog://mem/semantic/*")
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	// Create a file in the watched directory
	testFile := filepath.Join(memDir, "test-insight.cog.md")
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(testFile, []byte("---\ntype: insight\nid: test\ntitle: Test\ncreated: 2026-01-10\n---\n# Test"), 0644)
	}()

	// Wait for event
	select {
	case event := <-watcher.Events:
		if event.Type != WatchCreated {
			t.Errorf("event.Type = %q, want %q", event.Type, WatchCreated)
		}
		if event.FilePath != testFile {
			t.Errorf("event.FilePath = %q, want %q", event.FilePath, testFile)
		}
	case <-ctx.Done():
		t.Error("timeout waiting for watch event")
	}
}

func TestWatcher_ContextCancellation(t *testing.T) {
	// Create test workspace
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	memDir := filepath.Join(cogDir, "mem", "semantic")

	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create id.cog
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\n---\n# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.Close()

	ctx, cancel := context.WithCancel(context.Background())

	watcher, err := kernel.WatchURI(ctx, "cog://mem/semantic/*")
	if err != nil {
		t.Fatal(err)
	}

	// Cancel context
	cancel()

	// Give event loop time to process cancellation
	time.Sleep(100 * time.Millisecond)

	// Events channel should eventually close
	select {
	case _, ok := <-watcher.Events:
		if ok {
			// Got an event before close, that's fine
		}
		// Channel closed, as expected
	case <-time.After(time.Second):
		t.Error("timeout waiting for events channel to close")
	}

	watcher.Close()
}

func TestKernel_WatchURI_NotConnected(t *testing.T) {
	// Create and immediately close a kernel
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatal(err)
	}
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\n---\n# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	kernel.Close()

	// Should error since kernel is closed
	_, err = kernel.WatchURI(context.Background(), "cog://mem/*")
	if err == nil {
		t.Error("expected error for closed kernel")
	}
}

func TestPathToURI(t *testing.T) {
	// Create test workspace
	tmpDir := t.TempDir()
	cogDir := filepath.Join(tmpDir, ".cog")
	memDir := filepath.Join(cogDir, "mem")

	if err := os.MkdirAll(filepath.Join(memDir, "semantic"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create id.cog
	idPath := filepath.Join(cogDir, "id.cog")
	if err := os.WriteFile(idPath, []byte("---\nid: test\n---\n# Test"), 0644); err != nil {
		t.Fatal(err)
	}

	kernel, err := Connect(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer kernel.Close()

	// Create a mock watcher with the kernel
	w := &Watcher{kernel: kernel}

	pattern := &watchPattern{namespace: "mem"}
	mapping := map[string]string{
		memDir: "cog://mem",
	}

	tests := []struct {
		filePath string
		wantURI  string
	}{
		{
			filePath: filepath.Join(memDir, "semantic", "test.cog.md"),
			wantURI:  "cog://mem/semantic/test",
		},
		{
			filePath: filepath.Join(memDir, "semantic", "test.md"),
			wantURI:  "cog://mem/semantic/test",
		},
		{
			filePath: filepath.Join(memDir, "episodic", "session.cog.md"),
			wantURI:  "cog://mem/episodic/session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.filePath, func(t *testing.T) {
			got := w.pathToURI(tt.filePath, pattern, mapping)
			if got != tt.wantURI {
				t.Errorf("pathToURI() = %q, want %q", got, tt.wantURI)
			}
		})
	}
}
