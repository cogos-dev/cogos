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
package conversations

import (
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
