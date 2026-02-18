// .cog/memory_test.go
// Tests for memory.go (HMD operations)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizePath tests path normalization for waypoint comparison
func TestNormalizePath(t *testing.T) {
	cogRoot := "/home/user/workspace"

	tests := []struct {
		input    string
		expected string
	}{
		{
			"/home/user/workspace/.cog/mem/semantic/test.md",
			".cog/mem/semantic/test",
		},
		{
			".cog/mem/episodic/session.cog.md",
			".cog/mem/episodic/session",
		},
		{
			"/home/user/workspace/docs/test.md",
			"docs/test",
		},
	}

	for _, tt := range tests {
		result := NormalizePath(tt.input, cogRoot)
		if result != tt.expected {
			t.Errorf("NormalizePath(%s) = %s, want %s", tt.input, result, tt.expected)
		}
	}
}

// TestResolveMemoryPath tests the shared path resolution that prevents double-nesting
func TestResolveMemoryPath(t *testing.T) {
	memoryDir := "/home/user/workspace/.cog/mem"

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "memory-relative path (common case)",
			input:    "semantic/insights/topic.md",
			expected: "/home/user/workspace/.cog/mem/semantic/insights/topic.md",
		},
		{
			name:     "workspace-relative with .cog/mem/ prefix",
			input:    ".cog/mem/semantic/insights/topic.md",
			expected: "/home/user/workspace/.cog/mem/semantic/insights/topic.md",
		},
		{
			name:     "absolute path containing .cog/mem/",
			input:    "/home/user/workspace/.cog/mem/semantic/insights/topic.md",
			expected: "/home/user/workspace/.cog/mem/semantic/insights/topic.md",
		},
		{
			name:     "absolute path without .cog/mem/",
			input:    "/tmp/other/file.md",
			expected: "/tmp/other/file.md",
		},
		{
			name:     "deeply nested .cog/mem/ in absolute (double-nest scenario)",
			input:    "/home/user/workspace/.cog/mem/.cog/mem/procedural/foo.md",
			expected: "/home/user/workspace/.cog/mem/procedural/foo.md",
		},
		{
			name:     "workspace-relative procedural path",
			input:    ".cog/mem/procedural/guides/planning.cog.md",
			expected: "/home/user/workspace/.cog/mem/procedural/guides/planning.cog.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveMemoryPath(memoryDir, tt.input)
			if result != tt.expected {
				t.Errorf("resolveMemoryPath(%q) =\n  %s\nwant:\n  %s", tt.input, result, tt.expected)
			}
		})
	}
}

// TestMemoryWritePathNormalization verifies MemoryWrite doesn't double-nest .cog/mem
func TestMemoryWritePathNormalization(t *testing.T) {
	tmpDir := t.TempDir()

	// This is the exact pattern that caused the bug: passing .cog/mem/ prefixed path
	err := MemoryWrite(tmpDir, ".cog/mem/semantic/test/doc.md", "Test", "content")
	if err != nil {
		t.Fatalf("MemoryWrite failed: %v", err)
	}

	// Should be at the correct path, NOT double-nested
	correctPath := filepath.Join(tmpDir, ".cog", "mem", "semantic", "test", "doc.md")
	wrongPath := filepath.Join(tmpDir, ".cog", "mem", ".cog", "mem", "semantic", "test", "doc.md")

	if _, err := os.Stat(correctPath); os.IsNotExist(err) {
		t.Errorf("file not found at correct path: %s", correctPath)
	}
	if _, err := os.Stat(wrongPath); err == nil {
		t.Errorf("file found at WRONG double-nested path: %s", wrongPath)
	}
}

// TestLoadWaypointGraph tests loading waypoint graph from JSON
func TestLoadWaypointGraph(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	waypointsDir := filepath.Join(tmpDir, "waypoints")
	os.MkdirAll(waypointsDir, 0755)

	// Create test waypoints file
	waypointsJSON := `{
  "connections": {
    "semantic/arch": [
      {"target": "semantic/design", "weight": 0.8},
      {"target": "episodic/implementation", "weight": 0.5}
    ]
  }
}`
	waypointsFile := filepath.Join(waypointsDir, "connections.json")
	os.WriteFile(waypointsFile, []byte(waypointsJSON), 0644)

	// Load graph
	graph, err := LoadWaypointGraph(tmpDir)
	if err != nil {
		t.Fatalf("LoadWaypointGraph failed: %v", err)
	}

	// Verify connections
	connections, exists := graph.Connections["semantic/arch"]
	if !exists {
		t.Fatal("Expected connections for semantic/arch")
	}

	if len(connections) != 2 {
		t.Fatalf("Expected 2 connections, got %d", len(connections))
	}

	if connections[0].Target != "semantic/design" {
		t.Errorf("Expected target semantic/design, got %s", connections[0].Target)
	}

	if connections[0].Weight != 0.8 {
		t.Errorf("Expected weight 0.8, got %.2f", connections[0].Weight)
	}
}

// TestLoadWaypointGraphMissing tests graceful handling of missing waypoints file
func TestLoadWaypointGraphMissing(t *testing.T) {
	tmpDir := t.TempDir()

	graph, err := LoadWaypointGraph(tmpDir)
	if err != nil {
		t.Fatalf("Expected no error for missing waypoints, got: %v", err)
	}

	if len(graph.Connections) != 0 {
		t.Errorf("Expected empty graph, got %d connections", len(graph.Connections))
	}
}

