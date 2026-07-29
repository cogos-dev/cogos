// index_batch_atomicity_test.go — regression tests for the atomicity gap
// cog-review found on PR #495's second pass: UpsertSessions/DeleteSessions
// originally acquired each session's per-sessionID lock and immediately did
// that session's disk I/O (write or remove its turns file) in the SAME loop
// iteration, before the batch's single shared writeMetaFileLocked call. If
// a later session's lock acquisition failed (most plausibly a peer process
// holding that one sessionID's lock past metaLockTimeout — exactly the
// cross-process contention this whole PR is about), the loop returned an
// error, but earlier sessions in the batch had already had their turns
// files written or removed on disk with NO corresponding _meta.json commit
// (since writeMetaFileLocked is only reached after the whole loop
// succeeds). For DeleteSessions this reintroduces the exact sessions/turns
// split-brain issues #449/#458 fixed for the single-session case (a session
// listed in _meta.json/idx.sessions whose turns file is already gone), now
// reachable across a multi-session batch.
//
// The fix (see UpsertSessions'/DeleteSessions' phase-1 doc comments in
// index.go): acquire EVERY per-sessionID lock in the batch BEFORE doing any
// disk I/O for any of them. A lock-acquisition failure then aborts before
// anything has been written or removed, for every session in the batch —
// not just the one whose lock actually failed.
package conversations

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// withShortMetaLockTimeout temporarily shrinks the package-level
// metaLockTimeout so a deliberate lock-contention test doesn't have to wait
// out the real 5s production timeout, then restores it. metaLockTimeout is
// a var (not a const) specifically for this purpose — see its doc comment.
func withShortMetaLockTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	prev := metaLockTimeout
	metaLockTimeout = d
	defer func() { metaLockTimeout = prev }()
	fn()
}

