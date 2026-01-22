package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cogos-dev/cogos/sdk/types"
)

// === PERFORMANCE BENCHMARKS ===
// These benchmarks measure kernel operations against performance budgets

// BenchmarkEventAppend measures event append performance
// Target: <1ms per event
func BenchmarkEventAppend(b *testing.B) {
	tmpDir := b.TempDir()
	ledgerDir := filepath.Join(tmpDir, "ledger")
	sessionID := "bench-session"
	sessionDir := filepath.Join(ledgerDir, sessionID)

	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		b.Fatalf("Failed to create session directory: %v", err)
	}

	eventsFile := filepath.Join(sessionDir, "events.jsonl")
	f, err := os.OpenFile(eventsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		b.Fatalf("Failed to open events file: %v", err)
	}
	defer f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		event := types.NewEvent(types.EventTypeMessage, sessionID)
		event.Seq = int64(i + 1)
		event.WithData(types.MessageEventData{
			Role:    "user",
			Content: fmt.Sprintf("Message %d", i),
		})

		line, _ := event.ToJSONLine()
		f.Write(line)
	}
	b.StopTimer()

	// Calculate ops/sec
	opsPerSec := float64(b.N) / b.Elapsed().Seconds()
	msPerOp := b.Elapsed().Seconds() * 1000 / float64(b.N)

	b.ReportMetric(opsPerSec, "ops/sec")
	b.ReportMetric(msPerOp, "ms/op")

	// Check against budget
	if msPerOp > 1.0 {
		b.Logf("⚠ Warning: Event append exceeded 1ms budget (%.2f ms/op)", msPerOp)
	} else {
		b.Logf("✓ Event append within budget: %.2f ms/op", msPerOp)
	}
}

// BenchmarkValidation measures cogdoc validation performance
// Target: <10ms per artifact
func BenchmarkValidation(b *testing.B) {
	tmpDir := b.TempDir()
	cogRoot := filepath.Join(tmpDir, ".cog")
	memoryDir := filepath.Join(cogRoot, "memory", "semantic")

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		b.Fatalf("Failed to create memory directory: %v", err)
	}

	// Create test cogdoc
	testDoc := filepath.Join(memoryDir, "test.cog.md")
	content := `---
type: note
id: test-note
title: Test Note
created: 2026-01-16
tags: [test, benchmark]
refs:
  - cog://mem/semantic/other
---

# Test Document

This is a test document for benchmarking validation performance.
It contains multiple sections and references to test parsing speed.

## Section 1
Content here.

## Section 2
More content.
`
	if err := os.WriteFile(testDoc, []byte(content), 0644); err != nil {
		b.Fatalf("Failed to write test doc: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := validateCogdocFull(testDoc)
		if len(result.Errors) > 0 {
			b.Fatalf("Validation failed: %v", result.Errors)
		}
	}
	b.StopTimer()

	// Calculate metrics
	opsPerSec := float64(b.N) / b.Elapsed().Seconds()
	msPerOp := b.Elapsed().Seconds() * 1000 / float64(b.N)

	b.ReportMetric(opsPerSec, "ops/sec")
	b.ReportMetric(msPerOp, "ms/op")

	// Check against budget
	if msPerOp > 10.0 {
		b.Logf("⚠ Warning: Validation exceeded 10ms budget (%.2f ms/op)", msPerOp)
	} else {
		b.Logf("✓ Validation within budget: %.2f ms/op", msPerOp)
	}
}

// BenchmarkReplay measures event replay performance
// Target: <100ms for 1000 events
func BenchmarkReplay(b *testing.B) {
	tmpDir := b.TempDir()
	ledgerDir := filepath.Join(tmpDir, "ledger")
	sessionID := "bench-replay"
	sessionDir := filepath.Join(ledgerDir, sessionID)

	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		b.Fatalf("Failed to create session directory: %v", err)
	}

	eventsFile := filepath.Join(sessionDir, "events.jsonl")

	// Create 1000 events
	numEvents := 1000
	f, err := os.Create(eventsFile)
	if err != nil {
		b.Fatalf("Failed to create events file: %v", err)
	}

	for i := 0; i < numEvents; i++ {
		event := types.NewEvent(types.EventTypeMessage, sessionID)
		event.Seq = int64(i + 1)
		event.WithData(types.MessageEventData{
			Role:    "user",
			Content: fmt.Sprintf("Message %d", i),
		})

		line, _ := event.ToJSONLine()
		f.Write(line)
	}
	f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Replay events
		f, err := os.Open(eventsFile)
		if err != nil {
			b.Fatalf("Failed to open events file: %v", err)
		}

		decoder := json.NewDecoder(f)
		count := 0
		for decoder.More() {
			var event types.Event
			if err := decoder.Decode(&event); err != nil {
				b.Fatalf("Failed to decode event: %v", err)
			}
			count++
		}
		f.Close()

		if count != numEvents {
			b.Fatalf("Expected %d events, got %d", numEvents, count)
		}
	}
	b.StopTimer()

	// Calculate metrics
	msPerReplay := b.Elapsed().Seconds() * 1000 / float64(b.N)
	eventsPerSec := float64(numEvents*b.N) / b.Elapsed().Seconds()

	b.ReportMetric(msPerReplay, "ms/replay")
	b.ReportMetric(eventsPerSec, "events/sec")

	// Check against budget
	if msPerReplay > 100.0 {
		b.Logf("⚠ Warning: Replay exceeded 100ms budget (%.2f ms for %d events)", msPerReplay, numEvents)
	} else {
		b.Logf("✓ Replay within budget: %.2f ms for %d events", msPerReplay, numEvents)
	}
}

