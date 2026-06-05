// projection_compiler_test.go — acceptance tests for ProjectionCompiler v0.
//
// Covers the five v0 acceptance criteria from ADR
// projection-compiler-primitive §Acceptance criteria, restricted to the
// structural-extraction path (LLM extraction and FrictionEvent are deferred):
//
//  1. cogblock.py roundtrip is clean on both source cogdocs.
//  2. 2026-05-19 cogdoc emits one event per "## Quote N" section
//     (6 quote-boundaries in the current fixture).
//  3. 2026-05-20 cogdoc emits one event per "## Distinction N:" section.
//     The fixture grew from 10 → 17 distinctions since the ADR was drafted;
//     the test asserts the structural rule rather than the snapshot count
//     and records the live count for the report.
//  4. Re-running on unchanged cogdocs produces only Skip actions
//     (pointer-mode events; no Create/Update).
//  5. Modifying one block triggers Update for exactly that block; all
//     other blocks remain Skip (pointer-referenced).
//
// All tests skip cleanly when python3 or PyYAML is unavailable so CI
// without a Python toolchain still passes (the cogblock.py subprocess is
// the only external dependency in v0).

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── Test fixtures + helpers ─────────────────────────────────────────────────

// fixtureDir returns the absolute path to internal/engine/testdata/projection_compiler.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "projection_compiler")
}

// cogblockScript returns the testdata-local copy of cogblock.py.
func cogblockScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(fixtureDir(t), "cogblock.py")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing cogblock.py fixture: %v", err)
	}
	return p
}

// fixturePath returns the absolute path to a named fixture cogdoc.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(fixtureDir(t), name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing fixture %s: %v", name, err)
	}
	return p
}

// skipIfNoPython skips the test when python3 + PyYAML isn't usable. The
// compiler's only runtime dependency in v0 is the cogblock.py subprocess.
func skipIfNoPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping projection-compiler acceptance test")
	}
	cmd := exec.Command("python3", "-c", "import yaml")
	if err := cmd.Run(); err != nil {
		t.Skipf("python3 PyYAML not available: %v", err)
	}
}

// runReconcileCycle runs LoadConfig → FetchLive → ComputePlan → ApplyPlan →
// BuildState for the given source files. Returns the populated config,
// the live snapshot, the plan, the apply results, and the resulting state.
func runReconcileCycle(
	t *testing.T,
	c *ProjectionCompiler,
	tmpRoot string,
	sourceFiles []string,
) (*CompilerConfig, []*sourceCogdoc, *reconcile.Plan, []reconcile.Result, *reconcile.State) {
	t.Helper()
	cfgAny, err := c.LoadConfig(tmpRoot)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*CompilerConfig)
	cfg.SourceFiles = sourceFiles

	ctx := context.Background()
	liveAny, err := c.FetchLive(ctx, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	live := liveAny.([]*sourceCogdoc)

	plan, err := c.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	// ComputePlan must stamp compiler_config into plan.Metadata so ApplyPlan can
	// persist HashByAnchor state. Asserting it here exercises the production path
	// (the daemon relies on this auto-stamp; do NOT hand-stamp it in the harness,
	// or the test would mask a regression of that wiring).
	if got, ok := plan.Metadata["compiler_config"].(*CompilerConfig); !ok || got != cfg {
		t.Fatalf("ComputePlan did not stamp compiler_config into plan.Metadata (got %v, ok=%v)", plan.Metadata["compiler_config"], ok)
	}

	results, err := c.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	state, err := c.BuildState(cfg, live, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}

	return cfg, live, plan, results, state
}

// stageRoot prepares a temp workspace with .cog/mem/reflective/ existing
// and the cogblock.py path resolvable (via COGBLOCK_PY). Returns the root.
func stageRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "mem", "reflective"), 0o755); err != nil {
		t.Fatalf("mkdir reflective: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	// Symlink cogblock.py into the staged workspace so LoadConfig finds it
	// at the canonical <root>/scripts/cogblock.py location without needing
	// COGBLOCK_PY in the env.
	target, err := filepath.Abs(cogblockScript(t))
	if err != nil {
		t.Fatalf("abs cogblock.py: %v", err)
	}
	link := filepath.Join(root, "scripts", "cogblock.py")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink cogblock.py: %v", err)
	}
	return root
}

