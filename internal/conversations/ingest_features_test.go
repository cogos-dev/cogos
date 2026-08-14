// ingest_features_test.go — table-driven tests for the three new features:
//
//  1. Normalized ingest source: schema validation/rejection, per-record
//     session_id grouping (file = transport, not session), sessions spanning
//     files, (source, session_id) keying
//  2. Term-AND search with phrase quoting
//  3. UUID dedup on the CC parser path; stable_id/content-hash dedup on the
//     ingest path
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// makeIngestRecord builds one normalized ingest JSONL line.
func makeIngestRecord(source, sessionID, role, text, ts string, extras map[string]any) string {
	rec := map[string]any{
		"schema":     "cogos.observatory.conversations/v0.1",
		"source":     source,
		"session_id": sessionID,
		"role":       role,
		"timestamp":  ts,
		"text":       text,
	}
	for k, v := range extras {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// parseIngestLines feeds lines through a fresh accumulator and returns it.
func parseIngestLines(t *testing.T, lines []string, maxTurnLen int) *ingestAccumulator {
	t.Helper()
	acc := newIngestAccumulator(maxTurnLen)
	if err := acc.ConsumeFile(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}
	return acc
}

// totalTurns sums turns across all sessions in the accumulator.
func totalTurns(acc *ingestAccumulator) int {
	n := 0
	for _, s := range acc.Sessions() {
		n += len(s.Turns)
	}
	return n
}

// writeIngestDir creates <ingestRoot>/<source>/<filename>.jsonl and returns the path.
func writeIngestDir(t *testing.T, ingestRoot, source, filename string, lines []string) string {
	t.Helper()
	dir := filepath.Join(ingestRoot, source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir ingest dir: %v", err)
	}
	path := filepath.Join(dir, filename+".jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write ingest file: %v", err)
	}
	return path
}

// writeObservatoryConfigFull writes .cog/config/observatory.yaml with both
// source_dirs and ingest_dirs. When srcDirs is nil, an empty temp dir is
// used as source_dirs so the provider does not fall back to the default
// ~/.claude/projects path. Pass explicit paths in srcDirs when needed.
func writeObservatoryConfigFull(t *testing.T, root string, srcDirs, ingestDirs []string) {
	t.Helper()
	cfgDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	// When no source_dirs provided, use an empty temp dir so the provider
	// does not fall back to defaultSourceDirs() and pick up real CC sessions.
	if srcDirs == nil {
		emptyDir := t.TempDir()
		srcDirs = []string{emptyDir}
	}

	var lines []string
	lines = append(lines, "source_dirs:")
	for _, d := range srcDirs {
		lines = append(lines, "  - "+d)
	}

	if len(ingestDirs) > 0 {
		lines = append(lines, "ingest_dirs:")
		for _, d := range ingestDirs {
			lines = append(lines, "  - "+d)
		}
	}
	lines = append(lines, "include_patterns:")
	lines = append(lines, "  - \"*.jsonl\"")

	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(cfgDir, "observatory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write observatory.yaml: %v", err)
	}
}

// reconcileOnce runs one full LoadConfig→FetchLive→ComputePlan→ApplyPlan
// cycle, asserts every applied action succeeded, and returns the plan's
// create count plus the refreshed live state.
func reconcileOnce(t *testing.T, p *Provider, root string) (planSummaryCreates int, ls *liveState) {
	t.Helper()
	ctx := context.Background()
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	liveAny, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	for _, r := range results {
		if r.Status == "failed" {
			t.Fatalf("apply action %s/%s failed: %s", r.Action, r.Name, r.Error)
		}
	}
	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	return plan.Summary.Creates, liveAny2.(*liveState)
}

// ─── Feature 1: Normalized ingest source ─────────────────────────────────────

