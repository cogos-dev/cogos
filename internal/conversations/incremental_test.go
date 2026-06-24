package conversations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// userRecordLine builds one type="user" JSONL record line (with trailing \n)
// using the same shape the real Claude Code session files use.
func userRecordLine(t *testing.T, sessionID, uuid, text, ts string) string {
	t.Helper()
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	msg, _ := json.Marshal(map[string]any{"role": "user", "content": json.RawMessage(content)})
	rec := map[string]any{
		"type":       "user",
		"uuid":       uuid,
		"parentUuid": "",
		"sessionId":  sessionID,
		"timestamp":  ts,
		"message":    json.RawMessage(msg),
		"userType":   "external",
		"entrypoint": "cli",
	}
	b, _ := json.Marshal(rec)
	return string(b) + "\n"
}

// writeJSONL writes content to a temp file and returns its path + sourceFileInfo.
func writeJSONL(t *testing.T, dir, name, content string) (string, sourceFileInfo) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return path, sourceFileInfo{
		Path:      path,
		SessionID: strings.TrimSuffix(name, ".jsonl"),
		Mtime:     fi.ModTime(),
		Size:      fi.Size(),
	}
}

// TestComputeFilePrefixHash verifies the prefix hash is stable under append,
// changes under a head edit, and gracefully hashes files smaller than the
// window.
func TestComputeFilePrefixHash(t *testing.T) {
	dir := t.TempDir()

	// Small file (< window): whole file hashed, must be deterministic.
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := computeFilePrefixHash(small, prefixHashWindow)
	if err != nil {
		t.Fatalf("hash small: %v", err)
	}
	h1b, _ := computeFilePrefixHash(small, prefixHashWindow)
	if h1 != h1b {
		t.Fatalf("hash not deterministic: %q vs %q", h1, h1b)
	}

	// Build a file larger than the window so appends never touch the hashed head.
	head := strings.Repeat("A", int(prefixHashWindow)+1024)
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	hBefore, err := computeFilePrefixHash(big, prefixHashWindow)
	if err != nil {
		t.Fatalf("hash big: %v", err)
	}

	// Append beyond the window: prefix hash must be unchanged.
	f, _ := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(strings.Repeat("B", 4096))
	f.Close()
	hAfterAppend, _ := computeFilePrefixHash(big, prefixHashWindow)
	if hAfterAppend != hBefore {
		t.Errorf("append changed prefix hash: %q -> %q", hBefore, hAfterAppend)
	}

	// Rewrite the first byte: prefix hash must change.
	data, _ := os.ReadFile(big)
	data[0] = 'Z'
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}
	hAfterEdit, _ := computeFilePrefixHash(big, prefixHashWindow)
	if hAfterEdit == hBefore {
		t.Error("head edit did not change prefix hash")
	}
}

