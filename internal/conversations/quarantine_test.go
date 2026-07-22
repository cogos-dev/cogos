// quarantine_test.go — tests for quarantine-with-provenance behaviour.
//
// Covers:
//   1. QuarantineWriter writes a valid JSONL file with provenance
//   2. Multiple writes append (not overwrite)
//   3. Source name sanitization
//   4. Ingest accumulator routes unmapped-source records to quarantine
//   5. Quarantine counts are reflected in coverage tracker
package conversations

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── QuarantineWriter unit tests ─────────────────────────────────────────────

func TestQuarantineWriter_WritesRecord(t *testing.T) {
	qDir := t.TempDir()
	qw := NewQuarantineWriter(qDir)

	original := json.RawMessage(`{"schema":"cogos.observatory.conversations/v0.1","source":"test-source","session_id":"s1","role":"user","timestamp":"2026-06-10T00:00:00Z","text":"hello"}`) //nolint:lll
	prov := QuarantineProvenance{
		Reason:      QuarantineReasonUnmappedComponent,
		Component:   "session.turn",
		OntologyRef: "cogos.conversations@1.0.0",
		MappingRef:  "",
	}

	if err := qw.WriteRecord("test-source", original, prov); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	// Verify the file exists and is valid JSONL.
	path := filepath.Join(qDir, "test-source", "quarantine.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read quarantine file: %v", err)
	}

	var rec quarantineRecord
	if jsonErr := json.Unmarshal(data[:len(data)-1], &rec); jsonErr != nil { // trim trailing \n
		t.Fatalf("unmarshal quarantine record: %v", jsonErr)
	}

	if rec.Quarantine.Reason != QuarantineReasonUnmappedComponent {
		t.Errorf("reason: want %v, got %v", QuarantineReasonUnmappedComponent, rec.Quarantine.Reason)
	}
	if rec.Quarantine.OntologyRef != "cogos.conversations@1.0.0" {
		t.Errorf("ontology_ref: want cogos.conversations@1.0.0, got %q", rec.Quarantine.OntologyRef)
	}
	if rec.Quarantine.QuarantinedAt == "" {
		t.Error("quarantined_at should be non-empty")
	}
}

func TestQuarantineWriter_AppendsBetweenCalls(t *testing.T) {
	qDir := t.TempDir()
	qw := NewQuarantineWriter(qDir)

	prov := QuarantineProvenance{Reason: QuarantineReasonUnmappedComponent, Component: "session.turn"}

	// Three DISTINCT records (distinct stable_ids) must all append.
	for i := 0; i < 3; i++ {
		original := json.RawMessage(`{"schema":"cogos.observatory.conversations/v0.1","source":"src","session_id":"s1","role":"user","timestamp":"2026-06-10T00:00:00Z","text":"msg","refs":{"stable_id":"src:` + string(rune('a'+i)) + `"}}`) //nolint:lll
		if err := qw.WriteRecord("src", original, prov); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}

	if got := countLines(t, filepath.Join(qDir, "src", "quarantine.jsonl")); got != 3 {
		t.Errorf("expected 3 lines, got %d", got)
	}
}