// TestIngestSchemaValidation verifies that records with unknown schema values
// are rejected and logged, while valid records are accepted.
func TestIngestSchemaValidation(t *testing.T) {
	cases := []struct {
		name         string
		schema       string
		wantRejected int
		wantTurns    int
	}{
		{
			name:         "valid schema accepted",
			schema:       "cogos.observatory.conversations/v0.1",
			wantRejected: 0,
			wantTurns:    1,
		},
		{
			name:         "unknown schema rejected",
			schema:       "cogos.observatory.conversations/v0.0",
			wantRejected: 1,
			wantTurns:    0,
		},
		{
			name:         "empty schema rejected",
			schema:       "",
			wantRejected: 1,
			wantTurns:    0,
		},
		{
			name:         "future schema rejected",
			schema:       "cogos.observatory.conversations/v1.0",
			wantRejected: 1,
			wantTurns:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := map[string]any{
				"schema":     tc.schema,
				"source":     "test-source",
				"session_id": "test-session",
				"role":       "user",
				"timestamp":  "2026-06-01T10:00:00Z",
				"text":       "hello world",
			}
			b, _ := json.Marshal(rec)

			acc := parseIngestLines(t, []string{string(b)}, 8192)
			if acc.RejectedSchemas != tc.wantRejected {
				t.Errorf("rejected=%d want %d", acc.RejectedSchemas, tc.wantRejected)
			}
			if got := totalTurns(acc); got != tc.wantTurns {
				t.Errorf("turns=%d want %d", got, tc.wantTurns)
			}
		})
	}
}

// TestIngestRequiredFields verifies that records missing required fields are
// rejected (logged) without aborting the parse stream.
func TestIngestRequiredFields(t *testing.T) {
	ts := "2026-06-01T10:00:00Z"
	base := map[string]any{
		"schema":     "cogos.observatory.conversations/v0.1",
		"source":     "test-src",
		"session_id": "sess-1",
		"role":       "user",
		"timestamp":  ts,
		"text":       "complete record",
	}

	cases := []struct {
		name      string
		dropField string
		wantTurns int
	}{
		{"all fields present", "", 1},
		{"missing source", "source", 0},
		{"missing session_id", "session_id", 0},
		{"missing role", "role", 0},
		{"missing timestamp", "timestamp", 0},
		{"missing text", "text", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := make(map[string]any)
			for k, v := range base {
				rec[k] = v
			}
			if tc.dropField != "" {
				delete(rec, tc.dropField)
			}
			b, _ := json.Marshal(rec)

			acc := parseIngestLines(t, []string{string(b)}, 8192)
			if got := totalTurns(acc); got != tc.wantTurns {
				t.Errorf("turns=%d want %d (field=%s)", got, tc.wantTurns, tc.dropField)
			}
		})
	}
}

// TestIngestRoleValidation verifies that records with valid roles are accepted
// and records with invalid roles are rejected.
func TestIngestRoleValidation(t *testing.T) {
	cases := []struct {
		role      string
		wantTurns int
	}{
		{"user", 1},
		{"assistant", 1},
		{"system", 1},
		{"tool", 1},
		{"observer", 0},
		{"", 0},
		{"ASSISTANT", 0}, // case-sensitive — schema specifies lowercase
	}
	for _, tc := range cases {
		t.Run("role="+tc.role, func(t *testing.T) {
			rec := map[string]any{
				"schema":     "cogos.observatory.conversations/v0.1",
				"source":     "src",
				"session_id": "s1",
				"role":       tc.role,
				"timestamp":  "2026-06-01T10:00:00Z",
				"text":       "test",
			}
			b, _ := json.Marshal(rec)
			acc := parseIngestLines(t, []string{string(b)}, 8192)
			if got := totalTurns(acc); got != tc.wantTurns {
				t.Errorf("role=%q: turns=%d want %d", tc.role, got, tc.wantTurns)
			}
		})
	}
}

