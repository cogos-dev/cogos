// uri_registry_integration_test.go — end-to-end tests for the alias subsystem
// exercised through the full ResolveURI → URIRegistry chain.
//
// These tests validate the complete alias→canonical resolution pipeline and
// serve as the integration test suite for #167.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/myrgic/cogos/pkg/alias"
)

// ── Fixture ───────────────────────────────────────────────────────────────────

// newIntegrationEnv creates a complete isolated environment:
//   - nodeDir with global.yaml
//   - two workspace roots with .cog/ sub-trees
//   - URIRegistry swapped for a test-scoped registry (restored in t.Cleanup)
func newIntegrationEnv(t *testing.T) (nd, wsA, wsB string) {
	t.Helper()
	base := t.TempDir()
	nd = filepath.Join(base, "node")
	if err := os.MkdirAll(nd, 0755); err != nil {
		t.Fatal(err)
	}

	wsA = filepath.Join(base, "workspace-a")
	wsB = filepath.Join(base, "workspace-b")
	for _, ws := range []string{wsA, wsB} {
		for _, sub := range []string{
			filepath.Join(".cog", "mem", "semantic"),
			filepath.Join(".cog", "adr"),
			filepath.Join(".cog", "config"),
		} {
			if err := os.MkdirAll(filepath.Join(ws, sub), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}

	global := "version: \"1.0\"\nworkspaces:\n" +
		"  workspace-a:\n    path: " + wsA + "\n" +
		"  workspace-b:\n    path: " + wsB + "\n"
	if err := os.WriteFile(filepath.Join(nd, "global.yaml"), []byte(global), 0644); err != nil {
		t.Fatal(err)
	}

	testReg := &uriRegistryImpl{nodeDirFn: func() string { return nd }}
	origReg := URIRegistry
	URIRegistry = testReg
	t.Cleanup(func() { URIRegistry = origReg })

	return nd, wsA, wsB
}

func writeAlias(t *testing.T, nd, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// ── alias→canonical rewrite on resolve ───────────────────────────────────────

// TestAliasCanonicalRewriteOnResolve verifies that a cogdoc ref written with an
// alias authority resolves to the canonical workspace filesystem path.
func TestAliasCanonicalRewriteOnResolve(t *testing.T) {
	nd, wsA, _ := newIntegrationEnv(t)

	writeAlias(t, nd, "version: \"1.0\"\naliases:\n  ws: workspace-a\n")

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "insight.cog.md")
	if err := os.WriteFile(doc, []byte("# insight"), 0644); err != nil {
		t.Fatal(err)
	}

	content, err := URIRegistry.Resolve(context.Background(), "cog://ws/mem/semantic/insight.cog.md")
	if err != nil {
		t.Fatalf("Resolve via alias: %v", err)
	}
	path, ok := content.Metadata["path"].(string)
	if !ok {
		t.Fatal("path metadata missing")
	}
	if !strings.HasPrefix(path, wsA) {
		t.Errorf("expected path under workspace-a (%s), got %q", wsA, path)
	}
}

// ── alias collision with projection namespace ─────────────────────────────────

// TestAliasCollisionWithProjection verifies alias.Add rejects reserved
// projection names so they can never shadow local resolution.
func TestAliasCollisionWithProjectionIntegration(t *testing.T) {
	nd, _, _ := newIntegrationEnv(t)

	m, err := alias.Load(nd)
	if err != nil {
		t.Fatalf("alias.Load: %v", err)
	}
	// "adr" is a reserved projection namespace.
	if err := m.Add("adr", "workspace-a", alias.AliasOpts{}); err == nil {
		t.Fatal("expected error: 'adr' is a reserved projection name")
	}
	// "mem" too.
	if err := m.Add("mem", "workspace-b", alias.AliasOpts{}); err == nil {
		t.Fatal("expected error: 'mem' is a reserved projection name")
	}
}

// ── alias name == workspace name ──────────────────────────────────────────────

// TestAliasNameMatchesWorkspaceName verifies that when an alias name equals a
// registered workspace name, the system loads without error and alias wins at
// resolution time (the target is the canonical workspace path).
func TestAliasNameMatchesWorkspaceName(t *testing.T) {
	nd, wsA, _ := newIntegrationEnv(t)

	writeAlias(t, nd, "version: \"1.0\"\naliases:\n  workspace-a: workspace-a\n")

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "x.cog.md")
	if err := os.WriteFile(doc, []byte("# x"), 0644); err != nil {
		t.Fatal(err)
	}

	content, err := URIRegistry.Resolve(context.Background(), "cog://workspace-a/mem/semantic/x.cog.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// ── stale alias resolution error ─────────────────────────────────────────────

// TestStaleAliasResolution verifies that a URI resolved through a stale alias
// (target workspace absent from global.yaml) returns an error, not a
// silently-wrong path.
func TestStaleAliasResolution(t *testing.T) {
	nd, _, _ := newIntegrationEnv(t)

	writeAlias(t, nd, "version: \"1.0\"\naliases:\n  ghost: vanished-workspace\n")

	_, err := URIRegistry.Resolve(context.Background(), "cog://ghost/mem/semantic/test.cog.md")
	if err == nil {
		t.Fatal("expected error for stale alias, got nil")
	}
	if !strings.Contains(err.Error(), "vanished-workspace") {
		t.Errorf("error should name the missing workspace; got: %v", err)
	}
}

// ── ADR-067 grammar ───────────────────────────────────────────────────────────

// TestADR067BareForm verifies that bare cog:path resolves against the local
// workspace, bypassing alias lookup (per ADR-067: no // → always local).
func TestADR067BareForm(t *testing.T) {
	_, wsA, _ := newIntegrationEnv(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "bare.cog.md")
	if err := os.WriteFile(doc, []byte("# bare"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COG_ROOT", wsA)

	content, err := URIRegistry.Resolve(context.Background(), "cog:mem/semantic/bare.cog.md")
	if err != nil {
		t.Fatalf("Resolve bare form: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// TestADR067AuthorityForm verifies that cog://workspace/path resolves via the
// registry (cross-workspace route).
func TestADR067AuthorityForm(t *testing.T) {
	_, wsA, _ := newIntegrationEnv(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "auth.cog.md")
	if err := os.WriteFile(doc, []byte("# auth"), 0644); err != nil {
		t.Fatal(err)
	}

	content, err := URIRegistry.Resolve(context.Background(), "cog://workspace-a/mem/semantic/auth.cog.md")
	if err != nil {
		t.Fatalf("Resolve authority form: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// ── concurrent alias add ──────────────────────────────────────────────────────

// TestConcurrentAliasAddIntegration verifies that two parallel alias.Add calls
// both succeed and neither loses its write (serialised via filelock).
func TestConcurrentAliasAddIntegration(t *testing.T) {
	nd, _, _ := newIntegrationEnv(t)

	const n = 2
	maps := make([]*alias.AliasMap, n)
	for i := 0; i < n; i++ {
		m, err := alias.Load(nd)
		if err != nil {
			t.Fatalf("Load[%d]: %v", i, err)
		}
		maps[i] = m
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	go func() {
		defer wg.Done()
		errs[0] = maps[0].Add("c1", "workspace-a", alias.AliasOpts{})
	}()
	go func() {
		defer wg.Done()
		errs[1] = maps[1].Add("c2", "workspace-b", alias.AliasOpts{})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	final, err := alias.Load(nd)
	if err != nil {
		t.Fatalf("Load final: %v", err)
	}
	if _, _, ok := final.Expand("c1"); !ok {
		t.Error("c1 missing after concurrent add")
	}
	if _, _, ok := final.Expand("c2"); !ok {
		t.Error("c2 missing after concurrent add")
	}
}

// ── Bug 1 regression: ResolveURI must delegate to URIRegistry ────────────────

// TestResolveURIViaRegistry is the load-bearing test that would have caught Bug
// 1.  It calls ResolveURI directly (not URIRegistry.Resolve) with a
// cross-workspace cog://authority/... URI and verifies that the result
// round-trips through the registry to the correct workspace filesystem path.
//
// Before the fix, ResolveURI returned ErrUnknownAuthority immediately for any
// non-projection authority, so callers like PatchAndSync always failed for
// cross-workspace refs even when URIRegistry was wired.
func TestResolveURIViaRegistry(t *testing.T) {
	_, wsA, _ := newIntegrationEnv(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "regrtest.cog.md")
	if err := os.WriteFile(doc, []byte("# regr"), 0644); err != nil {
		t.Fatal(err)
	}

	// Call ResolveURI directly — the path that PatchAndSync, cogdoc_service,
	// and other kernel callers all use.  This must NOT return ErrUnknownAuthority
	// now that URIRegistry is wired into the fallback chain.
	res, err := ResolveURI(wsA, "cog://workspace-a/mem/semantic/regrtest.cog.md")
	if err != nil {
		t.Fatalf("ResolveURI with cross-workspace URI via registry: %v", err)
	}
	if res.Path != doc {
		t.Errorf("path: got %q, want %q", res.Path, doc)
	}
}

// TestResolveURIViaRegistryWithFragment verifies that the fragment is
// preserved through the ResolveURI → URIRegistry → projection chain.
func TestResolveURIViaRegistryWithFragment(t *testing.T) {
	_, wsA, _ := newIntegrationEnv(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "frag.cog.md")
	if err := os.WriteFile(doc, []byte("# frag"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveURI(wsA, "cog://workspace-a/mem/semantic/frag.cog.md#Section")
	if err != nil {
		t.Fatalf("ResolveURI with fragment: %v", err)
	}
	if res.Path != doc {
		t.Errorf("path: got %q, want %q", res.Path, doc)
	}
	if res.Fragment != "Section" {
		t.Errorf("fragment: got %q, want %q", res.Fragment, "Section")
	}
}

// ── Bug 2 regression: digest must not include the fragment text ───────────────

// TestDigestWithFragmentParseOrder is the regression test for Bug 2.
// Before the fix, ?digest=<hex>#frag would fold "#frag" into the digest hex,
// causing digest verification to fail even for a correct file (or silently
// pass with a corrupt digest value on some inputs).
//
// The test verifies:
//  1. A URI with ?digest=<correct-hex>#frag successfully verifies the file.
//  2. A URI with ?digest=<wrong-hex>#frag returns a mismatch error.
//  3. A URI with ?digest=<correct-hex-with-fragment-appended> returns mismatch
//     (proving that the fragment text is NOT included in the digest lookup).
func TestDigestWithFragmentParseOrder(t *testing.T) {
	nd, wsA, _ := newIntegrationEnv(t)

	// Write a file and compute its correct digest.
	content := []byte("digest-order-test content\n")
	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "digest_frag.cog.md")
	if err := os.WriteFile(doc, content, 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(content)
	correctHex := hex.EncodeToString(h[:])

	// Register wsA in node dir so the registry can resolve it.
	_ = nd // already registered by newIntegrationEnv

	// Case 1: correct digest + fragment — must succeed.
	uri := "cog://workspace-a/mem/semantic/digest_frag.cog.md?digest=" + correctHex + "#Section"
	ctx := context.Background()
	got, err := URIRegistry.Resolve(ctx, uri)
	if err != nil {
		t.Fatalf("case1 correct digest+frag: unexpected error: %v", err)
	}
	if path, ok := got.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("case1: path %v, want %q", got.Metadata["path"], doc)
	}
	if frag, ok := got.Metadata["fragment"].(string); !ok || frag != "Section" {
		t.Errorf("case1: fragment %v, want %q", got.Metadata["fragment"], "Section")
	}

	// Case 2: wrong digest — must error (fail-closed).
	badURI := "cog://workspace-a/mem/semantic/digest_frag.cog.md?digest=deadbeef#Section"
	_, err = URIRegistry.Resolve(ctx, badURI)
	if err == nil {
		t.Fatal("case2: expected digest mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("case2: error %q should contain 'digest mismatch'", err.Error())
	}

	// Case 3: digest value that would only match if fragment was erroneously
	// folded in (i.e. the pre-fix behaviour).  The digest is computed over
	// correctHex + "#Section" — this must NOT verify successfully.
	poisonedHex := func() string {
		p := sha256.Sum256([]byte(correctHex + "#Section"))
		return hex.EncodeToString(p[:])
	}()
	poisonedURI := "cog://workspace-a/mem/semantic/digest_frag.cog.md?digest=" + poisonedHex + "#Section"
	_, err = URIRegistry.Resolve(ctx, poisonedURI)
	if err == nil {
		t.Fatal("case3: poisoned digest (fragment folded in) must not pass — parse order bug still present")
	}
}