// TestQuarantineWriter_IdempotentByStableID is the regression test for the
// unbounded-growth bug: a quarantine-only source re-read every reconcile cycle
// re-quarantined the same records forever. Re-quarantining a record with the
// same stable_id must be a no-op.
func TestQuarantineWriter_IdempotentByStableID(t *testing.T) {
	qDir := t.TempDir()
	prov := QuarantineProvenance{Reason: QuarantineReasonDraftRole, Component: "session.turn"}
	original := json.RawMessage(`{"schema":"cogos.observatory.conversations/v0.1","source":"claude-ai-web","session_id":"c1","role":"user-draft","timestamp":"2026-06-14T19:52:32Z","text":"could you","refs":{"stable_id":"claude-ai-web:c1:6db3df9a4b641b8f"}}`) //nolint:lll
	path := filepath.Join(qDir, "claude-ai-web", "quarantine.jsonl")

	// Same record, many cycles, same writer.
	qw := NewQuarantineWriter(qDir)
	for i := 0; i < 50; i++ {
		if err := qw.WriteRecord("claude-ai-web", original, prov); err != nil {
			t.Fatalf("WriteRecord cycle %d: %v", i, err)
		}
	}
	if got := countLines(t, path); got != 1 {
		t.Fatalf("same-writer: expected 1 line, got %d", got)
	}

	// Idempotency must survive a process restart (fresh writer loads on-disk
	// keys) — models the kernel being restarted between reconcile cycles.
	qw2 := NewQuarantineWriter(qDir)
	for i := 0; i < 50; i++ {
		if err := qw2.WriteRecord("claude-ai-web", original, prov); err != nil {
			t.Fatalf("fresh-writer WriteRecord cycle %d: %v", i, err)
		}
	}
	if got := countLines(t, path); got != 1 {
		t.Fatalf("fresh-writer: expected 1 line, got %d", got)
	}

	// A record without a stable_id dedups by content hash — byte-identical
	// re-quarantines are still a no-op.
	noStable := json.RawMessage(`{"schema":"cogos.observatory.conversations/v0.1","source":"nostable","session_id":"n1","role":"user","timestamp":"2026-06-10T00:00:00Z","text":"x"}`) //nolint:lll
	qw3 := NewQuarantineWriter(qDir)
	for i := 0; i < 5; i++ {
		if err := qw3.WriteRecord("nostable", noStable, prov); err != nil {
			t.Fatalf("nostable WriteRecord cycle %d: %v", i, err)
		}
	}
	if got := countLines(t, filepath.Join(qDir, "nostable", "quarantine.jsonl")); got != 1 {
		t.Fatalf("nostable content-hash dedup: expected 1 line, got %d", got)
	}
}

// countLines counts non-empty lines in a JSONL file.
func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxScannerTokenSize), maxScannerTokenSize)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count
}

func TestQuarantineWriter_SourceNameSanitization(t *testing.T) {
	qDir := t.TempDir()
	qw := NewQuarantineWriter(qDir)

	original := json.RawMessage(`{"schema":"cogos.observatory.conversations/v0.1","source":"src/sub","session_id":"s1","role":"user","timestamp":"2026-06-10T00:00:00Z","text":"x"}`)
	prov := QuarantineProvenance{Reason: QuarantineReasonUnmappedComponent, Component: "session.turn"}

	if err := qw.WriteRecord("src/sub", original, prov); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	// Should create "src_sub" not "src/sub" (which would create a nested dir).
	path := filepath.Join(qDir, "src_sub", "quarantine.jsonl")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("sanitized path %s not found: %v", path, statErr)
	}
}

// ─── Ingest accumulator quarantine routing tests ─────────────────────────────

