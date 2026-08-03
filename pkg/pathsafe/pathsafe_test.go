package pathsafe

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeComponent_ColonKey(t *testing.T) {
	got := SanitizeComponent("http:cog")
	if strings.Contains(got, ":") {
		t.Fatalf("SanitizeComponent(%q) = %q, still contains a colon", "http:cog", got)
	}
	want := "http%3Acog"
	if got != want {
		t.Fatalf("SanitizeComponent(%q) = %q, want %q", "http:cog", got, want)
	}
}

// TestSanitizeComponent_ColonKey_BothSeparators verifies the sanitized form
// of a colon-bearing key stays colon-free once it's stitched into a full
// path, regardless of which path-separator convention wraps it: forward
// slash (POSIX / URL-style, and Windows accepts it too) and backslash
// (native Windows). This is the concrete regression case from #489 — the
// same key must produce a legal directory name under either convention.
func TestSanitizeComponent_ColonKey_BothSeparators(t *testing.T) {
	raw := "http:cog"
	safe := SanitizeComponent(raw)

	forwardSlashPath := strings.Join([]string{"", ".cog", "ledger", safe, "events.jsonl"}, "/")
	if strings.Contains(forwardSlashPath, ":") {
		t.Fatalf("forward-slash path %q still contains a colon", forwardSlashPath)
	}

	// Simulate a Windows absolute path: the drive-letter colon at index 1
	// ("C:") is legitimate and must survive; the session component must not
	// introduce any other colon.
	backslashPath := strings.Join([]string{`C:\Users\example-user\workspace`, ".cog", "ledger", safe, "events.jsonl"}, `\`)
	if n := strings.Count(backslashPath, ":"); n != 1 {
		t.Fatalf("backslash path %q has %d colons, want exactly 1 (the drive letter): %q", backslashPath, n, backslashPath)
	}
}

func TestSanitizeComponent_ControlAndReservedChars(t *testing.T) {
	cases := map[string]string{
		"a<b":    "a%3Cb",
		"a>b":    "a%3Eb",
		`a"b`:    "a%22b",
		"a|b":    "a%7Cb",
		"a?b":    "a%3Fb",
		"a*b":    "a%2Ab",
		"a/b":    "a%2Fb",
		`a\b`:    "a%5Cb",
		"a%b":    "a%b", // '%' is intentionally NOT escaped — see idempotence note in pathsafe.go
		"a\x00b": "a%00b",
		"a\x1fb": "a%1Fb",
		"a\tb":   "a%09b",
	}
	for in, want := range cases {
		if got := SanitizeComponent(in); got != want {
			t.Errorf("SanitizeComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeComponent_TrailingDotOrSpace(t *testing.T) {
	cases := map[string]string{
		"foo.":  "foo%2E",
		"foo ":  "foo%20",
		"foo..": "foo.%2E",
		".":     "%2E",
		"..":    ".%2E",
	}
	for in, want := range cases {
		if got := SanitizeComponent(in); got != want {
			t.Errorf("SanitizeComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeComponent_PathTraversalDefused(t *testing.T) {
	for _, in := range []string{".", ".."} {
		got := SanitizeComponent(in)
		if got == "." || got == ".." || got == "" {
			t.Errorf("SanitizeComponent(%q) = %q, path-traversal component not defused", in, got)
		}
	}
}

func TestSanitizeComponent_ReservedWindowsStems(t *testing.T) {
	cases := []string{"CON", "con", "PRN", "AUX", "NUL", "COM1", "com9", "LPT1", "lpt9", "CON.txt", "con.tar.gz"}
	for _, in := range cases {
		got := SanitizeComponent(in)
		if isReservedWindowsStem(got) {
			t.Errorf("SanitizeComponent(%q) = %q, still a reserved Windows stem", in, got)
		}
	}
}

func TestSanitizeComponent_Idempotent(t *testing.T) {
	inputs := []string{"http:cog", "a<b>c", "CON", "foo.", "..", "plain-uuid-1234", ""}
	for _, in := range inputs {
		once := SanitizeComponent(in)
		twice := SanitizeComponent(once)
		if once != twice {
			t.Errorf("SanitizeComponent not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestSanitizeComponent_AcceptedCollision documents (rather than hides) the
// one tradeoff idempotence costs: a raw key that already spells out another
// key's escape sequence collides with it. This is intentional — see the
// package doc comment — and this test exists so the behavior can't drift
// unnoticed.
func TestSanitizeComponent_AcceptedCollision(t *testing.T) {
	a := SanitizeComponent("http:cog")
	b := SanitizeComponent("http%3Acog")
	if a != b {
		t.Fatalf("expected documented collision: SanitizeComponent(%q)=%q, SanitizeComponent(%q)=%q", "http:cog", a, "http%3Acog", b)
	}
}

func TestSanitizeComponent_EmptyUnchanged(t *testing.T) {
	if got := SanitizeComponent(""); got != "" {
		t.Errorf("SanitizeComponent(\"\") = %q, want empty string preserved", got)
	}
}

func TestSanitizeComponent_OrdinaryKeysUnchanged(t *testing.T) {
	// The common case (UUIDs, plain slugs) must not be perturbed — this is
	// what keeps the fix backward compatible with the overwhelming majority
	// of existing on-disk session directories.
	for _, in := range []string{
		"34226033-b4b9-4642-8f5f-2ffbde21006a",
		"cs-1a2b3c4d5e6f",
		"identity-grants",
		"_default",
	} {
		if got := SanitizeComponent(in); got != in {
			t.Errorf("SanitizeComponent(%q) = %q, want unchanged", in, got)
		}
	}
}

// ── SanitizeRelPath ──────────────────────────────────────────────────────────
// Added round 5 (myrgic/cogos#489): promoted from the sdk module's private
// sanitizeRelPath to this canonical package because internal/engine/uri.go's
// resolveProjection needed the same multi-segment sanitization and cannot
// import sdk (wrong dependency direction).

func TestSanitizeRelPath_ColonSegment(t *testing.T) {
	got := SanitizeRelPath("http:cog")
	if strings.Contains(got, ":") {
		t.Errorf("SanitizeRelPath(\"http:cog\") = %q, still contains ':'", got)
	}
}

func TestSanitizeRelPath_TraversalDefused(t *testing.T) {
	base := t.TempDir()
	ledgerDir := filepath.Join(base, "ledger")

	joined := filepath.Join(ledgerDir, SanitizeRelPath("../../../../etc/passwd"))
	rel, err := filepath.Rel(ledgerDir, joined)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Fatalf("SanitizeRelPath allowed escape: joined=%q is outside base %q (rel=%q)", joined, ledgerDir, rel)
	}
}

func TestSanitizeRelPath_PreservesLegitimateMultiSegmentPaths(t *testing.T) {
	got := SanitizeRelPath("semantic/insights/eigenform")
	want := filepath.Join("semantic", "insights", "eigenform")
	if got != want {
		t.Errorf("SanitizeRelPath(%q) = %q, want %q", "semantic/insights/eigenform", got, want)
	}
}

func TestSanitizeRelPath_EmptyUnchanged(t *testing.T) {
	if got := SanitizeRelPath(""); got != "" {
		t.Errorf("SanitizeRelPath(\"\") = %q, want empty string preserved", got)
	}
}

func TestSanitizeRelPath_DropsEmptySegments(t *testing.T) {
	got := SanitizeRelPath("a//b")
	want := filepath.Join("a", "b")
	if got != want {
		t.Errorf("SanitizeRelPath(%q) = %q, want %q (doubled '/' should collapse)", "a//b", got, want)
	}
}