// TestIngestMultiSessionFanOut verifies the core transport semantics: ONE
// ingest file containing records from 3 different session_ids fans out into
// 3 distinct sessions in the index.
func TestIngestMultiSessionFanOut(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	source := "hermes-fan"
	ts := "2026-06-01T10:00:00Z"

	// One transport file, three interleaved sessions — mirrors real observer
	// output where a single run exports the whole state.db.
	lines := []string{
		makeIngestRecord(source, "sess-alpha", "user", "alpha question", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":1"}}),
		makeIngestRecord(source, "sess-beta", "user", "beta question", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":2"}, "session_title": "Beta Session"}),
		makeIngestRecord(source, "sess-alpha", "assistant", "alpha answer", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":3"}}),
		makeIngestRecord(source, "sess-gamma", "user", "gamma question", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":4"}}),
		makeIngestRecord(source, "sess-beta", "assistant", "beta answer", ts,
			map[string]any{"refs": map[string]any{"stable_id": source + ":5"}}),
	}
	writeIngestDir(t, ingestRoot, source, "20260601T100000Z-run1", lines)
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	creates, ls := reconcileOnce(t, p, root)
	if creates != 1 {
		t.Errorf("expected 1 create (per source), got %d", creates)
	}

	wantSessions := map[string]int{ // composite key → expected turn count
		source + "/sess-alpha": 2,
		source + "/sess-beta":  2,
		source + "/sess-gamma": 1,
	}
	if len(ls.Entries) != len(wantSessions) {
		t.Fatalf("expected %d sessions, got %d: %v", len(wantSessions), len(ls.Entries), liveStateKeys(ls))
	}
	for key, wantTurnCount := range wantSessions {
		entry, ok := ls.Entries[key]
		if !ok {
			t.Errorf("missing session %q", key)
			continue
		}
		if entry.Meta.TurnCount != wantTurnCount {
			t.Errorf("session %q: TurnCount=%d want %d", key, entry.Meta.TurnCount, wantTurnCount)
		}
		if entry.Meta.Source != source {
			t.Errorf("session %q: Source=%q want %q", key, entry.Meta.Source, source)
		}
	}

	// session_title propagated per session from any record carrying it.
	if got := ls.Entries[source+"/sess-beta"].Meta.Title; got != "Beta Session" {
		t.Errorf("sess-beta Title=%q want %q", got, "Beta Session")
	}
	if got := ls.Entries[source+"/sess-alpha"].Meta.Title; got != "" {
		t.Errorf("sess-alpha Title=%q want empty", got)
	}
}

// TestIngestSessionSpansFiles verifies that the same session_id spanning two
// observer-run files merges into ONE session, with stable_id dedup absorbing
// the overlapping record.
func TestIngestSessionSpansFiles(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	source := "hermes-span"
	sessionID := "sess-spanning"
	key := source + "/" + sessionID

	stable := func(n int) map[string]any {
		return map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, n)}}
	}

	// Run 1: messages 1-2.
	writeIngestDir(t, ingestRoot, source, "20260601T100000Z-run1", []string{
		makeIngestRecord(source, sessionID, "user", "first message", "2026-06-01T10:00:00Z", stable(1)),
		makeIngestRecord(source, sessionID, "assistant", "second message", "2026-06-01T10:00:01Z", stable(2)),
	})
	// Run 2 (incremental): re-emits message 2 (overlap) plus new message 3.
	writeIngestDir(t, ingestRoot, source, "20260601T110000Z-run2", []string{
		makeIngestRecord(source, sessionID, "assistant", "second message", "2026-06-01T10:00:01Z", stable(2)),
		makeIngestRecord(source, sessionID, "user", "third message", "2026-06-01T10:05:00Z", stable(3)),
	})
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	creates, ls := reconcileOnce(t, p, root)
	if creates != 1 {
		t.Errorf("expected 1 create, got %d", creates)
	}
	if len(ls.Entries) != 1 {
		t.Fatalf("expected exactly 1 session (merged across files), got %d: %v",
			len(ls.Entries), liveStateKeys(ls))
	}
	entry, ok := ls.Entries[key]
	if !ok {
		t.Fatalf("missing merged session %q", key)
	}
	// 3 distinct turns: overlap (stable_id src:2) deduplicated.
	if entry.Meta.TurnCount != 3 {
		t.Errorf("TurnCount=%d want 3 (overlapping record must dedup)", entry.Meta.TurnCount)
	}

	// Verify the turn UUIDs are the raw stable_ids, in order, no duplicates.
	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()
	wantUUIDs := []string{source + ":1", source + ":2", source + ":3"}
	for i, want := range wantUUIDs {
		turn, ok := idx.GetTurn(key, i)
		if !ok {
			t.Fatalf("turn %d not found in %q", i, key)
		}
		if turn.UUID != want {
			t.Errorf("turn[%d].UUID=%q want %q", i, turn.UUID, want)
		}
	}

	// Time bounds span both files.
	wantFirst, _ := time.Parse(time.RFC3339, "2026-06-01T10:00:00Z")
	wantLast, _ := time.Parse(time.RFC3339, "2026-06-01T10:05:00Z")
	if !entry.Meta.FirstTurnAt.Equal(wantFirst) {
		t.Errorf("FirstTurnAt=%v want %v", entry.Meta.FirstTurnAt, wantFirst)
	}
	if !entry.Meta.LastTurnAt.Equal(wantLast) {
		t.Errorf("LastTurnAt=%v want %v", entry.Meta.LastTurnAt, wantLast)
	}
}

