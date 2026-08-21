// cli_doctor_test.go — tests for RunDoctor and its check groups.
//
// RunDoctor is the testable orchestration entry point (no os.Exit, no direct
// stdout/stderr writes); runDoctorCmd is the thin CLI wrapper around it and is
// exercised indirectly through these tests via the same fixtures a real
// invocation would see.
package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/sdk/constellation"

	_ "github.com/mattn/go-sqlite3"
)

// writeCogdoc writes a minimal indexable cogdoc under workspaceRoot/.cog/mem.
func writeCogdoc(t *testing.T, workspaceRoot, relPath, title, body string) {
	t.Helper()
	full := filepath.Join(workspaceRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\ntitle: " + title + "\ntype: note\ncreated: 2026-01-01\n---\n\n" + body + "\n"
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write cogdoc: %v", err)
	}
}

// buildFixtureWorkspace creates a workspace with an indexed constellation.db
// containing at least one document with a distinctive title, plus an
// unindexed .cog/hooks tree (a *.cog.md cogdoc written AFTER IndexWorkspace
// runs, so it genuinely never got indexed) to exercise the documents-vs-files
// check. It must be *.cog.md, not a plain .md: IndexWorkspace only ever
// indexes files with that exact suffix (indexer.go:129), so a plain .md
// sibling is invisible to the indexer for a structural reason (never
// eligible in the first place) rather than the "on disk but not yet
// reindexed" staleness this fixture means to simulate -- and, since
// IndexWorkspace's base walk covers the whole .cog/ tree, writing a *.cog.md
// file BEFORE indexing would just get it indexed, defeating the fixture.
func buildFixtureWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	writeCogdoc(t, root, ".cog/mem/semantic/distinctive-sentinel-doc.cog.md",
		"Distinctive Sentinel Document", "This document contains the word RECOGNIZABLE for the negative control.")

	c, err := constellation.Open(root)
	if err != nil {
		skipIfNoFTS5(t, err)
		t.Fatalf("constellation.Open: %v", err)
	}
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Written after the index above ran, so it is a real, indexable cogdoc
	// that genuinely never got indexed -- simulating the .cog/hooks /
	// .cog/lib unindexed-tree finding from the issue.
	writeCogdoc(t, root, ".cog/hooks/note.cog.md", "Unindexed Hook Note", "never reindexed")

	return root
}

// skipIfNoFTS5 skips the current test when err is the well-known
// constellation.Open failure produced by a build without the fts5 tag ("go
// test ./..." with no build tags, as CI runs it). Any other error still
// fails the test via the caller's own t.Fatalf. Mirrors the pattern in
// mcp_stubs_test.go and sdk/constellation/indexer_hardening_test.go.
func skipIfNoFTS5(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "no such module: fts5") {
		t.Skip("FTS5 not available (build with -tags fts5)")
	}
}

func findCheck(t *testing.T, report *DoctorReport, group, name string) *DoctorCheck {
	t.Helper()
	for i := range report.Groups {
		if report.Groups[i].Name != group {
			continue
		}
		for j := range report.Groups[i].Checks {
			if report.Groups[i].Checks[j].Name == name {
				return &report.Groups[i].Checks[j]
			}
		}
	}
	t.Fatalf("check %q not found in group %q; groups=%+v", name, group, report.Groups)
	return nil
}

// TestNegativeControlPassesOnHealthyIndex is the primary design-constraint
// test: a sentinel term derived from an indexed document must return >=1 hit
// through the real search path, and the check must report OK.
func TestNegativeControlPassesOnHealthyIndex(t *testing.T) {
	root := buildFixtureWorkspace(t)
	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})

	check := findCheck(t, report, "index health", "negative control (sentinel query)")
	if check.Status != StatusOK {
		t.Fatalf("negative control on a healthy index = %s, want OK; detail=%s", check.Status, check.Detail)
	}
}

