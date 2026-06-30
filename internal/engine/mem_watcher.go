// mem_watcher.go — real-time FTS currency watcher for the .cog/mem/ corpus.
//
// macOS kqueue (and Windows ReadDirectoryChangesW) is non-recursive: adding a
// parent directory to an fsnotify watcher does NOT automatically watch newly
// created subdirectories.  When a caller creates a new memory sector directory
// (e.g. .cog/mem/semantic/newkind/) none of its files will be indexed until
// the daemon restarts or a reindex is triggered.
//
// MemWatcher fixes this by:
//
//  1. Walking .cog/mem/ at startup and adding every existing subdirectory to
//     the fsnotify watcher.
//  2. On a Create event whose target is a directory, immediately calling
//     watcher.Add on the new directory so its descendants are watched.
//  3. On a Create or Write event whose target is a *.cog.md file, calling
//     indexer.IndexFile after a 500ms debounce window (coalesces rapid saves).
//
// The watcher is started from Boot() after WireConstellationIndexer runs.
// When pkgFTSRepairIndexer is nil (tests, CLI) the watcher is not started.
package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const memWatcherDebounce = 500 * time.Millisecond

// MemWatcher watches .cog/mem/ for new subdirectories and .cog.md writes,
// forwarding index updates to the wired ConstellationIndexer.
type MemWatcher struct {
	memDir  string
	indexer ConstellationIndexer
	stopCh  chan struct{}

	mu      sync.Mutex
	running bool
}

// NewMemWatcher creates a watcher for the given memory directory.
// indexer must be non-nil.
func NewMemWatcher(memDir string, indexer ConstellationIndexer) *MemWatcher {
	return &MemWatcher{
		memDir:  memDir,
		indexer: indexer,
		stopCh:  make(chan struct{}),
	}
}

// Start begins watching memDir.  Non-blocking: the event loop runs in a
// goroutine.  Returns an error if the directory does not exist or fsnotify
// cannot be initialised.  Idempotent: calling Start on an already-running
// watcher returns an error.
func (w *MemWatcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return nil
	}

	if _, err := os.Stat(w.memDir); err != nil {
		return err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Add the root mem dir.
	if err := fsw.Add(w.memDir); err != nil {
		fsw.Close()
		return err
	}

	// Walk existing subdirectories and add each one so the watcher is
	// immediately current for all existing sector directories.
	_ = filepath.WalkDir(w.memDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == w.memDir {
			return nil
		}
		if addErr := fsw.Add(path); addErr != nil {
			slog.Debug("mem_watcher: could not add existing dir", "path", path, "err", addErr)
		}
		return nil
	})

	w.running = true
	go w.loop(fsw)
	return nil
}

// Stop halts the event loop and releases fsnotify resources.
func (w *MemWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	close(w.stopCh)
	w.running = false
}

// loop is the main event loop.  Each file-system event is classified:
//   - directory Create → add new dir to the watcher immediately (no debounce).
//   - *.cog.md Create/Write → enqueue path for debounced IndexFile call.
//
// The debounce map coalesces rapid writes within a 500ms window so that a
// fast sequence of saves (e.g. editor write + rename swap) results in a single
// IndexFile call rather than N redundant upserts.
func (w *MemWatcher) loop(fsw *fsnotify.Watcher) {
	defer fsw.Close()

	// debounce tracks paths queued for indexing: path → timer.
	debounce := make(map[string]*time.Timer)
	var mu sync.Mutex // guards debounce map

	for {
		select {
		case <-w.stopCh:
			mu.Lock()
			for _, t := range debounce {
				t.Stop()
			}
			mu.Unlock()
			return

		case event, ok := <-fsw.Events:
			if !ok {
				return
			}

			path := event.Name

			switch {
			// ── New directory: add to watcher immediately ────────────────────
			case event.Has(fsnotify.Create):
				info, err := os.Lstat(path)
				if err != nil {
					break
				}
				if info.IsDir() {
					if addErr := fsw.Add(path); addErr != nil {
						slog.Debug("mem_watcher: failed to add new dir",
							"path", path, "err", addErr)
					} else {
						slog.Debug("mem_watcher: added new sector dir", "path", path)
					}
					// Also index any .cog.md file if the Create event is for a file.
					break
				}
				// File create — fall through to the *.cog.md check below.
				if !strings.HasSuffix(path, ".cog.md") {
					break
				}
				w.scheduleIndex(path, debounce, &mu)

			// ── File write: debounced index call ─────────────────────────────
			case event.Has(fsnotify.Write):
				if !strings.HasSuffix(path, ".cog.md") {
					break
				}
				w.scheduleIndex(path, debounce, &mu)
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			slog.Debug("mem_watcher: fsnotify error", "err", err)
		}
	}
}

// scheduleIndex enqueues path for a debounced IndexFile call.  If the path is
// already queued the existing timer is reset, coalescing rapid writes into a
// single upsert.
func (w *MemWatcher) scheduleIndex(path string, debounce map[string]*time.Timer, mu *sync.Mutex) {
	mu.Lock()
	if t, ok := debounce[path]; ok {
		t.Stop()
	}
	debounce[path] = time.AfterFunc(memWatcherDebounce, func() {
		mu.Lock()
		delete(debounce, path)
		mu.Unlock()

		if err := w.indexer.IndexFile(path); err != nil {
			slog.Warn("mem_watcher: IndexFile failed", "path", path, "err", err)
		} else {
			slog.Debug("mem_watcher: indexed", "path", path)
		}
	})
	mu.Unlock()
}