// TestIngestIncrementalRunDrift verifies that appending a new observer-run
// file to a source triggers an update on the next plan cycle and the merged
// session grows.
func TestIngestIncrementalRunDrift(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	source := "hermes-incr"
	sessionID := "sess-1"
	key := source + "/" + sessionID
	stable := func(n int) map[string]any {
		return map[string]any{"refs": map[string]any{"stable_id": fmt.Sprintf("%s:%d", source, n)}}
	}

	writeIngestDir(t, ingestRoot, source, "20260601T100000Z-run1", []string{
		makeIngestRecord(source, sessionID, "user", "first", "2026-06-01T10:00:00Z", stable(1)),
	})
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	_, ls := reconcileOnce(t, p, root)
	if ls.Entries[key].Meta.TurnCount != 1 {
		t.Fatalf("after run1: TurnCount=%d want 1", ls.Entries[key].Meta.TurnCount)
	}

	// Second cycle without changes: skip (no drift).
	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan2, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan2.Summary.Creates != 0 || plan2.Summary.Updates != 0 {
		t.Errorf("expected skip with no new files; got creates=%d updates=%d",
			plan2.Summary.Creates, plan2.Summary.Updates)
	}

	// New observer run appends a second file for the same session.
	writeIngestDir(t, ingestRoot, source, "20260601T110000Z-run2", []string{
		makeIngestRecord(source, sessionID, "assistant", "second", "2026-06-01T10:00:01Z", stable(2)),
	})

	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan3, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan3.Summary.Updates != 1 {
		t.Errorf("expected 1 update after new run file, got %d", plan3.Summary.Updates)
	}
	results, err := p.ApplyPlan(ctx, plan3)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 || results[0].Status != "succeeded" {
		t.Fatalf("apply update failed: %v", results)
	}

	liveAny2, _ := p.FetchLive(ctx, cfgAny)
	ls2 := liveAny2.(*liveState)
	if ls2.Entries[key].Meta.TurnCount != 2 {
		t.Errorf("after run2: TurnCount=%d want 2 (merged across files)", ls2.Entries[key].Meta.TurnCount)
	}
}

