package sdk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the two confirmed findings from the PR #504 round-2
// review of myrgic/cogos#489: ledgerProjector.Resolve (cogos.go, was line
// 781) and ledgerProjector.Mutate/appendEvent (cogos.go, was line 882) each
// joined a caller-supplied URI path segment into a filesystem path with no
// sanitization and no rejection of ".." segments, reachable unauthenticated
// via sdk/httputil.Server's GET /resolve and POST /mutate.
//
// Two independent layers are exercised here:
//   - ParseURI (uri.go) rejecting ".." segments at parse time — the
//     defense-in-depth layer that protects every projector uniformly.
//   - sanitizePathComponent/sanitizeRelPath (pathsafe.go) at the actual
//     filesystem-join call sites — the primary fix, and the layer that
//     still holds even if a ParsedURI is constructed directly rather than
//     produced by ParseURI (which is possible in-package, since
//     ParsedURI's fields are exported).

// newTestKernel builds a Kernel rooted at a fresh temp workspace with the
// minimal .cog/id.cog scaffolding Connect requires, and registers the
// built-in projectors (including ledgerProjector).
func newTestKernel(t *testing.T) *Kernel {
	t.Helper()
	root := t.TempDir()
	cogDir := filepath.Join(root, ".cog")
	if err := os.MkdirAll(cogDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.cog): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cogDir, "id.cog"), []byte("test-workspace"), 0o644); err != nil {
		t.Fatalf("WriteFile(id.cog): %v", err)
	}

	kernel, err := Connect(root)
	if err != nil {
		t.Fatalf("Connect(%q): %v", root, err)
	}
	t.Cleanup(func() { kernel.Close() })
	return kernel
}

// --- Layer 1: ParseURI rejects '..' segments at parse time ---

func TestParseURIRejectsTraversalSegments(t *testing.T) {
	tests := []string{
		// The exact shape named in the review.
		"cog:ledger/../../../../etc/passwd",
		"cog://ledger/../../../../etc/passwd",
		// The mutate-path traversal shape named in the review.
		"cog:ledger/../../../../tmp/evil",
		// Percent-encoded '..' — net/url has already decoded parsed.Path by
		// the time we inspect it, so this must be caught the same way.
		"cog:ledger/%2e%2e/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
		// A single leading '..' is enough; it doesn't need to be repeated.
		"cog:ledger/..",
		// '..' buried in the middle of an otherwise-valid path.
		"cog:ledger/subdir/../../etc/passwd",
	}

	for _, uri := range tests {
		parsed, err := ParseURI(uri)
		if err == nil {
			t.Errorf("ParseURI(%q) = %+v, nil; want an error rejecting the '..' segment", uri, parsed)
		}
	}
}

func TestParseURIStillAcceptsLegitimatePaths(t *testing.T) {
	tests := []struct {
		uri  string
		path string
	}{
		{"cog:ledger/abc123", "abc123"},
		{"cog:ledger/abc123/events.jsonl", "abc123/events.jsonl"},
		{"cog:mem/semantic/insights/eigenform", "semantic/insights/eigenform"},
		// A single dot is not a traversal segment; leave it to filepath.Join
		// to treat as a no-op, same as before this fix.
		{"cog:ledger/./abc123", "./abc123"},
		// "session..name" contains ".." as a substring but is not a '..'
		// path SEGMENT, so it must not be rejected.
		{"cog:ledger/session..name", "session..name"},
	}

	for _, tt := range tests {
		parsed, err := ParseURI(tt.uri)
		if err != nil {
			t.Errorf("ParseURI(%q) unexpected error: %v", tt.uri, err)
			continue
		}
		if parsed.Path != tt.path {
			t.Errorf("ParseURI(%q).Path = %q, want %q", tt.uri, parsed.Path, tt.path)
		}
	}
}

// --- Layer 2: sanitizePathComponent / sanitizeRelPath at the join sites ---

