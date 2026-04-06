// benchmark.go — foveated context benchmark runner
//
// Measures context assembly quality against a set of prompts with known expected
// CogDoc matches. For each prompt it records:
//
//   - Recall: fraction of expected docs that appeared in the assembled context
//   - Precision: fraction of injected docs that were expected
//   - Assembly latency (ms)
//   - Total token count of assembled context
//   - Model response (for manual quality review)
//
// Run with: cogos-v3 bench [--prompts path] [--model name] [--budget n]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// BenchmarkPrompt is a single test case.
type BenchmarkPrompt struct {
	Prompt           string   `json:"prompt"`
	ExpectedDocs     []string `json:"expected_docs"`     // partial path/ID fragments to match
	ExpectedKeywords []string `json:"expected_keywords"` // words expected in response (future)
}

// BenchmarkResult is the measured output for a single prompt.
type BenchmarkResult struct {
	Prompt       string
	AssemblyMs   int64
	TotalTokens  int
	InjectedDocs []string
	ExpectedDocs []string
	Recall       float64 // |injected ∩ expected| / |expected|
	Precision    float64 // |injected ∩ expected| / |injected|
	Response     string  // for manual review
	ResponseMs   int64
}

// BenchmarkSuite runs a set of prompts through context assembly and optionally
// inference, collecting quality metrics.
type BenchmarkSuite struct {
	process *Process
	router  Router
	model   string
	budget  int
}

// NewBenchmarkSuite constructs a suite bound to the given process and router.
// Pass nil for router to skip inference (assembly metrics only).
func NewBenchmarkSuite(process *Process, router Router, model string, budget int) *BenchmarkSuite {
	if budget <= 0 {
		budget = 4096
	}
	return &BenchmarkSuite{
		process: process,
		router:  router,
		model:   model,
		budget:  budget,
	}
}