// TestIngestSourceSessionIDKeying verifies that ingest sessions are keyed as
// "<source>/<session_id>" (from the RECORD's session_id, not the file name)
// and that the Source field is populated in the returned SessionMeta.
func TestIngestSourceSessionIDKeying(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	source := "hermes-test"
	sessionID := "20260601_100000_abc123"
	indexKey := source + "/" + sessionID

	lines := []string{
		makeIngestRecord(source, sessionID, "user", "hello from hermes", "2026-06-01T10:00:00Z", nil),
		makeIngestRecord(source, sessionID, "assistant", "hello back", "2026-06-01T10:00:01Z",
			map[string]any{"session_title": "Hermes Test Session"}),
	}
	// File name deliberately differs from session_id — keying must come from
	// the record, not from the transport file name.
	writeIngestDir(t, ingestRoot, source, "20260601T999999Z-transport-artifact", lines)
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	creates, ls := reconcileOnce(t, p, root)
	if creates != 1 {
		t.Fatalf("expected 1 create, got %d", creates)
	}

	entry, ok := ls.Entries[indexKey]
	if !ok {
		t.Fatalf("expected session at key %q, got keys: %v", indexKey, liveStateKeys(ls))
	}
	if entry.Meta.Source != source {
		t.Errorf("Source=%q want %q", entry.Meta.Source, source)
	}
	if entry.Meta.TurnCount != 2 {
		t.Errorf("TurnCount=%d want 2", entry.Meta.TurnCount)
	}
	if entry.Meta.Title != "Hermes Test Session" {
		t.Errorf("Title=%q want %q", entry.Meta.Title, "Hermes Test Session")
	}

	// Search should return the source in the hit.
	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()
	hits := idx.Search("hermes", time.Time{}, time.Time{}, "", "", 10)
	if len(hits) == 0 {
		t.Fatal("expected search hits, got 0")
	}
	if hits[0].Source != source {
		t.Errorf("hit.Source=%q want %q", hits[0].Source, source)
	}
}

// TestIngestMonotonicTurnIndex verifies that monotonic turn_index is assigned
// per (source, session_id) when the records do not supply turn_index — and
// that the counter is per-session even when sessions interleave.
func TestIngestMonotonicTurnIndex(t *testing.T) {
	lines := []string{
		makeIngestRecord("src", "sess-a", "user", "a-first", "2026-06-01T10:00:00Z", nil),
		makeIngestRecord("src", "sess-b", "user", "b-first", "2026-06-01T10:00:01Z", nil),
		makeIngestRecord("src", "sess-a", "assistant", "a-second", "2026-06-01T10:00:02Z", nil),
		makeIngestRecord("src", "sess-a", "user", "a-third", "2026-06-01T10:00:03Z", nil),
		makeIngestRecord("src", "sess-b", "assistant", "b-second", "2026-06-01T10:00:04Z", nil),
	}
	acc := parseIngestLines(t, lines, 8192)

	sessions := acc.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	byKey := make(map[string]*ingestSessionAccum)
	for _, s := range sessions {
		byKey[s.Meta.SessionID] = s
	}

	a := byKey["src/sess-a"]
	if a == nil || len(a.Turns) != 3 {
		t.Fatalf("sess-a: expected 3 turns, got %v", a)
	}
	for i, turn := range a.Turns {
		if turn.TurnIndex != i {
			t.Errorf("sess-a turn[%d].TurnIndex=%d want %d", i, turn.TurnIndex, i)
		}
	}
	b := byKey["src/sess-b"]
	if b == nil || len(b.Turns) != 2 {
		t.Fatalf("sess-b: expected 2 turns, got %v", b)
	}
	for i, turn := range b.Turns {
		if turn.TurnIndex != i {
			t.Errorf("sess-b turn[%d].TurnIndex=%d want %d", i, turn.TurnIndex, i)
		}
	}
}

// TestIngestMaxTurnLength verifies that long text is truncated at max_turn_length.
func TestIngestMaxTurnLength(t *testing.T) {
	longText := strings.Repeat("x", 200)
	lines := []string{
		makeIngestRecord("src", "sess", "user", longText, "2026-06-01T10:00:00Z", nil),
	}
	acc := parseIngestLines(t, lines, 100)

	sessions := acc.Sessions()
	if len(sessions) != 1 || len(sessions[0].Turns) != 1 {
		t.Fatalf("expected 1 session with 1 turn")
	}
	text := sessions[0].Turns[0].Text
	if len(text) > 115 { // 100 + len(" [truncated]")
		t.Errorf("text not truncated: len=%d", len(text))
	}
	if !strings.HasSuffix(text, " [truncated]") {
		t.Errorf("truncation marker missing: %q", text)
	}
}