// TestUpsertSessionsAbortsBeforeAnyWriteOnLockContention seeds NO existing
// sessions, then calls UpsertSessions with a 3-session batch where one
// session's turnsLockPath is already held by a simulated peer. The whole
// call must fail, and — this is the actual regression check — NONE of the
// batch's turns files may exist on disk afterward and _meta.json must be
// untouched, regardless of where in sorted order the contended session
// falls relative to the others.
func TestUpsertSessionsAbortsBeforeAnyWriteOnLockContention(t *testing.T) {
	withShortMetaLockTimeout(t, 200*time.Millisecond, func() {
		dir := t.TempDir()
		idx, err := NewIndex(dir)
		if err != nil {
			t.Fatalf("NewIndex: %v", err)
		}

		now := time.Now()
		// Sorted order: contended-b, contended-b sorts between a and c so
		// both a lock acquired-before and a lock never-attempted case are
		// exercised in one batch.
		batch := []SessionAndTurns{
			{Meta: SessionMeta{SessionID: "sess-a", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
				Turns: []Turn{{UUID: "sess-a", SessionID: "sess-a", TurnIndex: 0, Role: "user", Timestamp: now, Text: "a"}}},
			{Meta: SessionMeta{SessionID: "sess-b-contended", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
				Turns: []Turn{{UUID: "sess-b-contended", SessionID: "sess-b-contended", TurnIndex: 0, Role: "user", Timestamp: now, Text: "b"}}},
			{Meta: SessionMeta{SessionID: "sess-c", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
				Turns: []Turn{{UUID: "sess-c", SessionID: "sess-c", TurnIndex: 0, Role: "user", Timestamp: now, Text: "c"}}},
		}

		// Simulate a peer process holding sess-b-contended's turns lock —
		// same technique TestReadsNotBlockedByPeerHoldingCrossProcessLock
		// uses: acquire it directly, bypassing the Index API.
		peer, err := filelock.Acquire(idx.turnsLockPath("sess-b-contended"), 2*time.Second)
		if err != nil {
			t.Fatalf("peer filelock.Acquire: %v", err)
		}
		defer peer.Release()

		if err := idx.UpsertSessions(batch); err == nil {
			t.Fatal("UpsertSessions succeeded despite a peer holding one session's lock — test setup is broken")
		}

		// The actual regression check: NEITHER sess-a NOR sess-c (both
		// unlocked, and sess-a sorts before the contended session) may have
		// had their turns file written. Phase 1 (acquire every lock first)
		// must abort before phase 2 (the writes) ever starts.
		for _, sid := range []string{"sess-a", "sess-c"} {
			if _, statErr := os.Stat(idx.turnsPath(sid)); statErr == nil {
				t.Errorf("turns file for %s was written to disk despite the batch failing on a different session's lock — phase-1/phase-2 ordering regression", sid)
			}
		}

		// _meta.json must not exist at all (nothing was ever committed).
		if _, statErr := os.Stat(idx.metaPath()); statErr == nil {
			raw, _ := os.ReadFile(idx.metaPath())
			t.Errorf("_meta.json was written despite the batch failing before writeMetaFileLocked ever ran: %s", raw)
		}

		// In-memory view must also be untouched.
		for _, sid := range []string{"sess-a", "sess-b-contended", "sess-c"} {
			if _, ok := idx.GetMeta(sid); ok {
				t.Errorf("in-memory index has %s despite the batch failing", sid)
			}
		}
	})
}

// TestDeleteSessionsAbortsBeforeAnyRemovalOnLockContention seeds 3 sessions
// successfully, then attempts to delete all 3 in one batch while a peer
// holds one of their locks. The batch must fail, and — the actual
// regression check — ALL 3 sessions' turns files and _meta.json entries
// must still be present afterward: no partial deletion, no split-brain.
func TestDeleteSessionsAbortsBeforeAnyRemovalOnLockContention(t *testing.T) {
	withShortMetaLockTimeout(t, 200*time.Millisecond, func() {
		dir := t.TempDir()
		idx, err := NewIndex(dir)
		if err != nil {
			t.Fatalf("NewIndex: %v", err)
		}

		now := time.Now()
		ids := []string{"sess-a", "sess-b-contended", "sess-c"}
		batch := make([]SessionAndTurns, 0, len(ids))
		for _, sid := range ids {
			batch = append(batch, SessionAndTurns{
				Meta:  SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
				Turns: []Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "seed " + sid}},
			})
		}
		if err := idx.UpsertSessions(batch); err != nil {
			t.Fatalf("seed UpsertSessions: %v", err)
		}

		peer, err := filelock.Acquire(idx.turnsLockPath("sess-b-contended"), 2*time.Second)
		if err != nil {
			t.Fatalf("peer filelock.Acquire: %v", err)
		}
		defer peer.Release()

		if err := idx.DeleteSessions(ids); err == nil {
			t.Fatal("DeleteSessions succeeded despite a peer holding one session's lock — test setup is broken")
		}

		// The actual regression check: sess-a and sess-c's turns files must
		// still be on disk — phase 1 (acquire every lock first) must abort
		// before phase 2 (the removals) ever starts.
		for _, sid := range []string{"sess-a", "sess-c"} {
			if _, statErr := os.Stat(idx.turnsPath(sid)); statErr != nil {
				t.Errorf("turns file for %s was removed despite the batch failing on a different session's lock — split-brain regression (issues #449/#458's bug class, reintroduced across a batch): %v", sid, statErr)
			}
			if _, ok := idx.GetMeta(sid); !ok {
				t.Errorf("in-memory index lost %s despite the batch failing", sid)
			}
		}

		// _meta.json must still list all three sessions — the delete never
		// committed at all.
		raw, err := os.ReadFile(idx.metaPath())
		if err != nil {
			t.Fatalf("read _meta.json: %v", err)
		}
		for _, sid := range ids {
			if !strings.Contains(string(raw), sid) {
				t.Errorf("_meta.json missing %s after a failed batch delete — got: %s", sid, raw)
			}
		}
	})
}