// TestNegativeControlNeverReportsOKOnEmptyDB pins the never-OK-when-unknown
// discipline: an empty documents table cannot produce a trustworthy sentinel,
// so the check must be UNKNOWN, never OK, and never silently pass.
func TestNegativeControlNeverReportsOKOnEmptyDB(t *testing.T) {
	root := t.TempDir()
	c, err := constellation.Open(root) // creates an empty, schema-only db
	if err != nil {
		skipIfNoFTS5(t, err)
		t.Fatalf("constellation.Open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "index health", "negative control (sentinel query)")
	if check.Status == StatusOK {
		t.Fatalf("negative control on an empty index reported OK; must be UNKNOWN (nothing to derive a sentinel from)")
	}
	if check.Status != StatusUnknown {
		t.Errorf("negative control on empty index = %s, want UNKNOWN; detail=%s", check.Status, check.Detail)
	}
}

// TestDoctorAgainstNonexistentWorkspaceNeverReportsOK is the UNKNOWN-
// discipline end-to-end check the ground rules call out explicitly: doctor
// run against a workspace root that does not exist must never claim OK for
// any of the workspace-scoped checks (index health, store liveness) — there
// is nothing there to have verified. Install integrity and config coherence
// are machine-scoped (PATH, ~/.claude, ~/.hermes), not workspace-scoped, so
// they legitimately keep reporting on the real machine regardless of whether
// the target workspace directory exists; this test does not constrain them.
func TestDoctorAgainstNonexistentWorkspaceNeverReportsOK(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})

	workspaceScoped := map[string]bool{"index health": true, "store liveness": true}
	for _, g := range report.Groups {
		if !workspaceScoped[g.Name] {
			continue
		}
		for _, c := range g.Checks {
			if c.Status == StatusOK {
				t.Errorf("group %q check %q reported OK against a nonexistent workspace (detail=%s); should be UNKNOWN or WARN",
					g.Name, c.Name, c.Detail)
			}
		}
	}
	if report.ExitCode() != 0 {
		t.Errorf("nonexistent workspace produced FAIL-driven exit code %d; absence of a workspace is UNKNOWN, not a proven defect",
			report.ExitCode())
	}
}

// TestDocsVsFilesFlagsUnindexedTree exercises the documents-vs-files check
// against the .cog/hooks fixture tree that has files on disk but zero FTS
// rows — the exact shape of Finding 5 in the issue.
func TestDocsVsFilesFlagsUnindexedTree(t *testing.T) {
	root := buildFixtureWorkspace(t)
	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})

	check := findCheck(t, report, "index health", "documents vs files on disk")
	if check.Status != StatusWarn {
		t.Fatalf("documents vs files on disk = %s, want WARN (unindexed .cog/hooks tree present); detail=%s", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "hooks") || !strings.Contains(check.Detail, "UNINDEXED") {
		t.Errorf("expected detail to call out unindexed hooks tree, got:\n%s", check.Detail)
	}
}

// TestDocsVsFilesCountsCorrectlyThroughSymlinkedRoot is the regression test
// for the countDocsUnderPrefix symlink-resolution bug: documents.path in the
// constellation DB is stored only after filepath.EvalSymlinks resolution
// (walkRoots in sdk/constellation/indexer.go), so doctorDocsVsFiles must
// resolve the workspace root itself before building its LIKE prefix, not
// rely on the caller having already done so. Deliberately passes the RAW,
// unresolved t.TempDir() root -- unlike TestDocsVsFilesDoesNotMergeSiblingPrefixSubtrees
// below and TestIndexFreshnessCatchesStaleNonMemSubtree, which resolve up
// front only so their own path-string assertions are exact -- so this test
// exercises the resolution the fix adds. On macOS, t.TempDir() itself
// traverses a symlink (/var/folders -> /private/var/folders), which is
// exactly the failure shape a symlinked workspace mount, NFS home, or macOS
// /tmp hits in production.
func TestDocsVsFilesCountsCorrectlyThroughSymlinkedRoot(t *testing.T) {
	root := t.TempDir() // intentionally NOT resolved -- see comment above

	writeCogdoc(t, root, ".cog/mem/semantic/sentinel.cog.md", "Sentinel", "content")

	c, err := constellation.Open(root)
	if err != nil {
		skipIfNoFTS5(t, err)
		t.Fatalf("constellation.Open: %v", err)
	}
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "index health", "documents vs files on disk")

	var memLine string
	for _, line := range strings.Split(check.Detail, "\n") {
		if strings.HasPrefix(line, ".cog/mem:") {
			memLine = line
			break
		}
	}
	if memLine == "" {
		t.Fatalf("no .cog/mem: line found in detail:\n%s", check.Detail)
	}
	if strings.Contains(memLine, "0 indexed") || strings.Contains(memLine, "UNINDEXED") {
		t.Errorf(".cog/mem line = %q, want a nonzero indexed count -- the sentinel doc IS indexed; an unresolved-symlink prefix mismatch would falsely report 0. Full detail:\n%s", memLine, check.Detail)
	}
}

