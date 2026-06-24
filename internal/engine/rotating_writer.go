// rotating_writer.go — size-capped, concurrency-safe io.Writer for kernel logs.
//
// The kernel slog JSON sink (see log_capture.go) was opened O_APPEND and left
// open for the process lifetime with no rotation, so kernel.log.jsonl grew
// without bound (observed at ~446 MB on a long-lived kernel). slog may call a
// handler's Handle — and therefore Write — from multiple goroutines, so the
// writer mutex-serializes every Write and the rotation it triggers.
//
// Rotation scheme: when a write would push the active file past maxBytes, the
// active file is renamed to "<path>.1", existing "<path>.N" backups shift up by
// one, and anything beyond maxBackups is deleted. "<path>.1" is always the
// newest backup. This mirrors the rename-and-prune convention already used by
// bus_session.go's events.jsonl rotation and config_write.go's rotateBackups,
// so we add no new dependency.
//
// IMPORTANT: this writer is itself the slog sink, so it must NEVER call slog
// (Warn/Info/etc.) — that would re-enter Write under the held mutex. Rotation
// faults are reported straight to os.Stderr.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingWriter is an io.WriteCloser that caps a file at maxBytes and keeps up
// to maxBackups rotated copies. Safe for concurrent Write/Close.
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
}

// newRotatingWriter opens (or creates, O_APPEND) path and returns a writer that
// rotates once the file would exceed maxBytes, retaining maxBackups copies.
// maxBytes <= 0 disables rotation (the writer becomes a plain appender);
// maxBackups < 1 is clamped to 1 so at least one rotated copy is kept.
func newRotatingWriter(path string, maxBytes int64, maxBackups int) (*rotatingWriter, error) {
	if maxBackups < 1 {
		maxBackups = 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, statErr := f.Stat(); statErr == nil {
		size = fi.Size()
	}
	return &rotatingWriter{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		f:          f,
		size:       size,
	}, nil
}

// Write implements io.Writer. It rotates before writing when the current file
// is non-empty and the incoming bytes would push it past maxBytes, so a single
// record never straddles a rotation boundary.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// Never drop the log line over a rotation fault — keep appending to
			// the current (possibly over-size) file. Report once to stderr;
			// must not use slog here (this writer IS the slog sink).
			fmt.Fprintf(os.Stderr, "kernel log: rotation failed, continuing on %s: %v\n", w.path, err)
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked renames the active file to "<path>.1", shifting older backups up
// and pruning beyond maxBackups, then opens a fresh active file. Caller holds mu.
func (w *rotatingWriter) rotateLocked() error {
	// Close the active handle so the rename releases it cleanly (matters on
	// Windows, where an open file cannot be renamed).
	_ = w.f.Close()

	// Shift existing backups upward: <path>.(N-1) -> <path>.N, dropping the
	// oldest. Descending order avoids clobbering a not-yet-moved backup.
	for i := w.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Remove(dst)
		_ = os.Rename(src, dst)
	}

	// Move the active file to the newest backup slot. If the rename fails the
	// file is left in place; reopening O_APPEND below means we resume on it
	// rather than losing the handle.
	newest := w.path + ".1"
	_ = os.Remove(newest)
	if err := os.Rename(w.path, newest); err != nil {
		f, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr != nil {
			return fmt.Errorf("rotate rename %s: %w (reopen also failed: %v)", w.path, err, openErr)
		}
		w.f = f
		if fi, statErr := f.Stat(); statErr == nil {
			w.size = fi.Size()
		}
		return fmt.Errorf("rotate rename %s: %w", w.path, err)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("rotate reopen %s: %w", w.path, err)
	}
	w.f = f
	w.size = 0
	return nil
}

// Close closes the active file handle. Production never calls this (the sink
// lives for the kernel lifetime); it exists for tests and graceful teardown.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
