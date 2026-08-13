package constellation

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempCogdoc creates a temporary cogdoc file with the given frontmatter and body.
// Returns the file path.
func writeTempCogdoc(t *testing.T, frontmatter, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.cog.md")
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp cogdoc: %v", err)
	}
	return path
}

// TestParseCogdocMemorySectorAlias verifies that memory_sector maps to Sector (CRITICAL-1).
func TestParseCogdocMemorySectorAlias(t *testing.T) {
	path := writeTempCogdoc(t,
		"memory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note",
		"body content",
	)
	doc, err := parseCogdoc([]byte(
		"---\nmemory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note\n---\n\nbody content",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Sector != "episodic" {
		t.Errorf("expected Sector=episodic from memory_sector alias, got %q", doc.Sector)
	}
}

// TestParseCogdocMemorySectorNoOverride verifies that explicit sector wins over memory_sector.
func TestParseCogdocMemorySectorNoOverride(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\nsector: semantic\nmemory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Sector != "semantic" {
		t.Errorf("expected explicit sector=semantic to win over memory_sector, got %q", doc.Sector)
	}
}

// TestParseCogdocCogSubmap verifies cog.type and cog.id lift to top-level (CRITICAL-2).
func TestParseCogdocCogSubmap(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ncog:\n  type: rfc\n  id: RFC-030\ntitle: Identity Contract\ncreated: 2026-04-30\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Type != "rfc" {
		t.Errorf("expected Type=rfc from cog.type, got %q", doc.Type)
	}
	if doc.ID != "RFC-030" {
		t.Errorf("expected ID=RFC-030 from cog.id, got %q", doc.ID)
	}
}

// TestParseCogdocCogSubmapNoOverride verifies top-level type wins over cog submap.
func TestParseCogdocCogSubmapNoOverride(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ntype: spec\nid: my-spec\ncog:\n  type: rfc\n  id: RFC-999\ntitle: Test\ncreated: 2026-04-30\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Type != "spec" {
		t.Errorf("expected top-level type=spec to win over cog.type, got %q", doc.Type)
	}
	if doc.ID != "my-spec" {
		t.Errorf("expected top-level id=my-spec to win over cog.id, got %q", doc.ID)
	}
}

// TestParseCogdocStatusLowercase verifies status is lowercased at parse time (CRITICAL-3).
func TestParseCogdocStatusLowercase(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"Active", "active"},
		{"DRAFT", "draft"},
		{"Canonical", "canonical"},
		{"active", "active"},
		{"", ""},
	}
	for _, tc := range cases {
		path := writeTempCogdoc(t, "", "")
		doc, err := parseCogdoc([]byte(
			"---\ntype: note\ntitle: Test\ncreated: 2026-01-01\nstatus: "+tc.input+"\n---\n\nbody",
		), path, "")
		if err != nil {
			t.Fatalf("parseCogdoc failed for status=%q: %v", tc.input, err)
		}
		if doc.Status != tc.expected {
			t.Errorf("status %q: expected %q after lowercase, got %q", tc.input, tc.expected, doc.Status)
		}
	}
}

// TestParseCogdocUpdatedAliases verifies modified and revised alias for updated (CRITICAL-1).
func TestParseCogdocUpdatedAliases(t *testing.T) {
	path := writeTempCogdoc(t, "", "")

	// modified alias
	doc, err := parseCogdoc([]byte(
		"---\ntype: note\ntitle: T\ncreated: 2026-01-01\nmodified: 2026-04-30\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Updated != "2026-04-30" {
		t.Errorf("expected Updated=2026-04-30 from modified alias, got %q", doc.Updated)
	}

	// revised alias
	doc, err = parseCogdoc([]byte(
		"---\ntype: note\ntitle: T\ncreated: 2026-01-01\nrevised: 2026-05-01\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Updated != "2026-05-01" {
		t.Errorf("expected Updated=2026-05-01 from revised alias, got %q", doc.Updated)
	}
}

// TestParseCogdocCanonicalFields verifies salience, confidence, ingested are parsed (PREREQ-1).
func TestParseCogdocCanonicalFields(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ntype: insight\ntitle: Test\ncreated: 2026-01-01\nsalience: high\nconfidence: empirical\ningested: 2026-05-01T00:00:00Z\n---\n\nbody",
	), path, "")
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Salience != "high" {
		t.Errorf("expected Salience=high, got %q", doc.Salience)
	}
	if doc.Confidence != "empirical" {
		t.Errorf("expected Confidence=empirical, got %q", doc.Confidence)
	}
	if doc.Ingested != "2026-05-01T00:00:00Z" {
		t.Errorf("expected Ingested=2026-05-01T00:00:00Z, got %q", doc.Ingested)
	}
}