// ─── A1: cogblock.py roundtrip ───────────────────────────────────────────────

// TestAcceptance_A1_RoundtripClean — A1: cogblock.py roundtrip clean on
// both source cogdocs. This is a precondition: if the parser cannot round-
// trip the inputs, downstream extraction is unsound.
func TestAcceptance_A1_RoundtripClean(t *testing.T) {
	skipIfNoPython(t)
	script := cogblockScript(t)

	for _, name := range []string{
		"2026-05-19-chaz-substrate-physics-sequence.cog.md",
		"2026-05-20-substrate-coupling-pattern-formalization.cog.md",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("python3", script, "roundtrip", fixturePath(t, name))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("roundtrip failed: %v\noutput:\n%s", err, string(out))
			}
		})
	}
}

// ─── A2: 2026-05-19 → one event per Quote-N ──────────────────────────────────

// TestAcceptance_A2_QuoteBoundaries — A2: the 2026-05-19 cogdoc emits one
// event per "## Quote N" section. The current fixture has six quote
// sections; the assertion fails loudly if a quote is added/removed so the
// count is checkpointed at the time of the assertion update.
func TestAcceptance_A2_QuoteBoundaries(t *testing.T) {
	skipIfNoPython(t)
	root := stageRoot(t)
	compiler := NewProjectionCompiler()

	src := fixturePath(t, "2026-05-19-chaz-substrate-physics-sequence.cog.md")
	_, _, plan, _, _ := runReconcileCycle(t, compiler, root, []string{src})

	events := compiler.EmittedEvents()
	const wantQuotes = 6

	// First, structural-rule assertion: every emitted event from this
	// cogdoc must have a "quote-N" anchor.
	for _, e := range events {
		if !strings.Contains(e.Source, "#quote-") {
			t.Errorf("expected quote-N anchor in source, got %q", e.Source)
		}
	}
	if got := len(events); got != wantQuotes {
		t.Errorf("expected %d events from quote-boundary extraction, got %d", wantQuotes, got)
	}

	// Plan must classify all six as Creates (first run, no prior state).
	if plan.Summary.Creates != wantQuotes {
		t.Errorf("expected %d Creates, got %d (Updates=%d Skipped=%d)",
			wantQuotes, plan.Summary.Creates, plan.Summary.Updates, plan.Summary.Skipped)
	}

	// CompileModel = "structural" on a first run.
	for _, e := range events {
		if e.CompileModel != "structural" {
			t.Errorf("event %q: expected CompileModel=structural, got %q", e.Source, e.CompileModel)
		}
	}
}

// ─── A3: 2026-05-20 → one event per Distinction-N ────────────────────────────

// TestAcceptance_A3_DistinctionBoundaries — A3: the 2026-05-20 cogdoc
// emits one event per "## Distinction N:" section.
//
// The ADR snapshot called for exactly 10 events; the live fixture has
// grown to 17 distinctions since the ADR was drafted. The test asserts
// the invariant rule (one event per structural boundary) and verifies the
// count matches the number of Distinction-N H2 sections actually present
// in the file. The count is also recorded via t.Log so the report can
// quote the live number.
func TestAcceptance_A3_DistinctionBoundaries(t *testing.T) {
	skipIfNoPython(t)
	root := stageRoot(t)
	compiler := NewProjectionCompiler()

	src := fixturePath(t, "2026-05-20-substrate-coupling-pattern-formalization.cog.md")
	_, _, plan, _, _ := runReconcileCycle(t, compiler, root, []string{src})

	events := compiler.EmittedEvents()

	// Count the structural boundaries in the source so the assertion
	// scales with the fixture. Anything tagged "Distinction <int>:" at
	// H2 contributes one event.
	wantDistinctions := countDistinctionSections(t, src)
	t.Logf("2026-05-20 fixture has %d ## Distinction N: sections; ADR snapshot expected 10", wantDistinctions)

	for _, e := range events {
		if !strings.Contains(e.Source, "#distinction-") {
			t.Errorf("expected distinction-N anchor in source, got %q", e.Source)
		}
	}
	if got := len(events); got != wantDistinctions {
		t.Errorf("expected %d events (one per Distinction-N section), got %d",
			wantDistinctions, got)
	}
	if plan.Summary.Creates != wantDistinctions {
		t.Errorf("expected %d Creates, got %d (Updates=%d Skipped=%d)",
			wantDistinctions, plan.Summary.Creates, plan.Summary.Updates, plan.Summary.Skipped)
	}
}

