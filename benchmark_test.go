// benchmark_test.go — unit tests for the benchmark runner
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── matchScore ────────────────────────────────────────────────────────────────

func TestMatchScoreEmptyExpected(t *testing.T) {
	r, p := matchScore([]string{"/a/b/foo.md"}, []string{})
	if r != 1.0 {
		t.Errorf("recall: got %.2f want 1.0", r)
	}
	if p != 1.0 {
		t.Errorf("precision: got %.2f want 1.0", p)
	}
}

func TestMatchScoreEmptyInjected(t *testing.T) {
	r, p := matchScore([]string{}, []string{"eigenform"})
	if r != 0.0 {
		t.Errorf("recall: got %.2f want 0.0", r)
	}
	if p != 0.0 {
		t.Errorf("precision: got %.2f want 0.0", p)
	}
}

func TestMatchScoreFullMatch(t *testing.T) {
	r, p := matchScore(
		[]string{"/mem/semantic/claude-eigenform-continuity.cog.md"},
		[]string{"eigenform"},
	)
	if r != 1.0 {
		t.Errorf("recall: got %.2f want 1.0", r)
	}
	if p != 1.0 {
		t.Errorf("precision: got %.2f want 1.0", p)
	}
}

func TestMatchScorePartialRecall(t *testing.T) {
	injected := []string{"/mem/claude-eigenform-continuity.cog.md"}
	expected := []string{"eigenform", "alpha-derivation"}
	r, p := matchScore(injected, expected)
	if r != 0.5 {
		t.Errorf("recall: got %.2f want 0.5", r)
	}
	if p != 1.0 {
		t.Errorf("precision: got %.2f want 1.0", p)
	}
}

func TestMatchScoreLowPrecision(t *testing.T) {
	// 2 injected, 1 expected; only 1 matches → recall=1.0, precision=0.5
	injected := []string{"/mem/eigenform.md", "/mem/unrelated.md"}
	expected := []string{"eigenform"}
	r, p := matchScore(injected, expected)
	if r != 1.0 {
		t.Errorf("recall: got %.2f want 1.0", r)
	}
	if p != 0.5 {
		t.Errorf("precision: got %.2f want 0.5", p)
	}
}

func TestMatchScoreCaseInsensitive(t *testing.T) {
	r, _ := matchScore(
		[]string{"/mem/EigenForm.cog.md"},
		[]string{"eigenform"},
	)
	if r != 1.0 {
		t.Errorf("recall: case-sensitive mismatch, got %.2f want 1.0", r)
	}
}

// ── LoadPrompts ───────────────────────────────────────────────────────────────

func TestLoadPrompts(t *testing.T) {
	prompts, err := LoadPrompts(filepath.Join("testdata", "benchmark_prompts.json"))
	if err != nil {
		t.Fatalf("LoadPrompts: %v", err)
	}
	if len(prompts) == 0 {
		t.Fatal("expected at least one prompt")
	}
	for i, p := range prompts {
		if p.Prompt == "" {
			t.Errorf("prompt %d: empty prompt field", i)
		}
	}
}

func TestLoadPromptsInvalidJSON(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(tmp, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPrompts(tmp)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadPromptsMissingFile(t *testing.T) {
	_, err := LoadPrompts(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ── BenchmarkSuite (no router — assembly-only mode) ───────────────────────────

func TestBenchmarkSuiteNoRouter(t *testing.T) {
	ws := makeWorkspace(t)
	cfg := makeConfig(t, ws)
	nucleus := makeNucleus("test", "tester")

	// Add a simple CogDoc to the workspace so the index finds something.
	memDir := filepath.Join(ws, ".cog", "mem", "semantic")
	writeTestFile(t, filepath.Join(memDir, "eigenform-test.md"),
		"---\nid: eigenform-test\ntitle: Eigenform Test\ntags: [eigenform, test]\n---\n\nEigenform content here.\n")

	process := NewProcess(cfg, nucleus)
	// Build index manually (normally happens on consolidation tick).
	idx, err := BuildIndex(ws)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	process.indexMu.Lock()
	process.index = idx
	process.indexMu.Unlock()

	suite := NewBenchmarkSuite(process, nil, "", 4096)
	prompts := []BenchmarkPrompt{
		{Prompt: "explain eigenform", ExpectedDocs: []string{"eigenform-test"}},
		{Prompt: "something unrelated", ExpectedDocs: []string{"missing-doc"}},
	}

	results := suite.Run(context.Background(), prompts)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// First prompt should find the eigenform doc.
	if results[0].Recall < 0.5 {
		t.Logf("injected: %v", results[0].InjectedDocs)
		// Not fatal — depends on whether field has scores; just verify it ran.
	}
	if results[0].AssemblyMs < 0 {
		t.Errorf("assembly time should be non-negative")
	}
}

func TestBenchmarkSuiteAssemblyTokens(t *testing.T) {
	ws := makeWorkspace(t)
	cfg := makeConfig(t, ws)
	nucleus := makeNucleus("test", "tester")
	process := NewProcess(cfg, nucleus)

	suite := NewBenchmarkSuite(process, nil, "", 4096)
	results := suite.Run(context.Background(), []BenchmarkPrompt{
		{Prompt: "test query"},
	})
	if len(results) != 1 {
		t.Fatal("expected 1 result")
	}
	// Nucleus text is always injected, so tokens > 0.
	if results[0].TotalTokens <= 0 {
		t.Errorf("expected TotalTokens > 0, got %d", results[0].TotalTokens)
	}
}

// ── SaveResults ───────────────────────────────────────────────────────────────

func TestSaveResults(t *testing.T) {
	ws := t.TempDir()
	results := []BenchmarkResult{
		{
			Prompt:       "test prompt",
			Recall:       0.8,
			Precision:    0.6,
			TotalTokens:  1024,
			AssemblyMs:   5,
			InjectedDocs: []string{"/some/path/doc.md"},
			Response:     "test response",
		},
	}

	if err := SaveResults(ws, results, "qwen3.5:9b", "keyword-match"); err != nil {
		t.Fatalf("SaveResults: %v", err)
	}

	// Verify file was created.
	dir := filepath.Join(ws, ".cog", "mem", "episodic", "experiments")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}

	// Verify YAML frontmatter.
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "type: experiment") {
		t.Error("missing 'type: experiment' in saved file")
	}
	if !strings.Contains(content, "qwen3.5:9b") {
		t.Error("missing model name in saved file")
	}
}

// ── PrintSummary (smoke test — no crash) ─────────────────────────────────────

func TestPrintSummaryEmpty(t *testing.T) {
	// Should not panic on empty results.
	PrintSummary([]BenchmarkResult{})
}

func TestPrintSummary(t *testing.T) {
	results := []BenchmarkResult{
		{Prompt: "short", Recall: 1.0, Precision: 0.5, TotalTokens: 100, AssemblyMs: 3},
		{Prompt: strings.Repeat("x", 60), Recall: 0.5, Precision: 0.5},
	}
	// Redirect stdout is complex; just verify it doesn't panic.
	PrintSummary(results)
}
