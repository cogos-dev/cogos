package alias_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/myrgic/cogos/pkg/alias"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// nodeDir creates a temp directory that mimics ~/.cog/node/.
// It optionally writes a global.yaml with the provided workspace names.
func nodeDir(t *testing.T, workspaces ...string) string {
	t.Helper()
	dir := t.TempDir()
	if len(workspaces) > 0 {
		writeGlobal(t, dir, workspaces)
	}
	return dir
}

// writeGlobal writes a minimal global.yaml with the given workspace names.
func writeGlobal(t *testing.T, nodeDir string, names []string) {
	t.Helper()
	lines := "version: \"1.0\"\nworkspaces:\n"
	for _, n := range names {
		lines += "  " + n + ":\n    path: /tmp/" + n + "\n"
	}
	path := filepath.Join(nodeDir, "global.yaml")
	if err := os.WriteFile(path, []byte(lines), 0644); err != nil {
		t.Fatalf("writeGlobal: %v", err)
	}
}

// writeAliases writes a minimal aliases.yaml into nodeDir.
func writeAliases(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, "aliases.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeAliases: %v", err)
	}
}

// ── Load ──────────────────────────────────────────────────────────────────────

func TestLoadEmpty(t *testing.T) {
	dir := nodeDir(t)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := m.List()
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestLoadMinimalFile(t *testing.T) {
	dir := nodeDir(t)
	writeAliases(t, dir, "version: \"1.0\"\n")
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.List()) != 0 {
		t.Fatal("expected zero entries from version-only file")
	}
}

