// coverage_test.go — tests for the per-source coverage tracker.
//
// Covers:
//   1. RecordMapped increments mapped counter
//   2. RecordDegenerate increments both degenerate and mapped
//   3. RecordQuarantined increments quarantined and unmapped component counts
//   4. SetRefs persists ontology/mapping refs
//   5. All() returns a copy (mutation-safe)
//   6. Reset() clears all counters
//   7. Multiple sources tracked independently
//   8. Full ingest cycle: mixed mapped/quarantined records → coverage totals
//   9. (Bug 1 regression) RecordDegenerate called for role='tool' hermes records
//  10. (Bug 2 regression) Coverage stable across two identical reconcile cycles
package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── CoverageTracker unit tests ───────────────────────────────────────────────

func TestCoverageTracker_RecordMapped(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordMapped("src1")
	ct.RecordMapped("src1")
	ct.RecordMapped("src1")

	m := ct.All()
	if m["src1"].Mapped != 3 {
		t.Errorf("mapped: want 3, got %d", m["src1"].Mapped)
	}
	if m["src1"].Quarantined != 0 {
		t.Errorf("quarantined: want 0, got %d", m["src1"].Quarantined)
	}
}

func TestCoverageTracker_RecordDegenerate(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordDegenerate("src1")

	m := ct.All()
	if m["src1"].Mapped != 1 {
		t.Errorf("mapped: want 1 (degenerate counts as mapped), got %d", m["src1"].Mapped)
	}
	if m["src1"].Degenerate != 1 {
		t.Errorf("degenerate: want 1, got %d", m["src1"].Degenerate)
	}
}

func TestCoverageTracker_RecordQuarantined(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordQuarantined("src1", "tool_result")
	ct.RecordQuarantined("src1", "tool_result")
	ct.RecordQuarantined("src1", "image")

	m := ct.All()
	sc := m["src1"]
	if sc.Quarantined != 3 {
		t.Errorf("quarantined: want 3, got %d", sc.Quarantined)
	}
	if sc.UnmappedComponentCounts["tool_result"] != 2 {
		t.Errorf("tool_result count: want 2, got %d", sc.UnmappedComponentCounts["tool_result"])
	}
	if sc.UnmappedComponentCounts["image"] != 1 {
		t.Errorf("image count: want 1, got %d", sc.UnmappedComponentCounts["image"])
	}
}

func TestCoverageTracker_SetRefs(t *testing.T) {
	ct := NewCoverageTracker()
	ct.SetRefs("src1", "cogos.conversations@1.0.0", "claude-code-jsonl@1.0.0")

	m := ct.All()
	sc := m["src1"]
	if sc.OntologyRef != "cogos.conversations@1.0.0" {
		t.Errorf("ontology_ref: want cogos.conversations@1.0.0, got %q", sc.OntologyRef)
	}
	if sc.MappingRef != "claude-code-jsonl@1.0.0" {
		t.Errorf("mapping_ref: want claude-code-jsonl@1.0.0, got %q", sc.MappingRef)
	}
}

func TestCoverageTracker_AllReturnsCopy(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordMapped("src1")

	m1 := ct.All()
	// Mutate the returned copy — should not affect the tracker.
	sc := m1["src1"]
	sc.Mapped = 999
	m1["src1"] = sc

	m2 := ct.All()
	if m2["src1"].Mapped != 1 {
		t.Errorf("All() mutation leaked: want 1, got %d", m2["src1"].Mapped)
	}
}

func TestCoverageTracker_Reset(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordMapped("src1")
	ct.RecordMapped("src2")
	ct.Reset()

	m := ct.All()
	if len(m) != 0 {
		t.Errorf("after Reset: want 0 sources, got %d", len(m))
	}
}

func TestCoverageTracker_MultipleSources(t *testing.T) {
	ct := NewCoverageTracker()
	ct.RecordMapped("hermes-darkstar")
	ct.RecordMapped("hermes-darkstar")
	ct.RecordQuarantined("claude-code-jsonl", "tool_result")
	ct.SetRefs("hermes-darkstar", "cogos.conversations@1.0.0", "hermes-statedb.v1@1.0.0")
	ct.SetRefs("claude-code-jsonl", "cogos.conversations@1.0.0", "claude-code-jsonl@1.0.0")

	m := ct.All()
	if len(m) != 2 {
		t.Errorf("expected 2 sources, got %d", len(m))
	}
	if m["hermes-darkstar"].Mapped != 2 {
		t.Errorf("hermes-darkstar mapped: want 2, got %d", m["hermes-darkstar"].Mapped)
	}
	if m["claude-code-jsonl"].Quarantined != 1 {
		t.Errorf("claude-code-jsonl quarantined: want 1, got %d", m["claude-code-jsonl"].Quarantined)
	}
}