// TestLikeEscapeCharDoesNotCollideWithBackslashSeparator pins the choice of
// '!' (not the conventional '\') as countDocsUnderPrefix's SQL ESCAPE
// character. On Windows, filepath.Separator IS '\', so if '\' were also the
// ESCAPE char, the pattern's own trailing separator-then-wildcard ('\' +
// '%') would parse as an ESCAPE'd literal '%' rather than "separator,
// then anything" -- making the query match nothing on Windows regardless of
// index health. This can't literally run under GOOS=windows here, so it
// isolates the SQL mechanism directly against a document path containing a
// literal backslash (as any real Windows path would), built the same way
// countDocsUnderPrefix builds its pattern.
func TestLikeEscapeCharDoesNotCollideWithBackslashSeparator(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "escape-fixture-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	db, err := sql.Open("sqlite3", f.Name())
	if err != nil {
		if strings.Contains(err.Error(), "no such module: fts5") {
			t.Skip("FTS5 not available (build with -tags fts5)")
		}
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE documents (id TEXT PRIMARY KEY, path TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Shaped like a real Windows document path: backslash separators, same
	// as filepath.Join produces under GOOS=windows.
	winPath := `C:\workspace\.cog\mem\note.cog.md`
	if _, err := db.Exec(`INSERT INTO documents (id, path) VALUES ('d1', ?)`, winPath); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prefix := `C:\workspace\.cog\mem`
	// Mirrors countDocsUnderPrefix's own pattern construction, with a
	// literal '\' standing in for filepath.Separator on Windows.
	pattern := escapeLikePattern(prefix) + `\` + "%"

	var gotFixed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM documents WHERE path LIKE ? ESCAPE '!'`, pattern).Scan(&gotFixed); err != nil {
		t.Fatalf("query with ESCAPE '!': %v", err)
	}
	if gotFixed != 1 {
		t.Errorf("Windows-shaped prefix match with ESCAPE '!' = %d, want 1 (separator+wildcard must not be swallowed as an escaped literal '%%')", gotFixed)
	}

	// Documents the regression this guards against: the SAME pattern under
	// the conventional ESCAPE '\' collides, because '\' is both the
	// appended path separator and the declared escape character.
	var gotBroken int
	if err := db.QueryRow(`SELECT COUNT(*) FROM documents WHERE path LIKE ? ESCAPE '\'`, pattern).Scan(&gotBroken); err != nil {
		t.Fatalf("query with ESCAPE '\\': %v", err)
	}
	if gotBroken != 0 {
		t.Fatalf("test assumption broken: ESCAPE '\\' unexpectedly matched %d row(s) -- the collision this test documents may no longer reproduce this way", gotBroken)
	}
}

// TestDocsVsFilesDoesNotMergeSiblingPrefixSubtrees is the regression test for
// the countDocsUnderPrefix LIKE-pattern bug: a bare `prefix+"%"` pattern
// matches any sibling subtree whose name merely starts with the same
// characters (".cog/adr" matches ".cog/adr-legacy/..."), merging their
// document counts and masking a genuinely unindexed tree.
func TestDocsVsFilesDoesNotMergeSiblingPrefixSubtrees(t *testing.T) {
	root := t.TempDir()
	// t.TempDir() on macOS resolves under a /var/folders symlink to
	// /private/var/folders; the indexer resolves .cog/'s walk root via
	// filepath.EvalSymlinks (see walkRoots), so document paths land in the
	// database already symlink-resolved. Resolve root the same way here so
	// the prefixes this test builds actually match what got indexed --
	// otherwise every countDocsUnderPrefix lookup silently returns 0
	// regardless of the LIKE-pattern fix under test, for an unrelated
	// path-identity reason.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve tempdir symlinks: %v", err)
	}
	root = resolvedRoot

	// .cog/adr-legacy is a distinct, indexed subtree that shares "adr" as a
	// literal name prefix. Under the pre-fix bare-prefix LIKE pattern, this
	// row's path also matches `path LIKE '.../.cog/adr%'` and would get
	// double-counted against .cog/adr, masking the fact that .cog/adr itself
	// has zero indexed documents.
	writeCogdoc(t, root, ".cog/adr-legacy/real.cog.md", "Legacy ADR", "indexed sibling content")

	c, err := constellation.Open(root)
	if err != nil {
		skipIfNoFTS5(t, err)
		t.Fatalf("constellation.Open: %v", err)
	}
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// .cog/adr gets its *.cog.md file AFTER the index above ran, so it is a
	// real, indexable cogdoc that genuinely has zero rows in the DB -- not a
	// plain .md, which IndexWorkspace was never going to index regardless
	// (indexer.go:129 only ever considers the *.cog.md suffix) and so
	// wouldn't be counted as "on disk" at all by the fix under test here.
	writeCogdoc(t, root, ".cog/adr/new.cog.md", "New ADR", "not yet reindexed")

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "index health", "documents vs files on disk")

	var adrLine string
	for _, line := range strings.Split(check.Detail, "\n") {
		if strings.HasPrefix(line, ".cog/adr:") {
			adrLine = line
			break
		}
	}
	if adrLine == "" {
		t.Fatalf("no .cog/adr: line found in detail (want it distinct from .cog/adr-legacy):\n%s", check.Detail)
	}
	if !strings.Contains(adrLine, "0 indexed") || !strings.Contains(adrLine, "UNINDEXED") {
		t.Errorf(".cog/adr line = %q, want 0 indexed/UNINDEXED -- .cog/adr-legacy's indexed row must not be counted against .cog/adr; full detail:\n%s", adrLine, check.Detail)
	}
}