func TestLoadShortForm(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	writeAliases(t, dir, `version: "1.0"
aliases:
  cog: cog-workspace
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ws, node, ok := m.Expand("cog")
	if !ok {
		t.Fatal("Expand: alias not found")
	}
	if ws != "cog-workspace" || node != "" {
		t.Fatalf("Expand: got ws=%q node=%q", ws, node)
	}
}

func TestLoadLongForm(t *testing.T) {
	dir := nodeDir(t, "cogos-dev/mod3")
	writeAliases(t, dir, `version: "1.0"
aliases:
  m3:
    workspace: cogos-dev/mod3
    description: "mod3 voice server"
    node: darkstar
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ws, node, ok := m.Expand("m3")
	if !ok {
		t.Fatal("Expand: alias not found")
	}
	if ws != "cogos-dev/mod3" || node != "darkstar" {
		t.Fatalf("Expand: got ws=%q node=%q", ws, node)
	}
}

func TestLoadMultiple(t *testing.T) {
	dir := nodeDir(t, "cog-workspace", "cogos-dev/cogos")
	writeAliases(t, dir, `version: "1.0"
aliases:
  cog: cog-workspace
  kernel: cogos-dev/cogos
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.List()) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.List()))
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestAddInvalidName(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)
	cases := []string{
		"",
		"A-starts-with-upper",
		"has spaces",
		"123numeric-start",
		"has.dot",
		"toolongnamethatexceedsthelimitofthirtyonecharacters",
	}
	for _, name := range cases {
		if err := m.Add(name, "cog-workspace", alias.AliasOpts{}); !errors.Is(err, alias.ErrInvalidAliasName) {
			t.Errorf("name %q: expected ErrInvalidAliasName, got %v", name, err)
		}
	}
}

func TestAddReservedName(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)
	// "mem" is a reserved projection namespace.
	if err := m.Add("mem", "cog-workspace", alias.AliasOpts{}); !errors.Is(err, alias.ErrReservedName) {
		t.Errorf("expected ErrReservedName, got %v", err)
	}
	// "adr" is also reserved.
	if err := m.Add("adr", "cog-workspace", alias.AliasOpts{}); !errors.Is(err, alias.ErrReservedName) {
		t.Errorf("expected ErrReservedName for 'adr', got %v", err)
	}
}

func TestAddAliasOfAlias(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	writeAliases(t, dir, `version: "1.0"
aliases:
  cog: cog-workspace
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// "cog" is already an alias; using it as a target should fail.
	if err := m.Add("shortcog", "cog", alias.AliasOpts{}); !errors.Is(err, alias.ErrAliasOfAlias) {
		t.Errorf("expected ErrAliasOfAlias, got %v", err)
	}
}

// ── Add / Remove ──────────────────────────────────────────────────────────────

func TestAddRemove(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)

	if err := m.Add("cog", "cog-workspace", alias.AliasOpts{Description: "primary"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ws, _, ok := m.Expand("cog")
	if !ok || ws != "cog-workspace" {
		t.Fatal("Add did not persist in-memory entry")
	}

	if err := m.Remove("cog"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, _, ok := m.Expand("cog"); ok {
		t.Fatal("Remove did not delete in-memory entry")
	}
}

func TestRemoveIdempotent(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)
	// Remove a name that was never added.
	if err := m.Remove("nonexistent"); err != nil {
		t.Fatalf("Remove nonexistent: %v", err)
	}
}

func TestAddPersistsToDisk(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)
	if err := m.Add("cog", "cog-workspace", alias.AliasOpts{}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Re-load from disk to verify persistence.
	m2, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load after Add: %v", err)
	}
	if _, _, ok := m2.Expand("cog"); !ok {
		t.Fatal("alias not found after reload from disk")
	}
}

// ── Stale detection ───────────────────────────────────────────────────────────

func TestStaleAlias(t *testing.T) {
	// Write aliases.yaml pointing at "gone-workspace", but global.yaml
	// only has "cog-workspace" (not "gone-workspace").
	dir := nodeDir(t, "cog-workspace")
	writeAliases(t, dir, `version: "1.0"
aliases:
  dead: gone-workspace
  live: cog-workspace
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := m.List()
	staleMap := map[string]bool{}
	for _, e := range entries {
		staleMap[e.Name] = e.Stale
	}
	if !staleMap["dead"] {
		t.Error("expected 'dead' alias to be stale")
	}
	if staleMap["live"] {
		t.Error("expected 'live' alias to not be stale")
	}
}

func TestStaleAliasNoGlobal(t *testing.T) {
	// Without a global.yaml, no alias is flagged stale.
	dir := t.TempDir()
	writeAliases(t, dir, `version: "1.0"
aliases:
  cog: cog-workspace
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range m.List() {
		if e.Stale {
			t.Errorf("alias %q flagged stale with no global.yaml", e.Name)
		}
	}
}

// ── Concurrent writes ─────────────────────────────────────────────────────────

func TestConcurrentAdd(t *testing.T) {
	dir := nodeDir(t, "ws-a", "ws-b")

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup

	// Pre-load a map for each goroutine.
	maps := make([]*alias.AliasMap, n)
	for i := 0; i < n; i++ {
		m, err := alias.Load(dir)
		if err != nil {
			t.Fatalf("Load[%d]: %v", i, err)
		}
		maps[i] = m
	}

	wg.Add(n)
	go func() {
		defer wg.Done()
		errs[0] = maps[0].Add("a1", "ws-a", alias.AliasOpts{})
	}()
	go func() {
		defer wg.Done()
		errs[1] = maps[1].Add("a2", "ws-b", alias.AliasOpts{})
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Reload and verify both aliases landed.
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load after concurrent Add: %v", err)
	}
	if _, _, ok := m.Expand("a1"); !ok {
		t.Error("alias a1 not found after concurrent add")
	}
	if _, _, ok := m.Expand("a2"); !ok {
		t.Error("alias a2 not found after concurrent add")
	}
}

// ── ADR-067 grammar acceptance ─────────────────────────────────────────────────

func TestExpandMissing(t *testing.T) {
	dir := t.TempDir()
	m, _ := alias.Load(dir)
	_, _, ok := m.Expand("doesnotexist")
	if ok {
		t.Error("expected Expand to return ok=false for unknown alias")
	}
}

// TestWorkspaceNameCollisionWarn verifies that loading when an alias name
// matches a workspace name does NOT error (warn at load time, alias wins).
func TestWorkspaceNameAliasCoexist(t *testing.T) {
	// "cog-workspace" is a workspace AND used as an alias name.
	// This is a warning scenario, not an error.
	dir := nodeDir(t, "cog-workspace")
	writeAliases(t, dir, `version: "1.0"
aliases:
  cog-workspace: cog-workspace
`)
	// Should load without error (name matches workspace name is allowed —
	// it just means alias and workspace agree on the canonical target).
	_, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: unexpected error when alias name == workspace name: %v", err)
	}
}

// ── List ordering ─────────────────────────────────────────────────────────────

func TestListOrdered(t *testing.T) {
	dir := nodeDir(t, "ws-a", "ws-b", "ws-c")
	writeAliases(t, dir, `version: "1.0"
aliases:
  zebra: ws-c
  alpha: ws-a
  mango: ws-b
`)
	m, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := m.List()
	if len(entries) != 3 {
		t.Fatalf("expected 3, got %d", len(entries))
	}
	names := []string{entries[0].Name, entries[1].Name, entries[2].Name}
	expected := []string{"alpha", "mango", "zebra"}
	for i, n := range names {
		if n != expected[i] {
			t.Errorf("List[%d]: got %q, want %q", i, n, expected[i])
		}
	}
}

// ── Valid name boundary ───────────────────────────────────────────────────────

func TestAddValidNames(t *testing.T) {
	dir := nodeDir(t, "cog-workspace")
	m, _ := alias.Load(dir)

	valid := []string{"a", "cog", "my-ws", "my_ws", "x1y2z3"}
	for _, name := range valid {
		if err := m.Add(name, "cog-workspace", alias.AliasOpts{}); err != nil {
			t.Errorf("name %q: unexpected error: %v", name, err)
		}
	}
}

// ── Round-trip long form ──────────────────────────────────────────────────────

func TestAddLongFormRoundTrip(t *testing.T) {
	dir := nodeDir(t, "cogos-dev/mod3")
	m, _ := alias.Load(dir)
	if err := m.Add("m3", "cogos-dev/mod3", alias.AliasOpts{
		Description: "voice server",
		Node:        "darkstar",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m2, err := alias.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries := m2.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Description != "voice server" || e.Node != "darkstar" {
		t.Errorf("round-trip: got desc=%q node=%q", e.Description, e.Node)
	}
}

