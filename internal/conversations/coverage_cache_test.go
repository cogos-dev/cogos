// coverage_cache_test.go — regression test for the per-source coverage cache.
//
// The conversations Reconcilable previously re-parsed the entire ingest corpus
// on every reconcile cycle (including ActionSkip sources) purely to recompute
// coverage. That pinned multiple CPU cores at the ~30s reconcile cadence. The
// fix caches per-source coverage and serves it on ActionSkip without parsing,
// since ActionSkip is itself the drift-free signal.
//
// This test asserts that an unchanged ingest source across two cycles is served
// from cache on the second cycle WITHOUT re-parsing, and that the cached numbers
// equal a from-scratch parse of the same input.
package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHermesOntologyDir writes a minimal L1 + L2 ontology into <ontDir> that the
// provider's LoadConfig will read. role='tool' is classified degenerate so the
// coverage numbers are non-trivial (mapped + degenerate + intended all exercised).
func writeHermesOntologyDir(t *testing.T, ontDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ontDir, "mappings"), 0o755); err != nil {
		t.Fatalf("mkdir ontology: %v", err)
	}
	ontL1 := `ontology: cogos.ontology/v1
id: cogos.conversations
version: 1.0.0
entities:
  session:
    description: One session.
    keys: [source, session_id]
components:
  session.turn:
    description: A turn.
    fields: { role: string, text: string, timestamp: rfc3339 }
    required: [role, text, timestamp]
relations: {}
`
	if err := os.WriteFile(filepath.Join(ontDir, "cogos.conversations.v1.yaml"), []byte(ontL1), 0o644); err != nil {
		t.Fatalf("write L1: %v", err)
	}
	ontL2 := `mapping:
  id: hermes-statedb.v1
  version: 1.0.0
  sources:
    - hermes-node-a
    - hermes-cog
  ontology: "cogos.conversations@^1"
rules:
  - id: text_user
    source_condition: "role = 'user' AND content IS NOT NULL AND content != ''"
    target_class: session.turn
    quality: intended
  - id: text_assistant_with_prose
    source_condition: "role = 'assistant' AND content IS NOT NULL AND content != ''"
    target_class: session.turn
    quality: intended
  - id: text_tool_degenerate
    source_condition: "role = 'tool' AND content IS NOT NULL AND content != ''"
    target_class: session.turn
    quality: degenerate
`
	if err := os.WriteFile(filepath.Join(ontDir, "mappings", "hermes-statedb.v1.yaml"), []byte(ontL2), 0o644); err != nil {
		t.Fatalf("write L2: %v", err)
	}
}

// writeCacheTestConfig writes an observatory.yaml pointing at an empty source
// dir (so the default ~/.claude path is not scanned), the given ingest root, and
// the given ontology dir.
func writeCacheTestConfig(t *testing.T, root, ingestRoot, ontDir string) {
	t.Helper()
	cfgDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	cfgContent := fmt.Sprintf(`source_dirs:
  - %s
ingest_dirs:
  - %s
ontology_dir: %s
include_patterns:
  - "*.jsonl"
`, t.TempDir(), ingestRoot, ontDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "observatory.yaml"), []byte(cfgContent), 0o644); err != nil {
		t.Fatalf("write observatory.yaml: %v", err)
	}
}

// fromScratchCoverage parses the given ingest lines through a fresh accumulator
// with the given ontology and returns the resulting per-source coverage. This is
// the ground-truth a cached value must match.
func fromScratchCoverage(t *testing.T, lines []string, lo *LoadedOntology) map[string]SourceCoverage {
	t.Helper()
	cov := NewCoverageTracker()
	a := newIngestAccumulator(defaultMaxTurnLen)
	a.Ontology = lo
	a.Coverage = cov
	if err := a.ConsumeFile(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("from-scratch ConsumeFile: %v", err)
	}
	return cov.All()
}

