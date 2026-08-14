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

	// The corrupted-sentinel technique below only proves "the parse seeks
	// to the watermark instead of re-scanning from byte 0" if the corrupted
	// bytes sit OUTSIDE tailFingerprintWindow — otherwise the
	// SourceTailHash check (added to close the rewrite-then-grow gap, see
	// TestIndexSessionIncremental_FallsBackOnRewriteThenGrow) would
	// correctly decline the fast path before the seek is ever reached, and
	// this test would stop exercising the seek-offset regression it's named
	// for. A filler turn larger than the fingerprint window pushes the
	// sentinel line's bytes below (watermark - tailFingerprintWindow), so
	// it falls outside the fingerprinted tail while the bytes the
	// fingerprint actually covers (the filler line's own tail) stay
	// untouched and matching.
	firstText := "first message"
	firstLine := makeUserRecord("u1", "", sid, firstText, "2026-05-01T10:00:00Z")
	fillerText := strings.Repeat("F", tailFingerprintWindow+1000)
	fillerLine := makeUserRecord("u2", "u1", sid, fillerText, "2026-05-01T10:00:30Z")
	path := writeJSONLFixture(t, dir, sid, []string{firstLine, fillerLine})

	// Cycle 1: full parse establishes the watermark (and its tail fingerprint).
	meta1, turns1, err := indexSession(path, sid, 2*tailFingerprintWindow+4096)
	if err != nil {
		t.Fatalf("indexSession (cycle 1): %v", err)
	}
	if len(turns1) != 2 {
		t.Fatalf("expected 2 turns after cycle 1, got %d", len(turns1))
	}
	if meta1.SourceOffset != meta1.SourceSize {
		t.Fatalf("expected SourceOffset == SourceSize after a full parse, got offset=%d size=%d",
			meta1.SourceOffset, meta1.SourceSize)
	}
	if meta1.SourceOffset <= int64(len(firstLine))+int64(tailFingerprintWindow) {
		t.Fatalf("test setup: watermark (%d) must exceed len(firstLine)+tailFingerprintWindow (%d) so the "+
			"sentinel line lands outside the fingerprinted tail", meta1.SourceOffset,
			int64(len(firstLine))+int64(tailFingerprintWindow))
	}

	// Corrupt the already-consumed sentinel line in place — same byte length
	// (so the watermark byte offset still lands exactly on the line
	// boundary), different uuid AND different text. Different uuid matters:
	// it is what keeps this corrupted record out of the dedup-by-uuid set
	// seeded from prevTurns, so a wrongly-reread copy of it cannot be
	// silently absorbed as a harmless "duplicate" — it would show up as a
	// distinct extra turn.
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
	secondLine := makeUserRecord("u3", "u2", sid, secondText, "2026-05-01T10:01:00Z")
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
	meta2, turns2, used, err := indexSessionIncremental(path, sid, 2*tailFingerprintWindow+4096, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used (the sentinel corruption sits below " +
			"the fingerprinted tail, so SourceTailHash still matches)")
	}
	// The length check is the load-bearing proof: a from-byte-0 re-read
	// would pick up the corrupted sentinel record (different uuid, so not
	// deduped) as a spurious extra turn. Exactly 3 back means the corrupted
	// prefix bytes were never read.
	if len(turns2) != 3 {
		t.Fatalf("expected exactly 3 turns after cycle 2 (got %d) — a from-byte-0 re-read would have "+
			"picked up the corrupted sentinel record (text %q) as a spurious extra turn, meaning the "+
			"parse read past the watermark into already-indexed, now-corrupted bytes instead of "+
			"resuming the tail from it", len(turns2), corruptedText)
	}
	if turns2[0].Text != firstText {
		t.Errorf("turns2[0].Text = %q, want %q (the caller-supplied prevTurns prefix)", turns2[0].Text, firstText)
	}
	if turns2[1].Text != fillerText {
		t.Errorf("turns2[1].Text mismatch on the filler turn (want the unmodified prevTurns prefix)")
	}
	if turns2[2].Text != secondText {
		t.Errorf("turns2[2].Text = %q, want %q — the sole new tail turn should be the genuinely "+
			"appended record, not the corrupted sentinel record", turns2[2].Text, secondText)
	}
	if turns2[0].TurnIndex != 0 || turns2[1].TurnIndex != 1 || turns2[2].TurnIndex != 2 {
		t.Errorf("expected contiguous turn indices 0,1,2; got %d,%d,%d",
			turns2[0].TurnIndex, turns2[1].TurnIndex, turns2[2].TurnIndex)
	}
	if meta2.TurnCount != 3 {
		t.Errorf("expected TurnCount 3, got %d", meta2.TurnCount)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta2.SourceOffset != fi.Size() {
		t.Errorf("expected watermark to advance to the new EOF (%d), got %d", fi.Size(), meta2.SourceOffset)
	}
}

