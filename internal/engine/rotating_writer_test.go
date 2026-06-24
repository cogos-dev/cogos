// rotating_writer_test.go — size-rotation + concurrency for the kernel log sink.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
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