// countDistinctionSections is a fixture-side counter: scans for H2 lines
// beginning with "Distinction <digits>:" so the acceptance assertion uses
// the same structural rule the compiler does, but computed independently.
func countDistinctionSections(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	count := 0
	inFence := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		heading := strings.TrimPrefix(line, "## ")
		if strings.HasPrefix(strings.ToLower(heading), "distinction ") {
			// Require a digit run + colon or em-dash separator to match
			// the compiler's classification rule.
			rest := heading[len("Distinction "):]
			i := 0
			for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
				i++
			}
			if i == 0 {
				continue
			}
			after := rest[i:]
			if after == "" || strings.HasPrefix(after, ":") ||
				strings.HasPrefix(after, " ") || strings.HasPrefix(after, "—") ||
				strings.HasPrefix(after, "-") {
				count++
			}
		}
	}
	return count
}

// ─── A4: idempotency ─────────────────────────────────────────────────────────

// TestAcceptance_A4_Idempotency — A4: re-running on unchanged cogdocs
// emits only pointer events (Skip actions) on the second pass; no
// Creates, no Updates.
func TestAcceptance_A4_Idempotency(t *testing.T) {
	skipIfNoPython(t)
	root := stageRoot(t)

	srcs := []string{
		fixturePath(t, "2026-05-19-chaz-substrate-physics-sequence.cog.md"),
		fixturePath(t, "2026-05-20-substrate-coupling-pattern-formalization.cog.md"),
	}

	// Pass 1: full extraction. Use one compiler instance per pass to
	// confirm that the persisted state file alone carries continuity.
	pass1 := NewProjectionCompiler()
	_, _, plan1, _, _ := runReconcileCycle(t, pass1, root, srcs)
	if plan1.Summary.Creates == 0 {
		t.Fatalf("pass 1: expected non-zero Creates, got %+v", plan1.Summary)
	}
	if plan1.Summary.Updates != 0 || plan1.Summary.Skipped != 0 {
		t.Errorf("pass 1: expected only Creates, got Updates=%d Skipped=%d",
			plan1.Summary.Updates, plan1.Summary.Skipped)
	}

	// Pass 2: same files, same content, different compiler instance.
	pass2 := NewProjectionCompiler()
	_, _, plan2, _, _ := runReconcileCycle(t, pass2, root, srcs)
	if plan2.Summary.Creates != 0 {
		t.Errorf("pass 2 idempotency: expected 0 Creates, got %d", plan2.Summary.Creates)
	}
	if plan2.Summary.Updates != 0 {
		t.Errorf("pass 2 idempotency: expected 0 Updates, got %d", plan2.Summary.Updates)
	}
	if plan2.Summary.Skipped != plan1.Summary.Creates {
		t.Errorf("pass 2 idempotency: expected Skipped=%d, got %d",
			plan1.Summary.Creates, plan2.Summary.Skipped)
	}

	// Pointer-mode CompileModel on every emitted event in pass 2.
	for _, e := range pass2.EmittedEvents() {
		if e.CompileModel != "pointer" {
			t.Errorf("pass 2: event %q expected pointer mode, got %q",
				e.Source, e.CompileModel)
		}
	}
}