// TestIndexSessionIncremental_FallsBackOnRewriteThenGrow guards against the
// residual left by the same-size rewrite check alone: a source file
// rewritten in place with different records that ALSO change the file's
// total size is invisible to both the shrink check (the file didn't shrink)
// and the same-size mtime check (the size did move, so that branch never
// runs) — a size-only/mtime-only heuristic sees this as ordinary append-only
// growth and would resume from the watermark, merging the stale prevTurns
// prefix onto a tail parsed from content that no longer follows it,
// producing a permanently and silently wrong index with no self-heal path
// (the watermark advances to the new EOF on the very cycle that corrupts
// it). SourceTailHash closes this: the bytes below the watermark no longer
// match what was fingerprinted at the last parse, so the fast path must
// decline regardless of which direction the size moved.
func TestIndexSessionIncremental_FallsBackOnRewriteThenGrow(t *testing.T) {
	dir := t.TempDir()
	const sid = "cccccccc-dddd-eeee-ffff-000000000000"

	lines := []string{
		makeUserRecord("u1", "", sid, "alpha", "2026-05-01T10:00:00Z"),
		makeUserRecord("u2", "u1", sid, "bravo", "2026-05-01T10:01:00Z"),
	}
	path := writeJSONLFixture(t, dir, sid, lines)

	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	if len(turns1) != 2 {
		t.Fatalf("test setup: expected 2 turns after cycle 1, got %d", len(turns1))
	}
	if meta1.SourceTailHash == "" {
		t.Fatalf("test setup: expected indexSession to populate SourceTailHash")
	}

	// Rewrite the whole file in place with 3 different records, strictly
	// larger than the original — the reviewer's exact rewrite-then-grow
	// scenario. Ground truth is whatever a full re-parse of this content
	// produces: [delta, echo, foxtrot], not a merge of the stale
	// [alpha, bravo] prefix with any part of the new content.
	replacementLines := []string{
		makeUserRecord("u3", "", sid, "delta-rewritten", "2026-05-01T11:00:00Z"),
		makeUserRecord("u4", "u3", sid, "echo-rewritten", "2026-05-01T11:01:00Z"),
		makeUserRecord("u5", "u4", sid, "foxtrot-rewritten", "2026-05-01T11:02:00Z"),
	}
	replacement := strings.Join(replacementLines, "\n") + "\n"
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if len(replacement) <= len(original) {
		t.Fatalf("test setup: replacement (%d bytes) must be strictly larger than the original (%d bytes)",
			len(replacement), len(original))
	}

	future := time.Now().Add(10 * time.Second)
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatalf("rewrite-then-grow: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() <= meta1.SourceOffset {
		t.Fatalf("test setup: rewritten file size %d must exceed the watermark %d (grown, not shrunk)",
			fi.Size(), meta1.SourceOffset)
	}

	_, _, used, err := indexSessionIncremental(path, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if used {
		t.Fatalf("expected the incremental fast path to decline on a rewrite-then-grow (content below the " +
			"watermark no longer matches SourceTailHash, even though the file grew) — taking it here " +
			"would silently merge the stale [alpha, bravo] prefix onto a tail read from unrelated " +
			"content, corrupting the index with no recovery path")
	}

	// The self-heal: the caller's fallback to indexSession must match a
	// from-scratch full re-parse of the rewritten content exactly — the
	// ground truth this scenario is checked against.
	meta3, turns3, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (fallback): %v", err)
	}
	if len(turns3) != 3 {
		t.Fatalf("expected 3 turns after fallback re-parse, got %d", len(turns3))
	}
	wantTexts := []string{"delta-rewritten", "echo-rewritten", "foxtrot-rewritten"}
	for i, want := range wantTexts {
		if turns3[i].Text != want {
			t.Errorf("turns3[%d].Text = %q, want %q", i, turns3[i].Text, want)
		}
	}
	if meta3.TurnCount != 3 {
		t.Errorf("expected TurnCount 3 after fallback, got %d", meta3.TurnCount)
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

// TestIndexSessionIncremental_FallsBackWhenTurnsProjectionMissing guards
// against a data-loss regression: prevMeta.TurnCount says N turns are
// indexed, but the caller-supplied prevTurns is empty (e.g. _meta.json
// survived while the session's turns projection file was deleted or never
// loaded — Index.loadTurnsFile returns (nil, nil) for a missing file,
// unlike a corrupt one). Taking the fast path in this state would merge the
// newly-appended tail onto an empty prefix and silently discard every
// already-indexed turn, with no self-healing path (the watermark keeps
// advancing on subsequent cycles). The fast path must decline so the
// caller falls back to a full re-parse, which rebuilds the correct history
// from disk.
func TestIndexSessionIncremental_FallsBackWhenTurnsProjectionMissing(t *testing.T) {
	dir := t.TempDir()
	const sid = "dddddddd-eeee-ffff-0000-111111111111"

	lines := []string{
		makeUserRecord("u1", "", sid, "first message", "2026-05-01T10:00:00Z"),
		makeUserRecord("u2", "u1", sid, "second message", "2026-05-01T10:01:00Z"),
	}
	path := writeJSONLFixture(t, dir, sid, lines)

	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	if len(turns1) != 2 || meta1.TurnCount != 2 {
		t.Fatalf("test setup: expected 2 turns/TurnCount after cycle 1, got turns=%d TurnCount=%d",
			len(turns1), meta1.TurnCount)
	}

	// Append a third turn — a legitimate, ordinary growth.
	thirdLine := makeUserRecord("u3", "u2", sid, "third message", "2026-05-01T10:02:00Z")
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := af.WriteString("\n" + thirdLine); err != nil {
		af.Close()
		t.Fatalf("append tail: %v", err)
	}
	af.Close()

	// Simulate the meta-present/turns-file-absent state: prevMeta still
	// claims TurnCount==2, but prevTurns comes back empty (as
	// Index.SessionTurns would report for a session whose turns file was
	// removed while _meta.json survived).
	var emptyPrevTurns []Turn
	_, _, used, err := indexSessionIncremental(path, sid, 8192, meta1, emptyPrevTurns)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if used {
		t.Fatalf("expected the incremental fast path to decline when len(prevTurns) (%d) != prevMeta.TurnCount (%d) — "+
			"taking it here would silently drop the 2 already-indexed turns and yield only the newly appended tail",
			len(emptyPrevTurns), meta1.TurnCount)
	}

	// The self-heal: the caller's fallback to indexSession must recover all
	// 3 turns from disk.
	meta2, turns2, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (fallback): %v", err)
	}
	if len(turns2) != 3 || meta2.TurnCount != 3 {
		t.Errorf("expected the full re-parse fallback to recover 3 turns, got turns=%d TurnCount=%d",
			len(turns2), meta2.TurnCount)
	}
}

// TestIndexSessionIncremental_FallsBackOnSameSizeRewrite guards against
// silent, permanent staleness: a source file rewritten in place at exactly
// the same byte size (different content, same length) is invisible to the
// size-only shrink/grow check — fi.Size() still equals the recorded
// watermark, so a naive "size didn't shrink" test would take the fast path,
// seek to a watermark whose already-consumed bytes are no longer what was
// actually parsed, and read zero new bytes forever after (since the
// watermark never again falls below fi.Size()). The fast path must decline
// whenever the file is at the watermark size AND its mtime moved, so the
// caller falls back to a full re-parse that picks up the rewritten content.
func TestIndexSessionIncremental_FallsBackOnSameSizeRewrite(t *testing.T) {
	dir := t.TempDir()
	const sid = "eeeeeeee-ffff-0000-1111-222222222222"

	lines := []string{
		makeUserRecord("u1", "", sid, "alpha", "2026-05-01T10:00:00Z"),
		makeUserRecord("u2", "u1", sid, "bravo", "2026-05-01T10:01:00Z"),
	}
	path := writeJSONLFixture(t, dir, sid, lines)

	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	if len(turns1) != 2 {
		t.Fatalf("test setup: expected 2 turns after cycle 1, got %d", len(turns1))
	}

	// Rewrite the whole file in place with different records of the exact
	// same total byte length (same uuid/text lengths as the originals).
	replacementLines := []string{
		makeUserRecord("u3", "", sid, "delta", "2026-05-01T11:00:00Z"),
		makeUserRecord("u4", "u3", sid, "echo!", "2026-05-01T11:01:00Z"),
	}
	replacement := strings.Join(replacementLines, "\n") + "\n"
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if len(replacement) != len(original) {
		t.Fatalf("test setup: replacement length %d != original length %d — fixture must stay same-size",
			len(replacement), len(original))
	}

	future := time.Now().Add(10 * time.Second)
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatalf("same-size rewrite: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != meta1.SourceOffset {
		t.Fatalf("test setup: rewritten file size %d must equal the watermark %d", fi.Size(), meta1.SourceOffset)
	}

	_, _, used, err := indexSessionIncremental(path, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if used {
		t.Fatalf("expected the incremental fast path to decline on a same-size in-place rewrite " +
			"(size unchanged, mtime moved) — taking it here would parse zero new bytes and leave the " +
			"index showing the pre-rewrite content forever, with no recovery path")
	}

	// The self-heal: the caller's fallback to indexSession must pick up the
	// rewritten content.
	meta2, turns2, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (fallback): %v", err)
	}
	if len(turns2) != 2 {
		t.Fatalf("expected 2 turns after fallback re-parse, got %d", len(turns2))
	}
	if turns2[0].Text != "delta" || turns2[1].Text != "echo!" {
		t.Errorf("expected the fallback re-parse to reflect the rewritten content, got %q, %q",
			turns2[0].Text, turns2[1].Text)
	}
	if meta2.TurnCount != 2 {
		t.Errorf("expected TurnCount 2 after fallback, got %d", meta2.TurnCount)
	}
}

// TestIndexSessionIncremental_TornLastLineNotLostOnCompletion guards against
// issue #558 part 1's torn-last-line finding: a session JSONL can have a
// partially-written last line whenever a cycle reads it mid-append (CC
// flushes a record's bytes incrementally, not atomically). parser.go already
// skips such a line as unparseable — but indexSession/indexSessionIncremental
// used to record the watermark as fi.Size() regardless, advancing it PAST
// those unparsed bytes. Once the writer finished the line, the bytes below
// the (wrongly advanced) watermark were unchanged, so SourceTailHash still
// matched, the incremental fast path was taken, and the now-complete record
// — sitting entirely below the watermark — was never read: a permanent,
// silent turn loss with no self-heal.
//
// Reproduction: 2 complete records, then a 3rd record torn mid-JSON with no
// trailing newline (the writer stopped mid-append). Cycle 1 must record the
// watermark at the end of the 2nd record, strictly before the torn bytes —
// proving indexSession no longer uses fi.Size(). The writer then completes
// the 3rd record by appending the rest of its JSON plus a trailing newline.
// Cycle 2 must pick up the completed 3rd record via the incremental path,
// yielding turns identical to a full re-parse ground truth.
func TestIndexSessionIncremental_TornLastLineNotLostOnCompletion(t *testing.T) {
	dir := t.TempDir()
	const sid = "ffffffff-0000-1111-2222-333333333333"

	alphaLine := makeUserRecord("u1", "", sid, "alpha", "2026-05-01T10:00:00Z")
	bravoLine := makeUserRecord("u2", "u1", sid, "bravo", "2026-05-01T10:01:00Z")
	charlieLine := makeUserRecord("u3", "u2", sid, "charlie-the-torn-record", "2026-05-01T10:02:00Z")

	// Cut charlieLine partway through — mid-JSON, no trailing newline — to
	// simulate the writer having flushed only a prefix of the record so far.
	cut := len(charlieLine) * 3 / 5
	tornPrefix := charlieLine[:cut]
	completionSuffix := charlieLine[cut:]
	if err := json.Unmarshal([]byte(tornPrefix), &map[string]any{}); err == nil {
		t.Fatalf("test setup: tornPrefix must NOT be valid JSON on its own (got a clean parse)")
	}

	path := filepath.Join(dir, sid+".jsonl")
	initial := alphaLine + "\n" + bravoLine + "\n" + tornPrefix // no trailing newline: torn
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial fixture: %v", err)
	}

	// Cycle 1: full parse. Only the 2 complete records are readable; the
	// torn 3rd line is skipped like any other unparseable line.
	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (cycle 1): %v", err)
	}
	if len(turns1) != 2 {
		t.Fatalf("expected 2 turns after cycle 1 (torn 3rd line must be skipped), got %d", len(turns1))
	}
	if turns1[0].Text != "alpha" || turns1[1].Text != "bravo" {
		t.Fatalf("unexpected cycle 1 turn texts: %q, %q", turns1[0].Text, turns1[1].Text)
	}

	wantWatermark := int64(len(alphaLine) + 1 + len(bravoLine) + 1)
	// THE LOAD-BEARING ASSERTION: the watermark must land exactly at the end
	// of the 2nd (complete) line, strictly before the torn bytes — never at
	// fi.Size(), which would include them.
	if meta1.SourceOffset != wantWatermark {
		t.Fatalf("meta1.SourceOffset = %d, want %d (end of the 2nd complete line) — a watermark of %d "+
			"(fi.Size()) would advance past the %d torn bytes and permanently lose the record once "+
			"completed", meta1.SourceOffset, wantWatermark, meta1.SourceSize, len(tornPrefix))
	}
	if meta1.SourceOffset >= meta1.SourceSize {
		t.Fatalf("test setup: watermark (%d) must be strictly less than the file size (%d) for this "+
			"reproduction to exercise anything", meta1.SourceOffset, meta1.SourceSize)
	}

	// The writer completes the 3rd record: appends the rest of its JSON plus
	// a trailing newline. The bytes below cycle 1's watermark are untouched.
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := af.WriteString(completionSuffix + "\n"); err != nil {
		af.Close()
		t.Fatalf("append completion: %v", err)
	}
	af.Close()

	// Cycle 2: incremental parse from cycle 1's watermark, seeded with cycle
	// 1's in-memory turns.
	meta2, turns2, used, err := indexSessionIncremental(path, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental (cycle 2): %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used (bytes below the watermark are unchanged)")
	}
	if len(turns2) != 3 {
		t.Fatalf("expected 3 turns after cycle 2 (got %d) — the completed 3rd record must be re-read from "+
			"below the (correctly unadvanced) watermark, not lost", len(turns2))
	}
	wantTexts := []string{"alpha", "bravo", "charlie-the-torn-record"}
	for i, want := range wantTexts {
		if turns2[i].Text != want {
			t.Errorf("turns2[%d].Text = %q, want %q", i, turns2[i].Text, want)
		}
	}
	if meta2.TurnCount != 3 {
		t.Errorf("expected TurnCount 3, got %d", meta2.TurnCount)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta2.SourceOffset != fi.Size() {
		t.Errorf("expected watermark to advance to the new EOF (%d) now that the 3rd line is complete, got %d",
			fi.Size(), meta2.SourceOffset)
	}

	// Ground truth: a full re-parse of the final file content must match
	// cycle 2's incremental result exactly.
	meta3, turns3, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession (ground truth): %v", err)
	}
	if len(turns3) != len(turns2) {
		t.Fatalf("ground truth full re-parse yielded %d turns, incremental cycle 2 yielded %d", len(turns3), len(turns2))
	}
	for i := range turns3 {
		if turns3[i].Text != turns2[i].Text || turns3[i].UUID != turns2[i].UUID {
			t.Errorf("turn %d mismatch vs ground truth: incremental={%q,%q} full-reparse={%q,%q}",
				i, turns2[i].UUID, turns2[i].Text, turns3[i].UUID, turns3[i].Text)
		}
	}
	if meta3.TurnCount != meta2.TurnCount {
		t.Errorf("ground truth TurnCount %d != incremental TurnCount %d", meta3.TurnCount, meta2.TurnCount)
	}

	// Cycle 3: repeat the torn-line scenario, but this time appended AFTER
	// an already-established incremental watermark, to exercise
	// indexSessionIncremental's own offset bookkeeping (not indexSession's) —
	// the two are separate call sites of ParseSession and each sets
	// meta.SourceOffset independently.
	deltaLine := makeUserRecord("u4", "u3", sid, "delta-the-second-torn-record", "2026-05-01T10:03:00Z")
	deltaCut := len(deltaLine) * 2 / 5
	deltaTornPrefix := deltaLine[:deltaCut]
	deltaCompletionSuffix := deltaLine[deltaCut:]
	if err := json.Unmarshal([]byte(deltaTornPrefix), &map[string]any{}); err == nil {
		t.Fatalf("test setup: deltaTornPrefix must NOT be valid JSON on its own (got a clean parse)")
	}

	af2, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for delta append: %v", err)
	}
	if _, err := af2.WriteString(deltaTornPrefix); err != nil { // no trailing newline: torn again
		af2.Close()
		t.Fatalf("append torn delta: %v", err)
	}
	af2.Close()

	meta4, turns4, used, err := indexSessionIncremental(path, sid, 8192, meta2, turns2)
	if err != nil {
		t.Fatalf("indexSessionIncremental (cycle 3, torn delta): %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used for cycle 3")
	}
	if len(turns4) != 3 {
		t.Fatalf("expected 3 turns after cycle 3 (torn 4th line must be skipped), got %d", len(turns4))
	}
	// THE LOAD-BEARING ASSERTION for indexSessionIncremental specifically:
	// the watermark must stay exactly where cycle 2 left it — never advance
	// into the torn delta bytes just because fi.Size() grew.
	if meta4.SourceOffset != meta2.SourceOffset {
		t.Fatalf("meta4.SourceOffset = %d, want %d (unchanged from cycle 2) — advancing past the torn "+
			"delta bytes here would permanently lose that record once completed, via "+
			"indexSessionIncremental's own offset computation rather than indexSession's",
			meta4.SourceOffset, meta2.SourceOffset)
	}

	// The writer completes the 4th record.
	af3, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for delta completion: %v", err)
	}
	if _, err := af3.WriteString(deltaCompletionSuffix + "\n"); err != nil {
		af3.Close()
		t.Fatalf("append delta completion: %v", err)
	}
	af3.Close()

	meta5, turns5, used, err := indexSessionIncremental(path, sid, 8192, meta4, turns4)
	if err != nil {
		t.Fatalf("indexSessionIncremental (cycle 4): %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used for cycle 4")
	}
	if len(turns5) != 4 {
		t.Fatalf("expected 4 turns after cycle 4 (got %d) — the completed 4th record must be re-read", len(turns5))
	}
	if turns5[3].Text != "delta-the-second-torn-record" {
		t.Errorf("turns5[3].Text = %q, want %q", turns5[3].Text, "delta-the-second-torn-record")
	}
	fi5, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta5.SourceOffset != fi5.Size() {
		t.Errorf("expected watermark to advance to the new EOF (%d), got %d", fi5.Size(), meta5.SourceOffset)
	}
	if meta5.TurnCount != 4 {
		t.Errorf("expected TurnCount 4, got %d", meta5.TurnCount)
	}
}