func TestSanitizePathComponentNeutralizesTraversal(t *testing.T) {
	if got := sanitizePathComponent(".."); got == ".." {
		t.Fatalf("sanitizePathComponent(\"..\") = %q, must not pass through as a literal '..'", got)
	}
	if strings.Contains(sanitizePathComponent(".."), "/") || strings.Contains(sanitizePathComponent(".."), "\\") {
		t.Fatalf("sanitizePathComponent(\"..\") = %q, must not contain a path separator", sanitizePathComponent(".."))
	}

	// Idempotence: sanitizing twice must equal sanitizing once.
	once := sanitizePathComponent("../weird:name")
	twice := sanitizePathComponent(once)
	if once != twice {
		t.Errorf("sanitizePathComponent not idempotent: once=%q twice=%q", once, twice)
	}

	// Embedded separators must be escaped so a single component can't
	// smuggle in extra path segments.
	if got := sanitizePathComponent("a/../b"); strings.ContainsAny(got, "/\\") {
		t.Errorf("sanitizePathComponent(%q) = %q, still contains a separator", "a/../b", got)
	}
}

func TestSanitizeRelPathCannotEscapeBase(t *testing.T) {
	base := t.TempDir()
	ledgerDir := filepath.Join(base, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The exact traversal shape from the review.
	joined := filepath.Join(ledgerDir, sanitizeRelPath("../../../../etc/passwd"))
	rel, err := filepath.Rel(ledgerDir, joined)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Fatalf("sanitizeRelPath allowed escape: joined=%q is outside base %q (rel=%q)", joined, ledgerDir, rel)
	}

	// Legitimate multi-segment paths must still resolve where expected.
	joined = filepath.Join(ledgerDir, sanitizeRelPath("abc123/events.jsonl"))
	want := filepath.Join(ledgerDir, "abc123", "events.jsonl")
	if joined != want {
		t.Errorf("sanitizeRelPath broke a legitimate path: got %q, want %q", joined, want)
	}
}

// --- End-to-end: the exact review scenarios via the public Kernel API ---

