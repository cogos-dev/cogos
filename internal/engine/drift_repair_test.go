// drift_repair_test.go — first-ever coverage for searchMemoryFTSDriftRepair,
// the lazy FTS drift-repair path in mcp_stubs.go. Exercises:
//   - scope: only rows under .cog/mem/ are sampled (symmetric with mem_watcher);
//     a drifted row outside the corpus is NOT repaired.
//   - mtime drift: a row whose on-disk mtime differs from the stored mtime is
//     repaired (IndexFile called).
//   - equal-second content-hash tiebreak: a row whose mtime string matches but
//     whose content changed within the same wall-clock second is repaired.
//   - equal-second, unchanged content: no repair (no false positive).
//   - widespread drift (> threshold): logs and skips inline repair.
//   - no indexer wired: silent no-op.
package engine

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// recordingIndexer implements ConstellationIndexer and records the paths passed
// to IndexFile so tests can assert exactly which rows were repaired.
type recordingIndexer struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingIndexer) IndexFile(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, path)
	return nil
}

func (r *recordingIndexer) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// withRepairIndexer swaps the package-level pkgFTSRepairIndexer for the duration
// of a test and restores it afterward. Tests using this must NOT run in parallel
// (the var is package-global).
func withRepairIndexer(t *testing.T, idx ConstellationIndexer) {
	t.Helper()
	prev := pkgFTSRepairIndexer
	pkgFTSRepairIndexer = idx
	t.Cleanup(func() { pkgFTSRepairIndexer = prev })
}

// driftFixture builds a workspace with a real constellation.db at the daemon's
// expected path and returns the workspace root. Documents are seeded via the
// provided callback so each test controls its own rows.
func driftFixture(t *testing.T, seed func(db *sql.DB, root string)) string {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".cog", ".state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	dbPath := filepath.Join(stateDir, "constellation.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	schema := `
CREATE TABLE documents (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    created TEXT NOT NULL,
    updated TEXT,
    sector TEXT,
    status TEXT,
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    word_count INTEGER,
    line_count INTEGER,
    indexed_at TEXT NOT NULL,
    file_mtime TEXT NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	seed(db, root)
	return root
}

// writeMemCogdoc writes a cogdoc under root/.cog/mem/<rel> with the given body
// and returns the absolute path. Frontmatter is minimal but real so the
// content-hash tiebreak (which strips frontmatter) computes correctly.
func writeMemCogdoc(t *testing.T, root, rel, body string) string {
	t.Helper()
	abs := filepath.Join(root, ".cog", "mem", rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nid: x\ntype: note\ntitle: X\ncreated: 2026-01-01\n---\n\n" + body
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return abs
}

// bodyHash mirrors the indexer's content-hash rule for a raw file's body.
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return fmt.Sprintf("%x", sum)
}

func insertDoc(t *testing.T, db *sql.DB, id, path, contentHash, fileMtime string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		 VALUES (?, ?, 'note', 'X', '2026-01-01', 'x', ?, '2026-01-01T00:00:00Z', ?)`,
		id, path, contentHash, fileMtime,
	)
	if err != nil {
		t.Fatalf("insert doc %s: %v", id, err)
	}
}

// TestDriftRepair_MtimeDrift: a mem cogdoc whose on-disk mtime differs from the
// stored mtime is repaired.
func TestDriftRepair_MtimeDrift(t *testing.T) {
	var absPath string
	root := driftFixture(t, func(db *sql.DB, root string) {
		absPath = writeMemCogdoc(t, root, "semantic/drifted.cog.md", "body content")
		// Stored mtime deliberately in the past → differs from the fresh file.
		insertDoc(t, db, "drifted", absPath, bodyHash("body content"), "2020-01-01T00:00:00Z")
	})

	idx := &recordingIndexer{}
	withRepairIndexer(t, idx)

	searchMemoryFTSDriftRepair(root)

	calls := idx.got()
	if len(calls) != 1 || calls[0] != absPath {
		t.Fatalf("expected exactly one repair of %s, got %v", absPath, calls)
	}
}

