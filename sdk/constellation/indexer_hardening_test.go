package constellation

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestIndexFileConcurrentNoLoss is the regression test for the BLOCKER:
// concurrent IndexFile calls previously raced on the single-writer connection
// (one goroutine's SAVEPOINT in upsertFTSRow issued while another held an open
// db.Begin() transaction) and produced "cannot start a transaction within a
// transaction", a failure that was swallowed by a slog.Warn — silently dropping
// index updates. With indexMu serialising the whole operation, 20 goroutines
// hammering IndexFile must produce zero errors and full row coverage.
func TestIndexFileConcurrentNoLoss(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	const n = 20

	// Pre-create N distinct cogdocs so every goroutine indexes a real file.
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("semantic/hammer-%02d.cog.md", i)
		paths[i] = writeCogdocInWorkspace(t, c,
			rel,
			fmt.Sprintf("id: hammer-%02d\ntype: note\ntitle: Hammer %02d\ncreated: 2026-01-01", i, i),
			fmt.Sprintf("Concurrent index body number %02d with token hammertoken%02d.", i, i),
		)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			<-start // maximise contention: release all goroutines at once
			if err := c.IndexFile(p); err != nil {
				errCh <- fmt.Errorf("IndexFile(%s): %w", p, err)
			}
		}(paths[i])
	}

	close(start)
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected zero IndexFile errors from %d concurrent goroutines, got %d: %v", n, len(errs), errs)
	}

	// Full row coverage: every document row present, and every FTS row present.
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("hammer-%02d", i)
		var docCount, ftsCount int
		if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`, id).Scan(&docCount); err != nil {
			t.Fatalf("documents count for %s: %v", id, err)
		}
		if docCount != 1 {
			t.Errorf("expected exactly 1 documents row for %s, got %d", id, docCount)
		}
		if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents_fts WHERE id = ?`, id).Scan(&ftsCount); err != nil {
			t.Fatalf("fts count for %s: %v", id, err)
		}
		if ftsCount != 1 {
			t.Errorf("expected exactly 1 documents_fts row for %s, got %d (index update dropped)", id, ftsCount)
		}
	}

	// And each unique token must be searchable — the FTS row content is correct.
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("hammertoken%02d", i)
		var found int
		if err := c.DB().QueryRow(
			`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH ?`, token,
		).Scan(&found); err != nil {
			t.Fatalf("FTS search for %s: %v", token, err)
		}
		if found != 1 {
			t.Errorf("expected token %s searchable in exactly 1 doc, got %d", token, found)
		}
	}
}

// TestIndexFileConcurrentSamePath hammers IndexFile on a SINGLE shared path from
// many goroutines. This is the tightest form of the transaction-state race
// (every goroutine contends for the same row + FTS entry). It must produce zero
// errors and leave exactly one document row and one FTS row.
func TestIndexFileConcurrentSamePath(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	path := writeCogdocInWorkspace(t, c,
		"semantic/shared.cog.md",
		"id: shared-doc\ntype: note\ntitle: Shared\ncreated: 2026-01-01",
		"Shared body content with token sharedtoken.",
	)

	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := c.IndexFile(path); err != nil {
				errCh <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("IndexFile error on shared path: %v", err)
	}

	var docCount, ftsCount int
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents WHERE id = 'shared-doc'`).Scan(&docCount); err != nil {
		t.Fatalf("documents count: %v", err)
	}
	if docCount != 1 {
		t.Errorf("expected 1 documents row for shared-doc, got %d", docCount)
	}
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents_fts WHERE id = 'shared-doc'`).Scan(&ftsCount); err != nil {
		t.Fatalf("fts count: %v", err)
	}
	if ftsCount != 1 {
		t.Errorf("expected 1 documents_fts row for shared-doc, got %d", ftsCount)
	}
}

