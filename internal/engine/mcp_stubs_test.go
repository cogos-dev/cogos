// mcp_stubs_test.go — regression tests for SearchMemory / searchMemoryFTS.
//
// Coverage:
//   - searchMemoryFTS returns results when the FTS5 JOIN uses d.id = f.id
//     (regression guard against the rowid-based JOIN bug that silently returned
//     zero rows because documents uses a TEXT primary key).
//   - sector filter narrows results correctly.
//   - buildFTSQuery preserves user-quoted phrases, defaults multi-word
//     queries to AND, honors an explicit bare OR token, and never produces
//     invalid FTS5 syntax from hostile or malformed input.
package engine

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupFTSFixture creates an in-memory SQLite database that mirrors the
// constellation schema and seeds a small set of documents + FTS rows so we
// can test the JOIN without touching the live DB.
func setupFTSFixture(t *testing.T) (dbPath string, cleanup func()) {
	t.Helper()

	// Use a temp file so the sqlite3 driver can open it by path (the driver
	// requires a file URI for in-memory DBs shared across connections, which is
	// simpler to manage as a temp file in tests).
	f, err := os.CreateTemp(t.TempDir(), "fts-fixture-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()

	db, err := sql.Open("sqlite3", f.Name())
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
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
);

CREATE VIRTUAL TABLE documents_fts USING fts5(
    id UNINDEXED,
    title,
    content,
    tags,
    sector,
    type,
    tokenize='porter unicode61'
);
`
	if _, err := db.Exec(schema); err != nil {
		// FTS5 requires a build tag (-tags fts5). Skip if unavailable.
		if strings.Contains(err.Error(), "no such module: fts5") {
			t.Skip("FTS5 not available (build with -tags fts5)")
		}
		t.Fatalf("create schema: %v", err)
	}

	// Insert three documents; two match "claude", one matches "kernel".
	rows := []struct {
		id, path, title, sector, status, content string
	}{
		{"doc-1", "/mem/a.md", "Claude Overview", "semantic", "active", "claude is an AI assistant made by anthropic"},
		{"doc-2", "/mem/b.md", "Claude Code Guide", "procedural", "active", "claude code helps you write software"},
		{"doc-3", "/mem/c.md", "Kernel Internals", "semantic", "active", "the kernel manages memory and execution"},
		{"doc-dep", "/mem/d.md", "Claude Deprecated", "semantic", "deprecated", "this document is deprecated"},
	}
	for _, r := range rows {
		_, err := db.Exec(
			`INSERT INTO documents (id, path, type, title, created, sector, status, content, content_hash, indexed_at, file_mtime)
			 VALUES (?, ?, 'insight', ?, '2026-01-01', ?, ?, ?, 'hash', '2026-01-01', '2026-01-01')`,
			r.id, r.path, r.title, r.sector, r.status, r.content,
		)
		if err != nil {
			t.Fatalf("insert document %q: %v", r.id, err)
		}
		_, err = db.Exec(
			`INSERT INTO documents_fts (id, title, content, tags, sector, type)
			 VALUES (?, ?, ?, '', ?, 'insight')`,
			r.id, r.title, r.content, r.sector,
		)
		if err != nil {
			t.Fatalf("insert fts row %q: %v", r.id, err)
		}
	}

	return f.Name(), func() { os.Remove(f.Name()) }
}

// TestSearchMemoryFTS_JoinReturnsResults is the primary regression guard.
// Before the fix, searchMemoryFTS JOINed on d.rowid = documents_fts.rowid,
// which always returned 0 rows because documents uses a TEXT primary key
// (rowid is an unrelated auto-integer). The fix JOINs on d.id = f.id using
// the UNINDEXED 'id' column carried by the FTS5 table.
func TestSearchMemoryFTS_JoinReturnsResults(t *testing.T) {
	dbPath, cleanup := setupFTSFixture(t)
	defer cleanup()

	got, err := searchMemoryFTS(dbPath, "/mem", "claude", 10, "")
	if err != nil {
		t.Fatalf("searchMemoryFTS: %v", err)
	}

	count, _ := got["count"].(int)
	if count == 0 {
		t.Fatal("searchMemoryFTS returned 0 results for 'claude'; JOIN is broken (rowid bug?)")
	}
	// Expect doc-1 and doc-2 to match; doc-dep should be excluded (deprecated).
	if count < 2 {
		t.Errorf("expected at least 2 results for 'claude', got %d", count)
	}
}

// TestSearchMemoryFTS_DeprecatedExcluded verifies that documents with
// status='deprecated' are filtered out even when their FTS content matches.
func TestSearchMemoryFTS_DeprecatedExcluded(t *testing.T) {
	dbPath, cleanup := setupFTSFixture(t)
	defer cleanup()

	got, err := searchMemoryFTS(dbPath, "/mem", "claude", 10, "")
	if err != nil {
		t.Fatalf("searchMemoryFTS: %v", err)
	}

	results, _ := got["results"].([]map[string]any)
	for _, r := range results {
		if r["path"] == "/mem/d.md" {
			t.Errorf("deprecated document appeared in results: %v", r)
		}
	}
}

// TestSearchMemoryFTS_SectorFilter verifies that the optional sector parameter
// narrows results to a single sector.
func TestSearchMemoryFTS_SectorFilter(t *testing.T) {
	dbPath, cleanup := setupFTSFixture(t)
	defer cleanup()

	// "claude" matches doc-1 (semantic) and doc-2 (procedural).
	// Filtering by "procedural" should return only doc-2.
	got, err := searchMemoryFTS(dbPath, "/mem", "claude", 10, "procedural")
	if err != nil {
		t.Fatalf("searchMemoryFTS with sector: %v", err)
	}

	count, _ := got["count"].(int)
	if count != 1 {
		t.Errorf("expected 1 result in sector 'procedural', got %d", count)
	}
}

// TestBuildFTSQuery covers the query builder used by searchMemoryFTS.
//
// Regression guard for cogos#568 finding 4: multi-word queries used to
// become an OR across every term (maximally unselective, and user-supplied
// double quotes were silently stripped so a phrase search was inexpressible).
// These cases pin the fix: quoted phrases are preserved, multi-word queries
// default to AND, and a bare uppercase OR token still broadens the search.
func TestBuildFTSQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Single bare word: unchanged legacy behaviour (unquoted, broader
		// token match).
		{"single word", "claude", "claude"},

		// Multi-word bare queries now default to AND instead of OR.
		{"and default, two words", "claude code", `"claude" AND "code"`},
		{"and default, extra whitespace", "  spaced  query  ", `"spaced" AND "query"`},
		{"and default, three words", "spirit over letter", `"spirit" AND "over" AND "letter"`},

		// A fully user-quoted phrase becomes a single FTS5 phrase term.
		{"quoted phrase", `"spirit over letter"`, `"spirit over letter"`},
		// A user-quoted single word still renders as a phrase term (not the
		// unquoted single-word shortcut, which only applies to bare input).
		{"quoted single word", `"claude"`, `"claude"`},

		// Explicit bare OR is honored as the FTS5 OR operator.
		{"explicit or", "spirit OR letter", `"spirit" OR "letter"`},
		{"explicit or, three terms", "spirit OR letter OR law", `"spirit" OR "letter" OR "law"`},

		// A quoted phrase mixed with a bare term still defaults to AND.
		{"mixed phrase and term", `"spirit over" letter`, `"spirit over" AND "letter"`},
		{"mixed term and phrase", `letter "spirit over"`, `"letter" AND "spirit over"`},

		// Unbalanced quotes must not crash or produce invalid FTS5 syntax;
		// the remainder after an unterminated quote falls back to bare
		// words.
		{"unbalanced quote, single word", `"spirit`, "spirit"},
		{"unbalanced quote, multi word", `foo "bar baz`, `"foo" AND "bar" AND "baz"`},

		// FTS5 special characters are stripped outside quoted phrases so
		// hostile input cannot produce a syntax error.
		{"leading dash single word", "-secret", "secret"},
		{"column filter colon", "type:foo bar", `"typefoo" AND "bar"`},
		// The '"' inside foo"bar terminates the bare word "foo" and opens a
		// new (unbalanced) quoted span, so "bar baz" falls back to bare
		// words rather than merging with "foo".
		{"embedded quote in bare word", `foo"bar baz`, `"foo" AND "bar" AND "baz"`},

		// Dangling OR operators (no operand on one side) are dropped rather
		// than emitted as invalid FTS5 syntax.
		{"lone or", "OR", ""},
		{"trailing or", "foo OR", "foo"},
		{"leading or", "OR foo", "foo"},

		// A sole bare term that is itself the literal (case-sensitive)
		// FTS5 reserved keyword AND or NOT must be quoted, not passed
		// through unquoted like an ordinary single word, or it produces
		// invalid FTS5 syntax (no operand on either side of the operator).
		{"sole term literal AND", "AND", `"AND"`},
		{"sole term literal NOT", "NOT", `"NOT"`},
		{"sole term dash-AND reduces to reserved word", "-AND", `"AND"`},
		{"sole term lowercase and is not reserved", "and", "and"},
		{"sole term mixed-case And is not reserved", "And", "And"},

		// Empty / whitespace-only input passes through unchanged.
		{"empty string", "", ""},
		{"whitespace only", "   ", "   "},

		// Degenerate quoted spans with no content still produce a valid
		// FTS5 query (an explicit empty-phrase operand) instead of an
		// empty string, which would error out of FTS5 and silently fall
		// back to the naive grep path.
		{"quote-space-quote", `" "`, `""`},
		{"two empty quote pairs", `"" ""`, `"" AND ""`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFTSQuery(tc.input)
			if got != tc.want {
				t.Errorf("buildFTSQuery(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}
