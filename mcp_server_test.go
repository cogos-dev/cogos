//go:build mcpserver

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolReadCogdocIncludesSchemaHints(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	path := filepath.Join(root, ".cog", "mem", "semantic", "hinted.cog.md")
	writeTestFile(t, path, "---\ntitle: Hinted\n---\n\nBody content here.\n")

	result, _, err := server.toolReadCogdoc(context.Background(), nil, readCogdocInput{URI: "cog://mem/semantic/hinted.cog.md"})
	if err != nil {
		t.Fatalf("toolReadCogdoc: %v", err)
	}

	var decoded readCogdocResult
	decodeMCPJSON(t, result, &decoded)
	if !hasSchemaIssue(decoded.SchemaIssues, "missing_description") {
		t.Fatalf("SchemaIssues = %v; want missing_description", decoded.SchemaIssues)
	}
	if decoded.PatchFrontmatter == nil {
		t.Fatal("PatchFrontmatter should be present")
	}
	if decoded.SchemaHint == "" {
		t.Fatal("SchemaHint should be present")
	}
}

func TestToolPatchFrontmatterWritesUpdates(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	path := filepath.Join(root, ".cog", "mem", "semantic", "patched.cog.md")
	writeTestFile(t, path, "---\ntitle: Patched\n---\n\nBody content here.\n")

	result, _, err := server.toolPatchFrontmatter(context.Background(), nil, patchFrontmatterInput{
		URI: "cog://mem/semantic/patched.cog.md",
		Patches: cogdocFrontmatterPatch{
			Description: "one-line summary",
			Tags:        []string{"alpha", "beta"},
			Type:        "insight",
		},
	})
	if err != nil {
		t.Fatalf("toolPatchFrontmatter: %v", err)
	}

	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	if updated, _ := decoded["updated"].(bool); !updated {
		t.Fatalf("updated = %v; want true", decoded["updated"])
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	fm, _ := parseCogdocFrontmatter(string(content))
	if fm.Description != "one-line summary" {
		t.Fatalf("Description = %q; want patched value", fm.Description)
	}
	if fm.Type != "insight" {
		t.Fatalf("Type = %q; want insight", fm.Type)
	}
	if len(fm.Tags) != 2 {
		t.Fatalf("Tags = %v; want 2 items", fm.Tags)
	}
}

func TestToolGetTrustReturnsIdentityMetadata(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	result, _, err := server.toolGetTrust(context.Background(), nil, getTrustInput{})
	if err != nil {
		t.Fatalf("toolGetTrust: %v", err)
	}

	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	if decoded["node_id"] == "" {
		t.Fatal("node_id should not be empty")
	}
	if fingerprint, _ := decoded["fingerprint"].(string); fingerprint == "" {
		t.Fatal("fingerprint should not be empty")
	}
}

func TestToolGetStateIncludesQueueCounts(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	if err := os.MkdirAll(filepath.Join(root, ".cog", "mem", "quarantine"), 0o755); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cog", "mem", "deferred"), 0o755); err != nil {
		t.Fatalf("mkdir deferred: %v", err)
	}
	writeTestFile(t, filepath.Join(root, ".cog", "mem", "quarantine", "q.cog.md"), "quarantine")
	writeTestFile(t, filepath.Join(root, ".cog", "mem", "deferred", "d.cog.md"), "deferred")

	result, _, err := server.toolGetState(context.Background(), nil, getStateInput{})
	if err != nil {
		t.Fatalf("toolGetState: %v", err)
	}

	var decoded map[string]any
	decodeMCPJSON(t, result, &decoded)
	if decoded["quarantined_count"].(float64) != 1 {
		t.Fatalf("quarantined_count = %v; want 1", decoded["quarantined_count"])
	}
	if decoded["deferred_count"].(float64) != 1 {
		t.Fatalf("deferred_count = %v; want 1", decoded["deferred_count"])
	}
}

func decodeMCPJSON(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content len = %d; want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T; want *mcp.TextContent", result.Content[0])
	}
	if err := json.Unmarshal([]byte(text.Text), target); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}
