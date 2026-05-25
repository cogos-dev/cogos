// serve_g3_test.go — G3: identity-embedding tests for spawn embodiment (Part A)
// and memory-scope foveation (Part B).
//
// Test matrix (three top-level scenarios as specified in the directive):
//
//  1. flag OFF, bound (with CRD providing WorkspaceRoot + MemoryNamespace)
//     → req.WorkDir empty (flag off = no-regression)
//     → nucleus card still present (flag off = no-regression)
//
//  2. flag ON, bound (CRD supplying WorkspaceRoot + MemoryNamespace)
//     → BoundIdentity.WorkspaceRoot / MemoryNamespace populated
//     → req.WorkDir resolves to a filesystem path
//     → WithMemoryScope applied (MemoryNamespace non-empty → scoped filter)
//
//  3. flag ON, unbound
//     → req.WorkDir is a non-empty temp directory (neutral dir)
//     → no memory scope applied
//
// Additional unit tests:
//   - resolveIdentityExpression: CRD present / absent / wildcard fallback.
//   - resolveWorkspaceRootPath: valid URI → path; empty URI → "".
//   - resolveMemoryNamespacePrefix: valid URI → path; empty → "".
//   - WithMemoryScope functional option: sets memoryNamespace field.
package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// makeWorkspaceWithIdentity extends makeWorkspace with an Identity CRD for the
// given subject. The CRD has a "kernel" expression carrying the supplied
// workspaceRootURI and memoryNamespace. Identity CRDs live at
// {root}/.cog/config/identities/{subject}.yaml.
func makeWorkspaceWithIdentity(t *testing.T, subject, workspaceRootURI, memNS string) string {
	t.Helper()
	root := makeWorkspace(t)

	idDir := filepath.Join(root, ".cog", "config", "identities")
	if err := os.MkdirAll(idDir, 0755); err != nil {
		t.Fatalf("makeWorkspaceWithIdentity: mkdir %s: %v", idDir, err)
	}

	// Minimal Identity CRD YAML.
	crdYAML := "apiVersion: cog.os/v1alpha1\nkind: Identity\n" +
		"metadata:\n  name: " + subject + "\n" +
		"spec:\n  iss: kernel\n  sub: " + subject + "\n  type: agent\n" +
		"  expressions:\n" +
		"  - aud: kernel\n" +
		"    display_name: " + subject + "\n" +
		"    workspace_root: " + workspaceRootURI + "\n" +
		"    memory_namespace: " + memNS + "\n"

	writeTestFile(t, filepath.Join(idDir, subject+".yaml"), crdYAML)
	return root
}

// bindSessionWithCRD registers a session → subject binding on fake and returns
// the workspace root that contains an Identity CRD for that subject.
func bindSessionWithCRD(
	t *testing.T,
	fake *fakeHarnessAttacher,
	sessionID, subject, workspaceRootURI, memNS string,
) string {
	t.Helper()
	root := makeWorkspaceWithIdentity(t, subject, workspaceRootURI, memNS)
	fake.AttachHarness(&subidentity.HarnessBindingCRD{
		Spec: subidentity.HarnessBindingSpec{
			SessionID: sessionID,
			Subject:   subject,
			Type:      "agent",
		},
	})
	return root
}

// newG3TestServer builds a Server configured for G3 tests.
// The workspace root must already be set up by the caller.
// nakedDefault sets cfg.IdentityNakedDefault.
func newG3TestServer(t *testing.T, root string, nakedDefault bool) (*Server, *StubProvider, *fakeHarnessAttacher) {
	t.Helper()
	cfg := makeConfig(t, root)
	cfg.IdentityNakedDefault = nakedDefault
	nucleus := makeNucleus("TestNucleus", "test-role")
	process := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, process)

	stub := NewStubProvider("stub", "reply")
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	fake := newFakeHarnessAttacher()
	srv.harnessBackend = fake

	return srv, stub, fake
}

// ─── Scenario 1: flag OFF, bound (full CRD present) → no-regression ──────────

// TestG3_FlagOff_Bound_WorkDirEmpty verifies that with IdentityNakedDefault=false,
// even when a full Identity CRD is present, CompletionRequest.WorkDir is NOT
// set (flag-off no-regression contract for Part A).
func TestG3_FlagOff_Bound_WorkDirEmpty(t *testing.T) {
	t.Parallel()

	fake := newFakeHarnessAttacher()
	// "TestNucleus" matches the nucleus name, so useFullEmbodiment stays true.
	root := bindSessionWithCRD(t, fake, "sess-nucleus", "TestNucleus",
		"cog://mem/semantic", "cog://mem/semantic/agents/testnucleus/")

	srv, stub, _ := newG3TestServer(t, root, false /* flag OFF */)
	srv.harnessBackend = fake

	w := doChatRequest(t, srv, "sess-nucleus")
	if w.Code != 200 {
		t.Fatalf("flag OFF bound: status %d, want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("flag OFF bound: no lastRequest captured")
	}

	// Flag OFF → WorkDir must be empty (no-regression: Part A).
	if lr.WorkDir != "" {
		t.Errorf("flag OFF bound: WorkDir = %q; want empty (no-regression)", lr.WorkDir)
	}
	// Flag OFF → nucleus card must still be present (no-regression: embodiment).
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("flag OFF bound: nucleus card missing from SystemPrompt (no-regression)")
	}
}