// TestIsDriftAppendOnlyClassification exercises the four drift outcomes:
// no-drift, append-only, truncation, and head-rewrite.
func TestIsDriftAppendOnlyClassification(t *testing.T) {
	dir := t.TempDir()
	sid := "11111111-1111-1111-1111-111111111111"

	// Initial content larger than the prefix window so appends stay clear of it.
	head := strings.Repeat("x", int(prefixHashWindow)+512)
	line0 := head + "\n"
	path, _ := writeJSONL(t, dir, sid+".jsonl", line0)

	prefix, err := computeFilePrefixHash(path, prefixHashWindow)
	if err != nil {
		t.Fatalf("prefix hash: %v", err)
	}
	fi, _ := os.Stat(path)
	meta := SessionMeta{
		SessionID:            sid,
		SourcePath:           path,
		SourceSize:           fi.Size(),
		SourceMtime:          fi.ModTime(),
		LastParsedByteOffset: fi.Size(),
		LastParsedTurnIndex:  1,
		PrefixSha256:         prefix,
	}

	// (a) No drift: identical size + mtime.
	if d := isDrift(meta, sourceFileInfo{Path: path, Size: fi.Size(), Mtime: fi.ModTime()}); d.Drifted {
		t.Errorf("expected no drift, got %+v", d)
	}

	// (b) Append-only: grow the file beyond the window, prefix unchanged.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(strings.Repeat("y", 2048) + "\n")
	f.Close()
	fiGrown, _ := os.Stat(path)
	dGrown := isDrift(meta, sourceFileInfo{Path: path, Size: fiGrown.Size(), Mtime: fiGrown.ModTime().Add(3 * time.Second)})
	if !dGrown.Drifted || !dGrown.IsAppendOnly {
		t.Errorf("expected append-only drift, got %+v", dGrown)
	}

	// (c) Truncation: size smaller than indexed → not append-only.
	dTrunc := isDrift(meta, sourceFileInfo{Path: path, Size: meta.SourceSize - 10, Mtime: fi.ModTime().Add(3 * time.Second)})
	if !dTrunc.Drifted || dTrunc.IsAppendOnly {
		t.Errorf("expected full-reparse drift on truncation, got %+v", dTrunc)
	}

	// (d) Head rewrite: same growth but stored prefix hash no longer matches.
	metaStale := meta
	metaStale.PrefixSha256 = "deadbeef"
	dRewrite := isDrift(metaStale, sourceFileInfo{Path: path, Size: fiGrown.Size(), Mtime: fiGrown.ModTime().Add(3 * time.Second)})
	if !dRewrite.Drifted || dRewrite.IsAppendOnly {
		t.Errorf("expected full-reparse drift on head rewrite, got %+v", dRewrite)
	}

	// (e) No cursor: append-only growth but no recorded offset/hash → full reparse.
	metaNoCursor := meta
	metaNoCursor.LastParsedByteOffset = 0
	metaNoCursor.PrefixSha256 = ""
	dNoCursor := isDrift(metaNoCursor, sourceFileInfo{Path: path, Size: fiGrown.Size(), Mtime: fiGrown.ModTime().Add(3 * time.Second)})
	if !dNoCursor.Drifted || dNoCursor.IsAppendOnly {
		t.Errorf("expected full-reparse drift without a cursor, got %+v", dNoCursor)
	}
}

// TestParseSessionIncrementalTail proves that, seeked to a cursor, the
// incremental parser emits only the appended records, numbers them from the
// supplied start index, and reports correct absolute offsets.
func TestParseSessionIncrementalTail(t *testing.T) {
	dir := t.TempDir()
	sid := "22222222-2222-2222-2222-222222222222"

	l0 := userRecordLine(t, sid, "u0", "first turn", "2026-05-01T10:00:00Z")
	l1 := userRecordLine(t, sid, "u1", "second turn", "2026-05-01T10:01:00Z")
	path, _ := writeJSONL(t, dir, sid+".jsonl", l0+l1)

	// Full parse to establish the cursor.
	fullMeta, fullTurns, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	if len(fullTurns) != 2 {
		t.Fatalf("expected 2 turns from full parse, got %d", len(fullTurns))
	}
	if fullMeta.LastParsedTurnIndex != 2 {
		t.Fatalf("cursor turn index = %d, want 2", fullMeta.LastParsedTurnIndex)
	}
	if fullMeta.LastParsedByteOffset != int64(len(l0)+len(l1)) {
		t.Fatalf("cursor byte offset = %d, want %d", fullMeta.LastParsedByteOffset, len(l0)+len(l1))
	}

	// Append two more records.
	l2 := userRecordLine(t, sid, "u2", "third turn", "2026-05-01T10:02:00Z")
	l3 := userRecordLine(t, sid, "u3", "fourth turn", "2026-05-01T10:03:00Z")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(l2 + l3)
	f.Close()

	incMeta, merged, newCount, err := indexSessionIncremental(path, sid, fullMeta, fullTurns, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if newCount != 2 {
		t.Fatalf("expected 2 new turns, got %d", newCount)
	}
	if len(merged) != 4 {
		t.Fatalf("expected 4 merged turns, got %d", len(merged))
	}
	// New turns must be numbered 2 and 3 and carry the appended text.
	if merged[2].TurnIndex != 2 || merged[2].UUID != "u2" {
		t.Errorf("turn[2] = idx %d uuid %q, want 2/u2", merged[2].TurnIndex, merged[2].UUID)
	}
	if merged[3].TurnIndex != 3 || merged[3].UUID != "u3" {
		t.Errorf("turn[3] = idx %d uuid %q, want 3/u3", merged[3].TurnIndex, merged[3].UUID)
	}
	// Cursor advances to end-of-file and to the new turn count.
	fi, _ := os.Stat(path)
	if incMeta.LastParsedByteOffset != fi.Size() {
		t.Errorf("cursor offset = %d, want EOF %d", incMeta.LastParsedByteOffset, fi.Size())
	}
	if incMeta.LastParsedTurnIndex != 4 || incMeta.TurnCount != 4 {
		t.Errorf("cursor turn index/count = %d/%d, want 4/4", incMeta.LastParsedTurnIndex, incMeta.TurnCount)
	}
}

// TestIncrementalDedupAgainstExisting verifies that re-appended historical
// records (same UUID) in the tail are deduplicated against the existing turn
// set, not re-emitted as duplicate turns.
func TestIncrementalDedupAgainstExisting(t *testing.T) {
	dir := t.TempDir()
	sid := "33333333-3333-3333-3333-333333333333"

	l0 := userRecordLine(t, sid, "d0", "alpha", "2026-05-01T10:00:00Z")
	path, _ := writeJSONL(t, dir, sid+".jsonl", l0)
	fullMeta, fullTurns, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}

	// Append a NEW record plus a re-appended copy of the already-indexed d0.
	l1 := userRecordLine(t, sid, "d1", "beta", "2026-05-01T10:01:00Z")
	dupD0 := userRecordLine(t, sid, "d0", "alpha", "2026-05-01T10:00:00Z")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(l1 + dupD0)
	f.Close()

	_, merged, newCount, err := indexSessionIncremental(path, sid, fullMeta, fullTurns, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if newCount != 1 {
		t.Fatalf("expected 1 new turn (d0 deduped), got %d", newCount)
	}
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged turns, got %d", len(merged))
	}
	uuids := map[string]int{}
	for _, tn := range merged {
		uuids[tn.UUID]++
	}
	if uuids["d0"] != 1 {
		t.Errorf("d0 appears %d times, want exactly 1 (dedup failed)", uuids["d0"])
	}
}