// makeTestOntology creates a minimal LoadedOntology with session.turn mapped
// for source "known-source" but NOT for "unknown-source".
func makeTestOntology(t *testing.T) (*LoadedOntology, string) {
	t.Helper()
	dir := t.TempDir()

	// Write L1.
	l1 := `ontology: cogos.ontology/v1
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
	if err := os.WriteFile(filepath.Join(dir, "cogos.conversations.v1.yaml"), []byte(l1), 0o644); err != nil {
		t.Fatalf("write L1: %v", err)
	}

	mappingsDir := filepath.Join(dir, "mappings")
	if err := os.Mkdir(mappingsDir, 0o755); err != nil {
		t.Fatalf("mkdir mappings: %v", err)
	}

	// L2 mapping only for "known-source".
	m2 := `mapping:
  id: test-mapping
  version: 1.0.0
  source: known-source
  ontology: "cogos.conversations@^1"
rules:
  - id: user-text
    emit:
      component: session.turn
`
	if err := os.WriteFile(filepath.Join(mappingsDir, "test-mapping.v1.yaml"), []byte(m2), 0o644); err != nil {
		t.Fatalf("write L2: %v", err)
	}

	lo, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir: %v", err)
	}
	return lo, dir
}

func TestIngestAccumulator_QuarantinesUnknownSource(t *testing.T) {
	lo, _ := makeTestOntology(t)
	qDir := t.TempDir()
	qw := NewQuarantineWriter(qDir)
	cov := NewCoverageTracker()

	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = lo
	acc.Quarantine = qw
	acc.Coverage = cov

	// Record from "unknown-source" — no mapping exists, should be quarantined.
	record := makeQRecord("unknown-source", "s1", "user", "hello unknown")
	if err := acc.ConsumeFile(strings.NewReader(record + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	// Accumulator should have 0 sessions (record was quarantined, not indexed).
	if len(acc.Sessions()) != 0 {
		t.Errorf("expected 0 sessions (quarantined), got %d", len(acc.Sessions()))
	}
	if acc.Quarantined != 1 {
		t.Errorf("Quarantined: want 1, got %d", acc.Quarantined)
	}

	// Coverage should show quarantined.
	covMap := cov.All()
	sc, ok := covMap["unknown-source"]
	if !ok {
		t.Fatal("expected coverage entry for unknown-source")
	}
	if sc.Quarantined != 1 {
		t.Errorf("coverage quarantined: want 1, got %d", sc.Quarantined)
	}
	if sc.Mapped != 0 {
		t.Errorf("coverage mapped: want 0, got %d", sc.Mapped)
	}

	// Quarantine file should exist.
	qPath := filepath.Join(qDir, "unknown-source", "quarantine.jsonl")
	if _, err := os.Stat(qPath); err != nil {
		t.Errorf("quarantine file not created: %v", err)
	}
}

func TestIngestAccumulator_IndexesKnownSource(t *testing.T) {
	lo, _ := makeTestOntology(t)
	qDir := t.TempDir()
	qw := NewQuarantineWriter(qDir)
	cov := NewCoverageTracker()

	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = lo
	acc.Quarantine = qw
	acc.Coverage = cov

	// Record from "known-source" — mapping exists, should be indexed.
	record := makeQRecord("known-source", "s1", "user", "hello known")
	if err := acc.ConsumeFile(strings.NewReader(record + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	if len(acc.Sessions()) != 1 {
		t.Errorf("expected 1 session, got %d", len(acc.Sessions()))
	}
	if acc.Quarantined != 0 {
		t.Errorf("Quarantined: want 0, got %d", acc.Quarantined)
	}

	// Coverage: mapped=1, quarantined=0.
	covMap := cov.All()
	sc, ok := covMap["known-source"]
	if !ok {
		t.Fatal("expected coverage entry for known-source")
	}
	if sc.Mapped != 1 {
		t.Errorf("mapped: want 1, got %d", sc.Mapped)
	}
	if sc.Quarantined != 0 {
		t.Errorf("quarantined: want 0, got %d", sc.Quarantined)
	}
}

func TestIngestAccumulator_L3Tags(t *testing.T) {
	lo, _ := makeTestOntology(t)
	acc := newIngestAccumulator(defaultMaxTurnLen)
	acc.Ontology = lo

	record := makeQRecord("known-source", "s1", "user", "text with L3 tags")
	if err := acc.ConsumeFile(strings.NewReader(record + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	sessions := acc.Sessions()
	if len(sessions) != 1 || len(sessions[0].Turns) != 1 {
		t.Fatalf("expected 1 session with 1 turn, got %d sessions", len(sessions))
	}
	turn := sessions[0].Turns[0]

	if turn.Component != "session.turn" {
		t.Errorf("Component: want session.turn, got %q", turn.Component)
	}
	if turn.OntologyVersion != "cogos.conversations@1.0.0" {
		t.Errorf("OntologyVersion: want cogos.conversations@1.0.0, got %q", turn.OntologyVersion)
	}
	if !strings.Contains(turn.MappingVersion, "test-mapping") {
		t.Errorf("MappingVersion should contain 'test-mapping', got %q", turn.MappingVersion)
	}
}

func TestIngestAccumulator_NoOntology_IndexesNormally(t *testing.T) {
	// Without ontology, all records are indexed with empty L3 tags.
	acc := newIngestAccumulator(defaultMaxTurnLen)
	// acc.Ontology is nil

	record := makeQRecord("any-source", "s1", "user", "normal ingest")
	if err := acc.ConsumeFile(strings.NewReader(record + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	sessions := acc.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	turn := sessions[0].Turns[0]
	if turn.Component != "session.turn" {
		// v0.1 invariant: component defaults to session.turn
		t.Errorf("Component: want session.turn (v0.1 default), got %q", turn.Component)
	}
	if turn.OntologyVersion != "" {
		t.Errorf("OntologyVersion: want empty (no ontology loaded), got %q", turn.OntologyVersion)
	}
}

// makeQRecord is a quarantine-test-local helper so we don't conflict with
// ingest_features_test.go's makeIngestRecord (which has a different signature).
func makeQRecord(source, sessionID, role, text string) string {
	return makeIngestRecord(source, sessionID, role, text, "2026-06-10T00:00:00Z", nil)
}
