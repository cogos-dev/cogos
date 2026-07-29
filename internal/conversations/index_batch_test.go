// index_batch_test.go — regression tests for issue #494's remedies 1 and 3:
// batching the _meta.json write across a whole UpsertSessions/DeleteSessions
// call (instead of once per session), and making sessionMapsEqual cheap.
//
// See index_multiwriter_test.go for the pre-existing cross-process
// correctness tests (#449/#458) that this batching must not regress — the
// tests here focus specifically on the write-amplification cost issue #494
// measured (~10.5 GB / ~175 s for a 2,649-session rebuild) and its fix.
package conversations

import (
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countMetaWrites installs writeMetaFileLockedHook for the duration of fn and
// returns how many times writeMetaFileLocked actually ran. Serializes against
// other tests in the package that might also install the hook, since it's a
// single package-level var.
func countMetaWrites(t *testing.T, fn func()) int64 {
	t.Helper()
	metaWriteHookMu.Lock()
	defer metaWriteHookMu.Unlock()

	var n int64
	writeMetaFileLockedHook = func() { atomic.AddInt64(&n, 1) }
	defer func() { writeMetaFileLockedHook = nil }()

	fn()
	return n
}

// metaWriteHookMu serializes tests in this file that install the
// package-level writeMetaFileLockedHook, since t.Parallel tests in the same
// package would otherwise race on setting/clearing it.
var metaWriteHookMu sync.Mutex

func makeBatch(n int, prefix string, now time.Time) []SessionAndTurns {
	batch := make([]SessionAndTurns, 0, n)
	for i := 0; i < n; i++ {
		sid := prefix + "-" + strconv.Itoa(i)
		batch = append(batch, SessionAndTurns{
			Meta: SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
			Turns: []Turn{
				{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi " + sid},
			},
		})
	}
	return batch
}

// TestUpsertSessionsWritesMetaOnce is the direct regression test for remedy 1:
// a batch of N sessions must produce exactly ONE writeMetaFileLocked call
// (one full _meta.json read-merge-write round trip), not N.
func TestUpsertSessionsWritesMetaOnce(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	const n = 250
	now := time.Now()
	batch := makeBatch(n, "sess", now)

	writes := countMetaWrites(t, func() {
		if _, err := idx.UpsertSessions(batch); err != nil {
			t.Fatalf("UpsertSessions: %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("writeMetaFileLocked ran %d times for a %d-session batch, want exactly 1", writes, n)
	}

	// All N sessions must actually be present, on disk and in memory.
	raw, err := os.ReadFile(idx.metaPath())
	if err != nil {
		t.Fatalf("read _meta.json: %v", err)
	}
	var onDisk map[string]SessionMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("_meta.json not valid JSON: %v", err)
	}
	if len(onDisk) != n {
		t.Fatalf("_meta.json has %d sessions, want %d", len(onDisk), n)
	}
	for i := 0; i < n; i++ {
		sid := "sess-" + strconv.Itoa(i)
		if _, ok := idx.GetMeta(sid); !ok {
			t.Fatalf("in-memory index missing %s after UpsertSessions", sid)
		}
		if _, ok := idx.GetTurn(sid, 0); !ok {
			t.Fatalf("in-memory turns missing %s after UpsertSessions", sid)
		}
	}
}

// TestUpsertSessionsEmptyBatchIsNoop guards the len(batch)==0 fast path: no
// meta write, no error.
func TestUpsertSessionsEmptyBatchIsNoop(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	writes := countMetaWrites(t, func() {
		if outcomes, err := idx.UpsertSessions(nil); err != nil || outcomes != nil {
			t.Fatalf("UpsertSessions(nil): outcomes=%v err=%v, want (nil, nil)", outcomes, err)
		}
	})
	if writes != 0 {
		t.Fatalf("writeMetaFileLocked ran %d times for an empty batch, want 0", writes)
	}
}

// TestUpsertSessionsDedupesSameSessionID verifies that a batch containing the
// same SessionID more than once (a) does not self-deadlock acquiring that
// session's turnsLockPath twice without an intervening release, and (b)
// applies last-write-wins semantics, matching plain map-assignment.
func TestUpsertSessionsDedupesSameSessionID(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	now := time.Now()
	const sid = "dup-session"
	batch := []SessionAndTurns{
		{
			Meta:  SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
			Turns: []Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "first"}},
		},
		{
			Meta:  SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now, Title: "second"},
			Turns: []Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "second"}},
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := idx.UpsertSessions(batch)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpsertSessions with duplicate SessionID: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UpsertSessions with a duplicate SessionID in the batch did not return within 3s — likely self-deadlocked re-acquiring the same turnsLockPath")
	}

	meta, ok := idx.GetMeta(sid)
	if !ok {
		t.Fatalf("session %s missing after UpsertSessions", sid)
	}
	if meta.Title != "second" {
		t.Fatalf("meta.Title = %q, want %q (last entry for a duplicated SessionID should win)", meta.Title, "second")
	}
	turn, ok := idx.GetTurn(sid, 0)
	if !ok || turn.Text != "second" {
		t.Fatalf("turns for %s = %+v, ok=%v — want the second (last) entry's turns", sid, turn, ok)
	}
}