// TestIncrementalNewlinelessTailNotCommitted verifies the tail-line safety
// rule: a complete record written WITHOUT its trailing newline is still parsed
// (its turn is captured immediately) but the durable cursor is NOT advanced
// past it. A second pass after the newline arrives re-reads the line and dedups
// it by UUID, so it is never double-counted.
func TestIncrementalNewlinelessTailNotCommitted(t *testing.T) {
	dir := t.TempDir()
	sid := "44444444-4444-4444-4444-444444444444"

	l0 := userRecordLine(t, sid, "p0", "complete", "2026-05-01T10:00:00Z")
	path, _ := writeJSONL(t, dir, sid+".jsonl", l0)
	fullMeta, fullTurns, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}

	// Append a COMPLETE record but WITHOUT its trailing newline.
	l1 := userRecordLine(t, sid, "p1", "newlineless", "2026-05-01T10:01:00Z")
	noNewline := strings.TrimSuffix(l1, "\n")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(noNewline)
	f.Close()

	incMeta, merged, newCount, err := indexSessionIncremental(path, sid, fullMeta, fullTurns, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	// The complete record is captured immediately.
	if newCount != 1 {
		t.Fatalf("newlineless complete record should yield 1 new turn, got %d", newCount)
	}
	if len(merged) != 2 || merged[1].UUID != "p1" {
		t.Fatalf("expected p1 captured as turn 1, got %d turns", len(merged))
	}
	// But the durable cursor must NOT have advanced past the uncommitted line.
	if incMeta.LastParsedByteOffset != fullMeta.LastParsedByteOffset {
		t.Errorf("cursor advanced over newlineless line: %d (was %d)", incMeta.LastParsedByteOffset, fullMeta.LastParsedByteOffset)
	}

	// Complete the record with its newline; re-parse must NOT double-count p1.
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("\n")
	f.Close()

	incMeta2, merged2, newCount2, err := indexSessionIncremental(path, sid, incMeta, merged, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental (completion): %v", err)
	}
	if newCount2 != 0 {
		t.Fatalf("re-read of committed record should yield 0 new turns (deduped), got %d", newCount2)
	}
	if len(merged2) != 2 {
		t.Fatalf("turn count must stay 2 after re-read, got %d", len(merged2))
	}
	// Now the cursor advances to EOF since the line is newline-terminated.
	fi, _ := os.Stat(path)
	if incMeta2.LastParsedByteOffset != fi.Size() {
		t.Errorf("cursor = %d after completion, want EOF %d", incMeta2.LastParsedByteOffset, fi.Size())
	}
}

