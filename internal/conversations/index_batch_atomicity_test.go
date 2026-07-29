// index_batch_atomicity_test.go — regression tests for two related findings
// cog-review raised across PR #495's second and third passes on
// UpsertSessions/DeleteSessions:
//
//   - Second pass: an earlier version acquired each session's per-sessionID
//     lock and immediately did that session's disk I/O in the SAME loop
//     iteration, before the batch's single shared writeMetaFileLocked call.
//     If a LATER session's lock acquisition failed (most plausibly a peer
//     process holding that one sessionID's lock past metaLockTimeout —
//     exactly the cross-process contention this whole PR is about), EARLIER
//     sessions in the batch had already had their turns files written or
//     removed on disk with NO corresponding _meta.json commit. For
//     DeleteSessions this reintroduced the exact sessions/turns split-brain
//     issues #449/#458 fixed for the single-session case, now reachable
//     across a batch.
//   - Third pass: the fix for the above (acquire every lock FIRST, then do
//     all the I/O) closed that gap by aborting the WHOLE batch on any single
//     lock-acquisition failure — but that coupled every OTHER, uncontended
//     session in the batch to one contended session's bad luck, a real
//     fault-isolation regression for the CC (source_dirs) ApplyPlan path
//     specifically (which, pre-batching, applied each action independently).
//
// The final design (see UpsertSessions'/DeleteSessions' doc comments in
// index.go, "Per-session fault isolation"): every session's lock
// acquisition and disk I/O are attempted INDEPENDENTLY; a failure for one
// session excludes only that session — recorded in its own
// SessionOpOutcome — while every other, unaffected session in the batch
// still commits via the one shared writeMetaFileLocked call. A session is
// never included in that shared commit unless its own lock was held across
// its own turns-file write/removal, which is what closes the second pass's
// split-brain gap without reintroducing all-or-nothing coupling.
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

// outcomeFor returns the SessionOpOutcome for sid, failing the test if sid
// is missing from outcomes entirely (every requested session must always
// get an outcome, success or failure).
func outcomeFor(t *testing.T, outcomes []SessionOpOutcome, sid string) SessionOpOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.SessionID == sid {
			return o
		}
	}
	t.Fatalf("no SessionOpOutcome for %s — every requested session must get one", sid)
	return SessionOpOutcome{}
}

// TestUpsertSessionsIsolatesLockContentionToContendedSession seeds no
// existing sessions, then calls UpsertSessions with a 3-session batch where
// one session's turnsLockPath is already held by a simulated peer. The
// contended session must fail (and must NOT be written to disk or memory),
// while the other two, uncontended sessions must succeed independently —
// this is the fault-isolation fix from cog-review's third pass.
func TestUpsertSessionsIsolatesLockContentionToContendedSession(t *testing.T) {
	withShortMetaLockTimeout(t, 200*time.Millisecond, func() {
		dir := t.TempDir()
		idx, err := NewIndex(dir)
		if err != nil {
			t.Fatalf("NewIndex: %v", err)
		}

		now := time.Now()
		// Sorted order places sess-b-contended between a and c, so both a
		// "processed earlier" and a "processed later" relative position are
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

		outcomes, err := idx.UpsertSessions(batch)
		if err == nil {
			t.Fatal("UpsertSessions returned a nil aggregate error despite one session's lock being contended")
		}
		if len(outcomes) != 3 {
			t.Fatalf("got %d outcomes, want 3 (one per requested session)", len(outcomes))
		}

		// The contended session must fail and must NOT exist on disk or in
		// memory.
		bOutcome := outcomeFor(t, outcomes, "sess-b-contended")
		if bOutcome.Err == nil {
			t.Error("sess-b-contended's outcome has no error, want a lock-acquisition failure")
		}
		if _, statErr := os.Stat(idx.turnsPath("sess-b-contended")); statErr == nil {
			t.Error("sess-b-contended's turns file was written despite its lock being contended")
		}
		if _, ok := idx.GetMeta("sess-b-contended"); ok {
			t.Error("in-memory index has sess-b-contended despite its lock being contended")
		}

		// The two UNCONTENDED sessions must succeed independently — this is
		// the actual fault-isolation regression check.
		for _, sid := range []string{"sess-a", "sess-c"} {
			o := outcomeFor(t, outcomes, sid)
			if o.Err != nil {
				t.Errorf("%s failed (%v) despite its own lock never being contended — collateral failure from an unrelated session", sid, o.Err)
			}
			if _, statErr := os.Stat(idx.turnsPath(sid)); statErr != nil {
				t.Errorf("%s's turns file missing from disk despite succeeding: %v", sid, statErr)
			}
			if _, ok := idx.GetMeta(sid); !ok {
				t.Errorf("%s missing from in-memory index despite succeeding", sid)
			}
		}

		// _meta.json must list sess-a and sess-c but not sess-b-contended.
		raw, err := os.ReadFile(idx.metaPath())
		if err != nil {
			t.Fatalf("read _meta.json: %v", err)
		}
		for _, sid := range []string{"sess-a", "sess-c"} {
			if !strings.Contains(string(raw), sid) {
				t.Errorf("_meta.json missing %s despite it succeeding — got: %s", sid, raw)
			}
		}
		if strings.Contains(string(raw), "sess-b-contended") {
			t.Errorf("_meta.json contains sess-b-contended despite its lock acquisition failing — got: %s", raw)
		}
	})
}