func TestLedgerResolveRejectsPathTraversal(t *testing.T) {
	kernel := newTestKernel(t)

	// Plant a secret file outside the ledger directory to prove it's never
	// reached, mirroring the review's "/etc/passwd" scenario without
	// touching a real system file.
	secret := filepath.Join(kernel.Root(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-the-ledger"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The ledger dir is ROOT/.cog/ledger; four '..' from a path segment
	// under it reaches ROOT, matching the depth of the review's
	// '../../../../etc/passwd' example relative to a nested ledger tree.
	resource, err := kernel.Resolve("cog:ledger/../../../../secret.txt")
	if err == nil {
		t.Fatalf("Resolve returned no error; resource=%+v (content=%q) — traversal was not blocked", resource, string(resource.Content))
	}
	if resource != nil && strings.Contains(string(resource.Content), "outside-the-ledger") {
		t.Fatalf("Resolve leaked file contents outside the ledger directory: %q", string(resource.Content))
	}
}

func TestLedgerMutateRejectsPathTraversal(t *testing.T) {
	kernel := newTestKernel(t)

	evilTarget := filepath.Join(filepath.Dir(kernel.Root()), "evil-from-mutate.jsonl")
	defer os.Remove(evilTarget)

	mutation := NewAppendMutation([]byte(`{"type":"message","data":{}}`))
	err := kernel.Mutate("cog:ledger/../../../../evil-from-mutate", mutation)
	if err == nil {
		t.Fatalf("Mutate returned no error — traversal was not blocked")
	}

	if _, statErr := os.Stat(evilTarget); statErr == nil {
		t.Fatalf("Mutate created a file outside the ledger tree at %q", evilTarget)
	}
}

// TestLedgerProjectorDirectResolveRejectsTraversal exercises the second
// defense layer (sanitizeRelPath at the filesystem-join site) in isolation,
// by constructing a ParsedURI directly rather than through ParseURI. This
// is the scenario that would matter if the first layer (ParseURI's '..'
// rejection) were ever removed, bypassed, or not applied to some future
// caller of the projector.
func TestLedgerProjectorDirectResolveRejectsTraversal(t *testing.T) {
	kernel := newTestKernel(t)

	secret := filepath.Join(kernel.Root(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-the-ledger"), 0o644); err != nil {
		t.Fatal(err)
	}

	proj, ok := kernel.projectors["ledger"]
	if !ok {
		t.Fatal("ledger projector not registered")
	}

	uri := &ParsedURI{
		Namespace: "ledger",
		Path:      "../../../../secret.txt",
		Raw:       "cog:ledger/../../../../secret.txt",
	}

	resource, err := proj.Resolve(context.Background(), uri)
	if resource != nil && strings.Contains(string(resource.Content), "outside-the-ledger") {
		t.Fatalf("ledgerProjector.Resolve leaked file contents outside the ledger directory via a hand-built ParsedURI: %q", string(resource.Content))
	}
	// With every '..' segment sanitized to the literal component ".%2E",
	// the resulting path never matches an existing file, so Resolve should
	// report not-found rather than serving anything.
	if err == nil {
		t.Fatalf("ledgerProjector.Resolve(%+v) = %+v, nil; want a not-found error", uri, resource)
	}
}

// TestLedgerProjectorDirectMutateRejectsTraversal is the Mutate-side
// counterpart of the above, targeting appendEvent directly.
func TestLedgerProjectorDirectMutateRejectsTraversal(t *testing.T) {
	kernel := newTestKernel(t)

	evilTarget := filepath.Join(filepath.Dir(kernel.Root()), "evil-from-direct-mutate.jsonl")
	defer os.Remove(evilTarget)

	proj, ok := kernel.projectors["ledger"]
	if !ok {
		t.Fatal("ledger projector not registered")
	}

	uri := &ParsedURI{
		Namespace: "ledger",
		Path:      "../../../../evil-from-direct-mutate",
		Raw:       "cog:ledger/../../../../evil-from-direct-mutate",
	}
	mutation := NewAppendMutation([]byte(`{"type":"message","data":{}}`))

	if err := proj.Mutate(context.Background(), uri, mutation); err != nil {
		t.Fatalf("ledgerProjector.Mutate unexpected error: %v", err)
	}

	if _, statErr := os.Stat(evilTarget); statErr == nil {
		t.Fatalf("ledgerProjector.Mutate created a file outside the ledger tree at %q via a hand-built ParsedURI", evilTarget)
	}

	// Confirm the write actually landed safely inside the ledger tree
	// (sessionID "..", the first path segment, sanitizes to the literal
	// component ".%2E") rather than silently failing, i.e. the fix
	// confines the write, it doesn't just swallow it.
	confined := filepath.Join(kernel.CogDir(), "ledger", ".%2E", "events.jsonl")
	if _, statErr := os.Stat(confined); statErr != nil {
		t.Fatalf("expected sanitized write at %q, got stat error: %v", confined, statErr)
	}
}

// TestLedgerRoundTripStillWorksForLegitimateSessionIDs guards against the
// traversal fix being overly strict: an ordinary, non-hostile session ID
// must still write and read back correctly through the public Kernel API.
func TestLedgerRoundTripStillWorksForLegitimateSessionIDs(t *testing.T) {
	kernel := newTestKernel(t)

	mutation := NewAppendMutation([]byte(`{"type":"message","source":"test","data":{"role":"user","content":"hi"}}`))
	if err := kernel.Mutate("cog:ledger/session-abc123", mutation); err != nil {
		t.Fatalf("Mutate(cog:ledger/session-abc123) unexpected error: %v", err)
	}

	eventsPath := filepath.Join(kernel.CogDir(), "ledger", "session-abc123", "events.jsonl")
	content, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("expected events file at %q: %v", eventsPath, err)
	}
	if !strings.Contains(string(content), `"role":"user"`) {
		t.Errorf("events file missing expected content: %s", content)
	}

	resource, err := kernel.Resolve("cog:ledger/session-abc123/events.jsonl")
	if err != nil {
		t.Fatalf("Resolve(cog:ledger/session-abc123/events.jsonl) unexpected error: %v", err)
	}
	if !strings.Contains(string(resource.Content), `"role":"user"`) {
		t.Errorf("resolved resource missing expected content: %s", resource.Content)
	}
}