// TestDeleteSessionsWritesMetaOnce is the delete-path counterpart of
// TestUpsertSessionsWritesMetaOnce (remedy 1's "same treatment for the prune
// path via metaDelta.deletes").
func TestDeleteSessionsWritesMetaOnce(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	const n = 100
	now := time.Now()
	batch := makeBatch(n, "doomed", now)
	if _, err := idx.UpsertSessions(batch); err != nil {
		t.Fatalf("seed UpsertSessions: %v", err)
	}

	ids := make([]string, n)
	for i := range ids {
		ids[i] = "doomed-" + strconv.Itoa(i)
	}

	writes := countMetaWrites(t, func() {
		if _, err := idx.DeleteSessions(ids); err != nil {
			t.Fatalf("DeleteSessions: %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("writeMetaFileLocked ran %d times deleting a %d-session batch, want exactly 1", writes, n)
	}

	raw, err := os.ReadFile(idx.metaPath())
	if err != nil {
		t.Fatalf("read _meta.json: %v", err)
	}
	var onDisk map[string]SessionMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("_meta.json not valid JSON: %v", err)
	}
	if len(onDisk) != 0 {
		t.Fatalf("_meta.json has %d sessions after deleting all of them, want 0", len(onDisk))
	}
	for _, sid := range ids {
		if _, ok := idx.GetMeta(sid); ok {
			t.Fatalf("in-memory index still has %s after DeleteSessions", sid)
		}
		if _, statErr := os.Stat(idx.turnsPath(sid)); statErr == nil {
			t.Fatalf("turns file for %s still present after DeleteSessions", sid)
		}
	}
}

// TestUpsertSessionEquivalentToUpsertSessionsOfOne asserts UpsertSession (the
// N=1 caller-facing API) and DeleteSession behave identically to calling the
// batched form with a single-element slice — i.e. the thin-wrapper refactor
// changed nothing observable for single-session callers.
func TestUpsertSessionEquivalentToUpsertSessionsOfOne(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	now := time.Now()
	meta := SessionMeta{SessionID: "solo", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now}
	turns := []Turn{{UUID: "solo", SessionID: "solo", TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi"}}

	writes := countMetaWrites(t, func() {
		if err := idx.UpsertSession(meta, turns); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("UpsertSession triggered %d meta writes, want 1", writes)
	}
	if _, ok := idx.GetMeta("solo"); !ok {
		t.Fatal("UpsertSession did not persist session")
	}

	writes = countMetaWrites(t, func() {
		if err := idx.DeleteSession("solo"); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("DeleteSession triggered %d meta writes, want 1", writes)
	}
	if _, ok := idx.GetMeta("solo"); ok {
		t.Fatal("DeleteSession did not remove session")
	}
}

// ─── sessionMapsEqual (remedy 3) ────────────────────────────────────────────

// TestSessionMapsEqualMatchesOldSemantics drives sessionMapsEqual through the
// same cases the original per-value-json.Marshal implementation had to get
// right, asserting the cheap whole-map-marshal rewrite is behaviorally
// identical: same length+key+value equality, regardless of Go's randomized
// map iteration order (encoding/json sorts string map keys, which is exactly
// what makes the rewrite sound).
func TestSessionMapsEqualMatchesOldSemantics(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	metaA := SessionMeta{SessionID: "a", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now, Title: "Alpha"}
	metaB := SessionMeta{SessionID: "b", TurnCount: 2, FirstTurnAt: now, LastTurnAt: now, Title: "Beta"}
	metaBModified := metaB
	metaBModified.Title = "Beta (edited)"

	cases := []struct {
		name string
		a, b map[string]SessionMeta
		want bool
	}{
		{
			name: "both empty",
			a:    map[string]SessionMeta{},
			b:    map[string]SessionMeta{},
			want: true,
		},
		{
			name: "identical multi-entry maps",
			a:    map[string]SessionMeta{"a": metaA, "b": metaB},
			b:    map[string]SessionMeta{"a": metaA, "b": metaB},
			want: true,
		},
		{
			name: "different lengths",
			a:    map[string]SessionMeta{"a": metaA},
			b:    map[string]SessionMeta{"a": metaA, "b": metaB},
			want: false,
		},
		{
			name: "same length, different keys",
			a:    map[string]SessionMeta{"a": metaA},
			b:    map[string]SessionMeta{"c": metaA},
			want: false,
		},
		{
			name: "same keys, one value differs",
			a:    map[string]SessionMeta{"a": metaA, "b": metaB},
			b:    map[string]SessionMeta{"a": metaA, "b": metaBModified},
			want: false,
		},
		{
			name: "large map, all equal, built in different insertion order",
			a:    buildSessionMap(200, false),
			b:    buildSessionMap(200, true),
			want: true,
		},
		{
			name: "large map, one entry differs deep in the set",
			a:    buildSessionMap(200, false),
			b:    withOneChanged(buildSessionMap(200, false), "sess-150"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionMapsEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("sessionMapsEqual() = %v, want %v", got, tc.want)
			}
			// Symmetry: equality must not depend on argument order.
			if rev := sessionMapsEqual(tc.b, tc.a); rev != tc.want {
				t.Errorf("sessionMapsEqual(b, a) = %v, want %v (asymmetric result)", rev, tc.want)
			}
		})
	}
}

func buildSessionMap(n int, reverseInsertOrder bool) map[string]SessionMeta {
	now := time.Now().UTC().Truncate(time.Second)
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}
	if reverseInsertOrder {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	m := make(map[string]SessionMeta, n)
	for _, i := range ids {
		sid := "sess-" + strconv.Itoa(i)
		m[sid] = SessionMeta{SessionID: sid, TurnCount: i, FirstTurnAt: now, LastTurnAt: now}
	}
	return m
}

func withOneChanged(m map[string]SessionMeta, sid string) map[string]SessionMeta {
	out := make(map[string]SessionMeta, len(m))
	for k, v := range m {
		out[k] = v
	}
	changed := out[sid]
	changed.Title = "changed"
	out[sid] = changed
	return out
}
