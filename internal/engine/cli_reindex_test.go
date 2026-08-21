// cli_reindex_test.go — tests for the reindex command's corruption-safe
// store preservation (issue #571 item 2): nothing in the kernel may destroy
// a SQLite store it could not read.
//
// These exercise runReindex — the testable core of runReindexCmd extracted
// specifically so tests don't have to fork a subprocess or risk os.Exit
// bringing down the test binary on a bad-store path (see the doc comment on
// runReindex in cli_reindex.go).
package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrgic/cogos/sdk/constellation"
)

// writeGarbageAt overwrites the SQLite header region of an otherwise-real
// small db file with garbage bytes, the same corruption shape used by
// sdk/constellation's own store_guard_test.go.
func writeGarbageAt(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 32 {
		t.Fatalf("file too small to corrupt meaningfully: %d bytes", len(data))
	}
	for i := 16; i < 32; i++ {
		data[i] = 0xFF
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write corrupted %s: %v", path, err)
	}
}

// TestRunReindex_HealthyWorkspaceNoCorruptFile verifies the unchanged half
// of the spec: a workspace with no pre-existing store (or, per the SPEC
// comment on runReindex, a healthy one) reindexes normally with no
// .corrupt-* file appearing anywhere under .cog/.state.
func TestRunReindex_HealthyWorkspaceNoCorruptFile(t *testing.T) {
	root := t.TempDir()
	writeCogdoc(t, root, ".cog/mem/semantic/alpha.cog.md", "Alpha Doc", "content for alpha")

	var out bytes.Buffer
	err := runReindex(root, &out)
	skipIfNoFTS5(t, err)
	if err != nil {
		t.Fatalf("runReindex: %v", err)
	}

	if strings.Contains(out.String(), "WARNING") {
		t.Fatalf("expected no preservation warning for a healthy/no-store workspace, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "reindex complete") {
		t.Fatalf("expected completion message, got:\n%s", out.String())
	}

	stateDir := filepath.Join(root, ".cog", ".state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", stateDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			t.Fatalf("unexpected .corrupt-* file for a healthy workspace: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "constellation.db")); err != nil {
		t.Fatalf("expected constellation.db to exist after reindex: %v", err)
	}
}

// TestRunReindex_CorruptStorePreservedAndRebuilt drives the full spec
// through the actual CLI entry point (runReindex, called the same way
// runReindexCmd calls it): a corrupted constellation.db is renamed aside
// with the loud WARNING log lines, and a fresh working index is built in
// its place.
func TestRunReindex_CorruptStorePreservedAndRebuilt(t *testing.T) {
	root := t.TempDir()
	writeCogdoc(t, root, ".cog/mem/semantic/alpha.cog.md", "Alpha Doc", "content for alpha")

	dbPath := constellation.StorePath(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// Build a real small SQLite file directly (no fts5 needed) and corrupt
	// it, mirroring sdk/constellation/store_guard_test.go's fixture shape.
	c, err := constellation.Open(root)
	skipIfNoFTS5(t, err)
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	writeGarbageAt(t, dbPath)

	preCorrupt, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read corrupted fixture: %v", err)
	}

	var out bytes.Buffer
	if err := runReindex(root, &out); err != nil {
		t.Fatalf("runReindex after preservation should succeed (fts5 already proven available above): %v", err)
	}

	logged := out.String()
	if !strings.Contains(logged, "WARNING") {
		t.Fatalf("expected a loud WARNING for the preserved store, got:\n%s", logged)
	}
	if !strings.Contains(logged, dbPath) {
		t.Fatalf("expected the WARNING to name the failing store path %s, got:\n%s", dbPath, logged)
	}

	stateDir := filepath.Dir(dbPath)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var corruptFile string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			corruptFile = filepath.Join(stateDir, e.Name())
		}
	}
	if corruptFile == "" {
		t.Fatalf("expected a .corrupt-* file under %s, found none among: %v", stateDir, entries)
	}
	if !strings.Contains(logged, filepath.Base(corruptFile)) {
		t.Fatalf("expected the WARNING to name the preserved path %s, got:\n%s", corruptFile, logged)
	}

	gotPreserved, err := os.ReadFile(corruptFile)
	if err != nil {
		t.Fatalf("read preserved file: %v", err)
	}
	if !bytes.Equal(gotPreserved, preCorrupt) {
		t.Fatalf("preserved corpse is not byte-identical to the corrupted original")
	}

	// A fresh, working index must now exist at the original path.
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected a fresh constellation.db at %s: %v", dbPath, err)
	}
	c2, err := constellation.Open(root)
	if err != nil {
		t.Fatalf("fresh store should open cleanly: %v", err)
	}
	defer c2.Close()
	health, err := c2.Health()
	if err != nil {
		t.Fatalf("Health() on rebuilt store: %v", err)
	}
	if docs, _ := health["documents"].(int); docs < 1 {
		t.Fatalf("expected the rebuilt index to contain the alpha cogdoc, health=%v", health)
	}
}