// TestMemoryWrite tests creating a new memory document
func TestMemoryWrite(t *testing.T) {
	tmpDir := t.TempDir()

	err := MemoryWrite(tmpDir, "semantic/test/doc.md", "Test Document", "Test content")
	if err != nil {
		t.Fatalf("MemoryWrite failed: %v", err)
	}

	// Verify file was created
	fullPath := filepath.Join(tmpDir, ".cog", "mem", "semantic", "test", "doc.md")
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		t.Fatal("File was not created")
	}

	// Verify content
	content, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, "title: Test Document") {
		t.Error("Missing title in frontmatter")
	}
	if !strings.Contains(contentStr, "Test content") {
		t.Error("Missing content in document")
	}
	if !HasFrontmatter(contentStr) {
		t.Error("Document missing frontmatter")
	}
}

// TestMemoryAppend tests appending to existing document
func TestMemoryAppend(t *testing.T) {
	tmpDir := t.TempDir()

	// Create initial document
	err := MemoryWrite(tmpDir, "semantic/test.md", "Test", "Initial content")
	if err != nil {
		t.Fatalf("MemoryWrite failed: %v", err)
	}

	// Append to it
	err = MemoryAppend(tmpDir, "semantic/test.md", "Appended content")
	if err != nil {
		t.Fatalf("MemoryAppend failed: %v", err)
	}

	// Verify appended content
	fullPath := filepath.Join(tmpDir, ".cog", "mem", "semantic", "test.md")
	content, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, "Initial content") {
		t.Error("Missing initial content")
	}
	if !strings.Contains(contentStr, "Appended content") {
		t.Error("Missing appended content")
	}
}

// TestMemoryRead tests reading a memory document
func TestMemoryRead(t *testing.T) {
	tmpDir := t.TempDir()

	// Create document
	err := MemoryWrite(tmpDir, "episodic/session.md", "Session", "Session content")
	if err != nil {
		t.Fatalf("MemoryWrite failed: %v", err)
	}

	// Read it back
	content, err := MemoryRead(tmpDir, "episodic/session.md")
	if err != nil {
		t.Fatalf("MemoryRead failed: %v", err)
	}

	if !strings.Contains(content, "Session content") {
		t.Error("Read content doesn't match written content")
	}
}

// TestMemoryList tests listing documents in a sector
func TestMemoryList(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple documents
	MemoryWrite(tmpDir, "semantic/doc1.md", "Doc 1", "Content 1")
	MemoryWrite(tmpDir, "semantic/doc2.md", "Doc 2", "Content 2")
	MemoryWrite(tmpDir, "semantic/subdir/doc3.md", "Doc 3", "Content 3")

	// List semantic sector
	results, err := MemoryList(tmpDir, "semantic", "")
	if err != nil {
		t.Fatalf("MemoryList failed: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("Expected 3 results, got %d", len(results))
	}
}

// TestMemoryStats tests memory statistics
func TestMemoryStats(t *testing.T) {
	tmpDir := t.TempDir()

	// Create documents in multiple sectors
	MemoryWrite(tmpDir, "semantic/doc1.md", "Doc 1", "Content")
	MemoryWrite(tmpDir, "semantic/doc2.md", "Doc 2", "Content")
	MemoryWrite(tmpDir, "episodic/session1.md", "Session", "Content")

	// Get stats (just verify it doesn't error)
	err := MemoryStats(tmpDir)
	if err != nil {
		t.Fatalf("MemoryStats failed: %v", err)
	}
}

// TestTraverseWaypoints tests waypoint graph traversal
func TestTraverseWaypoints(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".cog", "mem")
	os.MkdirAll(memoryDir, 0755)

	// Create test documents
	doc1Path := filepath.Join(memoryDir, "semantic", "doc1.md")
	doc2Path := filepath.Join(memoryDir, "semantic", "doc2.md")
	doc3Path := filepath.Join(memoryDir, "semantic", "doc3.md")

	os.MkdirAll(filepath.Dir(doc1Path), 0755)
	os.WriteFile(doc1Path, []byte("---\ntitle: Doc 1\n---\nContent"), 0644)
	os.WriteFile(doc2Path, []byte("---\ntitle: Doc 2\n---\nContent"), 0644)
	os.WriteFile(doc3Path, []byte("---\ntitle: Doc 3\n---\nContent"), 0644)

	// Create waypoint graph
	graph := &WaypointGraph{
		Connections: map[string][]WaypointConnection{
			".cog/mem/semantic/doc1": {
				{Target: ".cog/mem/semantic/doc2.md", Weight: 0.8},
			},
			".cog/mem/semantic/doc2": {
				{Target: ".cog/mem/semantic/doc3.md", Weight: 0.6},
			},
		},
	}

	// Initial matches (doc1 only)
	initialMatches := []MemorySearchResult{
		{
			Path:       doc1Path,
			Score:      1.0,
			Title:      "Doc 1",
			SourceType: "direct",
			Depth:      0,
		},
	}

	// Traverse graph
	results := TraverseWaypoints(initialMatches, graph, 2, 0.7, tmpDir)

	// Should discover doc2 at depth 1 and doc3 at depth 2
	if len(results) < 1 {
		t.Fatalf("Expected at least 1 waypoint result, got %d", len(results))
	}

	// Check that waypoint nodes were discovered
	foundDoc2 := false
	for _, r := range results {
		if strings.Contains(r.Path, "doc2") {
			foundDoc2 = true
			if r.SourceType != "waypoint" {
				t.Error("doc2 should be marked as waypoint")
			}
		}
	}

	if !foundDoc2 {
		t.Error("Expected to discover doc2 via waypoints")
	}
}

// TestMemorySearchNoResults tests search with no matches
func TestMemorySearchNoResults(t *testing.T) {
	tmpDir := t.TempDir()

	results, err := MemorySearch(tmpDir, "nonexistent_query_xyz", false, 2, 0.7, false)
	if err != nil {
		t.Fatalf("MemorySearch failed: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("Expected 0 results for nonexistent query, got %d", len(results))
	}
}