// ─── A5: single-block modification ───────────────────────────────────────────

// TestAcceptance_A5_SingleBlockModification — A5: modifying one block in
// a compiled cogdoc triggers exactly the events for that block; all other
// blocks remain pointer-referenced.
//
// The fixture is copied into the staged workspace so the modification
// doesn't touch the test inputs. We change the distinction text inside
// Quote-3's blockquote (a meaningful surface change) and re-reconcile.
func TestAcceptance_A5_SingleBlockModification(t *testing.T) {
	skipIfNoPython(t)
	root := stageRoot(t)

	// Copy the 2026-05-19 fixture into a writable location.
	original := fixturePath(t, "2026-05-19-chaz-substrate-physics-sequence.cog.md")
	mutable := filepath.Join(root, "fixture.cog.md")
	originalData, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if err := os.WriteFile(mutable, originalData, 0o644); err != nil {
		t.Fatalf("write mutable: %v", err)
	}

	// Pass 1: compile the unmodified copy.
	pass1 := NewProjectionCompiler()
	_, _, plan1, _, _ := runReconcileCycle(t, pass1, root, []string{mutable})
	totalEvents := plan1.Summary.Creates
	if totalEvents < 2 {
		t.Fatalf("pass 1: expected at least 2 events for a single-block-edit test, got %d",
			totalEvents)
	}

	// Modify a single block's content: swap a substring inside Quote 3's
	// blockquote. The marker is a verbatim phrase from the fixture.
	const marker = "This also means that the wave is initiated by the highest-energy distinction"
	const replacement = "MODIFIED: the wave is initiated by the highest-energy distinction"
	if !strings.Contains(string(originalData), marker) {
		t.Fatalf("test marker %q not in fixture; update marker", marker)
	}
	modified := strings.Replace(string(originalData), marker, replacement, 1)
	if err := os.WriteFile(mutable, []byte(modified), 0o644); err != nil {
		t.Fatalf("write modified: %v", err)
	}

	// Pass 2: same fixture, one block modified.
	pass2 := NewProjectionCompiler()
	_, _, plan2, _, _ := runReconcileCycle(t, pass2, root, []string{mutable})
	if plan2.Summary.Updates != 1 {
		t.Errorf("pass 2 single-block edit: expected exactly 1 Update, got %d (Skipped=%d Creates=%d)",
			plan2.Summary.Updates, plan2.Summary.Skipped, plan2.Summary.Creates)
	}
	if plan2.Summary.Skipped != totalEvents-1 {
		t.Errorf("pass 2 single-block edit: expected Skipped=%d, got %d",
			totalEvents-1, plan2.Summary.Skipped)
	}
	if plan2.Summary.Creates != 0 {
		t.Errorf("pass 2 single-block edit: expected 0 Creates, got %d", plan2.Summary.Creates)
	}

	// Confirm the single Update points at the quote-3 anchor.
	for _, action := range plan2.Actions {
		if action.Action != reconcile.ActionUpdate {
			continue
		}
		if !strings.Contains(action.Name, "#quote-3") {
			t.Errorf("expected Update on #quote-3 anchor, got %q", action.Name)
		}
	}
}

// ─── Sanity: Reconcilable contract surface ───────────────────────────────────

// TestProjectionCompiler_Type pins the registry key.
func TestProjectionCompiler_Type(t *testing.T) {
	c := NewProjectionCompiler()
	if got := c.Type(); got != "projection-compiler" {
		t.Errorf("Type() = %q, want %q", got, "projection-compiler")
	}
}

// TestProjectionCompiler_HealthInitiallyProgressing pins the v0 health
// starting condition.
func TestProjectionCompiler_HealthInitiallyProgressing(t *testing.T) {
	c := NewProjectionCompiler()
	if got := c.Health().Health; got != reconcile.HealthProgressing {
		t.Errorf("initial Health = %q, want %q", got, reconcile.HealthProgressing)
	}
}