// ─── Scenario 2: flag ON, bound → BoundIdentity enriched + WorkDir set ───────

// TestG3_FlagOn_Bound_BoundIdentityEnriched verifies that resolveBoundIdentity
// populates WorkspaceRoot and MemoryNamespace from the Identity CRD when a CRD
// is present for the bound subject.
func TestG3_FlagOn_Bound_BoundIdentityEnriched(t *testing.T) {
	t.Parallel()

	fake := newFakeHarnessAttacher()
	root := bindSessionWithCRD(t, fake, "sess-sandy", "sandy",
		"cog://mem/semantic/agents/sandy", "cog://mem/semantic/agents/sandy/")

	cfg := makeConfig(t, root)
	nucleus := makeNucleus("TestNucleus", "r")
	srv := NewServer(cfg, nucleus, NewProcess(cfg, nucleus))
	srv.harnessBackend = fake

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Cogos-Session-Id", "sess-sandy")
	bi := srv.resolveBoundIdentity(req, "")

	if !bi.Bound {
		t.Fatal("expected Bound=true")
	}
	if bi.Subject != "sandy" {
		t.Errorf("Subject = %q, want sandy", bi.Subject)
	}
	// CRD provides workspace_root and memory_namespace — both must be non-empty.
	if bi.WorkspaceRoot == "" {
		t.Error("WorkspaceRoot empty; expected CRD-provided URI")
	}
	if bi.MemoryNamespace == "" {
		t.Error("MemoryNamespace empty; expected CRD-provided URI")
	}
}

// TestG3_FlagOn_Bound_WorkDirResolved verifies that with IdentityNakedDefault=true
// and a bound session, creq.WorkDir is set to the resolved fs path of the
// identity's WorkspaceRoot cog:// URI.
func TestG3_FlagOn_Bound_WorkDirResolved(t *testing.T) {
	t.Parallel()

	fake := newFakeHarnessAttacher()
	// "TestNucleus" matches the nucleus name so useFullEmbodiment stays true
	// (full embodiment + G3 enrichment).
	root := bindSessionWithCRD(t, fake, "sess-nucleus-on", "TestNucleus",
		"cog://mem/semantic", "cog://mem/semantic/agents/testnucleus/")

	// Make sure the memory dir exists so resolve has something to stat.
	_ = os.MkdirAll(filepath.Join(root, ".cog", "mem", "semantic"), 0755)

	srv, stub, _ := newG3TestServer(t, root, true /* flag ON */)
	srv.harnessBackend = fake

	w := doChatRequest(t, srv, "sess-nucleus-on")
	if w.Code != 200 {
		t.Fatalf("flag ON bound: status %d, want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("flag ON bound: no lastRequest captured")
	}

	// Flag ON + bound with WorkspaceRoot → WorkDir must be non-empty.
	if lr.WorkDir == "" {
		t.Error("flag ON bound: WorkDir is empty; want resolved fs path")
	}
	// Resolved path must be under the workspace root.
	if !strings.HasPrefix(lr.WorkDir, root) {
		t.Errorf("flag ON bound: WorkDir = %q, want prefix %q", lr.WorkDir, root)
	}
}

// ─── Scenario 3: flag ON, unbound → neutral temp WorkDir ────────────────────

// TestG3_FlagOn_Unbound_NeutralWorkDir verifies that with IdentityNakedDefault=true
// and no session header (unbound), creq.WorkDir is set to a non-empty temp
// directory that is NOT the workspace root (so no workspace CLAUDE.md loads).
func TestG3_FlagOn_Unbound_NeutralWorkDir(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	srv, stub, _ := newG3TestServer(t, root, true /* flag ON */)
	// No session binding → unbound.

	w := doChatRequest(t, srv, "")
	if w.Code != 200 {
		t.Fatalf("flag ON unbound: status %d, want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("flag ON unbound: no lastRequest captured")
	}

	// Flag ON + unbound → WorkDir is a neutral temp directory (non-empty).
	if lr.WorkDir == "" {
		t.Error("flag ON unbound: WorkDir is empty; want a neutral temp dir")
	}
	// Neutral temp dir must differ from the workspace root (costume-leak guard).
	if lr.WorkDir == root {
		t.Errorf("flag ON unbound: WorkDir == workspace root %q; costume-leak!", root)
	}
}

// ─── resolveIdentityExpression unit tests ────────────────────────────────────

// TestResolveIdentityExpression_Present verifies that a well-formed CRD on disk
// is loaded and the "kernel" expression returned.
func TestResolveIdentityExpression_Present(t *testing.T) {
	t.Parallel()

	root := makeWorkspaceWithIdentity(t, "alice",
		"cog://mem/semantic", "cog://mem/semantic/agents/alice/")

	expr := resolveIdentityExpression(root, "alice", "kernel")
	if expr == nil {
		t.Fatal("expected non-nil expression for existing CRD")
	}
	if expr.WorkspaceRoot != "cog://mem/semantic" {
		t.Errorf("WorkspaceRoot = %q, want cog://mem/semantic", expr.WorkspaceRoot)
	}
	if expr.MemoryNamespace != "cog://mem/semantic/agents/alice/" {
		t.Errorf("MemoryNamespace = %q, want cog://mem/semantic/agents/alice/",
			expr.MemoryNamespace)
	}
}

// TestResolveIdentityExpression_Missing verifies that a missing CRD returns nil
// (minimal binding, no error).
func TestResolveIdentityExpression_Missing(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t) // no identity CRD for "ghost"
	expr := resolveIdentityExpression(root, "ghost", "kernel")
	if expr != nil {
		t.Errorf("missing CRD: expected nil, got %+v", expr)
	}
}

// TestResolveIdentityExpression_WildcardFallback verifies that when the CRD
// has a "*" expression but no "kernel" expression, ExpressionFor returns the
// wildcard.
func TestResolveIdentityExpression_WildcardFallback(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	idDir := filepath.Join(root, ".cog", "config", "identities")
	if err := os.MkdirAll(idDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	crdYAML := "apiVersion: cog.os/v1alpha1\nkind: Identity\n" +
		"metadata:\n  name: wildcard\n" +
		"spec:\n  iss: kernel\n  sub: wildcard\n  type: agent\n" +
		"  expressions:\n" +
		"  - aud: \"*\"\n" +
		"    workspace_root: cog://mem/semantic\n" +
		"    memory_namespace: cog://mem/semantic/agents/wildcard/\n"
	writeTestFile(t, filepath.Join(idDir, "wildcard.yaml"), crdYAML)

	expr := resolveIdentityExpression(root, "wildcard", "kernel")
	if expr == nil {
		t.Fatal("expected wildcard expression, got nil")
	}
	if expr.WorkspaceRoot != "cog://mem/semantic" {
		t.Errorf("WorkspaceRoot = %q, want cog://mem/semantic", expr.WorkspaceRoot)
	}
}

// ─── resolveWorkspaceRootPath unit tests ─────────────────────────────────────

// TestResolveWorkspaceRootPath_ValidURI verifies that a cog:mem URI resolves
// to a path under the workspace root.
func TestResolveWorkspaceRootPath_ValidURI(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)

	got := resolveWorkspaceRootPath(root, "cog://mem/semantic")
	if got == "" {
		t.Fatal("expected non-empty path for cog://mem/semantic")
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("resolved path %q does not start with workspace root %q", got, root)
	}
}

// TestResolveWorkspaceRootPath_EmptyURI verifies that an empty URI returns "".
func TestResolveWorkspaceRootPath_EmptyURI(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)

	got := resolveWorkspaceRootPath(root, "")
	if got != "" {
		t.Errorf("empty URI: expected \"\", got %q", got)
	}
}