// TestResolveURIMemoryNormalization verifies cog://memory/ → cog://mem/ normalization (PREREQ-2).
// We test indirectly by checking that both URIs resolve the same way via the function.
func TestResolveURIMemoryNormalization(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	tx, err := c.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	// Insert a test document at a mem path
	_, err = tx.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('test-doc', '/workspace/.cog/mem/semantic/test.cog.md', 'note', 'Test', '2026-01-01', 'body', 'hash', '2026-01-01', '2026-01-01')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Resolve via cog://memory/ (should normalize to cog://mem/)
	result := resolveURIToID(tx, "cog://memory/semantic/test")
	if !result.Valid {
		t.Error("expected cog://memory/semantic/test to resolve after normalization, got null")
	}

	// Also test cog://mem/ directly
	result2 := resolveURIToID(tx, "cog://mem/semantic/test")
	if !result2.Valid {
		t.Error("expected cog://mem/semantic/test to resolve, got null")
	}
}

// TestResolveURIRFCScheme verifies cog://rfc/NNN resolves via glob (PREREQ-2).
func TestResolveURIRFCScheme(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	tx, err := c.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	// Insert RFC-030 at its canonical path
	_, err = tx.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('RFC-030', '/workspace/.cog/conf/spec/rfc/RFC-030-kernel-issued-cogdoc-identity-contract.cog.md',
		        'rfc', 'RFC-030', '2026-04-30', 'body', 'hash', '2026-04-30', '2026-04-30')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	cases := []struct {
		uri    string
		wantID string
		wantOK bool
	}{
		{"cog://rfc/030", "RFC-030", true},
		{"cog://rfc/30", "RFC-030", true}, // zero-padded normalization
		{"cog://rfc/999", "", false},      // nonexistent RFC
	}

	for _, tc := range cases {
		result := resolveURIToID(tx, tc.uri)
		if result.Valid != tc.wantOK {
			t.Errorf("uri %q: valid=%v want %v", tc.uri, result.Valid, tc.wantOK)
			continue
		}
		if tc.wantOK && result.String != tc.wantID {
			t.Errorf("uri %q: id=%q want %q", tc.uri, result.String, tc.wantID)
		}
	}
}

// TestIndexWorkspacePreservesOutOfWorkspaceRows verifies that IndexWorkspace does NOT
// delete rows whose path falls outside the current workspace root. The documents table
// legitimately holds out-of-workspace rows (e.g. conversation/session documents indexed
// from ~/.claude), so the earlier blanket "DELETE WHERE path NOT LIKE <root>/%" purge was
// unsafe and has been removed. This test locks in the corrected invariant.
func TestIndexWorkspacePreservesOutOfWorkspaceRows(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	// Insert a legitimately-indexed out-of-workspace row (e.g. a claude-code session).
	_, err := c.db.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('session:abc', '/Users/someone/.claude/projects/p/abc.jsonl',
		        'session', 'Session', '2025-01-01', 'body', 'hash', '2025-01-01', '2025-01-01')`)
	if err != nil {
		t.Fatalf("insert out-of-workspace doc: %v", err)
	}

	// Create the .cog directory structure required for WalkDir.
	cogDir := filepath.Join(c.root, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Run a full index over an (otherwise empty) workspace.
	_ = c.IndexWorkspace() // errors from empty .cog dir are acceptable

	// The out-of-workspace row must survive — IndexWorkspace must not purge it.
	var after int
	c.db.QueryRow("SELECT COUNT(*) FROM documents WHERE id = 'session:abc'").Scan(&after)
	if after != 1 {
		t.Errorf("out-of-workspace row was deleted by IndexWorkspace; want preserved, got count=%d", after)
	}
}

// TestNewColumnsExistAfterMigration verifies ingested, salience, confidence, display_number columns (PREREQ-1).
func TestNewColumnsExistAfterMigration(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	columns := []string{"ingested", "salience", "confidence", "display_number"}
	for _, col := range columns {
		var count int
		err := c.DB().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('documents') WHERE name = ?`, col,
		).Scan(&count)
		if err != nil {
			t.Fatalf("pragma check for %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("expected column %q to exist after migration, but it's absent", col)
		}
	}
}

// TestMigrationConflictsTableExists verifies migration_conflicts table was created (CRITICAL-6).
func TestMigrationConflictsTableExists(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	var count int
	err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_conflicts'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("sqlite_master check: %v", err)
	}
	if count == 0 {
		t.Error("expected migration_conflicts table to exist")
	}
}

// writeCogdocInWorkspace writes a *.cog.md file under a temp workspace's .cog/mem/ tree
// and returns the absolute path.
func writeCogdocInWorkspace(t *testing.T, c *Constellation, relPath, frontmatter, body string) string {
	t.Helper()
	// c.root is the workspace root used by openTestDB
	absDir := filepath.Join(c.root, ".cog", "mem", filepath.Dir(relPath))
	if err := os.MkdirAll(absDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", absDir, err)
	}
	absPath := filepath.Join(c.root, ".cog", "mem", relPath)
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		t.Fatalf("write cogdoc %s: %v", absPath, err)
	}
	return absPath
}