// BenchmarkHashComputation measures hash computation performance
func BenchmarkHashComputation(b *testing.B) {
	data := []byte("This is test data for hash computation benchmarking")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hash(data)
	}
	b.StopTimer()

	opsPerSec := float64(b.N) / b.Elapsed().Seconds()
	nsPerOp := b.Elapsed().Nanoseconds() / int64(b.N)

	b.ReportMetric(opsPerSec, "ops/sec")
	b.ReportMetric(float64(nsPerOp)/1000, "μs/op")
}

// BenchmarkCogdocIndexBuild measures index building performance
func BenchmarkCogdocIndexBuild(b *testing.B) {
	tmpDir := b.TempDir()
	cogRoot := filepath.Join(tmpDir, ".cog")
	memoryDir := filepath.Join(cogRoot, "memory", "semantic")

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		b.Fatalf("Failed to create memory directory: %v", err)
	}

	// Create 100 test cogdocs
	numDocs := 100
	for i := 0; i < numDocs; i++ {
		docPath := filepath.Join(memoryDir, fmt.Sprintf("doc%03d.cog.md", i))
		content := fmt.Sprintf(`---
type: note
id: doc%03d
title: Document %d
created: 2026-01-16
tags: [test, benchmark]
---

This is document %d.
`, i, i, i)
		if err := os.WriteFile(docPath, []byte(content), 0644); err != nil {
			b.Fatalf("Failed to write doc %d: %v", i, err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		index, err := BuildCogdocIndex(cogRoot)
		if err != nil {
			b.Fatalf("Failed to build index: %v", err)
		}
		if len(index.ByURI) != numDocs {
			b.Fatalf("Expected %d documents, got %d", numDocs, len(index.ByURI))
		}
	}
	b.StopTimer()

	msPerBuild := b.Elapsed().Seconds() * 1000 / float64(b.N)
	docsPerSec := float64(numDocs*b.N) / b.Elapsed().Seconds()

	b.ReportMetric(msPerBuild, "ms/build")
	b.ReportMetric(docsPerSec, "docs/sec")

	b.Logf("Index build: %.2f ms for %d documents", msPerBuild, numDocs)
}

// BenchmarkTaskGraphBuild measures task graph construction performance
func BenchmarkTaskGraphBuild(b *testing.B) {
	// Create a complex task graph
	tasks := make(map[string]Task)
	numTasks := 50

	for i := 0; i < numTasks; i++ {
		deps := []string{}
		if i > 0 {
			// Each task depends on previous task
			deps = append(deps, fmt.Sprintf("task%03d", i-1))
		}
		if i > 10 {
			// Some tasks have additional dependencies
			deps = append(deps, fmt.Sprintf("task%03d", i-10))
		}

		tasks[fmt.Sprintf("task%03d", i)] = Task{
			Name:      fmt.Sprintf("task%03d", i),
			Command:   "echo test",
			DependsOn: deps,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		graph, err := buildTaskGraph(tasks)
		if err != nil {
			b.Fatalf("Failed to build graph: %v", err)
		}

		// Verify graph
		if len(graph.Nodes) != numTasks {
			b.Fatalf("Expected %d nodes, got %d", numTasks, len(graph.Nodes))
		}

		// Topological sort
		_, err = topoSort(graph)
		if err != nil {
			b.Fatalf("Failed to sort graph: %v", err)
		}
	}
	b.StopTimer()

	msPerBuild := b.Elapsed().Seconds() * 1000 / float64(b.N)
	b.ReportMetric(msPerBuild, "ms/build")

	b.Logf("Task graph build + sort: %.2f ms for %d tasks", msPerBuild, numTasks)
}

// BenchmarkURIResolution measures URI resolution performance
func BenchmarkURIResolution(b *testing.B) {
	tmpDir := b.TempDir()
	cogRoot := filepath.Join(tmpDir, ".cog")
	memoryDir := filepath.Join(cogRoot, "memory", "semantic")

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		b.Fatalf("Failed to create memory directory: %v", err)
	}

	// Create test file
	testFile := filepath.Join(memoryDir, "test.cog.md")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		b.Fatalf("Failed to write test file: %v", err)
	}

	// Change to temp directory for resolution
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	uri := "cog://mem/semantic/test"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := resolveURI(uri)
		if err != nil {
			b.Fatalf("Failed to resolve URI: %v", err)
		}
	}
	b.StopTimer()

	μsPerOp := b.Elapsed().Microseconds() / int64(b.N)
	b.ReportMetric(float64(μsPerOp), "μs/op")

	b.Logf("URI resolution: %d μs/op", μsPerOp)
}

