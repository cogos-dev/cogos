// mcp_architecture_test.go — Smoke test for the architecture skill MCP wiring.
//
// Strategy (following mcp_modality_proxy_test.go): construct a minimal
// MCPServer with just the cfg field set, write a fake Python script that
// echoes a known JSON literal at the canonical script path, and exercise
// the tool handlers directly. This validates the Python <-> Go contract
// without requiring the real cog-workspace architecture skill to be
// installed.

package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// scriptsDir builds the canonical script path layout the kernel expects
// for the architecture skill plugin and returns it.
func scriptsDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, ".cog", "skills", "architecture", "tools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeFakeScript writes a Python script that emits the given JSON literal
// and exits 0. The script accepts arbitrary args (ignored) so all 8 tool
// handlers' arg-passing variants work.
func writeFakeScript(t *testing.T, path, jsonOut string) {
	t.Helper()
	body := "#!/usr/bin/env python3\nimport sys\nsys.stdout.write('''" + jsonOut + "''')\nsys.exit(0)\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// readResultText extracts the text content of an MCP CallToolResult,
// concatenating all text parts. Useful for asserting on the handler's
// returned JSON payload. marshalResult places the JSON-encoded result
// in result.Content[0].(*mcp.TextContent).Text.
func readResultText(t *testing.T, res *mcpResultLike) string {
	t.Helper()
	if res == nil || res.Content == nil {
		return ""
	}
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp_TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

// Imported names for clarity — re-aliased here to avoid pulling the mcp
// package alias into every test assertion site.
type mcpResultLike = mcp.CallToolResult
type mcp_TextContent = mcp.TextContent

// fakeMCPServer is the minimum MCPServer needed to exercise the architecture
// tool handlers — only cfg.WorkspaceRoot is read.
func fakeMCPServer(t *testing.T, workspaceRoot string) *MCPServer {
	t.Helper()
	return &MCPServer{
		cfg: &Config{WorkspaceRoot: workspaceRoot},
	}
}

// TestArchitectureScriptPath verifies the path the handler constructs
// matches the documented layout.
func TestArchitectureScriptPath(t *testing.T) {
	m := fakeMCPServer(t, "/x")
	got := m.architectureScriptPath("architecture_resolve.py")
	want := filepath.Join("/x", ".cog", "skills", "architecture", "tools", "architecture_resolve.py")
	if got != want {
		t.Errorf("architectureScriptPath = %q, want %q", got, want)
	}
}

// TestToolArchitectureResolve_HappyPath validates the full handler chain
// for cog_architecture_resolve: input struct -> subprocess exec -> JSON
// unmarshal -> MCP result.
func TestToolArchitectureResolve_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("python3 fake script uses POSIX shebang")
	}
	root := t.TempDir()
	dir := scriptsDir(t, root)
	expected := `{"canonical_uri":"cog://architecture/adrs/foo","slug":"foo","kind":"adr","on_disk_path":"/x","resolved_via":"canonical"}`
	writeFakeScript(t, filepath.Join(dir, "architecture_resolve.py"), expected)

	m := fakeMCPServer(t, root)
	res, _, err := m.toolArchitectureResolve(context.Background(), nil, architectureResolveInput{Handle: "cog://adr/052"})
	if err != nil {
		t.Fatalf("toolArchitectureResolve: %v", err)
	}
	got := readResultText(t, res)
	for _, needle := range []string{`"slug":"foo"`, `"kind":"adr"`, `"resolved_via":"canonical"`} {
		if !strings.Contains(got, needle) {
			t.Errorf("expected result to contain %q; got: %s", needle, got)
		}
	}
}

// TestToolArchitectureList_ArgPassing verifies that filter args are
// translated correctly into CLI flags. The fake script ignores args but
// the handler must construct them without crashing.
func TestToolArchitectureList_ArgPassing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("python3 fake script uses POSIX shebang")
	}
	root := t.TempDir()
	dir := scriptsDir(t, root)
	writeFakeScript(t, filepath.Join(dir, "architecture_list.py"), `[]`)

	m := fakeMCPServer(t, root)
	in := architectureListInput{Kind: "adr", Status: "accepted", Tag: "substrate", Limit: 5}
	res, _, err := m.toolArchitectureList(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("toolArchitectureList: %v", err)
	}
	got := readResultText(t, res)
	if got != "[]" {
		t.Errorf("expected empty JSON array '[]', got %q", got)
	}
}

// TestToolArchitectureResolve_MissingScript exercises the fallback path
// when the Python script is absent (the case where kernel is running in
// a workspace without the architecture skill installed).
func TestToolArchitectureResolve_MissingScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("python3 fake script uses POSIX shebang")
	}
	root := t.TempDir() // no .cog/skills/ subdir
	m := fakeMCPServer(t, root)
	res, _, err := m.toolArchitectureResolve(context.Background(), nil, architectureResolveInput{Handle: "foo"})
	if err != nil {
		t.Fatalf("expected nil error (handler returns fallback result), got: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil fallback result")
	}
	// fallbackResult wraps a structured message; verify it indicates failure
	// rather than masquerading as success.
	got := readResultText(t, res)
	if !strings.Contains(got, "failed") && !strings.Contains(got, "no such file") && !strings.Contains(got, "Fallback") {
		t.Errorf("expected fallback message in result; got: %s", got)
	}
}

// TestToolArchitectureWrite_StdinPiping exercises the write tool's
// stdin-piping path, which is the only handler that pipes JSON into the
// Python script's stdin rather than passing it as a CLI arg.
func TestToolArchitectureWrite_StdinPiping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("python3 fake script uses POSIX shebang")
	}
	root := t.TempDir()
	dir := scriptsDir(t, root)
	// Echo stdin verbatim into the JSON output so the test asserts on it.
	body := `#!/usr/bin/env python3
import sys, json
data = json.load(sys.stdin)
result = {"uri": "cog://architecture/adrs/test", "kind": "adr", "action": "created", "echoed_slug": data.get("frontmatter", {}).get("slug", "")}
sys.stdout.write(json.dumps(result))
`
	if err := os.WriteFile(filepath.Join(dir, "architecture_write.py"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	m := fakeMCPServer(t, root)
	in := architectureWriteInput{
		Slug:   "test",
		Author: "tester",
		Tree: map[string]any{
			"frontmatter": map[string]any{"slug": "test", "title": "T", "type": "adr"},
			"blocks":      []any{},
		},
	}
	res, _, err := m.toolArchitectureWrite(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("toolArchitectureWrite: %v", err)
	}
	got := readResultText(t, res)
	if !strings.Contains(got, `"echoed_slug":"test"`) {
		t.Errorf("expected stdin tree to round-trip into result; got: %s", got)
	}
}