// TestIndexSessionIncremental_RefreshesSourcePath verifies that a session
// whose file relocates while keeping its UUID (CC derives its project
// directory from the cwd slug, so renaming/moving a project dir moves
// <uuid>.jsonl) does not keep serving the pre-move source_path indefinitely
// through the incremental fast path — matching indexSession's behavior of
// always recording the sourcePath it actually parsed.
func TestIndexSessionIncremental_RefreshesSourcePath(t *testing.T) {
	dir := t.TempDir()
	const sid = "ffffffff-0000-1111-2222-333333333333"

	line := makeUserRecord("u1", "", sid, "first message", "2026-05-01T10:00:00Z")
	path := writeJSONLFixture(t, dir, sid, []string{line})

	meta1, turns1, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}

	// Simulate a relocated source path (new directory, same UUID filename)
	// by passing a different sourcePath into the incremental call while
	// appending a new turn to the original file (indexSessionIncremental
	// itself reads from the sourcePath argument, so point it at a copy).
	newDir := filepath.Join(dir, "moved")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	newPath := filepath.Join(newDir, sid+".jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	secondLine := makeUserRecord("u2", "u1", sid, "second message", "2026-05-01T10:01:00Z")
	if err := os.WriteFile(newPath, append(original, []byte("\n"+secondLine)...), 0o644); err != nil {
		t.Fatalf("write moved fixture: %v", err)
	}

	meta2, _, used, err := indexSessionIncremental(newPath, sid, 8192, meta1, turns1)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if !used {
		t.Fatalf("expected the incremental fast path to be used")
	}
	if meta2.SourcePath != newPath {
		t.Errorf("meta.SourcePath = %q, want %q (the relocated path actually parsed, not prevMeta's stale %q)",
			meta2.SourcePath, newPath, meta1.SourcePath)
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
	_, err := ParseSession(strings.NewReader(jsonl), "test-session", 8192, &meta, func(t Turn) bool {
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
	_, _ = ParseSession(strings.NewReader(jsonl), "test-session-2", 8192, &meta, func(t Turn) bool {
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

// TestProviderIngest_ThreadPartition indexes a session whose JSONL contains a
// braided parentUuid DAG: a linear main thread plus a second thread — a
// subagent-sidechain fresh root (parentUuid:null, isSidechain:true), the
// verified on-disk mechanism from the #557 plan — and asserts the split is
// detected, represented in SessionMeta.Threads, and survives the full
// ApplyPlan → disk (_meta.json) → FetchLive round trip (so a silent field
// drop in the JSON marshal/unmarshal path would be caught here, not just in
// the pure PartitionThreads unit tests).
func TestProviderIngest_ThreadPartition(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	const threadSID = "22222222-3333-4444-5555-666666666666"

	lines := []string{
		makeUserRecord("main-u1", "", threadSID, "main thread question", "2026-06-01T10:00:00Z"),
		makeAssistantRecord("main-a1", "main-u1", threadSID, "main thread answer", "2026-06-01T10:01:00Z"),
		makeSidechainUserRecord("sub-u1", "", threadSID, "subagent sidechain turn", "2026-06-01T10:02:00Z"),
		makeSidechainAssistantRecord("sub-a1", "sub-u1", threadSID, "subagent sidechain reply", "2026-06-01T10:03:00Z"),
	}
	writeJSONLFixture(t, srcDir, threadSID, lines)
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
	if _, err := p.ApplyPlan(ctx, plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	// Round trip through disk: FetchLive reloads from _meta.json.
	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	ls, ok := liveAny2.(*liveState)
	if !ok {
		t.Fatal("expected *liveState")
	}
	entry, ok := ls.Entries[threadSID]
	if !ok {
		t.Fatalf("session %s not in live state", threadSID)
	}

	threads := entry.Meta.Threads
	if len(threads) != 2 {
		t.Fatalf("want 2 threads after round trip, got %d: %+v", len(threads), threads)
	}

	var main, sidechain *ThreadMeta
	for i := range threads {
		switch threads[i].Role {
		case ThreadRoleMain:
			main = &threads[i]
		case ThreadRoleSubagentSidechain:
			sidechain = &threads[i]
		}
	}
	if main == nil {
		t.Fatalf("no main thread survived the round trip: %+v", threads)
	}
	if sidechain == nil {
		t.Fatalf("no subagent-sidechain thread survived the round trip: %+v", threads)
	}
	if main.ThreadID != "main-u1" {
		t.Errorf("main ThreadID: want main-u1, got %q", main.ThreadID)
	}
	if main.MessageCount != 2 {
		t.Errorf("main MessageCount: want 2, got %d", main.MessageCount)
	}
	if sidechain.ThreadID != "sub-u1" {
		t.Errorf("sidechain ThreadID: want sub-u1, got %q", sidechain.ThreadID)
	}
	if sidechain.MessageCount != 2 {
		t.Errorf("sidechain MessageCount: want 2, got %d", sidechain.MessageCount)
	}

	// Per-turn ThreadID/IsSidechain must also survive the round trip.
	idxTurn, ok := p.index.GetTurn(threadSID, 2) // turn_index 2 = sub-u1
	if !ok {
		t.Fatalf("GetTurn(threadSID, 2) not found")
	}
	if idxTurn.ThreadID != "sub-u1" {
		t.Errorf("turn 2 ThreadID: want sub-u1, got %q", idxTurn.ThreadID)
	}
	if !idxTurn.IsSidechain {
		t.Error("turn 2 IsSidechain: want true, got false")
	}
}

// TestIngestProvenance_HandCarriedRecord exercises the Phase 3 provenance
// headroom: a normalized-ingest record declaring a non-default provenance
// (e.g. content recovered by some means other than a direct JSONL parse)
// carries that value through to the indexed Turn. This is schema headroom
// only — no importer sets this field yet; the fixture simulates what a
// future importer's emitted record would look like.
func TestIngestProvenance_HandCarriedRecord(t *testing.T) {
	acc := newIngestAccumulator(defaultMaxTurnLen)

	rec := map[string]any{
		"schema":     "cogos.observatory.conversations/v0.1",
		"source":     "hand-carried-side-chat",
		"session_id": "sidechat-001",
		"role":       "user",
		"timestamp":  "2026-06-01T10:00:00Z",
		"text":       "recovered side-chat turn",
		"provenance": "hand-carried",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	line := append(b, '\n')

	if err := acc.ConsumeFile(strings.NewReader(string(line))); err != nil {
		t.Fatalf("ConsumeFile: %v", err)
	}

	sessions := acc.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	turns := sessions[0].Turns
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].Provenance != "hand-carried" {
		t.Errorf("Provenance: want %q, got %q", "hand-carried", turns[0].Provenance)
	}

	// A record with no provenance= field defaults to "" (the "direct-jsonl"
	// convention — absence means direct parse, not a distinct string value).
	recDefault := map[string]any{
		"schema":     "cogos.observatory.conversations/v0.1",
		"source":     "hand-carried-side-chat",
		"session_id": "sidechat-002",
		"role":       "user",
		"timestamp":  "2026-06-01T10:00:00Z",
		"text":       "normal ingest turn",
	}
	b2, err := json.Marshal(recDefault)
	if err != nil {
		t.Fatalf("marshal default fixture record: %v", err)
	}
	line2 := append(b2, '\n')
	if err := acc.ConsumeFile(strings.NewReader(string(line2))); err != nil {
		t.Fatalf("ConsumeFile 2: %v", err)
	}
	sessions2 := acc.Sessions()
	var defaultTurn *Turn
	for _, s := range sessions2 {
		if s.Meta.SessionID == indexKeyForIngest("hand-carried-side-chat", "sidechat-002") {
			defaultTurn = &s.Turns[0]
		}
	}
	if defaultTurn == nil {
		t.Fatalf("session sidechat-002 not found")
	}
	if defaultTurn.Provenance != "" {
		t.Errorf("default Provenance: want empty, got %q", defaultTurn.Provenance)
	}
}

// makeSidechainUserRecord returns a JSON line representing a user message
// carrying isSidechain:true — the CC subagent-transcript marker.
func makeSidechainUserRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":        "user",
		"uuid":        uuid,
		"parentUuid":  parentUUID,
		"sessionId":   sessionID,
		"timestamp":   ts,
		"message":     json.RawMessage(msg),
		"userType":    "external",
		"entrypoint":  "cli",
		"isSidechain": true,
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// makeSidechainAssistantRecord returns a JSON line representing an assistant
// message carrying isSidechain:true.
func makeSidechainAssistantRecord(uuid, parentUUID, sessionID, text, ts string) string {
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"model":   "claude-sonnet-4-6",
		"content": json.RawMessage(content),
	})
	rec := map[string]any{
		"type":        "assistant",
		"uuid":        uuid,
		"parentUuid":  parentUUID,
		"sessionId":   sessionID,
		"timestamp":   ts,
		"message":     json.RawMessage(msg),
		"userType":    "external",
		"entrypoint":  "cli",
		"isSidechain": true,
	}
	b, _ := json.Marshal(rec)
	return string(b)
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