// BenchmarkCoherenceCheck measures coherence checking performance
func BenchmarkCoherenceCheck(b *testing.B) {
	tmpDir := b.TempDir()
	cogRoot := filepath.Join(tmpDir, ".cog")
	memoryDir := filepath.Join(cogRoot, "memory")

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		b.Fatalf("Failed to create memory directory: %v", err)
	}

	// Create test files
	for i := 0; i < 10; i++ {
		testFile := filepath.Join(memoryDir, fmt.Sprintf("test%d.cog.md", i))
		content := fmt.Sprintf(`---
type: note
id: test%d
title: Test %d
created: 2026-01-16
---

Test content %d
`, i, i, i)
		if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
			b.Fatalf("Failed to write test file: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := checkCoherence(tmpDir)
		if err != nil {
			// Git might not be initialized, that's ok for benchmark
			continue
		}
	}
	b.StopTimer()

	msPerCheck := b.Elapsed().Seconds() * 1000 / float64(b.N)
	b.ReportMetric(msPerCheck, "ms/check")

	b.Logf("Coherence check: %.2f ms/op", msPerCheck)
}

// === STRESS TESTS ===

// TestLargeEventStream tests handling of large event streams
func TestLargeEventStream(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large event stream test in short mode")
	}

	tmpDir := t.TempDir()
	ledgerDir := filepath.Join(tmpDir, "ledger")
	sessionID := "large-session"
	sessionDir := filepath.Join(ledgerDir, sessionID)

	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatalf("Failed to create session directory: %v", err)
	}

	eventsFile := filepath.Join(sessionDir, "events.jsonl")

	// Create 10,000 events
	numEvents := 10000
	f, err := os.Create(eventsFile)
	if err != nil {
		t.Fatalf("Failed to create events file: %v", err)
	}

	for i := 0; i < numEvents; i++ {
		event := types.NewEvent(types.EventTypeMessage, sessionID)
		event.Seq = int64(i + 1)
		event.WithData(types.MessageEventData{
			Role:    "user",
			Content: fmt.Sprintf("Message %d", i),
		})

		line, _ := event.ToJSONLine()
		f.Write(line)
	}
	f.Close()

	// Replay all events
	f, err = os.Open(eventsFile)
	if err != nil {
		t.Fatalf("Failed to open events file: %v", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	count := 0
	for decoder.More() {
		var event types.Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatalf("Failed to decode event at position %d: %v", count, err)
		}
		count++
	}

	if count != numEvents {
		t.Errorf("Expected %d events, got %d", numEvents, count)
	}

	t.Logf("✓ Successfully processed %d events", count)
}

// TestConcurrentValidation tests parallel validation
func TestConcurrentValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cogRoot := filepath.Join(tmpDir, ".cog")
	memoryDir := filepath.Join(cogRoot, "memory", "semantic")

	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		t.Fatalf("Failed to create memory directory: %v", err)
	}

	// Create multiple test files
	numDocs := 20
	for i := 0; i < numDocs; i++ {
		docPath := filepath.Join(memoryDir, fmt.Sprintf("doc%d.cog.md", i))
		content := fmt.Sprintf(`---
type: note
id: doc%d
title: Document %d
created: 2026-01-16
---

Document %d content.
`, i, i, i)
		if err := os.WriteFile(docPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write doc %d: %v", i, err)
		}
	}

	// Validate all concurrently
	docs, _ := filepath.Glob(filepath.Join(memoryDir, "*.cog.md"))
	results := make(chan *CogdocValidation, len(docs))

	for _, doc := range docs {
		go func(path string) {
			results <- validateCogdocFull(path)
		}(doc)
	}

	// Collect results
	failures := 0
	for i := 0; i < len(docs); i++ {
		result := <-results
		if len(result.Errors) > 0 {
			failures++
		}
	}

	if failures > 0 {
		t.Errorf("Concurrent validation failed for %d documents", failures)
	}

	t.Logf("✓ Concurrent validation succeeded for %d documents", len(docs))
}
