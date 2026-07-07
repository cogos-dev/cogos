package experiment

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCollectCellReplicates_Ks0_SmallN exercises the full D6 collection
// pipeline (real testkernel boot -> Module E cadence taps -> discard-first
// -> M11r) at the reference cell Ks0 (10,4,2; ceil=3, cadence=12s) with a
// small n, small enough to run in this session (n=3 => 4 events => ~48s +
// boot overhead). This is NOT the full confirmatory sweep (which needs
// n up to 240 per the frozen n-ladder, ~11.87h across all 9 cells) — it is
// the correctness proof that the pipeline computes M11r == the law-
// predicted ceil(C/H) on real kernel data.
func TestCollectCellReplicates_Ks0_SmallN(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cadence-wait test in -short mode")
	}
	cell, ok := CellByID("Ks0")
	if !ok {
		t.Fatal("Ks0 not found")
	}
	const n = 3
	// n+2 events at ~cadence seconds each, generous overhead.
	lifetime := time.Duration(cell.CadenceLaw()*(n+3))*time.Second + 15*time.Second

	result := CollectCellReplicates(t, cell, n, lifetime)
	if len(result.ConsolidationIntervalsSeconds) < n {
		t.Fatalf("collected %d consolidation intervals; want >= %d", len(result.ConsolidationIntervalsSeconds), n)
	}
	if result.RunError {
		t.Fatal("RunError = true; want false for a healthy fresh test workspace")
	}
	if result.AnyProcessActiveOverlap {
		t.Error("AnyProcessActiveOverlap = true; want false for a dormant measurement boot (H6)")
	}

	m11r, consCadence, hbCadence := M11rFromResult(result)
	wantRatio := float64(cell.CeilCH())
	const tol = 0.5 // seconds-scale jitter tolerance on the ratio
	if diff := m11r - wantRatio; diff > tol || diff < -tol {
		t.Errorf("M11r = %v; want ~%v (ceil(C/H) at the non-divisible reference cell), cons=%.2fs hb=%.2fs", m11r, wantRatio, consCadence, hbCadence)
	}

	obs := EvaluateCellObservation(result, 30)
	if obs.KC3LawConfirmed == nil {
		t.Fatal("KC3LawConfirmed is nil; want set for a non-divisible cell")
	}
	if !*obs.KC3LawConfirmed {
		t.Errorf("KC3LawConfirmed = false; want true (real kernel data at Ks0 should confirm the law), residual=%v", *obs.KC3LawResidualMs)
	}
}

// TestObservationWriter_AppendOnlyOutOfTree confirms the observation
// writer targets the out-of-tree data store (H3) and appends rather than
// truncates.
func TestObservationWriter_AppendOnlyOutOfTree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("COGOS_WORKSPACE_ROOT", tmpHome)

	runID := NewRunID("test-run")
	w, err := NewObservationWriter(runID)
	if err != nil {
		t.Fatalf("NewObservationWriter: %v", err)
	}

	wantPath := filepath.Join(tmpHome, "first-instruments-runs", runID, "observations.jsonl")
	if w.Path() != wantPath {
		t.Errorf("Path() = %q; want %q", w.Path(), wantPath)
	}

	if err := w.Write(Observation{CellID: "Ks0", M11r: 3.0}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(Observation{CellID: "Ks2", M11r: 3.0}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read observations.jsonl: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("observations.jsonl has %d lines; want 2", lines)
	}

	// Re-open and append a third row — must not truncate the first two.
	w2, err := NewObservationWriter(runID)
	if err != nil {
		t.Fatalf("NewObservationWriter (reopen): %v", err)
	}
	if err := w2.Write(Observation{CellID: "Ks4", M11r: 3.0}); err != nil {
		t.Fatalf("Write (reopen): %v", err)
	}
	w2.Close()

	data2, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read observations.jsonl (after reopen): %v", err)
	}
	lines2 := 0
	for _, b := range data2 {
		if b == '\n' {
			lines2++
		}
	}
	if lines2 != 3 {
		t.Errorf("observations.jsonl has %d lines after reopen+append; want 3 (append-only, K7)", lines2)
	}
}

// TestRunsRoot_DefaultsToHomeWorkspaces confirms the env-var-pathing
// default when COGOS_WORKSPACE_ROOT is unset.
func TestRunsRoot_DefaultsToHomeWorkspaces(t *testing.T) {
	t.Setenv("COGOS_WORKSPACE_ROOT", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "workspaces", "first-instruments-runs")
	got, err := RunsRoot()
	if err != nil {
		t.Fatalf("RunsRoot: %v", err)
	}
	if got != want {
		t.Errorf("RunsRoot() = %q; want %q", got, want)
	}
}

// TestBuildCellManifest_NineCells confirms the manifest cell table has all
// 9 frozen cells with correct kill-eligibility and divisibility tags.
func TestBuildCellManifest_NineCells(t *testing.T) {
	entries := BuildCellManifest()
	if len(entries) != 9 {
		t.Fatalf("len(entries) = %d; want 9", len(entries))
	}
	byID := map[string]CellManifestEntry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	ks0 := byID["Ks0"]
	if !ks0.NonDivisibleStabilityFamily {
		t.Error("Ks0.NonDivisibleStabilityFamily = false; want true")
	}
	ksND2 := byID["KsND2"]
	if ksND2.NonDivisibleStabilityFamily {
		t.Error("KsND2.NonDivisibleStabilityFamily = true; want false (not a pure co-scale)")
	}
	k0 := byID["K0"]
	if !k0.Divisible {
		t.Error("K0.Divisible = false; want true (production anchor, mixture-check only)")
	}
}

// ─── Injector tests ─────────────────────────────────────────────────────────

func TestMemoryFileDriftInjector_WritesNFiles(t *testing.T) {
	dir := t.TempDir()
	inj := MemoryFileDriftInjector{WorkspaceRoot: dir, N: 3}
	written, err := inj.Inject()
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(written) != 3 {
		t.Fatalf("len(written) = %d; want 3", len(written))
	}
	for _, p := range written {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s to exist: %v", p, err)
		}
	}
}

func TestWorktreeDivergenceInjector_RequiresGitRepo(t *testing.T) {
	dir := t.TempDir() // NOT a git repo
	inj := WorktreeDivergenceInjector{WorkspaceRoot: dir, CommitsAhead: FrozenConfirmatoryCommitsAhead}
	err := inj.Inject()
	if err == nil {
		t.Fatal("Inject succeeded against a non-git directory; want a clear error")
	}
}

func TestConfigDriftInjector_WritesYAML(t *testing.T) {
	dir := t.TempDir()
	inj := ConfigDriftInjector{WorkspaceRoot: dir, YAML: "consolidation_interval: 999\n"}
	if err := inj.Inject(); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".cog", "config", "kernel.yaml"))
	if err != nil {
		t.Fatalf("read kernel.yaml: %v", err)
	}
	if string(data) != "consolidation_interval: 999\n" {
		t.Errorf("kernel.yaml content = %q; want the injected YAML", data)
	}
}