// TestDeleteSessionsIsolatesLockContentionToContendedSession seeds 3
// sessions successfully, then attempts to delete all 3 in one batch while a
// peer holds one of their locks. The contended session must survive
// entirely (turns file AND meta entry both still present — no split-brain),
// while the other two, uncontended sessions must be deleted independently.
func TestDeleteSessionsIsolatesLockContentionToContendedSession(t *testing.T) {
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
		if _, err := idx.UpsertSessions(batch); err != nil {
			t.Fatalf("seed UpsertSessions: %v", err)
		}

		peer, err := filelock.Acquire(idx.turnsLockPath("sess-b-contended"), 2*time.Second)
		if err != nil {
			t.Fatalf("peer filelock.Acquire: %v", err)
		}
		defer peer.Release()

		outcomes, err := idx.DeleteSessions(ids)
		if err == nil {
			t.Fatal("DeleteSessions returned a nil aggregate error despite one session's lock being contended")
		}
		if len(outcomes) != 3 {
			t.Fatalf("got %d outcomes, want 3 (one per requested session)", len(outcomes))
		}

		// The contended session must survive completely intact: this is the
		// actual split-brain regression check (issues #449/#458's bug
		// class) — turns file AND meta entry must agree, both present.
		bOutcome := outcomeFor(t, outcomes, "sess-b-contended")
		if bOutcome.Err == nil {
			t.Error("sess-b-contended's outcome has no error, want a lock-acquisition failure")
		}
		if _, statErr := os.Stat(idx.turnsPath("sess-b-contended")); statErr != nil {
			t.Errorf("sess-b-contended's turns file was removed despite its lock being contended (split-brain risk): %v", statErr)
		}
		if _, ok := idx.GetMeta("sess-b-contended"); !ok {
			t.Error("sess-b-contended missing from in-memory index despite its lock being contended (should survive untouched)")
		}

		// The two UNCONTENDED sessions must be deleted independently.
		for _, sid := range []string{"sess-a", "sess-c"} {
			o := outcomeFor(t, outcomes, sid)
			if o.Err != nil {
				t.Errorf("%s failed to delete (%v) despite its own lock never being contended — collateral failure from an unrelated session", sid, o.Err)
			}
			if _, statErr := os.Stat(idx.turnsPath(sid)); statErr == nil {
				t.Errorf("%s's turns file still present despite succeeding", sid)
			}
			if _, ok := idx.GetMeta(sid); ok {
				t.Errorf("%s still present in in-memory index despite succeeding", sid)
			}
		}

		// _meta.json must still list sess-b-contended but not the other two.
		raw, err := os.ReadFile(idx.metaPath())
		if err != nil {
			t.Fatalf("read _meta.json: %v", err)
		}
		if !strings.Contains(string(raw), "sess-b-contended") {
			t.Errorf("_meta.json missing sess-b-contended despite its delete failing — got: %s", raw)
		}
		for _, sid := range []string{"sess-a", "sess-c"} {
			if strings.Contains(string(raw), sid) {
				t.Errorf("_meta.json still contains %s despite it being successfully deleted — got: %s", sid, raw)
			}
		}
	})
}
