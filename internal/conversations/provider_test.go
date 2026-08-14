// provider_test.go — integration tests for the Conversations Observatory provider.
//
// Covers:
//   1. Ingesting a 1-session JSONL fixture; assert turn count, identity, time bounds
//   2. Streaming a large fixture (512KB equivalent) without OOM
//   3. Drift detection: session modified after index, re-projection runs
//   4. MCP tool query path: search for a known string, assert hit
//   5. Idempotency: re-run ApplyPlan, no double-projection
//   6. LoadConfig with empty config (default source dirs)
//   7. FetchLive round-trip (write then read)
//   8. BuildState structure
//   9. Health() status transitions
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

// ─── JSONL fixture helpers ────────────────────────────────────────────────────

// sessionUUID is a fixed UUID for test sessions.
const sessionUUID = "11111111-2222-3333-4444-555555555555"

// makeUserRecord returns a JSON line representing a user message.
func makeUserRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       uuid,
		"parentUuid": parentUUID,
		"sessionId":  sessionID,
		"timestamp":  ts,
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeAssistantRecord returns a JSON line representing an assistant message.
func makeAssistantRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"model":   "claude-sonnet-4-6",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "assistant",
		"uuid":       uuid,
		"parentUuid": parentUUID,
		"sessionId":  sessionID,
		"timestamp":  ts,
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeAITitleRecord returns a JSON line for an ai-title record.
func makeAITitleRecord(sessionID, title string) string {
	rec := map[string]any{
		"type":      "ai-title",
		"sessionId": sessionID,
		"title":     title,
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// writeJSONLFixture writes a JSONL fixture file to dir/<sessionID>.jsonl.
func writeJSONLFixture(t *testing.T, dir, sessionID string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// ─── Test helpers ─────────────────────────────────────────────────────────────

// newTestProvider creates a Provider backed by a temp workspace.
func newTestProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	root := t.TempDir()
	p := NewProvider()
	return p, root
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestProviderIngest_SingleSession asserts that a single-session JSONL is
// parsed correctly: turn count, identity attribution, and time bounds.
func TestProviderIngest_SingleSession(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	ts1 := "2026-05-01T10:00:00Z"
	ts2 := "2026-05-01T10:01:00Z"
	ts3 := "2026-05-01T10:02:00Z"

	lines := []string{
		makeAITitleRecord(sessionUUID, "Test Session Title"),
		makeUserRecord("uuid-u1", "", sessionUUID, "what is the harness attestation policy?", ts1),
		makeAssistantRecord("uuid-a1", "uuid-u1", sessionUUID, "The harness attestation policy is...", ts2),
		makeUserRecord("uuid-u2", "uuid-a1", sessionUUID, "tell me more about operator identity", ts3),
	}
	writeJSONLFixture(t, srcDir, sessionUUID, lines)

	// Write a custom observatory.yaml pointing at our temp source dir.
	writeObservatoryConfig(t, root, []string{srcDir})

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

	if plan.Summary.Creates != 1 {
		t.Errorf("expected 1 create, got %d", plan.Summary.Creates)
	}

	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "succeeded" {
		t.Errorf("expected succeeded, got %s: %s", results[0].Status, results[0].Error)
	}

	// Re-fetch live to confirm the session was indexed.
	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	ls, ok := liveAny2.(*liveState)
	if !ok {
		t.Fatal("expected *liveState")
	}
	entry, ok := ls.Entries[sessionUUID]
	if !ok {
		t.Fatalf("session %s not in live state", sessionUUID)
	}

	// 2 user turns + 1 assistant turn = 3 total turns indexed.
	if entry.Meta.TurnCount != 3 {
		t.Errorf("expected 3 turns, got %d", entry.Meta.TurnCount)
	}
	if entry.Meta.Title != "Test Session Title" {
		t.Errorf("expected title 'Test Session Title', got %q", entry.Meta.Title)
	}
	if entry.Meta.Entrypoint != "cli" {
		t.Errorf("expected entrypoint 'cli', got %q", entry.Meta.Entrypoint)
	}

	// Check time bounds.
	wantFirst, _ := time.Parse(time.RFC3339, ts1)
	wantLast, _ := time.Parse(time.RFC3339, ts3)
	if !entry.Meta.FirstTurnAt.Equal(wantFirst) {
		t.Errorf("FirstTurnAt: want %v, got %v", wantFirst, entry.Meta.FirstTurnAt)
	}
	if !entry.Meta.LastTurnAt.Equal(wantLast) {
		t.Errorf("LastTurnAt: want %v, got %v", wantLast, entry.Meta.LastTurnAt)
	}
}

// TestProviderIngest_LargeSession verifies that indexing a large synthetic
// session (many turns totalling ~1MB) completes without OOM and indexes
// a sensible turn count.
func TestProviderIngest_LargeSession(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	const largeSID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const nTurns = 2000
	lines := make([]string, 0, nTurns+1)
	lines = append(lines, makeAITitleRecord(largeSID, "Large Session"))

	for i := 0; i < nTurns; i++ {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		text := strings.Repeat("a", 400) // 400 bytes per turn × 2000 = ~800KB of content
		if i%2 == 0 {
			lines = append(lines, makeUserRecord(
				fmt.Sprintf("uuid-u%d", i), "", largeSID, text, ts,
			))
		} else {
			lines = append(lines, makeAssistantRecord(
				fmt.Sprintf("uuid-a%d", i), fmt.Sprintf("uuid-u%d", i-1), largeSID, text, ts,
			))
		}
	}
	writeJSONLFixture(t, srcDir, largeSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) == 0 || results[0].Status != "succeeded" {
		t.Fatalf("expected success, got: %v", results)
	}

	liveAny2, _ := p.FetchLive(ctx, cfgAny)
	ls := liveAny2.(*liveState)
	entry := ls.Entries[largeSID]

	if entry.Meta.TurnCount != nTurns {
		t.Errorf("expected %d turns, got %d", nTurns, entry.Meta.TurnCount)
	}
}

// TestProviderDriftDetection verifies that a modified session (mtime/size
// change) triggers an update action on the next plan cycle.
func TestProviderDriftDetection(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const driftSID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	// Initial index.
	lines := []string{
		makeUserRecord("u1", "", driftSID, "first message", "2026-05-01T10:00:00Z"),
	}
	fixturePath := writeJSONLFixture(t, srcDir, driftSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	// Confirm first cycle: skip (just indexed).
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan2, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan2.Summary.Updates != 0 || plan2.Summary.Creates != 0 {
		t.Error("expected skip after initial index, got changes")
	}

	// Modify the file (add a new line to change size + mtime).
	additional := "\n" + makeUserRecord("u2", "u1", driftSID, "second message", "2026-05-01T10:01:00Z")
	f, err := os.OpenFile(fixturePath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open fixture for append: %v", err)
	}
	_, err = f.WriteString(additional)
	f.Close()
	if err != nil {
		t.Fatalf("append to fixture: %v", err)
	}

	// Ensure mtime is different (filesystem resolution can be 1s).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(fixturePath, future, future); err != nil {
		t.Logf("chtimes: %v (non-fatal)", err)
	}

	// Re-plan: should detect update.
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan3, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan 3: %v", err)
	}
	if plan3.Summary.Updates != 1 {
		t.Errorf("expected 1 update after file modification, got %d", plan3.Summary.Updates)
	}

	// Apply the update and verify new turn count.
	results, _ := p.ApplyPlan(ctx, plan3)
	if len(results) == 0 || results[0].Status != "succeeded" {
		t.Fatalf("apply update failed: %v", results)
	}
	liveAny4, _ := p.FetchLive(ctx, cfgAny)
	ls := liveAny4.(*liveState)
	if ls.Entries[driftSID].Meta.TurnCount != 2 {
		t.Errorf("expected 2 turns after update, got %d", ls.Entries[driftSID].Meta.TurnCount)
	}
}

// TestIndexSessionIncremental_ReadsOnlyAppendedTail proves the issue #558
// fix: given a watermark from a prior full parse, a second parse pass reads
// only the bytes appended since — it never re-derives the already-indexed
// prefix from disk.
//
// The proof: after the first full parse, the on-disk prefix (the bytes
// already consumed, [0, SourceOffset)) is overwritten in place with a
// same-length record carrying a DIFFERENT uuid and different text before
// the tail is appended. The merged result always starts with the
// caller-supplied prevTurns (so turns2[0] alone can't prove anything — a
// buggy from-byte-0 re-read would still be prefixed with prevTurns by the
// merge step), so the real tell is length and position 1: a from-byte-0 re-
// read would encounter the corrupted record under its new uuid — distinct
// from anything in prevTurns' dedup set — and append it as a 3rd, bogus
// turn ahead of the genuinely-new tail turn. A correct tail-only read never
// sees those corrupted bytes at all: exactly 2 turns come back, and the one
// tail turn is the genuinely appended one.
func TestIndexSessionIncremental_ReadsOnlyAppendedTail(t *testing.T) {
	dir := t.TempDir()
	const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	firstText := "first message"
	firstLine := makeUserRecord("u1", "", sid, firstText, "2026-05-01T10:00:00Z")
	path := writeJSONLFixture(t, dir, sid, []string{firstLine})

	// Cycle 1: full parse establishes the watermark.
	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (cycle 1): %v", err)
	}
	if len(turns1) != 1 {
		t.Fatalf("expected 1 turn after cycle 1, got %d", len(turns1))
	}
	if meta1.SourceOffset != meta1.SourceSize {
		t.Fatalf("expected SourceOffset == SourceSize after a full parse, got offset=%d size=%d",
			meta1.SourceOffset, meta1.SourceSize)
	}
	if meta1.SourceOffset == 0 {
		t.Fatalf("expected a non-zero watermark after cycle 1")
	}

	// Corrupt the already-consumed prefix in place — same byte length (so
	// the watermark byte offset still lands exactly on the line boundary),
	// different uuid AND different text. Different uuid matters: it is what
	// keeps this corrupted record out of the dedup-by-uuid set seeded from
	// prevTurns, so a wrongly-reread copy of it cannot be silently absorbed
	// as a harmless "duplicate" — it would show up as a distinct 3rd turn.
	corruptedText := strings.Repeat("Z", len(firstText))
	corruptedLine := strings.Replace(firstLine, `"`+firstText+`"`, `"`+corruptedText+`"`, 1)
	corruptedLine = strings.Replace(corruptedLine, `"u1"`, `"z1"`, 1)
	if len(corruptedLine) != len(firstLine) {
		t.Fatalf("test setup: corrupted line length %d != original %d", len(corruptedLine), len(firstLine))
	}
	wf, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for prefix overwrite: %v", err)
	}
	if _, err := wf.WriteAt([]byte(corruptedLine), 0); err != nil {
		wf.Close()
		t.Fatalf("overwrite prefix: %v", err)
	}
	wf.Close()

	// Append a genuinely new turn after the watermark.
	secondText := "second message"
	secondLine := makeUserRecord("u2", "u1", sid, secondText, "2026-05-01T10:01:00Z")
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := af.WriteString("\n" + secondLine); err != nil {
		af.Close()
		t.Fatalf("append tail: %v", err)
	}
	af.Close()

	// Cycle 2: incremental parse from the watermark, seeded with cycle 1's
	// in-memory turns (as ApplyPlan would supply via idx.SessionTurns).
	meta2, turns2, used, err := indexSessionIncremental(path, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used")
	}
	// The length check is the load-bearing proof: a from-byte-0 re-read
	// would pick up the corrupted record (different uuid, so not deduped)
	// as a spurious 3rd turn. Exactly 2 back means the corrupted prefix
	// bytes were never read.
	if len(turns2) != 2 {
		t.Fatalf("expected exactly 2 turns after cycle 2 (got %d) — a from-byte-0 re-read would have "+
			"picked up the corrupted prefix record (text %q) as a spurious extra turn, meaning the "+
			"parse read past the watermark into already-indexed, now-corrupted bytes instead of "+
			"resuming the tail from it", len(turns2), corruptedText)
	}
	if turns2[0].Text != firstText {
		t.Errorf("turns2[0].Text = %q, want %q (the caller-supplied prevTurns prefix)", turns2[0].Text, firstText)
	}
	if turns2[1].Text != secondText {
		t.Errorf("turns2[1].Text = %q, want %q — the sole new tail turn should be the genuinely "+
			"appended record, not the corrupted prefix record", turns2[1].Text, secondText)
	}
	if turns2[0].TurnIndex != 0 || turns2[1].TurnIndex != 1 {
		t.Errorf("expected contiguous turn indices 0,1; got %d,%d", turns2[0].TurnIndex, turns2[1].TurnIndex)
	}
	if meta2.TurnCount != 2 {
		t.Errorf("expected TurnCount 2, got %d", meta2.TurnCount)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta2.SourceOffset != fi.Size() {
		t.Errorf("expected watermark to advance to the new EOF (%d), got %d", fi.Size(), meta2.SourceOffset)
	}
}

// TestIndexSessionIncremental_FallsBackOnTruncation verifies that when the
// source file is smaller than the recorded watermark (a compaction rewrite,
// or any other truncation), the incremental path declines (usedIncremental
// == false) rather than seeking past EOF or trusting stale offsets, leaving
// the caller to fall back to a full re-parse.
func TestIndexSessionIncremental_FallsBackOnTruncation(t *testing.T) {
	dir := t.TempDir()
	const sid = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"

	lines := []string{
		makeUserRecord("u1", "", sid, "first message", "2026-05-01T10:00:00Z"),
		makeUserRecord("u2", "u1", sid, "second message", "2026-05-01T10:01:00Z"),
	}
	path := writeJSONLFixture(t, dir, sid, lines)

	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	if meta1.SourceOffset == 0 {
		t.Fatalf("expected a non-zero watermark")
	}

	// Replace with a smaller file (e.g. a compaction rewrite).
	replacement := makeUserRecord("u3", "", sid, "replaced", "2026-05-01T10:02:00Z") + "\n"
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatalf("truncate/replace: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Size() >= meta1.SourceOffset {
		t.Fatalf("test setup: replacement (%d bytes) must be smaller than the watermark (%d bytes)",
			fi.Size(), meta1.SourceOffset)
	}

	_, _, used, err := indexSessionIncremental(path, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if used {
		t.Errorf("expected the incremental path to decline when the file shrank below the watermark")
	}
}

// TestProviderApplyPlan_WatermarkAdvancesAcrossCycles is the end-to-end
// counterpart to TestIndexSessionIncremental_ReadsOnlyAppendedTail: it drives
// the same watermark logic through LoadConfig/FetchLive/ComputePlan/ApplyPlan
// across three append cycles on a live-growing session, and asserts that
// each cycle's applied SourceOffset lands exactly on the file's new EOF and
// turn counts accumulate correctly — i.e. the provider, not just the parsing
// helper in isolation, drives a second cycle off the appended tail.
func TestProviderApplyPlan_WatermarkAdvancesAcrossCycles(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const sid = "cccccccc-dddd-eeee-ffff-000000000000"

	fixturePath := writeJSONLFixture(t, srcDir, sid, []string{
		makeUserRecord("u1", "", sid, "turn one", "2026-05-01T10:00:00Z"),
	})
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	apply := func() {
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
		if _, err := p.ApplyPlan(ctx, plan); err != nil {
			t.Fatalf("ApplyPlan: %v", err)
		}
	}
	appendLine := func(line string) {
		f, err := os.OpenFile(fixturePath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open for append: %v", err)
		}
		if _, err := f.WriteString("\n" + line); err != nil {
			f.Close()
			t.Fatalf("append: %v", err)
		}
		f.Close()
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(fixturePath, future, future); err != nil {
			t.Logf("chtimes: %v (non-fatal)", err)
		}
	}

	// Cycle 1: initial create.
	apply()
	meta, ok := p.index.GetMeta(sid)
	if !ok {
		t.Fatalf("session not indexed after cycle 1")
	}
	if meta.TurnCount != 1 {
		t.Fatalf("expected 1 turn after cycle 1, got %d", meta.TurnCount)
	}
	fi, _ := os.Stat(fixturePath)
	if meta.SourceOffset != fi.Size() {
		t.Fatalf("expected watermark == EOF (%d) after cycle 1, got %d", fi.Size(), meta.SourceOffset)
	}

	// Cycle 2: append and re-apply.
	appendLine(makeUserRecord("u2", "u1", sid, "turn two", "2026-05-01T10:01:00Z"))
	apply()
	meta, ok = p.index.GetMeta(sid)
	if !ok {
		t.Fatalf("session missing after cycle 2")
	}
	if meta.TurnCount != 2 {
		t.Fatalf("expected 2 turns after cycle 2, got %d", meta.TurnCount)
	}
	fi, _ = os.Stat(fixturePath)
	if meta.SourceOffset != fi.Size() {
		t.Fatalf("expected watermark to advance to EOF (%d) after cycle 2, got %d", fi.Size(), meta.SourceOffset)
	}

	// Cycle 3: append again and re-apply — the watermark from cycle 2 must
	// keep advancing, not reset.
	appendLine(makeUserRecord("u3", "u2", sid, "turn three", "2026-05-01T10:02:00Z"))
	apply()
	meta, ok = p.index.GetMeta(sid)
	if !ok {
		t.Fatalf("session missing after cycle 3")
	}
	if meta.TurnCount != 3 {
		t.Fatalf("expected 3 turns after cycle 3, got %d", meta.TurnCount)
	}
	fi, _ = os.Stat(fixturePath)
	if meta.SourceOffset != fi.Size() {
		t.Fatalf("expected watermark to advance to EOF (%d) after cycle 3, got %d", fi.Size(), meta.SourceOffset)
	}

	turns := p.index.SessionTurns(sid)
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns in the index, got %d", len(turns))
	}
	wantTexts := []string{"turn one", "turn two", "turn three"}
	for i, want := range wantTexts {
		if turns[i].Text != want {
			t.Errorf("turn[%d].Text = %q, want %q", i, turns[i].Text, want)
		}
		if turns[i].TurnIndex != i {
			t.Errorf("turn[%d].TurnIndex = %d, want %d", i, turns[i].TurnIndex, i)
		}
	}
}

// TestProviderIdempotency verifies that running ApplyPlan twice on the same
// session produces a skip on the second pass (no double-indexing).
func TestProviderIdempotency(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const idemSID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

	lines := []string{
		makeUserRecord("u1", "", idemSID, "idempotency test message", "2026-05-01T10:00:00Z"),
	}
	writeJSONLFixture(t, srcDir, idemSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()

	// First pass: create.
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan1, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan1)

	// Second pass: should skip.
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan2, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan 2: %v", err)
	}
	if plan2.Summary.Creates != 0 || plan2.Summary.Updates != 0 {
		t.Errorf("expected skip on second pass; got creates=%d updates=%d",
			plan2.Summary.Creates, plan2.Summary.Updates)
	}
	if plan2.Summary.Skipped != 1 {
		t.Errorf("expected 1 skip, got %d", plan2.Summary.Skipped)
	}
}