// TestCoverageCache_SkipServesFromCacheWithoutReparse asserts the hot-loop fix:
// across two reconcile cycles over an UNCHANGED ingest source, the second cycle
// serves coverage from the cache and does NOT re-parse the source files.
//
// Re-parse detection is hermetic and unambiguous: after cycle 1 primes the cache,
// the source file is made unreadable (chmod 0000) WITHOUT altering its size or
// mtime, so ComputePlan still emits ActionSkip. If cycle 2 attempted a re-parse it
// would fail to open the file (recorded as a non-fatal coverage error and yielding
// empty coverage). A cache hit avoids the open entirely, so coverage stays correct
// and no error is recorded.
func TestCoverageCache_SkipServesFromCacheWithoutReparse(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	ontDir := filepath.Join(root, ".cog", "observatory", "ontology")
	writeHermesOntologyDir(t, ontDir)

	// 4 user + 3 assistant (intended) + 2 tool (degenerate) = 9 mapped, 2 degenerate.
	source := "hermes-node-a"
	ts := "2026-06-10T00:00:00Z"
	var lines []string
	for i := 1; i <= 4; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "user",
			fmt.Sprintf("user msg %d", i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	for i := 5; i <= 7; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "assistant",
			fmt.Sprintf("asst reply %d", i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	for i := 8; i <= 9; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "tool",
			fmt.Sprintf(`{"result":"r%d"}`, i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	ingestFile := writeIngestDir(t, ingestRoot, source, "20260610T000000Z-run1", lines)

	writeCacheTestConfig(t, root, ingestRoot, ontDir)

	// ── Cycle 1: full reconcile parses the source and primes the cache. ──
	reconcileOnce(t, p, root)
	cov1 := p.Coverage()
	sc1 := cov1[source]
	if sc1.Mapped != 9 {
		t.Fatalf("cycle 1 Mapped: want 9, got %d", sc1.Mapped)
	}
	if sc1.Degenerate != 2 {
		t.Fatalf("cycle 1 Degenerate: want 2, got %d", sc1.Degenerate)
	}

	// Sanity: cache is warm for the source after cycle 1.
	p.mu.Lock()
	_, warm := p.coverageCache[source]
	p.mu.Unlock()
	if !warm {
		t.Fatalf("coverageCache not primed for %q after cycle 1", source)
	}

	// ── Make the source unreadable WITHOUT changing size/mtime. ──
	// chmod 0000 leaves stat() metadata (size, mtime) intact, so ComputePlan
	// still classifies the source as ActionSkip; only an actual open() would
	// fail. This is the re-parse tripwire.
	if err := os.Chmod(ingestFile, 0o000); err != nil {
		t.Fatalf("chmod source unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ingestFile, 0o644) })

	// Confirm an open really would fail now (guards against running as root).
	if f, err := os.Open(ingestFile); err == nil {
		_ = f.Close()
		t.Skip("source file still readable after chmod 0000 (likely running as root); re-parse tripwire unavailable")
	}

	// ── Cycle 2: source unchanged → ActionSkip → must serve from cache. ──
	reconcileOnce(t, p, root)

	// No errors must have been recorded. A re-parse attempt would have appended
	// a non-fatal "coverage-only pass" error from the failed open().
	p.mu.Lock()
	cycle2Errs := append([]string(nil), p.lastErrors...)
	p.mu.Unlock()
	if len(cycle2Errs) != 0 {
		t.Fatalf("cycle 2 recorded errors (indicates a re-parse was attempted): %v", cycle2Errs)
	}

	cov2 := p.Coverage()
	sc2 := cov2[source]

	// Coverage served from cache must be byte-for-byte equal to cycle 1.
	if sc2.Mapped != sc1.Mapped || sc2.Degenerate != sc1.Degenerate || sc2.Quarantined != sc1.Quarantined {
		t.Fatalf("cached coverage drifted: cycle1=%+v cycle2=%+v", sc1, sc2)
	}

	// And it must equal a from-scratch parse of the same input (correctness, not
	// just stability).
	want := fromScratchCoverage(t, lines, p.Ontology())[source]
	if sc2.Mapped != want.Mapped || sc2.Degenerate != want.Degenerate || sc2.Quarantined != want.Quarantined {
		t.Fatalf("cached coverage != from-scratch parse: cached=%+v scratch=%+v", sc2, want)
	}
}

// TestCoverageCache_RemovedSourceIsPruned asserts the dead-eviction fix:
// when an ingest source that was cached in coverageCache is absent from the
// plan on the next cycle (i.e. the source directory was removed from the
// observatory config), its cache entry must be gone after ApplyPlan completes.
//
// Before the fix, the ActionDelete branch did `delete(p.coverageCache,
// action.Name)` where action.Name is a SESSION id, not a SOURCE name, so the
// delete never matched and the stale entry leaked forever. The fix prunes by
// diffing coverageCache keys against the set of sources that emitted any
// create/update/skip action this cycle.
func TestCoverageCache_RemovedSourceIsPruned(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	ontDir := filepath.Join(root, ".cog", "observatory", "ontology")
	writeHermesOntologyDir(t, ontDir)

	source := "hermes-node-a"
	ts := "2026-06-10T00:00:00Z"
	var lines []string
	for i := 1; i <= 3; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "user",
			fmt.Sprintf("msg %d", i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	writeIngestDir(t, ingestRoot, source, "run1", lines)
	writeCacheTestConfig(t, root, ingestRoot, ontDir)

	// ── Cycle 1: source is created + cache is primed. ──
	reconcileOnce(t, p, root)

	p.mu.Lock()
	_, warm := p.coverageCache[source]
	p.mu.Unlock()
	if !warm {
		t.Fatalf("coverageCache not primed for %q after cycle 1", source)
	}

	// ── Remove the ingest source from the config so cycle 2 has no ingest. ──
	// Point ingest_dirs at a fresh empty directory (no source sub-directories),
	// so ComputePlan emits ActionDelete for the now-stale index sessions and
	// emits NO create/update/skip for the source — making it absent from
	// liveSources and triggering the prune path.
	emptyIngest := t.TempDir()
	writeCacheTestConfig(t, root, emptyIngest, ontDir)

	// ── Cycle 2: source is gone → cache entry must be pruned. ──
	reconcileOnce(t, p, root)

	p.mu.Lock()
	_, stillWarm := p.coverageCache[source]
	p.mu.Unlock()
	if stillWarm {
		t.Fatalf("coverageCache still contains %q after source was removed — eviction is broken", source)
	}
}

// TestCoverageCache_OntologyEditInvalidatesCache is the end-to-end regression
// test for SourceFingerprint (ontology.go): an in-place edit to an L2 mapping
// file must invalidate the coverage cache for the sources it serves, even
// though the ingest JSONL files themselves are byte-for-byte unchanged and
// ComputePlan therefore still emits ActionSkip (isIngestDrift only compares
// source-file size/mtime — it is structurally blind to an ontology_dir edit).
//
// Before SourceFingerprint was wired into ApplyPlan's ActionSkip cache check,
// this scenario served cycle 1's stale coverage forever: warm-cache-hit had no
// invalidation trigger other than a process restart.
func TestCoverageCache_OntologyEditInvalidatesCache(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	ontDir := filepath.Join(root, ".cog", "observatory", "ontology")
	writeHermesOntologyDir(t, ontDir)

	// 3 user (intended) + 2 tool (degenerate, per the fixture's
	// text_tool_degenerate rule) = 5 mapped, 2 degenerate.
	source := "hermes-node-a"
	ts := "2026-06-10T00:00:00Z"
	var lines []string
	for i := 1; i <= 3; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "user",
			fmt.Sprintf("user msg %d", i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	for i := 4; i <= 5; i++ {
		lines = append(lines, makeIngestRecord(source, "sess-1", "tool",
			fmt.Sprintf(`{"result":"r%d"}`, i), ts,
			map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, i)}}))
	}
	writeIngestDir(t, ingestRoot, source, "20260610T000000Z-run1", lines)
	writeCacheTestConfig(t, root, ingestRoot, ontDir)

	// ── Cycle 1: full reconcile parses the source and primes the cache. ──
	reconcileOnce(t, p, root)
	sc1 := p.Coverage()[source]
	if sc1.Mapped != 5 {
		t.Fatalf("cycle 1 Mapped: want 5, got %d", sc1.Mapped)
	}
	if sc1.Degenerate != 2 {
		t.Fatalf("cycle 1 Degenerate: want 2, got %d", sc1.Degenerate)
	}

	// ── In-place edit to the L2 mapping: role='tool' is no longer degenerate,
	// it is now an intended mapping. The ingest source files are never
	// touched, so this is exactly the drift class isIngestDrift cannot see.
	hermesMappingPath := filepath.Join(ontDir, "mappings", "hermes-statedb.v1.yaml")
	data, err := os.ReadFile(hermesMappingPath)
	if err != nil {
		t.Fatalf("read hermes mapping: %v", err)
	}
	edited := strings.Replace(string(data),
		"    target_class: session.turn\n    quality: degenerate\n",
		"    target_class: session.turn\n    quality: intended\n", 1)
	if edited == string(data) {
		t.Fatal("test fixture drifted: expected 'quality: degenerate' rule text not found")
	}
	if err := os.WriteFile(hermesMappingPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("rewrite hermes mapping: %v", err)
	}

	// ── Cycle 2: ingest source is unchanged (still ActionSkip) but the
	// ontology fingerprint has changed, so coverage must be recomputed. ──
	reconcileOnce(t, p, root)
	sc2 := p.Coverage()[source]

	if sc2.Degenerate != 0 {
		t.Fatalf("cycle 2 Degenerate: want 0 (mapping edit reclassified role='tool' as intended), got %d — coverage cache did not invalidate on ontology edit", sc2.Degenerate)
	}
	if sc2.Mapped != 5 {
		t.Fatalf("cycle 2 Mapped: want 5 (total record count unchanged by the reclassification), got %d", sc2.Mapped)
	}

	// And it must equal a from-scratch parse under the EDITED ontology
	// (correctness, not just "changed").
	want := fromScratchCoverage(t, lines, p.Ontology())[source]
	if sc2.Mapped != want.Mapped || sc2.Degenerate != want.Degenerate || sc2.Quarantined != want.Quarantined {
		t.Fatalf("post-edit cached coverage != from-scratch parse under edited ontology: cached=%+v scratch=%+v", sc2, want)
	}
}
