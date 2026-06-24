// rotating_writer_test.go — size-rotation + concurrency for the kernel log sink.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingWriterRotatesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kernel.log.jsonl")

	// 100-byte cap, keep 2 backups. Each Write is 60 bytes, so the 2nd write
	// (60+60 > 100) rotates, and so on.
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	line := make([]byte, 60)
	for i := range line {
		line[i] = 'x'
	}
	for i := 0; i < 6; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Active file must be at or below the cap (last write started a fresh file).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if fi.Size() > 100 {
		t.Errorf("active file %d bytes, want <= 100", fi.Size())
	}

	// At most maxBackups rotated copies survive; .3 must have been pruned.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected backup .1 to exist: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf("expected backup .2 to exist: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("backup .3 should have been pruned, stat err=%v", err)
	}
}

func TestRotatingWriterDisabledWhenMaxBytesZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.log")
	w, err := newRotatingWriter(path, 0, 3) // 0 disables rotation
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	for i := 0; i < 50; i++ {
		if _, err := w.Write([]byte("some log line\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotation must be disabled, but .1 exists")
	}
}

// TestRotatingWriterConcurrent exercises the mutex under -race: slog may call
// Handle (and therefore Write) from multiple goroutines.
func TestRotatingWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.log")
	w, err := newRotatingWriter(path, 4096, 3)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = w.Write([]byte(fmt.Sprintf("g=%d i=%d payload-padding-padding\n", g, i)))
			}
		}(g)
	}
	wg.Wait()
}

// TestRotatingWriterWriteAfterClose covers the Write self-heal path: if the
// active handle is gone (here via Close, but in production via a failed
// post-rotation reopen that leaves w.f nil), Write must reopen rather than
// panic on a nil/closed handle or silently drop the record.
func TestRotatingWriterWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.log")
	w, err := newRotatingWriter(path, 1<<30, 2) // rotation effectively off
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if err := w.Close(); err != nil { // w.f -> nil
		t.Fatalf("Close: %v", err)
	}

	n, err := w.Write([]byte("recovered\n"))
	if err != nil {
		t.Fatalf("Write after Close: %v", err)
	}
	if n == 0 {
		t.Fatal("Write reported 0 bytes after self-heal")
	}
	_ = w.Close()
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "recovered") {
		t.Errorf("self-healed write not persisted; file=%q", string(b))
	}
}

// TestRotatingWriterRotateWithNilHandle reproduces the post-failed-rotation
// state — handle closed, w.f nil, size over the cap — and asserts the next
// Write neither panics on the nil w.f.Close() in rotateLocked nor drops the
// record. This is the regression for the closed-handle bug.
func TestRotatingWriterRotateWithNilHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.log")
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}

	// Force the exact state a failed reopen leaves behind.
	w.mu.Lock()
	_ = w.f.Close()
	w.f = nil
	w.size = 1000 // > maxBytes, so the next Write triggers rotateLocked
	w.mu.Unlock()

	n, err := w.Write([]byte("after-nil\n")) // must not panic on nil w.f.Close()
	if err != nil {
		t.Fatalf("Write with nil handle: %v", err)
	}
	if n == 0 {
		t.Fatal("Write reported 0 bytes")
	}
	_ = w.Close()

	found := false
	for _, p := range []string{path, path + ".1"} {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "after-nil") {
			found = true
		}
	}
	if !found {
		t.Error("write after nil handle not found on disk")
	}
}