// ─── resolveMemoryNamespacePrefix unit tests ─────────────────────────────────

// TestResolveMemoryNamespacePrefix_ValidURI resolves a cog:mem URI to a path.
func TestResolveMemoryNamespacePrefix_ValidURI(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)

	got := resolveMemoryNamespacePrefix(root, "cog://mem/semantic/agents/sandy/")
	if got == "" {
		t.Fatal("expected non-empty prefix")
	}
	if !strings.HasPrefix(got, root) {
		t.Errorf("prefix %q does not start with workspace root %q", got, root)
	}
}

// TestResolveMemoryNamespacePrefix_Empty verifies empty namespace returns "".
func TestResolveMemoryNamespacePrefix_Empty(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)

	got := resolveMemoryNamespacePrefix(root, "")
	if got != "" {
		t.Errorf("empty namespace: expected \"\", got %q", got)
	}
}

// ─── WithMemoryScope functional-option unit tests ────────────────────────────

// TestWithMemoryScope_SetsField verifies that WithMemoryScope sets the
// memoryNamespace field on assembleOpts.
func TestWithMemoryScope_SetsField(t *testing.T) {
	t.Parallel()

	ao := assembleDefaults()
	WithMemoryScope("cog://mem/semantic/agents/sandy/")(&ao)
	if ao.memoryNamespace != "cog://mem/semantic/agents/sandy/" {
		t.Errorf("memoryNamespace = %q, want cog://mem/semantic/agents/sandy/",
			ao.memoryNamespace)
	}
}

// TestWithMemoryScope_Empty verifies that passing "" is a no-op.
func TestWithMemoryScope_Empty(t *testing.T) {
	t.Parallel()

	ao := assembleDefaults()
	WithMemoryScope("")(&ao)
	if ao.memoryNamespace != "" {
		t.Errorf("memoryNamespace = %q after empty arg; want \"\"", ao.memoryNamespace)
	}
}