// TestIncrementalGenuinelyPartialTail verifies that a tail line which is NOT
// valid JSON (a true mid-write fragment) is never emitted and never advances
// the cursor; once the rest of the record arrives it parses correctly.
func TestIncrementalGenuinelyPartialTail(t *testing.T) {
	dir := t.TempDir()
	sid := "4a4a4a4a-4a4a-4a4a-4a4a-4a4a4a4a4a4a"

	l0 := userRecordLine(t, sid, "g0", "complete", "2026-05-01T10:00:00Z")
	path, _ := writeJSONL(t, dir, sid+".jsonl", l0)
	fullMeta, fullTurns, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}

	// Append the FIRST HALF of a record (invalid JSON fragment).
	l1 := userRecordLine(t, sid, "g1", "the second turn text here", "2026-05-01T10:01:00Z")
	half := l1[:len(l1)/2]
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(half)
	f.Close()

	incMeta, merged, newCount, err := indexSessionIncremental(path, sid, fullMeta, fullTurns, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental: %v", err)
	}
	if newCount != 0 {
		t.Fatalf("invalid-JSON fragment must yield 0 turns, got %d", newCount)
	}
	if incMeta.LastParsedByteOffset != fullMeta.LastParsedByteOffset {
		t.Errorf("cursor advanced over invalid fragment: %d (was %d)", incMeta.LastParsedByteOffset, fullMeta.LastParsedByteOffset)
	}

	// Write the remaining half plus newline; now the record is complete.
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(l1[len(l1)/2:])
	f.Close()

	_, merged2, newCount2, err := indexSessionIncremental(path, sid, incMeta, merged, 8192)
	if err != nil {
		t.Fatalf("indexSessionIncremental (completion): %v", err)
	}
	if newCount2 != 1 {
		t.Fatalf("completed record should yield 1 new turn, got %d", newCount2)
	}
	if len(merged2) != 2 || merged2[1].UUID != "g1" {
		t.Errorf("expected g1 as turn 1, got %d turns", len(merged2))
	}
}

// TestConvergenceNoDriftWhenIdle is the regression guard for the bug this fix
// targets: once a session is fully indexed (cursor seeded), an unchanged file
// must report NO drift so the reconcile loop converges to zero changes.
func TestConvergenceNoDriftWhenIdle(t *testing.T) {
	dir := t.TempDir()
	sid := "55555555-5555-5555-5555-555555555555"
	l0 := userRecordLine(t, sid, "c0", "only turn", "2026-05-01T10:00:00Z")
	path, src := writeJSONL(t, dir, sid+".jsonl", l0)

	meta, _, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}
	// The just-indexed file is unchanged; isDrift must say so across repeats.
	for i := 0; i < 3; i++ {
		if d := isDrift(meta, src); d.Drifted {
			t.Fatalf("idle cycle %d reported drift %+v; reconcile would never converge", i, d)
		}
	}
}

// TestFullReparseFallbackOnRewrite proves the safety net: when a file's head is
// rewritten (not a pure append), the drift classifier refuses the incremental
// path and a full re-parse rebuilds the turn set correctly.
func TestFullReparseFallbackOnRewrite(t *testing.T) {
	dir := t.TempDir()
	sid := "66666666-6666-6666-6666-666666666666"

	l0 := userRecordLine(t, sid, "r0", "original first", "2026-05-01T10:00:00Z")
	l1 := userRecordLine(t, sid, "r1", "original second", "2026-05-01T10:01:00Z")
	path, _ := writeJSONL(t, dir, sid+".jsonl", l0+l1)
	fullMeta, _, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("indexSession: %v", err)
	}

	// Rewrite the file entirely (compaction): different head, different UUIDs,
	// larger overall so size grows but the prefix hash will not match.
	n0 := userRecordLine(t, sid, "n0", "rewritten alpha", "2026-05-01T11:00:00Z")
	n1 := userRecordLine(t, sid, "n1", "rewritten beta", "2026-05-01T11:01:00Z")
	n2 := userRecordLine(t, sid, "n2", "rewritten gamma", "2026-05-01T11:02:00Z")
	if err := os.WriteFile(path, []byte(n0+n1+n2), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	d := isDrift(fullMeta, sourceFileInfo{Path: path, Size: fi.Size(), Mtime: fi.ModTime().Add(5 * time.Second)})
	if !d.Drifted {
		t.Fatal("expected drift after rewrite")
	}
	if d.IsAppendOnly {
		t.Fatal("rewrite misclassified as append-only — would corrupt the turn set")
	}

	// The fallback path (indexSession) rebuilds from scratch with the new turns.
	newMeta, newTurns, err := indexSession(path, sid, 8192)
	if err != nil {
		t.Fatalf("full re-parse: %v", err)
	}
	if len(newTurns) != 3 {
		t.Fatalf("expected 3 turns after rewrite, got %d", len(newTurns))
	}
	if newMeta.TurnCount != 3 {
		t.Errorf("meta turn count = %d, want 3", newMeta.TurnCount)
	}
	for i, want := range []string{"n0", "n1", "n2"} {
		if newTurns[i].UUID != want {
			t.Errorf("turn[%d] uuid = %q, want %q", i, newTurns[i].UUID, want)
		}
	}
}