// LoadPrompts reads benchmark prompts from a JSON file.
func LoadPrompts(path string) ([]BenchmarkPrompt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var prompts []BenchmarkPrompt
	if err := json.Unmarshal(data, &prompts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return prompts, nil
}

// Run executes all prompts sequentially and returns results.
func (b *BenchmarkSuite) Run(ctx context.Context, prompts []BenchmarkPrompt) []BenchmarkResult {
	results := make([]BenchmarkResult, 0, len(prompts))
	for i, p := range prompts {
		slog.Info("bench: prompt", "n", i+1, "total", len(prompts))
		results = append(results, b.runOne(ctx, p))
	}
	return results
}

// runOne executes a single benchmark prompt.
func (b *BenchmarkSuite) runOne(ctx context.Context, p BenchmarkPrompt) BenchmarkResult {
	r := BenchmarkResult{
		Prompt:       p.Prompt,
		ExpectedDocs: p.ExpectedDocs,
	}

	// Assemble context.
	t0 := time.Now()
	benchMsgs := []ProviderMessage{{Role: "user", Content: p.Prompt}}
	pkg, err := b.process.AssembleContext(p.Prompt, benchMsgs, b.budget)
	r.AssemblyMs = time.Since(t0).Milliseconds()
	if err != nil {
		slog.Warn("bench: assembly error", "err", err)
		return r
	}
	r.TotalTokens = pkg.TotalTokens
	r.InjectedDocs = pkg.InjectedPaths
	r.Recall, r.Precision = matchScore(pkg.InjectedPaths, p.ExpectedDocs)

	// Optionally run inference.
	if b.router != nil {
		systemPrompt, msgs := pkg.FormatForProvider()
		creq := &CompletionRequest{
			SystemPrompt:  systemPrompt,
			Messages:      msgs,
			ModelOverride: b.model,
			Metadata: RequestMetadata{
				RequestID:    fmt.Sprintf("bench-%d", time.Now().UnixNano()),
				ProcessState: b.process.State().String(),
				Source:       "benchmark",
			},
		}
		if provider, _, rerr := b.router.Route(ctx, creq); rerr == nil {
			t1 := time.Now()
			if resp, cerr := provider.Complete(ctx, creq); cerr == nil {
				r.Response = resp.Content
			} else {
				slog.Warn("bench: inference error", "err", cerr)
			}
			r.ResponseMs = time.Since(t1).Milliseconds()
		}
	}

	return r
}

// matchScore computes recall and precision.
// A doc is "matched" if any injected path contains any expected fragment (case-insensitive).
func matchScore(injected, expected []string) (recall, precision float64) {
	if len(expected) == 0 {
		return 1.0, 1.0
	}
	if len(injected) == 0 {
		return 0.0, 0.0
	}

	tp := 0
	for _, exp := range expected {
		expL := strings.ToLower(exp)
		for _, inj := range injected {
			if strings.Contains(strings.ToLower(inj), expL) {
				tp++
				break
			}
		}
	}

	recall = float64(tp) / float64(len(expected))
	precision = float64(tp) / float64(len(injected))
	return
}

// PrintSummary writes a tabular result summary to stdout.
func PrintSummary(results []BenchmarkResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tRecall\tPrec\tTokens\tAsmMs\tInfMs\tPrompt")
	fmt.Fprintln(w, "-\t------\t----\t------\t-----\t-----\t------")
	for i, r := range results {
		prompt := r.Prompt
		if len(prompt) > 52 {
			prompt = prompt[:49] + "..."
		}
		fmt.Fprintf(w, "%d\t%.2f\t%.2f\t%d\t%d\t%d\t%s\n",
			i+1, r.Recall, r.Precision,
			r.TotalTokens, r.AssemblyMs, r.ResponseMs,
			prompt,
		)
	}
	w.Flush()

	if len(results) == 0 {
		return
	}
	var sumRecall, sumPrec float64
	for _, r := range results {
		sumRecall += r.Recall
		sumPrec += r.Precision
	}
	n := float64(len(results))
	fmt.Printf("\nAverage  recall=%.2f  precision=%.2f  n=%d\n", sumRecall/n, sumPrec/n, len(results))
}

// SaveResults writes benchmark results as a CogDoc-format experiment log.
// The file is written under .cog/mem/episodic/experiments/.
func SaveResults(workspaceRoot string, results []BenchmarkResult, model, method string) error {
	dir := filepath.Join(workspaceRoot, ".cog", "mem", "episodic", "experiments")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	ts := time.Now().UTC().Format("2006-01-02T150405")
	path := filepath.Join(dir, fmt.Sprintf("bench-%s.md", ts))

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("type: experiment\n")
	fmt.Fprintf(&sb, "title: \"Foveated Context Benchmark Run\"\n")
	fmt.Fprintf(&sb, "created: \"%s\"\n", time.Now().UTC().Format(time.RFC3339))
	sb.WriteString("config:\n")
	fmt.Fprintf(&sb, "  model: %s\n", model)
	sb.WriteString("  budget: 4096\n")
	fmt.Fprintf(&sb, "  method: %s\n", method)
	sb.WriteString("---\n\n")
	sb.WriteString("# Foveated Context Benchmark Run\n\n")

	// Aggregate stats.
	var sumRecall, sumPrec float64
	for _, r := range results {
		sumRecall += r.Recall
		sumPrec += r.Precision
	}
	n := float64(len(results))
	fmt.Fprintf(&sb, "**Avg Recall:** %.2f  **Avg Precision:** %.2f  **N:** %d\n\n",
		sumRecall/n, sumPrec/n, len(results))

	// Per-prompt results.
	for i, r := range results {
		fmt.Fprintf(&sb, "## Prompt %d\n\n", i+1)
		fmt.Fprintf(&sb, "**Prompt:** %s\n\n", r.Prompt)
		fmt.Fprintf(&sb, "**Recall:** %.2f  **Precision:** %.2f  **Tokens:** %d  **Assembly:** %dms\n\n",
			r.Recall, r.Precision, r.TotalTokens, r.AssemblyMs)
		if len(r.InjectedDocs) > 0 {
			sb.WriteString("**Injected:**\n")
			for _, p := range r.InjectedDocs {
				fmt.Fprintf(&sb, "- `%s`\n", filepath.Base(p))
			}
			sb.WriteString("\n")
		}
		if r.Response != "" {
			sb.WriteString("**Response:**\n\n")
			fmt.Fprintf(&sb, "```\n%s\n```\n\n", r.Response)
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("Results saved: %s\n", path)
	return nil
}

// runBenchCmd is the entry point for the "bench" subcommand.
//
//	cogos-v3 bench [--workspace path] [--prompts path] [--model name] [--budget n] [--no-inference]
func runBenchCmd(args []string, workspaceRoot string, defaultPort int) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	workspace := fs.String("workspace", workspaceRoot, "Workspace root path (auto-detected if empty)")
	promptsFile := fs.String("prompts", "", "Path to benchmark_prompts.json (default: testdata/)")
	model := fs.String("model", "", "Model override (e.g. qwen3.5:9b)")
	budget := fs.Int("budget", 4096, "Token budget for context assembly")
	noInference := fs.Bool("no-inference", false, "Skip inference, measure assembly only")
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if os.Getenv("COG_LOG_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := LoadConfig(*workspace, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	nucleus, err := LoadNucleus(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load nucleus: %v\n", err)
		os.Exit(1)
	}

	process := NewProcess(cfg, nucleus)

	// Build index eagerly.
	if idx, ierr := BuildIndex(cfg.WorkspaceRoot); ierr != nil {
		slog.Warn("bench: index build failed", "err", ierr)
	} else {
		process.indexMu.Lock()
		process.index = idx
		process.indexMu.Unlock()
		slog.Info("bench: index built", "docs", len(idx.ByURI))
	}

	// Update attentional field.
	if ferr := process.field.Update(); ferr != nil {
		slog.Warn("bench: field update failed", "err", ferr)
	}
	slog.Info("bench: field updated", "docs", process.field.Len())

	// Resolve prompts file.
	pf := *promptsFile
	if pf == "" {
		// Default: look next to this binary, then fall back to repo location.
		exe, _ := os.Executable()
		candidates := []string{
			filepath.Join(filepath.Dir(exe), "testdata", "benchmark_prompts.json"),
			filepath.Join(cfg.WorkspaceRoot, "apps", "cogos-v3", "testdata", "benchmark_prompts.json"),
		}
		for _, c := range candidates {
			if _, serr := os.Stat(c); serr == nil {
				pf = c
				break
			}
		}
	}
	if pf == "" {
		fmt.Fprintln(os.Stderr, "error: cannot find benchmark_prompts.json; use --prompts flag")
		os.Exit(1)
	}

	prompts, err := LoadPrompts(pf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	slog.Info("bench: loaded prompts", "n", len(prompts), "file", pf)

	// Build router unless --no-inference.
	var router Router
	if !*noInference {
		router, err = BuildRouter(cfg)
		if err != nil {
			slog.Warn("bench: router unavailable; assembly-only mode", "err", err)
		}
	}

	suite := NewBenchmarkSuite(process, router, *model, *budget)
	results := suite.Run(context.Background(), prompts)

	fmt.Println()
	PrintSummary(results)

	if err := SaveResults(cfg.WorkspaceRoot, results, *model, "keyword-match"); err != nil {
		slog.Warn("bench: save results failed", "err", err)
	}
}