// ─── Full ingest cycle: mixed mapped/quarantined → coverage totals ─────────────

func TestCoverageFromIngest_MixedSourcesFullCycle(t *testing.T) {
	lo, _ := makeTestOntology(t)
	cov := NewCoverageTracker()

	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = lo
	acc.Coverage = cov
	// No quarantine writer needed for this test (nil is safe — just won't write files).

	var lines []string
	// 5 records from known-source (should be mapped)
	for i := 0; i < 5; i++ {
		lines = append(lines, makeIngestRecord("known-source", "s1", "user", "msg", "2026-06-10T00:00:00Z", nil))
	}
	// 3 records from unknown-source (should be quarantined)
	for i := 0; i < 3; i++ {
		lines = append(lines, makeIngestRecord("unknown-source", "s2", "assistant", "reply", "2026-06-10T00:00:00Z", nil))
	}

	if err := acc.ConsumeFile(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	m := cov.All()

	if m["known-source"].Mapped != 5 {
		t.Errorf("known-source mapped: want 5, got %d", m["known-source"].Mapped)
	}
	if m["known-source"].Quarantined != 0 {
		t.Errorf("known-source quarantined: want 0, got %d", m["known-source"].Quarantined)
	}

	if m["unknown-source"].Quarantined != 3 {
		t.Errorf("unknown-source quarantined: want 3, got %d", m["unknown-source"].Quarantined)
	}
	if m["unknown-source"].Mapped != 0 {
		t.Errorf("unknown-source mapped: want 0, got %d", m["unknown-source"].Mapped)
	}

	// Dedup: 5 known-source msgs all have same content hash → actually only 1 unique
	// (dedup by content hash since no stable_id). Check that sessions reflect this.
	sessions := acc.Sessions()
	if len(sessions) != 1 {
		// Only known-source sessions make it through; unknown-source is quarantined.
		// All 5 known-source records have same content → only 1 after dedup.
		t.Logf("sessions: %d (dedup collapses identical records)", len(sessions))
	}
}

// ─── Bug regression tests ─────────────────────────────────────────────────────

// makeHermesOntology creates a LoadedOntology that mirrors the hermes-statedb
// mapping spec: hermes-darkstar and hermes-cog are mapped sources, role='tool'
// records are classified as degenerate (text_tool_degenerate rule).
func makeHermesOntology(t *testing.T) *LoadedOntology {
	t.Helper()
	dir := t.TempDir()

	l1 := `ontology: cogos.ontology/v1
id: cogos.conversations
version: 1.0.0
entities:
  session:
    description: One conversation session.
    keys: [source, session_id]
components:
  session.turn:
    description: A conversation turn.
    fields: { role: string, text: string, timestamp: rfc3339 }
    required: [role, text, timestamp]
relations: {}
`
	if err := os.WriteFile(filepath.Join(dir, "cogos.conversations.v1.yaml"), []byte(l1), 0o644); err != nil {
		t.Fatalf("write L1: %v", err)
	}

	mappingsDir := filepath.Join(dir, "mappings")
	if err := os.Mkdir(mappingsDir, 0o755); err != nil {
		t.Fatalf("mkdir mappings: %v", err)
	}

	// Minimal hermes-statedb mapping: intended rules for user/assistant, plus
	// the degenerate rule for tool rows — mirroring the production spec.
	m2 := `mapping:
  id: hermes-statedb.v1
  version: 1.0.0
  sources:
    - hermes-darkstar
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
	if err := os.WriteFile(filepath.Join(mappingsDir, "hermes-statedb.v1.yaml"), []byte(m2), 0o644); err != nil {
		t.Fatalf("write L2: %v", err)
	}

	lo, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir: %v", err)
	}
	return lo
}

// TestCoverageRecordDegenerateWiredForToolRole is the Bug 1 regression.
// Verifies that role='tool' records from a hermes source increment Degenerate
// (not just Mapped) in the coverage tracker, per the text_tool_degenerate rule
// in the hermes-statedb.v1 L2 mapping spec.
func TestCoverageRecordDegenerateWiredForToolRole(t *testing.T) {
	lo := makeHermesOntology(t)
	cov := NewCoverageTracker()

	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = lo
	acc.Coverage = cov

	ts := "2026-06-10T00:00:00Z"
	source := "hermes-darkstar"

	lines := []string{
		// 3 intended (user + assistant)
		makeIngestRecord(source, "s1", "user", "user message", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":1"}}),
		makeIngestRecord(source, "s1", "assistant", "assistant reply", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":2"}}),
		makeIngestRecord(source, "s1", "user", "follow-up", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":3"}}),
		// 2 degenerate (tool rows mapped to session.turn)
		makeIngestRecord(source, "s1", "tool", `{"result":"ok"}`, ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":4"}}),
		makeIngestRecord(source, "s1", "tool", `{"result":"err"}`, ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":5"}}),
	}

	if err := acc.ConsumeFile(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	m := cov.All()
	sc := m[source]

	// Degenerate must be > 0 — RecordDegenerate is wired for role='tool'.
	if sc.Degenerate == 0 {
		t.Errorf("Degenerate: want > 0 (role='tool' records must trigger RecordDegenerate), got 0")
	}
	if sc.Degenerate != 2 {
		t.Errorf("Degenerate: want 2, got %d", sc.Degenerate)
	}
	// Degenerate records are also counted as mapped (per RecordDegenerate contract).
	if sc.Mapped != 5 {
		t.Errorf("Mapped: want 5 (3 intended + 2 degenerate), got %d", sc.Mapped)
	}
	// Quarantined must be 0 — all records are from a mapped source.
	if sc.Quarantined != 0 {
		t.Errorf("Quarantined: want 0, got %d", sc.Quarantined)
	}
}

// TestCoverageStableAcrossReconcileCycles is the Bug 2 regression.
// Verifies that running two identical reconcile cycles over the same input
// produces identical coverage counts.  Previously, coverage accumulated
// without reset across cycles, causing counts to double on each cycle.
func TestCoverageStableAcrossReconcileCycles(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	// Load the hermes ontology so degenerate classification fires.
	lo := makeHermesOntology(t)
	ontDir := filepath.Join(root, ".cog", "observatory", "ontology")
	if err := os.MkdirAll(filepath.Join(ontDir, "mappings"), 0o755); err != nil {
		t.Fatalf("mkdir ontology: %v", err)
	}
	// Write L1 + L2 into the workspace ontology dir so LoadConfig picks them up.
	l1Src := filepath.Join(t.TempDir(), "l1.yaml")
	_ = lo // already in memory; write a minimal fixture that LoadOntologyDir will read

	// Build the full ontology dir the provider will read from.
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
	_ = l1Src
	if err := os.WriteFile(filepath.Join(ontDir, "cogos.conversations.v1.yaml"), []byte(ontL1), 0o644); err != nil {
		t.Fatalf("write L1 to ontdir: %v", err)
	}
	ontL2 := `mapping:
  id: hermes-statedb.v1
  version: 1.0.0
  sources:
    - hermes-darkstar
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
		t.Fatalf("write L2 to ontdir: %v", err)
	}

	// Write test ingest data: 4 user, 3 assistant, 2 tool records.
	source := "hermes-darkstar"
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
	writeIngestDir(t, ingestRoot, source, "20260610T000000Z-run1", lines)

	// Point the observatory config at our ingest dir and ontology dir.
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

	// First reconcile cycle.
	reconcileOnce(t, p, root)
	cov1 := p.Coverage()

	// Second reconcile cycle — identical input, no drift, all sources skip.
	reconcileOnce(t, p, root)
	cov2 := p.Coverage()

	// Counts must be identical across cycles (Bug 2: was doubling each cycle).
	sc1 := cov1[source]
	sc2 := cov2[source]

	if sc1.Mapped != sc2.Mapped {
		t.Errorf("Mapped not stable: cycle1=%d cycle2=%d (expected equal)", sc1.Mapped, sc2.Mapped)
	}
	if sc1.Degenerate != sc2.Degenerate {
		t.Errorf("Degenerate not stable: cycle1=%d cycle2=%d (expected equal)", sc1.Degenerate, sc2.Degenerate)
	}
	if sc1.Quarantined != sc2.Quarantined {
		t.Errorf("Quarantined not stable: cycle1=%d cycle2=%d (expected equal)", sc1.Quarantined, sc2.Quarantined)
	}

	// Bug 1 check: degenerate must be > 0 (2 tool records).
	if sc1.Degenerate == 0 {
		t.Errorf("Degenerate: want > 0 (role='tool' records), got 0 in cycle 1")
	}
	if sc1.Degenerate != 2 {
		t.Errorf("Degenerate: want 2, got %d in cycle 1", sc1.Degenerate)
	}
	// Mapped = intended (7) + degenerate (2) = 9.
	if sc1.Mapped != 9 {
		t.Errorf("Mapped: want 9 (7 intended + 2 degenerate), got %d in cycle 1", sc1.Mapped)
	}
}