// TestDriftRepair_ScopedToMemCorpus: a drifted row OUTSIDE .cog/mem/ must NOT be
// sampled or repaired (symmetric with the mem_watcher scope).
func TestDriftRepair_ScopedToMemCorpus(t *testing.T) {
	root := driftFixture(t, func(db *sql.DB, root string) {
		// A real file outside the mem corpus, with a stale stored mtime.
		outsideDir := filepath.Join(root, ".cog", "sessions")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			t.Fatalf("mkdir sessions: %v", err)
		}
		outside := filepath.Join(outsideDir, "conv.cog.md")
		if err := os.WriteFile(outside, []byte("session body"), 0o644); err != nil {
			t.Fatalf("write outside: %v", err)
		}
		insertDoc(t, db, "conv", outside, "hash", "2020-01-01T00:00:00Z")
	})

	idx := &recordingIndexer{}
	withRepairIndexer(t, idx)

	searchMemoryFTSDriftRepair(root)

	if calls := idx.got(); len(calls) != 0 {
		t.Fatalf("expected no repair for out-of-corpus row, got %v", calls)
	}
}

// TestDriftRepair_EqualSecondContentChanged: mtime string matches (same second)
// but content changed → content-hash tiebreak must detect drift and repair.
func TestDriftRepair_EqualSecondContentChanged(t *testing.T) {
	var absPath string
	root := driftFixture(t, func(db *sql.DB, root string) {
		absPath = writeMemCogdoc(t, root, "semantic/samesecond.cog.md", "NEW content now")
		info, err := os.Stat(absPath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// Store the file's actual current mtime (so mtime strings match), but an
		// OLD content hash (content changed within the same second).
		storedMtime := info.ModTime().Format(time.RFC3339)
		insertDoc(t, db, "samesecond", absPath, bodyHash("OLD content before"), storedMtime)
	})

	idx := &recordingIndexer{}
	withRepairIndexer(t, idx)

	searchMemoryFTSDriftRepair(root)

	calls := idx.got()
	if len(calls) != 1 || calls[0] != absPath {
		t.Fatalf("expected content-hash tiebreak to repair %s, got %v", absPath, calls)
	}
}

// TestDriftRepair_EqualSecondContentUnchanged: mtime string matches AND content
// hash matches → no repair (no false positive from the tiebreak).
func TestDriftRepair_EqualSecondContentUnchanged(t *testing.T) {
	root := driftFixture(t, func(db *sql.DB, root string) {
		abs := writeMemCogdoc(t, root, "semantic/clean.cog.md", "stable body")
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		storedMtime := info.ModTime().Format(time.RFC3339)
		insertDoc(t, db, "clean", abs, bodyHash("stable body"), storedMtime)
	})

	idx := &recordingIndexer{}
	withRepairIndexer(t, idx)

	searchMemoryFTSDriftRepair(root)

	if calls := idx.got(); len(calls) != 0 {
		t.Fatalf("expected no repair for unchanged doc, got %v", calls)
	}
}

// TestDriftRepair_WidespreadSkipsInline: more than the wide threshold of drifted
// rows → the function logs and returns without repairing the bulk.
func TestDriftRepair_WidespreadSkipsInline(t *testing.T) {
	root := driftFixture(t, func(db *sql.DB, root string) {
		for i := 0; i < 15; i++ { // > wideThresh (10)
			rel := fmt.Sprintf("semantic/wide-%02d.cog.md", i)
			abs := writeMemCogdoc(t, root, rel, fmt.Sprintf("body %d", i))
			insertDoc(t, db, fmt.Sprintf("wide-%02d", i), abs, bodyHash("stale"), "2020-01-01T00:00:00Z")
		}
	})

	idx := &recordingIndexer{}
	withRepairIndexer(t, idx)

	searchMemoryFTSDriftRepair(root)

	if calls := idx.got(); len(calls) != 0 {
		t.Fatalf("expected no inline repair when drift is widespread, got %d calls", len(calls))
	}
}

// TestDriftRepair_NoIndexerIsNoOp: with no wired indexer the function returns
// silently and does not panic even when the DB is absent.
func TestDriftRepair_NoIndexerIsNoOp(t *testing.T) {
	withRepairIndexer(t, nil)
	// A workspace with no DB at all.
	searchMemoryFTSDriftRepair(t.TempDir())
}