// TestFTSIndexFileSearchable verifies that IndexFile makes a new document
// immediately searchable via FTS5 without a full rebuildFTS.
func TestFTSIndexFileSearchable(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	path := writeCogdocInWorkspace(t, c,
		"semantic/alpha.cog.md",
		"id: alpha-doc\ntype: note\ntitle: Alpha Searchable\ncreated: 2026-01-01\ntags:\n  - fts-test-alpha",
		"This document contains the word quuxbaz for FTS testing.",
	)

	if err := c.IndexFile(path); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	// FTS5 query for the unique token
	var found int
	err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH 'quuxbaz'`,
	).Scan(&found)
	if err != nil {
		t.Fatalf("FTS query: %v", err)
	}
	if found == 0 {
		t.Error("expected IndexFile to make document searchable via FTS, got 0 matches")
	}
}

// TestFTSIndexFileIdempotent verifies that calling IndexFile twice on an
// unchanged file does not duplicate FTS rows.
func TestFTSIndexFileIdempotent(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	path := writeCogdocInWorkspace(t, c,
		"semantic/beta.cog.md",
		"id: beta-doc\ntype: note\ntitle: Beta Idempotent\ncreated: 2026-01-01",
		"Idempotent body content.",
	)

	if err := c.IndexFile(path); err != nil {
		t.Fatalf("IndexFile first call: %v", err)
	}
	if err := c.IndexFile(path); err != nil {
		t.Fatalf("IndexFile second call: %v", err)
	}

	var count int
	err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE id = 'beta-doc'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("FTS count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 FTS row after two IndexFile calls, got %d", count)
	}
}

// TestIndexWorkspaceToleratesMalformedFrontmatter verifies that IndexWorkspace
// exits without error when one cogdoc has unparseable YAML frontmatter but the
// remaining docs are valid. The valid docs must be indexed and searchable; the
// malformed doc is skipped with a warning. This is a regression test for the
// bug where a single bad-frontmatter doc caused cogos reindex to exit non-zero
// even after indexing thousands of other docs successfully.
func TestIndexWorkspaceToleratesMalformedFrontmatter(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	// Write two valid cogdocs.
	writeCogdocInWorkspace(t, c,
		"semantic/good-alpha.cog.md",
		"id: good-alpha\ntype: note\ntitle: Good Alpha\ncreated: 2026-01-01",
		"This document has a unique token: xyzzy42.",
	)
	writeCogdocInWorkspace(t, c,
		"semantic/good-beta.cog.md",
		"id: good-beta\ntype: note\ntitle: Good Beta\ncreated: 2026-01-01",
		"Another valid document with token: plugh77.",
	)

	// Write one cogdoc with malformed YAML frontmatter (mapping value in a
	// scalar context — the same failure class observed in production).
	malformedFM := "id: bad-doc\ntitle: Bad: value: with colons: everywhere\ntype: note"
	writeCogdocInWorkspace(t, c,
		"semantic/malformed.cog.md",
		malformedFM,
		"This document will be skipped.",
	)

	// IndexWorkspace must return nil — per-doc parse failures are warnings.
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("IndexWorkspace returned error on malformed doc: %v", err)
	}

	// Both valid docs must be indexed.
	var docCount int
	if err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM documents WHERE id IN ('good-alpha', 'good-beta')`,
	).Scan(&docCount); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if docCount != 2 {
		t.Errorf("expected 2 valid docs indexed, got %d", docCount)
	}

	// Valid docs must be FTS-searchable.
	var ftsCount int
	if err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH 'xyzzy42'`,
	).Scan(&ftsCount); err != nil {
		t.Fatalf("FTS query: %v", err)
	}
	if ftsCount == 0 {
		t.Error("expected good-alpha to be FTS-searchable after IndexWorkspace, got 0 matches")
	}
}

// TestFTSIndexFileChangedContent verifies that after content changes and a
// second IndexFile call, the FTS reflects the new content (not the old).
func TestFTSIndexFileChangedContent(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	path := writeCogdocInWorkspace(t, c,
		"semantic/gamma.cog.md",
		"id: gamma-doc\ntype: note\ntitle: Gamma Changed\ncreated: 2026-01-01",
		"Original content with token zorgblat.",
	)
	if err := c.IndexFile(path); err != nil {
		t.Fatalf("IndexFile v1: %v", err)
	}

	// Overwrite with new content
	newContent := "---\nid: gamma-doc\ntype: note\ntitle: Gamma Changed\ncreated: 2026-01-01\n---\n\nUpdated content with token wumblefritz."
	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := c.IndexFile(path); err != nil {
		t.Fatalf("IndexFile v2: %v", err)
	}

	// Old token must be gone
	var oldCount int
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH 'zorgblat'`).Scan(&oldCount); err != nil {
		t.Fatalf("old token query: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("expected old token 'zorgblat' to be absent after update, got %d", oldCount)
	}

	// New token must be present
	var newCount int
	if err := c.DB().QueryRow(`SELECT COUNT(*) FROM documents_fts WHERE documents_fts MATCH 'wumblefritz'`).Scan(&newCount); err != nil {
		t.Fatalf("new token query: %v", err)
	}
	if newCount == 0 {
		t.Error("expected new token 'wumblefritz' to appear after update, got 0")
	}
}