// TestIndexCogdocHashSkipRefreshesMtime is the regression test for the MAJOR
// hash-skip bug: indexCogdoc's early return on a content-hash match previously
// preceded the only file_mtime write, so a touched-but-unchanged file kept a
// stale stored mtime and was flagged as drifted by the engine's drift repair on
// every search, forever. After the fix, a hash-match path must still refresh the
// stored file_mtime in place.
func TestIndexCogdocHashSkipRefreshesMtime(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	path := writeCogdocInWorkspace(t, c,
		"semantic/touchme.cog.md",
		"id: touch-doc\ntype: note\ntitle: Touch Me\ncreated: 2026-01-01",
		"Unchanging body content.",
	)

	if err := c.IndexFile(path); err != nil {
		t.Fatalf("initial IndexFile: %v", err)
	}

	var mtime1 string
	if err := c.DB().QueryRow(`SELECT file_mtime FROM documents WHERE id = 'touch-doc'`).Scan(&mtime1); err != nil {
		t.Fatalf("read mtime1: %v", err)
	}

	// Touch the file: bump mtime forward by 5 minutes without changing content.
	future := time.Now().Add(5 * time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Re-index. Content hash matches, so indexCogdoc takes the early-return
	// branch — but it must now refresh file_mtime.
	if err := c.IndexFile(path); err != nil {
		t.Fatalf("re-IndexFile after touch: %v", err)
	}

	var mtime2 string
	if err := c.DB().QueryRow(`SELECT file_mtime FROM documents WHERE id = 'touch-doc'`).Scan(&mtime2); err != nil {
		t.Fatalf("read mtime2: %v", err)
	}
	if mtime2 == mtime1 {
		t.Fatalf("stored file_mtime not refreshed on touched-but-unchanged file: still %q", mtime2)
	}

	// The refreshed stored mtime must equal the on-disk mtime (no residual
	// drift): a second-pass drift check would report clean.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wantMtime := info.ModTime().Format(time.RFC3339)
	if mtime2 != wantMtime {
		t.Errorf("refreshed stored mtime %q != on-disk mtime %q (drift would persist)", mtime2, wantMtime)
	}
}

// TestIndexWorkspacePrunesGhostCogdocs is the regression test for the additive-
// only reindex path leaving permanent ghost rows: a cogdoc whose file is deleted
// from disk previously kept its documents/FTS/tags/refs rows forever. After the
// fix, IndexWorkspace (the reindex path) prunes rows for managed cogdocs
// (path LIKE '%/.cog/mem/%.cog.md') whose files are gone, while leaving rows for
// still-present files untouched.
func TestIndexWorkspacePrunesGhostCogdocs(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	keepPath := writeCogdocInWorkspace(t, c,
		"semantic/keep.cog.md",
		"id: keep-doc\ntype: note\ntitle: Keep\ncreated: 2026-01-01\ntags:\n  - keeptag",
		"Body that stays. Token keepstoken.",
	)
	_ = keepPath
	ghostPath := writeCogdocInWorkspace(t, c,
		"semantic/ghost.cog.md",
		"id: ghost-doc\ntype: note\ntitle: Ghost\ncreated: 2026-01-01\ntags:\n  - ghosttag",
		"Body that will vanish. Token ghoststoken.",
	)

	// First index: both docs present.
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("first IndexWorkspace: %v", err)
	}
	for _, id := range []string{"keep-doc", "ghost-doc"} {
		var n int
		if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if n != 1 {
			t.Fatalf("expected %s indexed after first pass, got %d", id, n)
		}
	}

	// Delete the ghost file on disk.
	if err := os.Remove(ghostPath); err != nil {
		t.Fatalf("remove ghost file: %v", err)
	}

	// Re-index: ghost row (and its cascaded tags) must be pruned; keep row stays.
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("second IndexWorkspace: %v", err)
	}

	var ghostDocs, ghostFTS, ghostTags int
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents WHERE id = 'ghost-doc'`).Scan(&ghostDocs); err != nil {
		t.Fatalf("ghost doc count: %v", err)
	}
	if ghostDocs != 0 {
		t.Errorf("expected ghost-doc pruned from documents, got %d rows", ghostDocs)
	}
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents_fts WHERE id = 'ghost-doc'`).Scan(&ghostFTS); err != nil {
		t.Fatalf("ghost fts count: %v", err)
	}
	if ghostFTS != 0 {
		t.Errorf("expected ghost-doc pruned from documents_fts, got %d rows", ghostFTS)
	}
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM tags WHERE document_id = 'ghost-doc'`).Scan(&ghostTags); err != nil {
		t.Fatalf("ghost tags count: %v", err)
	}
	if ghostTags != 0 {
		t.Errorf("expected ghost-doc tags cascade-deleted, got %d rows", ghostTags)
	}

	// The surviving doc must be untouched and still searchable.
	var keepDocs int
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents WHERE id = 'keep-doc'`).Scan(&keepDocs); err != nil {
		t.Fatalf("keep doc count: %v", err)
	}
	if keepDocs != 1 {
		t.Errorf("expected keep-doc to survive prune, got %d rows", keepDocs)
	}
	var keepFound int
	if err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH 'keepstoken'`,
	).Scan(&keepFound); err != nil {
		t.Fatalf("keep search: %v", err)
	}
	if keepFound != 1 {
		t.Errorf("expected keep-doc still searchable after prune, got %d", keepFound)
	}
}