// TestIngestTurnUUIDRawStableID verifies the uuid is the RAW stable_id value
// (e.g. "hermes-cog:1") — no internal dedup-key prefix, no double source
// prefix — and the bare content hash when stable_id is absent.
func TestIngestTurnUUIDRawStableID(t *testing.T) {
	lines := []string{
		makeIngestRecord("hermes-cog", "sess", "user", "with stable id", "2026-06-01T10:00:00Z",
			map[string]any{"refs": map[string]any{"stable_id": "hermes-cog:1", "message_id": 1}}),
		makeIngestRecord("hermes-cog", "sess", "user", "without stable id", "2026-06-01T10:00:01Z", nil),
	}
	acc := parseIngestLines(t, lines, 8192)

	sessions := acc.Sessions()
	if len(sessions) != 1 || len(sessions[0].Turns) != 2 {
		t.Fatalf("expected 1 session with 2 turns")
	}
	turns := sessions[0].Turns

	if turns[0].UUID != "hermes-cog:1" {
		t.Errorf("uuid=%q want raw stable_id %q", turns[0].UUID, "hermes-cog:1")
	}
	if strings.Contains(turns[0].UUID, "stable:") {
		t.Errorf("internal dedup prefix leaked into uuid: %q", turns[0].UUID)
	}

	// No stable_id → 16-hex-char content hash, no namespace prefix.
	if len(turns[1].UUID) != 16 {
		t.Errorf("hash uuid length=%d want 16: %q", len(turns[1].UUID), turns[1].UUID)
	}
	if strings.Contains(turns[1].UUID, ":") {
		t.Errorf("hash uuid contains prefix separator: %q", turns[1].UUID)
	}
}

// ─── Feature 2: Term-AND search ───────────────────────────────────────────────

// TestSearchTermAND verifies that multi-term queries require ALL terms.
func TestSearchTermAND(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantMatch bool
	}{
		{"single term match", "harness", true},
		{"single term no match", "kubernetes", false},
		{"two terms both present", "harness attestation", true},
		{"two terms first missing", "kubernetes attestation", false},
		{"two terms second missing", "harness kubernetes", false},
		{"two terms both missing", "kubernetes flannel", false},
		{"three terms all present", "harness attestation policy", true},
		{"three terms one missing", "harness attestation flannel", false},
		{"phrase match exact", `"attestation policy"`, true},
		{"phrase match partial word", `"attest"`, true},
		{"phrase match not present", `"kubernetes attestation"`, false},
		{"phrase plus term match", `"attestation policy" harness`, true},
		{"phrase plus term missing term", `"attestation policy" kubernetes`, false},
	}

	// Seed text that contains: "harness attestation policy"
	seedText := "The harness attestation policy defines the trust model."

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			terms := parseSearchQuery(tc.query)
			got := matchesAllTerms(seedText, terms)
			if got != tc.wantMatch {
				t.Errorf("query=%q matchesAllTerms=%v want %v", tc.query, got, tc.wantMatch)
			}
		})
	}
}

