package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// ── test fixtures ─────────────────────────────────────────────────────────────

// testNodeSetup creates a temporary node dir with global.yaml and returns
// the node dir path plus two workspace roots (wsA and wsB) on disk.
func testNodeSetup(t *testing.T) (nd string, wsA string, wsB string, reg *uriRegistryImpl) {
	t.Helper()
	base := t.TempDir()
	nd = filepath.Join(base, "node")
	if err := os.MkdirAll(nd, 0755); err != nil {
		t.Fatal(err)
	}

	wsA = filepath.Join(base, "ws-a")
	wsB = filepath.Join(base, "ws-b")
	for _, ws := range []string{wsA, wsB} {
		for _, sub := range []string{
			filepath.Join(".cog", "mem", "semantic"),
			filepath.Join(".cog", "adr"),
		} {
			if err := os.MkdirAll(filepath.Join(ws, sub), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}

	global := "version: \"1.0\"\nworkspaces:\n" +
		"  ws-a:\n    path: " + wsA + "\n" +
		"  ws-b:\n    path: " + wsB + "\n"
	if err := os.WriteFile(filepath.Join(nd, "global.yaml"), []byte(global), 0644); err != nil {
		t.Fatal(err)
	}

	reg = &uriRegistryImpl{nodeDirFn: func() string { return nd }}
	return
}

// fileDigest computes the sha256 hex digest of the file at path.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ── parseGlobalYAML ───────────────────────────────────────────────────────────

func TestParseGlobalYAML(t *testing.T) {
	yaml := `version: "1.0"
workspaces:
  cog-workspace:
    path: /home/user/workspaces/cog
  myrgic/cogos:
    path: /home/user/workspaces/cogos
`
	got, err := parseGlobalYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("parseGlobalYAML: %v", err)
	}
	if got["cog-workspace"] != "/home/user/workspaces/cog" {
		t.Errorf("cog-workspace: got %q", got["cog-workspace"])
	}
	if got["myrgic/cogos"] != "/home/user/workspaces/cogos" {
		t.Errorf("myrgic/cogos: got %q", got["myrgic/cogos"])
	}
}

func TestParseGlobalYAMLEmpty(t *testing.T) {
	got, err := parseGlobalYAML([]byte("version: \"1.0\"\n"))
	if err != nil {
		t.Fatalf("parseGlobalYAML empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// ── isProjectionNamespace ─────────────────────────────────────────────────────

func TestIsProjectionNamespace(t *testing.T) {
	for _, ns := range []string{"mem", "adr", "ledger", "conf", "config", "roles", "agents", "kernel"} {
		if !isProjectionNamespace(ns) {
			t.Errorf("expected %q to be a projection namespace", ns)
		}
	}
	for _, ns := range []string{"cog", "my-workspace", "cogos-dev-cogos", "primary-ws"} {
		if isProjectionNamespace(ns) {
			t.Errorf("expected %q NOT to be a projection namespace", ns)
		}
	}
}

// ── resolveAuthority ─────────────────────────────────────────────────────────

func TestResolveAuthorityDirect(t *testing.T) {
	nd, wsA, _, reg := testNodeSetup(t)
	root, err := reg.resolveAuthority(nd, "ws-a")
	if err != nil {
		t.Fatalf("resolveAuthority: %v", err)
	}
	if root != wsA {
		t.Errorf("got %q, want %q", root, wsA)
	}
}

func TestResolveAuthorityViaAlias(t *testing.T) {
	nd, wsA, _, reg := testNodeSetup(t)

	aliasYAML := "version: \"1.0\"\naliases:\n  alpha: ws-a\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	root, err := reg.resolveAuthority(nd, "alpha")
	if err != nil {
		t.Fatalf("resolveAuthority via alias: %v", err)
	}
	if root != wsA {
		t.Errorf("got %q, want %q", root, wsA)
	}
}

func TestResolveAuthorityUnknown(t *testing.T) {
	nd, _, _, reg := testNodeSetup(t)
	_, err := reg.resolveAuthority(nd, "nonexistent-workspace")
	if err == nil {
		t.Fatal("expected error for unknown authority, got nil")
	}
}

// ── Resolve: cross-workspace ──────────────────────────────────────────────────

func TestResolveCrossWorkspace(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "insight.cog.md")
	if err := os.WriteFile(doc, []byte("# insight"), 0644); err != nil {
		t.Fatal(err)
	}

	content, err := reg.Resolve(context.Background(), "cog://ws-a/mem/semantic/insight.cog.md")
	if err != nil {
		t.Fatalf("Resolve cross-workspace: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// ── Resolve: alias → workspace ────────────────────────────────────────────────

func TestResolveAliasToWorkspace(t *testing.T) {
	nd, _, wsB, reg := testNodeSetup(t)

	aliasYAML := "version: \"1.0\"\naliases:\n  beta: ws-b\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	doc := filepath.Join(wsB, ".cog", "mem", "semantic", "note.cog.md")
	if err := os.WriteFile(doc, []byte("# note"), 0644); err != nil {
		t.Fatal(err)
	}

	content, err := reg.Resolve(context.Background(), "cog://beta/mem/semantic/note.cog.md")
	if err != nil {
		t.Fatalf("Resolve alias→workspace: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// ── Resolve: digest verification ──────────────────────────────────────────────

func TestResolveDigestGood(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "dgst.cog.md")
	if err := os.WriteFile(doc, []byte("# digest test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	good := fileDigest(t, doc)

	uri := "cog://ws-a/mem/semantic/dgst.cog.md?digest=" + good
	c, err := reg.Resolve(context.Background(), uri)
	if err != nil {
		t.Fatalf("Resolve good digest: %v", err)
	}
	if c == nil {
		t.Fatal("expected content, got nil")
	}
}

func TestResolveDigestBad(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "baddgst.cog.md")
	if err := os.WriteFile(doc, []byte("# content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	uri := "cog://ws-a/mem/semantic/baddgst.cog.md?digest=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	_, err := reg.Resolve(context.Background(), uri)
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}
}

// ── Resolve: non-cog URI ──────────────────────────────────────────────────────

func TestResolveNonCogURI(t *testing.T) {
	_, _, _, reg := testNodeSetup(t)
	_, err := reg.Resolve(context.Background(), "https://example.com/foo")
	if err == nil {
		t.Fatal("expected error for non-cog URI")
	}
}

// ── Resolve: workspace root (no path) ─────────────────────────────────────────

func TestResolveCrossWorkspaceRootOnly(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)
	content, err := reg.Resolve(context.Background(), "cog://ws-a")
	if err != nil {
		t.Fatalf("Resolve root only: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != wsA {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], wsA)
	}
}

// ── ADR-067 grammar: bare cog:path routes through legacy resolver ─────────────

func TestResolveBareLocalForm(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)

	// ADR-067 bare form (no //) resolves against local workspace.
	// Inject workspace root via env var (workspace.resolveWorkspaceUncached tier 1).
	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "local.cog.md")
	if err := os.WriteFile(doc, []byte("# local"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COG_ROOT", wsA)

	content, err := reg.Resolve(context.Background(), "cog:mem/semantic/local.cog.md")
	if err != nil {
		t.Fatalf("Resolve bare form: %v", err)
	}
	if path, ok := content.Metadata["path"].(string); !ok || path != doc {
		t.Errorf("path: got %v, want %q", content.Metadata["path"], doc)
	}
}

// ── Resolve: fragment-before-query rejection (issue #171 follow-up) ───────────

// TestResolveFragmentBeforeQuery_Rejected verifies that URIRegistry.Resolve
// rejects a URI where '#' appears before '?' in the cog://authority form.
//
// PR #176 added the rejection only inside ResolveURI; the registry-direct path
// was not covered.  Without this fix, cog://ws/mem/x#frag?digest=... reaches
// the digest-verification block with digestHex="" because the earlier
// fragment-strip consumed the entire tail including the query string.
func TestResolveFragmentBeforeQuery_Rejected(t *testing.T) {
	_, wsA, _, reg := testNodeSetup(t)

	doc := filepath.Join(wsA, ".cog", "mem", "semantic", "bypass.cog.md")
	if err := os.WriteFile(doc, []byte("# bypass attempt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	digest := fileDigest(t, doc)

	// Fragment-before-query: the '#frag' should NOT cause the digest param to be
	// ignored.  The registry must reject this URI as malformed.
	malformed := "cog://ws-a/mem/semantic/bypass.cog.md#frag?digest=" + digest
	_, err := reg.Resolve(context.Background(), malformed)
	if err == nil {
		t.Fatalf("Resolve(%q): expected error for fragment-before-query, got nil (digest bypass)", malformed)
	}
}

// ── ResolveWorkspacePath: slash-bearing name regression (#175 fixup) ─────────

// testNodeSetupWithSlashName creates a node dir whose global.yaml has a
// slash-bearing workspace name ("myrgic/cogos"), swaps URIRegistry for the
// duration of the test, and returns the node dir and workspace root.
func testNodeSetupWithSlashName(t *testing.T) (nd, wsSlash string) {
	t.Helper()
	base := t.TempDir()
	nd = filepath.Join(base, "node")
	if err := os.MkdirAll(nd, 0755); err != nil {
		t.Fatal(err)
	}

	wsSlash = filepath.Join(base, "myrgic", "cogos")
	if err := os.MkdirAll(wsSlash, 0755); err != nil {
		t.Fatal(err)
	}

	global := "version: \"1.0\"\nworkspaces:\n" +
		"  myrgic/cogos:\n    path: " + wsSlash + "\n"
	if err := os.WriteFile(filepath.Join(nd, "global.yaml"), []byte(global), 0644); err != nil {
		t.Fatal(err)
	}

	testReg := &uriRegistryImpl{nodeDirFn: func() string { return nd }}
	orig := URIRegistry
	URIRegistry = testReg
	t.Cleanup(func() { URIRegistry = orig })

	return nd, wsSlash
}

// TestResolveWorkspacePathSlashBearing is the regression test for the bug found
// by Codex review of PR #180.
//
// Before the fix, ResolveWorkspacePath built "cog://myrgic/cogos" and called
// URIRegistry.Resolve, which split the authority at the first slash and looked
// up workspace "cogos-dev" instead of "myrgic/cogos". The test would have
// returned ErrUnknownAuthority.
//
// After the fix, ResolveWorkspacePath resolves the raw name directly via
// resolveAuthority, bypassing URI authority splitting.
func TestResolveWorkspacePathSlashBearing(t *testing.T) {
	_, wsSlash := testNodeSetupWithSlashName(t)

	got, err := ResolveWorkspacePath(context.Background(), "myrgic/cogos")
	if err != nil {
		t.Fatalf("ResolveWorkspacePath(myrgic/cogos): %v", err)
	}
	if got != wsSlash {
		t.Errorf("path: got %q, want %q", got, wsSlash)
	}
}

// TestResolveWorkspacePathSlashBearing_NotFound verifies that looking up a
// name not in the registry returns ErrUnknownAuthority (not a panic or empty
// string).
func TestResolveWorkspacePathSlashBearing_NotFound(t *testing.T) {
	testNodeSetupWithSlashName(t) // sets up URIRegistry with myrgic/cogos only

	_, err := ResolveWorkspacePath(context.Background(), "myrgic/other-repo")
	if err == nil {
		t.Fatal("expected ErrUnknownAuthority for unknown slash-bearing name, got nil")
	}
}