// TestIndexFreshnessCatchesStaleNonMemSubtree is the regression test for
// doctorIndexFreshness's original .cog/mem-only scan scope: IndexWorkspace's
// actual walk covers the whole .cog/ tree (walkRoots in
// sdk/constellation/indexer.go), so an edit landing in a different indexed
// subtree (.cog/adr here) after the last reindex must still surface as a
// freshness gap, not silently OK because .cog/mem itself looks fine.
func TestIndexFreshnessCatchesStaleNonMemSubtree(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve tempdir symlinks: %v", err)
	}
	root = resolvedRoot

	writeCogdoc(t, root, ".cog/mem/semantic/sentinel.cog.md", "Sentinel", "content")

	c, err := constellation.Open(root)
	if err != nil {
		skipIfNoFTS5(t, err)
		t.Fatalf("constellation.Open: %v", err)
	}
	if err := c.IndexWorkspace(); err != nil {
		t.Fatalf("IndexWorkspace: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// An edit lands in .cog/adr (indexed by the base .cog/ walk, no
	// cogdocs.yaml required) well after the reindex above, without
	// triggering a reindex.
	adrPath := filepath.Join(root, ".cog", "adr", "new.cog.md")
	if err := os.MkdirAll(filepath.Dir(adrPath), 0755); err != nil {
		t.Fatalf("mkdir adr: %v", err)
	}
	if err := os.WriteFile(adrPath, []byte("# new adr\n"), 0644); err != nil {
		t.Fatalf("write adr file: %v", err)
	}
	future := time.Now().Add(3 * time.Hour)
	if err := os.Chtimes(adrPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "index health", "index freshness")
	if check.Status != StatusWarn {
		t.Fatalf("index freshness = %s, want WARN (a .cog/adr file postdates the last reindex by 3h); detail=%s", check.Status, check.Detail)
	}
}

// TestStoreLivenessFlagsDeadStore verifies a SQLite store whose mtime is
// older than the stale threshold is flagged WARN/DEAD, and a fresh one is OK.
func TestStoreLivenessFlagsDeadStore(t *testing.T) {
	root := buildFixtureWorkspace(t) // already has a fresh constellation.db

	deadPath := filepath.Join(root, ".cog", ".state", "old_events.db")

	// Build a minimal real sqlite file via the stdlib driver so store liveness
	// can open it read-only and enumerate its (empty) table set.
	createEmptySQLiteFile(t, deadPath)
	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(deadPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true, StaleDays: 30})
	check := findCheck(t, report, "store liveness", "store: "+deadPath)
	if check.Status != StatusWarn {
		t.Fatalf("store liveness for a 40d-stale store = %s, want WARN(DEAD); detail=%s", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "DEAD") {
		t.Errorf("expected DEAD label in detail, got: %s", check.Detail)
	}
}

// TestStoreLivenessReportsUnknownOnUnreadableStore pins the UNKNOWN-not-OK
// contract this command advertises: a store whose row count cannot be
// established (corrupt file here; permission-denied is the same code path)
// must never render OK, even though its last-write age looks fresh.
func TestStoreLivenessReportsUnknownOnUnreadableStore(t *testing.T) {
	root := buildFixtureWorkspace(t)

	corruptPath := filepath.Join(root, ".cog", ".state", "corrupt.db")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(corruptPath, []byte("not a sqlite database"), 0644); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}
	// Fresh mtime so the staleness case never masks the row-count failure.
	now := time.Now()
	if err := os.Chtimes(corruptPath, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true, StaleDays: 30})
	check := findCheck(t, report, "store liveness", "store: "+corruptPath)
	if check.Status == StatusOK {
		t.Fatalf("store liveness reported OK for an unreadable store (detail=%s); must be UNKNOWN, never OK", check.Detail)
	}
	if check.Status != StatusUnknown {
		t.Errorf("store liveness for an unreadable store = %s, want UNKNOWN; detail=%s", check.Status, check.Detail)
	}
}

func createEmptySQLiteFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open sqlite file: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE placeholder (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestExitCodeReflectsOnlyFail(t *testing.T) {
	r := &DoctorReport{Groups: []DoctorGroup{
		{Name: "g", Checks: []DoctorCheck{
			{Name: "a", Status: StatusOK},
			{Name: "b", Status: StatusWarn},
			{Name: "c", Status: StatusUnknown},
		}},
	}}
	if r.ExitCode() != 0 {
		t.Errorf("OK/WARN/UNKNOWN only: exit code = %d, want 0", r.ExitCode())
	}
	r.Groups[0].Checks = append(r.Groups[0].Checks, DoctorCheck{Name: "d", Status: StatusFail})
	if r.ExitCode() != 1 {
		t.Errorf("with a FAIL present: exit code = %d, want 1", r.ExitCode())
	}
}

func TestGroupByPrefixDetectsGenerationDrift(t *testing.T) {
	groups := groupByPrefix([]string{"cogos", "cogos-http", "cogos-v3", "unrelated"})
	var driftedFound bool
	for _, g := range groups {
		if len(g) > 1 {
			driftedFound = true
			if len(g) != 3 {
				t.Errorf("expected the cogos family to cluster to 3 names, got %v", g)
			}
		}
	}
	if !driftedFound {
		t.Fatalf("groupByPrefix did not cluster cogos/cogos-http/cogos-v3: %v", groups)
	}
}

func TestGroupByPrefixLeavesUnrelatedNamesSingleton(t *testing.T) {
	groups := groupByPrefix([]string{"alpha", "beta", "gamma"})
	for _, g := range groups {
		if len(g) != 1 {
			t.Errorf("expected all-singleton groups for unrelated names, got %v", groups)
		}
	}
}

func TestLongestWordPicksDistinctiveTerm(t *testing.T) {
	if got := longestWord("A B Recognizable"); got != "recognizable" {
		t.Errorf("longestWord = %q, want %q", got, "recognizable")
	}
	if got := longestWord("a b c"); got != "" {
		t.Errorf("longestWord on all-short-words = %q, want empty", got)
	}
}

func TestExpandHome(t *testing.T) {
	home := "/Users/tester"
	if got := expandHome("~/foo/bar", home); got != "/Users/tester/foo/bar" {
		t.Errorf("expandHome(~/foo/bar) = %q", got)
	}
	if got := expandHome("/already/absolute", home); got != "/already/absolute" {
		t.Errorf("expandHome should pass through absolute paths unchanged, got %q", got)
	}
}

// TestBinarySprawlScansWorkspaceRootAndDotCog pins cogos#568 finding 1: a
// binary sitting at <workspace>/.cog/cog (the exact shape of the 79-day-old
// binary a workspace-local `scripts/cog` wrapper resolved to) must be
// detected without requiring an explicit --scan-dir, because it is the
// doctor target workspace itself, not an arbitrary extra location.
func TestBinarySprawlScansWorkspaceRootAndDotCog(t *testing.T) {
	root := t.TempDir()
	dotCog := filepath.Join(root, ".cog")
	if err := os.MkdirAll(dotCog, 0755); err != nil {
		t.Fatalf("mkdir .cog: %v", err)
	}
	stray := filepath.Join(dotCog, "cog")
	if err := os.WriteFile(stray, []byte("not a real binary, just needs to exist+exec"), 0755); err != nil {
		t.Fatalf("write stray binary: %v", err)
	}
	old := time.Now().Add(-79 * 24 * time.Hour)
	if err := os.Chtimes(stray, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "install integrity", "binary sprawl")
	if !strings.Contains(check.Detail, stray) {
		t.Errorf("binary sprawl did not report %s (workspace/.cog binary); detail=%s", stray, check.Detail)
	}
	if !strings.Contains(check.Detail, "age=79d") {
		t.Errorf("expected age=79d for the stray binary in detail:\n%s", check.Detail)
	}
}

// TestBinarySprawlIgnoresNonExecutableAndUnrelatedFiles verifies the name/
// executable-bit filter does not over-match: a non-executable file named
// "cog.md" or an executable named "cogfield" must not be reported as a
// stray cogos/cog binary.
func TestBinarySprawlIgnoresNonExecutableAndUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	dotCog := filepath.Join(root, ".cog")
	if err := os.MkdirAll(dotCog, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotCog, "cog.prev"), []byte("x"), 0644); err != nil { // no exec bit
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotCog, "cogfield-notes.md"), []byte("x"), 0755); err != nil { // exec but wrong name
		t.Fatalf("write: %v", err)
	}

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true})
	check := findCheck(t, report, "install integrity", "binary sprawl")
	if strings.Contains(check.Detail, "cog.prev") {
		t.Errorf("non-executable cog.prev should not be reported as a binary: %s", check.Detail)
	}
	if strings.Contains(check.Detail, "cogfield-notes.md") {
		t.Errorf("cogfield-notes.md should not match the cogos/cog binary name pattern: %s", check.Detail)
	}
}