// TestSearchTermANDIntegration verifies term-AND search end-to-end through the
// index layer, including that single-term behavior is backward-compatible.
func TestSearchTermANDIntegration(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const sid = "aaaabbbb-cccc-dddd-eeee-ffffaaaabbbb"

	lines := []string{
		makeUserRecord("u1", "", sid,
			"what is the harness attestation policy?", "2026-06-01T10:00:00Z"),
		makeAssistantRecord("a1", "u1", sid,
			"The attestation model is defined in ADR-073.", "2026-06-01T10:01:00Z"),
		makeUserRecord("u2", "a1", sid,
			"tell me about operator identity", "2026-06-01T10:02:00Z"),
	}
	writeJSONLFixture(t, srcDir, sid, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()

	tests := []struct {
		query     string
		wantCount int
	}{
		{"harness", 1},                         // single term
		{"harness attestation", 1},             // AND: both present in turn u1
		{"attestation", 2},                     // present in u1 + a1
		{"attestation model", 1},               // AND: "attestation model" only in a1
		{"harness operator", 0},                // AND: no single turn has both
		{`"harness attestation"`, 1},           // exact phrase match
		{`"attestation policy"`, 1},            // phrase
		{`"attestation policy" harness`, 1},    // phrase + term
		{`"attestation policy" kubernetes`, 0}, // phrase + missing term
	}
	for _, tc := range tests {
		hits := idx.Search(tc.query, time.Time{}, time.Time{}, "", "", 0)
		if len(hits) != tc.wantCount {
			t.Errorf("query=%q: got %d hits want %d", tc.query, len(hits), tc.wantCount)
		}
	}
}

// TestParseSearchQuery verifies the query tokenizer independently.
func TestParseSearchQuery(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"hello", []string{"hello"}},
		{"foo bar", []string{"foo", "bar"}},
		{"foo  bar", []string{"foo", "bar"}}, // extra whitespace
		{`"foo bar"`, []string{"foo bar"}},
		{`"foo bar" baz`, []string{"foo bar", "baz"}},
		{`baz "foo bar"`, []string{"baz", "foo bar"}},
		{`"a" "b c"`, []string{"a", "b c"}},
		{`"unclosed`, []string{"unclosed"}}, // unclosed quote treated as literal
	}
	for _, tc := range cases {
		got := parseSearchQuery(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("parseSearchQuery(%q) = %v want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseSearchQuery(%q)[%d] = %q want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

// ─── Feature 3: dedup ────────────────────────────────────────────────────────

// TestCCParserUUIDDedup verifies that when a CC JSONL contains duplicate uuid
// values (resumed/compacted sessions), only the first occurrence is indexed.
func TestCCParserUUIDDedup(t *testing.T) {
	sid := "dedup-session-uuid-test"
	ts := "2026-06-01T10:00:00Z"

	// Build a JSONL where "uuid-u1" appears twice — simulating a compacted
	// append that re-inserts historical turns.
	line1 := makeUserRecord("uuid-u1", "", sid, "original message", ts)
	line2 := makeUserRecord("uuid-u1", "", sid, "duplicate of original", ts) // same uuid
	line3 := makeUserRecord("uuid-u2", "uuid-u1", sid, "new message", ts)

	jsonl := strings.Join([]string{line1, line2, line3}, "\n") + "\n"

	var meta SessionMeta
	var turns []Turn
	_, err := ParseSession(strings.NewReader(jsonl), sid, 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if err != nil {
		t.Fatalf("ParseSession: %v", err)
	}

	// Expect 2 turns (line2 is a duplicate of line1 by uuid and should be skipped).
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns after uuid dedup, got %d", len(turns))
	}
	// The first turn should be the original, not the duplicate.
	if turns[0].Text != "original message" {
		t.Errorf("first turn text=%q want %q", turns[0].Text, "original message")
	}
	// Turn indices should be 0, 1 (monotonic after dedup).
	if turns[0].TurnIndex != 0 || turns[1].TurnIndex != 1 {
		t.Errorf("turn indices after dedup: %d, %d; want 0, 1",
			turns[0].TurnIndex, turns[1].TurnIndex)
	}
}

// TestCCParserUUIDDedup_EmptyUUID verifies that records with empty uuid are
// not cross-deduplicated against each other (they are distinct turns that
// happen to lack a uuid).
func TestCCParserUUIDDedup_EmptyUUID(t *testing.T) {
	sid := "empty-uuid-session"
	ts := "2026-06-01T10:00:00Z"

	// Two records with no uuid — both should be indexed.
	makeRecNoUUID := func(text string) string {
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		msg, _ := json.Marshal(map[string]any{
			"role":    "user",
			"content": json.RawMessage(content),
		})
		rec := map[string]any{
			"type":      "user",
			"sessionId": sid,
			"timestamp": ts,
			"message":   json.RawMessage(msg),
		}
		b, _ := json.Marshal(rec)
		return string(b)
	}

	line1 := makeRecNoUUID("turn one")
	line2 := makeRecNoUUID("turn two")
	jsonl := line1 + "\n" + line2 + "\n"

	var meta SessionMeta
	var turns []Turn
	_, _ = ParseSession(strings.NewReader(jsonl), sid, 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})

	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (no uuid should not collapse), got %d", len(turns))
	}
}

// TestIngestDedupByStableID verifies that ingest records with the same
// refs.stable_id are deduplicated, retaining only the first occurrence.
func TestIngestDedupByStableID(t *testing.T) {
	makeRecWithStableID := func(sid, text, stableID string) string {
		refs := map[string]any{"stable_id": stableID}
		return makeIngestRecord("src", sid, "user", text, "2026-06-01T10:00:00Z",
			map[string]any{"refs": refs})
	}

	lines := []string{
		makeRecWithStableID("sess", "first occurrence", "src:1"),
		makeRecWithStableID("sess", "duplicate of src:1", "src:1"),
		makeRecWithStableID("sess", "different record", "src:2"),
	}
	acc := parseIngestLines(t, lines, 8192)

	sessions := acc.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	turns := sessions[0].Turns
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns after stable_id dedup, got %d", len(turns))
	}
	if turns[0].Text != "first occurrence" {
		t.Errorf("first turn text=%q want %q", turns[0].Text, "first occurrence")
	}
}