// TestReconcileAppendOnlyEndToEnd drives the full reconcile cycle through the
// public provider methods to prove: (1) a grown source yields an UPDATE action
// flagged is_append_only=true, (2) ApplyPlan parses only the appended tail and
// merges it (correct total turn count), and (3) a subsequent unchanged cycle
// converges to zero create/update actions (the bug this fix targets).
func TestReconcileAppendOnlyEndToEnd(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()
	sid := "77777777-7777-7777-7777-777777777777"
	ctx := context.Background()

	// Cycle 1: initial index (one turn) → create.
	l0 := userRecordLine(t, sid, "e0", "first", "2026-05-01T10:00:00Z")
	path := filepath.Join(srcDir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(l0), 0o644); err != nil {
		t.Fatal(err)
	}
	writeObservatoryConfig(t, root, []string{srcDir})

	cfgAny, _ := p.LoadConfig(root)
	liveAny, _ := p.FetchLive(ctx, cfgAny)
	plan1, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan1.Summary.Creates != 1 {
		t.Fatalf("cycle1: expected 1 create, got %d", plan1.Summary.Creates)
	}
	if _, err := p.ApplyPlan(ctx, plan1); err != nil {
		t.Fatalf("cycle1 ApplyPlan: %v", err)
	}

	// Cycle 2: append two turns → update flagged append-only.
	l1 := userRecordLine(t, sid, "e1", "second", "2026-05-01T10:01:00Z")
	l2 := userRecordLine(t, sid, "e2", "third", "2026-05-01T10:02:00Z")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(l1 + l2)
	f.Close()

	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan2, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan2.Summary.Updates != 1 {
		t.Fatalf("cycle2: expected 1 update, got %d", plan2.Summary.Updates)
	}
	var sawAppendOnly bool
	for _, a := range plan2.Actions {
		if a.Action == reconcile.ActionUpdate {
			if ao, _ := a.Details["is_append_only"].(bool); ao {
				sawAppendOnly = true
			}
		}
	}
	if !sawAppendOnly {
		t.Fatal("cycle2: update action was not classified is_append_only")
	}
	if _, err := p.ApplyPlan(ctx, plan2); err != nil {
		t.Fatalf("cycle2 ApplyPlan: %v", err)
	}

	// Confirm the merged session has all 3 turns with correct indexes.
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	ls := liveAny.(*liveState)
	entry, ok := ls.Entries[sid]
	if !ok {
		t.Fatal("session missing from index after cycle2")
	}
	if entry.Meta.TurnCount != 3 {
		t.Fatalf("expected 3 turns after append, got %d", entry.Meta.TurnCount)
	}
	_, turns, _ := p.index.GetTurns(sid)
	if len(turns) != 3 {
		t.Fatalf("expected 3 indexed turns, got %d", len(turns))
	}
	for i, want := range []string{"e0", "e1", "e2"} {
		if turns[i].UUID != want || turns[i].TurnIndex != i {
			t.Errorf("turn[%d] = uuid %q idx %d, want %s/%d", i, turns[i].UUID, turns[i].TurnIndex, want, i)
		}
	}

	// Cycle 3: no changes → convergence. No create/update; only skip.
	cfgAny, _ = p.LoadConfig(root)
	liveAny, _ = p.FetchLive(ctx, cfgAny)
	plan3, _ := p.ComputePlan(cfgAny, liveAny, nil)
	if plan3.Summary.Creates != 0 || plan3.Summary.Updates != 0 || plan3.Summary.Deletes != 0 {
		t.Fatalf("cycle3 did not converge: creates=%d updates=%d deletes=%d skipped=%d",
			plan3.Summary.Creates, plan3.Summary.Updates, plan3.Summary.Deletes, plan3.Summary.Skipped)
	}
	if plan3.Summary.Skipped != 1 {
		t.Fatalf("cycle3: expected 1 skip, got %d", plan3.Summary.Skipped)
	}
}