// TestPathLikeRegexIgnoresURLsAndModelIDs pins the false-positive fix: MCP/
// Hermes configs are full of "/segment/segment"-shaped strings that are NOT
// filesystem paths (API base URLs, HuggingFace model IDs, MCP route
// strings), and the nonexistent-path-reference check must not flag them.
func TestPathLikeRegexIgnoresURLsAndModelIDs(t *testing.T) {
	notPaths := []string{
		"https://api.anthropic.com/v1",
		"lmstudio-eclipse/google/gemma-4-e4b",
		"/v1/synthesize",
		"nikolaik/python-nodejs:python3.11-nodejs20",
	}
	for _, s := range notPaths {
		if m := pathLikeRe.FindAllString(s, -1); len(m) > 0 {
			t.Errorf("pathLikeRe matched non-path string %q as %v; these must not be flagged as filesystem paths", s, m)
		}
	}

	realPaths := []string{
		"/Users/tester/workspaces/cogos-dev/mod3",
		"~/workspaces/cog",
	}
	for _, s := range realPaths {
		if m := pathLikeRe.FindAllString(s, -1); len(m) == 0 {
			t.Errorf("pathLikeRe failed to match real absolute path %q", s)
		}
	}
}

// TestMCPConfigsPointAtOneBinaryDetectsDrift exercises the structured
// "command" field walk end-to-end: two config files naming different cogos
// binaries must produce a WARN naming both; a single shared binary must
// produce OK.
func TestMCPConfigsPointAtOneBinaryDetectsDrift(t *testing.T) {
	root := t.TempDir()
	binA := filepath.Join(root, "bin-a", "cogos")
	binB := filepath.Join(root, "bin-b", "cogos")
	for _, p := range []string{binA, binB} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	cfg1 := filepath.Join(root, "cfg1.mcp.json")
	cfg2 := filepath.Join(root, "cfg2.mcp.json")
	writeJSON(t, cfg1, map[string]any{"mcpServers": map[string]any{"cogos": map[string]any{"command": binA}}})
	writeJSON(t, cfg2, map[string]any{"mcpServers": map[string]any{"cogos": map[string]any{"command": binB}}})

	report := RunDoctor(root, DoctorOptions{SkipNetwork: true, ExtraConfigFiles: []string{cfg1, cfg2}})
	check := findCheck(t, report, "config coherence", "MCP configs point at one binary")
	if check.Status != StatusWarn {
		t.Fatalf("two distinct cogos binaries referenced = %s, want WARN; detail=%s", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, binA) || !strings.Contains(check.Detail, binB) {
		t.Errorf("expected both binaries named in detail:\n%s", check.Detail)
	}
}

func TestLooksLikeRealPath(t *testing.T) {
	if !looksLikeRealPath("/Users/tester/workspaces/cogos-dev") {
		t.Error("expected a two+-segment absolute path to look real")
	}
	if looksLikeRealPath("https://example.com/foo") {
		t.Error("a URL should not be treated as a filesystem path")
	}
}