// TestIngestDedupByContentHash verifies that ingest records without a
// refs.stable_id are deduplicated by content hash (role+timestamp+text).
func TestIngestDedupByContentHash(t *testing.T) {
	lines := []string{
		makeIngestRecord("src", "sess", "user", "hello world", "2026-06-01T10:00:00Z", nil),
		makeIngestRecord("src", "sess", "user", "hello world", "2026-06-01T10:00:00Z", nil), // exact dup
		makeIngestRecord("src", "sess", "user", "different text", "2026-06-01T10:00:00Z", nil),
	}
	acc := parseIngestLines(t, lines, 8192)

	sessions := acc.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if len(sessions[0].Turns) != 2 {
		t.Fatalf("expected 2 turns after content-hash dedup, got %d", len(sessions[0].Turns))
	}
}

// TestIngestSessionCollisionIsolation verifies that ingest sessions with the
// same session_id but different sources are kept separate in the index.
func TestIngestSessionCollisionIsolation(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()

	const rawSessID = "my-session"
	ts := "2026-06-01T10:00:00Z"

	// Two sources with the same session_id — must not collide.
	writeIngestDir(t, ingestRoot, "source-a", "run1", []string{
		makeIngestRecord("source-a", rawSessID, "user", "message from source-a", ts, nil),
	})
	writeIngestDir(t, ingestRoot, "source-b", "run1", []string{
		makeIngestRecord("source-b", rawSessID, "user", "message from source-b", ts, nil),
	})
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	creates, ls := reconcileOnce(t, p, root)
	if creates != 2 {
		t.Fatalf("expected 2 creates (one per source), got %d", creates)
	}

	keyA := fmt.Sprintf("source-a/%s", rawSessID)
	keyB := fmt.Sprintf("source-b/%s", rawSessID)

	if _, ok := ls.Entries[keyA]; !ok {
		t.Errorf("missing index key %q; have: %v", keyA, liveStateKeys(ls))
	}
	if _, ok := ls.Entries[keyB]; !ok {
		t.Errorf("missing index key %q; have: %v", keyB, liveStateKeys(ls))
	}
}

// ─── helper ──────────────────────────────────────────────────────────────────

func liveStateKeys(ls *liveState) []string {
	keys := make([]string, 0, len(ls.Entries))
	for k := range ls.Entries {
		keys = append(keys, k)
	}
	return keys
}
