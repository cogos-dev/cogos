// pin_adapter_test.go — integration tests for the production pin locator adapter.
//
// These tests exercise the full adapter path:
//
//	engine.ResolveWorkspacePath (production adapter) →
//	uriRegistryImpl.resolveAuthority (direct name lookup) →
//	lookupWorkspaceRoot (reads global.yaml)
//
// They are the tests that would have caught the bug reported in the Codex
// review of PR #180 (#175 fixup): ResolveWorkspacePath was building
// "cog://"+name and calling URIRegistry.Resolve, which splits the authority
// at the first slash, so "myrgic/cogos" was looked up as "myrgic"
// instead of the full slash-bearing key.
//
// These tests must fail on the pre-fix code (Resolve round-trip) and pass
// on the post-fix code (direct resolveAuthority call).
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/internal/providers/pin"
)

// realAdapterLocator adapts engine.ResolveWorkspacePath to pin.WorkspaceLocator.
// This is the same shape as cmd/cogos/providers_wire.go:uriRegistryLocatorAdapter.
// Defined here (package engine) so tests can swap URIRegistry and observe the
// real adapter path without export tricks.
type realAdapterLocator struct{}

func (a *realAdapterLocator) LocateWorkspace(ctx context.Context, name string) (string, error) {
	path, err := ResolveWorkspacePath(ctx, name)
	if err != nil {
		return "", pin.ErrWorkspaceNotFound
	}
	return path, nil
}

// setupSlashNameRegistry writes a global.yaml with "myrgic/cogos" as a
// workspace, swaps URIRegistry for the duration of the test, creates the
// workspace directory, and returns its path.
func setupSlashNameRegistry(t *testing.T) (targetDir string) {
	t.Helper()
	base := t.TempDir()
	nd := filepath.Join(base, "node")
	if err := os.MkdirAll(nd, 0755); err != nil {
		t.Fatal(err)
	}

	targetDir = filepath.Join(base, "myrgic", "cogos")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	global := "version: \"1.0\"\nworkspaces:\n" +
		"  myrgic/cogos:\n    path: " + targetDir + "\n"
	if err := os.WriteFile(filepath.Join(nd, "global.yaml"), []byte(global), 0644); err != nil {
		t.Fatal(err)
	}

	testReg := &uriRegistryImpl{nodeDirFn: func() string { return nd }}
	orig := URIRegistry
	URIRegistry = testReg
	t.Cleanup(func() { URIRegistry = orig })

	return targetDir
}

// TestPinAdapter_SlashBearingWorkspaceName is the load-bearing regression test
// for the Codex finding on PR #180.
//
// It wires the real engine.ResolveWorkspacePath adapter (not a stub) against a
// registry containing "myrgic/cogos" (slash-bearing), creates a minimal git
// repo at that path, and asserts that pin FetchLive resolves HEAD without
// error.
//
// Without the fix, ResolveWorkspacePath calls URIRegistry.Resolve("cog://myrgic/cogos"),
// which splits authority at "/" → looks up "myrgic" → workspace not found →
// returns ErrUnknownAuthority → pin.LocateWorkspace returns ErrWorkspaceNotFound
// → FetchLive falls back to sibling scan → no sibling found → error.
//
// With the fix, ResolveWorkspacePath calls resolveAuthority("myrgic/cogos")
// directly → global.yaml lookup succeeds → returns targetDir → FetchLive resolves HEAD.
func TestPinAdapter_SlashBearingWorkspaceName(t *testing.T) {
	targetDir := setupSlashNameRegistry(t)

	// Initialise a real git repo so git rev-parse HEAD succeeds.
	for _, args := range [][]string{
		{"git", "-C", targetDir, "init"},
		{"git", "-C", targetDir, "config", "user.email", "test@example.com"},
		{"git", "-C", targetDir, "config", "user.name", "Test"},
		{"git", "-C", targetDir, "commit", "--allow-empty", "-m", "initial"},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Skipf("git setup failed (%v): %s — skipping pin adapter integration test", err, out)
		}
	}

	// Set up source workspace with a pin targeting "myrgic/cogos".
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, ".cog", "pins"), 0755); err != nil {
		t.Fatal(err)
	}
	pinYAML := "target: myrgic/cogos\npin:\n  ref: abc1234567890abcdef1234567890abcdef123456\nsync: read-only\n"
	if err := os.WriteFile(filepath.Join(source, ".cog", "pins", "myrgic_cogos.yaml"), []byte(pinYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Wire the real adapter — same shape as production cmd/cogos/providers_wire.go.
	p := pin.New(nil)
	p.SetWorkspaceLocator(&realAdapterLocator{})

	cfgAny, err := p.LoadConfig(source)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	liveAny, err := p.FetchLive(context.Background(), cfgAny)
	if err != nil {
		t.Fatalf("FetchLive with real adapter on slash-bearing name: %v", err)
	}

	// Verify the plan computes without panic — confirms liveAny is well-formed.
	planAny, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if planAny == nil {
		t.Fatal("ComputePlan returned nil plan")
	}
}