// TestMCPSearchConversations verifies the MCP tool query path: index a session
// with a known string, then search for it and assert a hit.
func TestMCPSearchConversations(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const searchSID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const needle = "harness attestation policy"

	lines := []string{
		makeUserRecord("u1", "", searchSID,
			"what is the "+needle+"?", "2026-05-10T12:00:00Z"),
		makeAssistantRecord("a1", "u1", searchSID,
			"The attestation model is defined in ADR-073.", "2026-05-10T12:01:00Z"),
	}
	writeJSONLFixture(t, srcDir, searchSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	// Direct index search (bypasses MCP layer, tests the query logic).
	p.mu.Lock()
	idx := p.index
	p.mu.Unlock()

	if idx == nil {
		t.Fatal("index is nil after ApplyPlan")
	}

	hits := idx.Search(needle, time.Time{}, time.Time{}, "", "", 10)
	if len(hits) == 0 {
		t.Fatalf("expected at least 1 hit for %q, got 0", needle)
	}
	if hits[0].SessionID != searchSID {
		t.Errorf("expected session %s, got %s", searchSID, hits[0].SessionID)
	}
	if hits[0].Role != RoleUser {
		t.Errorf("expected user role, got %s", hits[0].Role)
	}
	if !strings.Contains(strings.ToLower(hits[0].Excerpt), strings.ToLower(needle)) {
		t.Errorf("excerpt %q does not contain needle %q", hits[0].Excerpt, needle)
	}
}

// TestBuildState verifies that BuildState produces a well-formed reconcile.State.
func TestBuildState(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const bsSID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	lines := []string{
		makeUserRecord("u1", "", bsSID, "build state test", "2026-05-01T10:00:00Z"),
	}
	writeJSONLFixture(t, srcDir, bsSID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan, _ := p.ComputePlan(cfgAny, liveAny, nil)
	_, _ = p.ApplyPlan(ctx, plan)

	liveAny2, _ := p.FetchLive(ctx, cfgAny)
	state, err := p.BuildState(cfgAny, liveAny2, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != "conversations" {
		t.Errorf("expected resource_type 'conversations', got %q", state.ResourceType)
	}
	if len(state.Resources) != 1 {
		t.Errorf("expected 1 resource, got %d", len(state.Resources))
	}
	r := state.Resources[0]
	if r.ExternalID != bsSID {
		t.Errorf("expected external_id %s, got %s", bsSID, r.ExternalID)
	}
}

// TestHealthTransitions verifies Health() returns correct statuses.
func TestHealthTransitions(t *testing.T) {
	p := NewProvider()

	// Before any plan: no changes → Synced.
	h := p.Health()
	if h.Sync != "Synced" {
		t.Errorf("initial sync: want Synced, got %s", h.Sync)
	}
	if h.Health != "Healthy" {
		t.Errorf("initial health: want Healthy, got %s", h.Health)
	}

	// Simulate a plan with creates.
	p.mu.Lock()
	p.lastPlanSummary.Creates = 3
	p.mu.Unlock()

	h2 := p.Health()
	if h2.Sync != "OutOfSync" {
		t.Errorf("after creates: want OutOfSync, got %s", h2.Sync)
	}

	// Simulate errors.
	p.mu.Lock()
	p.lastErrors = []string{"session x failed"}
	p.mu.Unlock()

	h3 := p.Health()
	if h3.Health != "Degraded" {
		t.Errorf("after errors: want Degraded, got %s", h3.Health)
	}
}

// TestParserSystemReminderStripped verifies that <system-reminder> blocks
// are stripped from user turns and do not produce empty turns.
func TestParserSystemReminderStripped(t *testing.T) {
	sysReminder := "<system-reminder>This is a system note.</system-reminder>"
	realText := "what should I do next?"

	// A user record where content is a text block with a system-reminder prefix.
	content, _ := json.Marshal([]map[string]string{
		{"type": "text", "text": sysReminder + "\n" + realText},
	})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       "test-uuid",
		"parentUuid": "",
		"sessionId":  "test-session",
		"timestamp":  "2026-05-01T10:00:00Z",
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	jsonl := string(b) + "\n"

	var meta SessionMeta
	var turns []Turn
	err := ParseSession(strings.NewReader(jsonl), "test-session", 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if err != nil {
		t.Fatalf("ParseSession: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if strings.Contains(turns[0].Text, "system-reminder") {
		t.Errorf("system-reminder not stripped: %q", turns[0].Text)
	}
	if !strings.Contains(turns[0].Text, realText) {
		t.Errorf("real text missing from turn: %q", turns[0].Text)
	}
}

// TestParserSkipsToolResults verifies that tool_result content blocks in
// user records are skipped (they are tool outputs, not operator utterances).
func TestParserSkipsToolResults(t *testing.T) {
	content, _ := json.Marshal([]map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "tool output"},
		{"type": "text", "text": "here is the real question"},
	})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":       "user",
		"uuid":       "test-uuid-2",
		"parentUuid": "",
		"sessionId":  "test-session-2",
		"timestamp":  "2026-05-01T10:00:00Z",
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	jsonl := string(b) + "\n"

	var meta SessionMeta
	var turns []Turn
	_ = ParseSession(strings.NewReader(jsonl), "test-session-2", 8192, &meta, func(t Turn) bool {
		turns = append(turns, t)
		return true
	})
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	if strings.Contains(turns[0].Text, "tool output") {
		t.Error("tool_result content leaked into turn text")
	}
	if !strings.Contains(turns[0].Text, "real question") {
		t.Errorf("real text missing: %q", turns[0].Text)
	}
}

// ─── Fixture helpers ─────────────────────────────────────────────────────────

// writeObservatoryConfig writes a minimal .cog/config/observatory.yaml that
// points at srcDirs.
func writeObservatoryConfig(t *testing.T, root string, srcDirs []string) {
	t.Helper()
	cfgDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	var lines []string
	lines = append(lines, "source_dirs:")
	for _, d := range srcDirs {
		lines = append(lines, "  - "+d)
	}
	lines = append(lines, "include_patterns:")
	lines = append(lines, "  - \"*.jsonl\"")

	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(cfgDir, "observatory.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write observatory.yaml: %v", err)
	}
}
