//go:build mcpserver

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCogDocService_WriteAndSync_CreatesFileWithFrontmatter(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	svc := NewCogDocService(cfg, process)

	opts := CogDocWriteOpts{
		Title:   "Test Insight",
		Content: "Some body content.",
		Tags:    []string{"test", "unit"},
		Status:  "active",
		DocType: "insight",
	}

	result, err := svc.WriteAndSync("semantic/insights/test-insight.cog.md", opts)
	if err != nil {
		t.Fatalf("WriteAndSync: %v", err)
	}

	// Verify file exists at the expected absolute path.
	expectedPath := filepath.Join(root, ".cog", "mem", "semantic", "insights", "test-insight.cog.md")
	if result.Path != expectedPath {
		t.Errorf("Path = %q; want %q", result.Path, expectedPath)
	}

	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)

	// Check frontmatter markers.
	if !strings.HasPrefix(content, "---\n") {
		t.Error("file should start with YAML frontmatter delimiter")
	}
	if !strings.Contains(content, "title: \"Test Insight\"") {
		t.Errorf("frontmatter missing title; got:\n%s", content)
	}
	if !strings.Contains(content, "status: active") {
		t.Errorf("frontmatter missing status; got:\n%s", content)
	}
	if !strings.Contains(content, "type: insight") {
		t.Errorf("frontmatter missing type; got:\n%s", content)
	}
	if !strings.Contains(content, "memory_sector: semantic") {
		t.Errorf("frontmatter missing memory_sector; got:\n%s", content)
	}
	if !strings.Contains(content, "Some body content.") {
		t.Error("body content missing from written file")
	}
}

func TestCogDocService_WriteAndSync_ReturnsCorrectURI(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	svc := NewCogDocService(cfg, process)

	opts := CogDocWriteOpts{
		Title:   "URI Test",
		Content: "Check URI.",
	}

	result, err := svc.WriteAndSync("semantic/insights/uri-test.cog.md", opts)
	if err != nil {
		t.Fatalf("WriteAndSync: %v", err)
	}

	expectedURI := "cog:mem/semantic/insights/uri-test.cog.md"
	if result.URI != expectedURI {
		t.Errorf("URI = %q; want %q", result.URI, expectedURI)
	}
}

func TestCogDocService_WriteAndSync_NilProcessDoesNotPanic(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	// Construct with nil process — should degrade gracefully.
	svc := NewCogDocService(cfg, nil)

	opts := CogDocWriteOpts{
		Title:   "Nil Process Test",
		Content: "Should not panic.",
	}

	result, err := svc.WriteAndSync("semantic/insights/nil-process.cog.md", opts)
	if err != nil {
		t.Fatalf("WriteAndSync with nil process: %v", err)
	}

	// File should still be written successfully.
	if _, err := os.Stat(result.Path); err != nil {
		t.Errorf("file should exist at %s: %v", result.Path, err)
	}

	expectedURI := "cog:mem/semantic/insights/nil-process.cog.md"
	if result.URI != expectedURI {
		t.Errorf("URI = %q; want %q", result.URI, expectedURI)
	}
}
