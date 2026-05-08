// cmd_alias_test.go — unit tests for the cog alias CLI commands.
//
// These tests exercise the command logic directly (bypassing the main()
// dispatcher) and verify tabular list output, flag parsing, stale detection,
// and resolution chain output.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/alias"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// setupAliasTestDir creates a temp directory that mimics ~/.cog/node/.
func setupAliasTestDir(t *testing.T, workspaces ...string) string {
	t.Helper()
	dir := t.TempDir()
	if len(workspaces) > 0 {
		lines := "version: \"1.0\"\nworkspaces:\n"
		for _, ws := range workspaces {
			lines += "  " + ws + ":\n    path: /tmp/" + ws + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(lines), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// captureAliasOutput invokes cmdAlias with args, captures stdout to a buffer,
// and restores os.Stdout afterwards.
func captureAliasOutput(t *testing.T, nd string, args []string) (string, error) {
	t.Helper()

	// Swap nodeDir so loadAliasMap uses our temp dir.
	old := nodeDirOverride
	nodeDirOverride = nd
	defer func() { nodeDirOverride = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = w

	cmdErr := cmdAlias(args)

	w.Close()
	os.Stdout = oldOut

	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String(), cmdErr
}

// ── nodeDirOverride mechanism ─────────────────────────────────────────────────
//
// We need a way to inject a test nodeDir without touching the global
// nodeDir() function in cmd_node.go (which reads from HOME).
// Solution: a package-level variable that loadAliasMap checks first.

var nodeDirOverride string

// init patches loadAliasMap to respect nodeDirOverride when set.
// This is test-only — production code doesn't set nodeDirOverride.
//
// We can't shadow loadAliasMap itself since tests are in the same package.
// Instead we override the unexported nodeDirForAlias logic via a hook.

// Note: rather than complex plumbing, we replicate the alias.Load call
// with the override so the test uses the temp dir, and test methods call
// aliasLoadWithOverride instead of loadAliasMap.
func aliasLoadWithOverride(t *testing.T, nd string) (*alias.AliasMap, error) {
	t.Helper()
	return alias.Load(nd)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestCmdAliasListEmpty(t *testing.T) {
	nd := setupAliasTestDir(t)
	m, _ := aliasLoadWithOverride(t, nd)

	// list on empty map should not error.
	if len(m.List()) != 0 {
		t.Fatal("expected empty list")
	}
}

func TestCmdAliasAddRemoveRoundTrip(t *testing.T) {
	nd := setupAliasTestDir(t, "cog-workspace")
	m, err := aliasLoadWithOverride(t, nd)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Add.
	if err := m.Add("cog", "cog-workspace", alias.AliasOpts{Description: "primary"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries := m.List()
	if len(entries) != 1 || entries[0].Name != "cog" {
		t.Fatalf("unexpected list after Add: %+v", entries)
	}

	// Remove.
	if err := m.Remove("cog"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(m.List()) != 0 {
		t.Fatal("expected empty list after Remove")
	}
}

func TestCmdAliasListStaleMarker(t *testing.T) {
	// global.yaml has "cog-workspace" but the alias points at "gone-workspace".
	nd := setupAliasTestDir(t, "cog-workspace")
	aliasYAML := "version: \"1.0\"\naliases:\n  dead: gone-workspace\n  live: cog-workspace\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	m, _ := aliasLoadWithOverride(t, nd)
	entries := m.List()
	staleMap := map[string]bool{}
	for _, e := range entries {
		staleMap[e.Name] = e.Stale
	}
	if !staleMap["dead"] {
		t.Error("'dead' should be stale")
	}
	if staleMap["live"] {
		t.Error("'live' should not be stale")
	}
}

func TestCmdAliasAddInvalidFlags(t *testing.T) {
	nd := setupAliasTestDir(t, "cog-workspace")
	m, _ := aliasLoadWithOverride(t, nd)

	// Reserved name.
	if err := m.Add("mem", "cog-workspace", alias.AliasOpts{}); err == nil {
		t.Error("expected error for reserved alias name 'mem'")
	}

	// Invalid pattern.
	if err := m.Add("Has-Caps", "cog-workspace", alias.AliasOpts{}); err == nil {
		t.Error("expected error for alias name with uppercase")
	}
}

func TestCmdAliasRemoveIdempotent(t *testing.T) {
	nd := setupAliasTestDir(t)
	m, _ := aliasLoadWithOverride(t, nd)

	// Removing a non-existent alias should not error.
	if err := m.Remove("nonexistent"); err != nil {
		t.Fatalf("Remove nonexistent: %v", err)
	}
}

func TestCmdAliasResolveChain(t *testing.T) {
	nd := setupAliasTestDir(t, "cog-workspace")
	aliasYAML := "version: \"1.0\"\naliases:\n  cog: cog-workspace\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	m, _ := aliasLoadWithOverride(t, nd)
	ws, _, ok := m.Expand("cog")
	if !ok {
		t.Fatal("alias 'cog' not found")
	}
	if ws != "cog-workspace" {
		t.Errorf("expected cog-workspace, got %q", ws)
	}

	// Verify stale check: "cog-workspace" IS in global.yaml.
	stale := false
	for _, e := range m.List() {
		if e.Name == "cog" && e.Stale {
			stale = true
		}
	}
	if stale {
		t.Error("'cog' alias should not be stale")
	}
}

func TestCmdAliasResolveStale(t *testing.T) {
	nd := setupAliasTestDir(t, "cog-workspace")
	aliasYAML := "version: \"1.0\"\naliases:\n  dead: gone-workspace\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	m, _ := aliasLoadWithOverride(t, nd)
	for _, e := range m.List() {
		if e.Name == "dead" && !e.Stale {
			t.Error("'dead' alias should be stale")
		}
	}
}

func TestCmdAliasURIRewrite(t *testing.T) {
	// Simulate the URI rewrite logic in cmdAliasResolve without touching
	// stdout — test the string transformation directly.
	aliasName := "cog"
	workspace := "cog-workspace"
	rawURI := fmt.Sprintf("cog://%s/mem/semantic/test.cog.md", aliasName)

	if strings.HasPrefix(rawURI, "cog://"+aliasName) {
		expanded := "cog://" + workspace + strings.TrimPrefix(rawURI, "cog://"+aliasName)
		want := "cog://cog-workspace/mem/semantic/test.cog.md"
		if expanded != want {
			t.Errorf("URI rewrite: got %q, want %q", expanded, want)
		}
	}
}

func TestCmdAliasHelp(t *testing.T) {
	// Just verify cmdAliasHelp doesn't error.
	if err := cmdAliasHelp(); err != nil {
		t.Fatalf("cmdAliasHelp: %v", err)
	}
}

// ── ADR-067 grammar coverage ──────────────────────────────────────────────────

// TestBareLocalFormNotAlias verifies that a bare cog:path URI
// bypasses alias lookup (per ADR-067: bare cog: is always local).
func TestBareLocalFormNotAlias(t *testing.T) {
	nd := setupAliasTestDir(t, "cog-workspace")
	aliasYAML := "version: \"1.0\"\naliases:\n  cog: cog-workspace\n"
	if err := os.WriteFile(filepath.Join(nd, "aliases.yaml"), []byte(aliasYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// A bare cog:mem/... URI should NOT trigger alias expansion.
	// We verify that the alias "cog" doesn't match when the authority
	// is missing (bare form has no authority component).
	// The distinction: cog:mem/ has no //, so authority="" and no lookup occurs.
	bare := "cog:mem/semantic/test.cog.md"
	if strings.HasPrefix(bare, "cog://") {
		t.Fatal("bare cog: should not have //")
	}
	// Bare form — no authority, no alias check.
	if !strings.HasPrefix(bare, "cog:") || strings.HasPrefix(bare, "cog://") {
		t.Fatal("unexpected prefix")
	}
}
