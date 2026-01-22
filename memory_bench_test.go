// .cog/memory_bench_test.go
// Benchmarks for memory.go (HMD operations)

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkMemoryWrite benchmarks creating new memory documents
func BenchmarkMemoryWrite(b *testing.B) {
	tmpDir := b.TempDir()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := fmt.Sprintf("semantic/bench/doc%d.md", i)
		MemoryWrite(tmpDir, path, "Benchmark Doc", "Benchmark content")
	}
}

// BenchmarkMemoryRead benchmarks reading memory documents
func BenchmarkMemoryRead(b *testing.B) {
	tmpDir := b.TempDir()

	// Create a test document
	MemoryWrite(tmpDir, "semantic/test.md", "Test", "Content")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MemoryRead(tmpDir, "semantic/test.md")
	}
}

// BenchmarkMemoryList benchmarks listing documents in a sector
func BenchmarkMemoryList(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 50 test documents
	for i := 0; i < 50; i++ {
		path := fmt.Sprintf("semantic/doc%d.md", i)
		MemoryWrite(tmpDir, path, fmt.Sprintf("Doc %d", i), "Content")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MemoryList(tmpDir, "semantic", "")
	}
}

// BenchmarkLoadWaypointGraph benchmarks loading waypoint graph
func BenchmarkLoadWaypointGraph(b *testing.B) {
	tmpDir := b.TempDir()
	waypointsDir := filepath.Join(tmpDir, "waypoints")
	os.MkdirAll(waypointsDir, 0755)

	// Create a test waypoints file with 20 connections
	waypointsJSON := `{
  "connections": {
    "semantic/doc1": [
      {"target": "semantic/doc2", "weight": 0.8},
      {"target": "semantic/doc3", "weight": 0.5}
    ],
    "semantic/doc2": [
      {"target": "semantic/doc4", "weight": 0.7}
    ]
  }
}`
	waypointsFile := filepath.Join(waypointsDir, "connections.json")
	os.WriteFile(waypointsFile, []byte(waypointsJSON), 0644)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		LoadWaypointGraph(tmpDir)
	}
}

// BenchmarkNormalizePath benchmarks path normalization
func BenchmarkNormalizePath(b *testing.B) {
	cogRoot := "/home/user/workspace"
	path := "/home/user/workspace/.cog/mem/semantic/test.cog.md"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NormalizePath(path, cogRoot)
	}
}

// BenchmarkTraverseWaypoints benchmarks graph traversal
func BenchmarkTraverseWaypoints(b *testing.B) {
	tmpDir := b.TempDir()
	memoryDir := filepath.Join(tmpDir, ".cog", "memory", "semantic")
	os.MkdirAll(memoryDir, 0755)

	// Create 10 test documents
	docs := make([]string, 10)
	for i := 0; i < 10; i++ {
		path := filepath.Join(memoryDir, fmt.Sprintf("doc%d.md", i))
		docs[i] = path
		content := fmt.Sprintf("---\ntitle: Doc %d\ntype: note\n---\nContent", i)
		os.WriteFile(path, []byte(content), 0644)
	}

	// Create waypoint graph (linear chain)
	graph := &WaypointGraph{
		Connections: make(map[string][]WaypointConnection),
	}
	for i := 0; i < 9; i++ {
		normalized := fmt.Sprintf(".cog/mem/semantic/doc%d", i)
		target := fmt.Sprintf(".cog/mem/semantic/doc%d.md", i+1)
		graph.Connections[normalized] = []WaypointConnection{
			{Target: target, Weight: 0.8},
		}
	}

	// Initial matches
	initialMatches := []MemorySearchResult{
		{
			Path:       docs[0],
			Score:      1.0,
			Title:      "Doc 0",
			Type:       "note",
			SourceType: "direct",
			Depth:      0,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		TraverseWaypoints(initialMatches, graph, 5, 0.7, tmpDir)
	}
}

// BenchmarkMemorySearchSmall benchmarks search on small dataset (10 docs)
func BenchmarkMemorySearchSmall(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 10 test documents
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("semantic/doc%d.md", i)
		content := fmt.Sprintf("This is test document %d with searchable content", i)
		MemoryWrite(tmpDir, path, fmt.Sprintf("Doc %d", i), content)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MemorySearch(tmpDir, "searchable", false, 2, 0.7, false)
	}
}

// BenchmarkMemorySearchMedium benchmarks search on medium dataset (50 docs)
func BenchmarkMemorySearchMedium(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 50 test documents
	for i := 0; i < 50; i++ {
		path := fmt.Sprintf("semantic/doc%d.md", i)
		content := fmt.Sprintf("This is test document %d with searchable content", i)
		MemoryWrite(tmpDir, path, fmt.Sprintf("Doc %d", i), content)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MemorySearch(tmpDir, "searchable", false, 2, 0.7, false)
	}
}

// BenchmarkMemorySearchRaw benchmarks raw search (no ranking)
func BenchmarkMemorySearchRaw(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 50 test documents
	for i := 0; i < 50; i++ {
		path := fmt.Sprintf("semantic/doc%d.md", i)
		content := fmt.Sprintf("This is test document %d with searchable content", i)
		MemoryWrite(tmpDir, path, fmt.Sprintf("Doc %d", i), content)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MemorySearch(tmpDir, "searchable", false, 2, 0.7, true) // raw mode
	}
}
