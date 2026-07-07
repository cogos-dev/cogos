// index_multiwriter_test.go — cross-process write-race regression test for
// _meta.json (issue #449).
//
// index_race_test.go covers concurrency within a single *Index (in-memory map
// races between goroutines sharing one process's view). This file covers the
// distinct failure mode reported in #449: two independent *Index instances —
// standing in for a CLI-invoked "cog reconcile conversations" process and the
// daemon's own reconcile cycle — pointed at the same on-disk projDir, each
// doing its own read-modify-write of _meta.json with no shared in-memory
// state. Before the filelock+merge fix, whichever instance's
// writeMetaFileLocked ran last would marshal only its own idx.sessions and
// silently drop whatever the other instance had already added — the exact
// "last writer wins" bug the issue describes.
package conversations

import (
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestCrossProcessUpsertRace drives two separate *Index instances against the
// same projDir concurrently, each upserting its own disjoint set of sessions,
// and asserts that every session from both instances survives on disk in a
// well-formed _meta.json — i.e. no interleaving of the two writers silently
// drops an entry or corrupts the file.
func TestCrossProcessUpsertRace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	idxA, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex A: %v", err)
	}
	idxB, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex B: %v", err)
	}

	const perWriter = 100
	now := time.Now()

	writer := func(idx *Index, tag string, wg *sync.WaitGroup, errs chan<- error) {
		defer wg.Done()
		for i := 0; i < perWriter; i++ {
			sid := tag + "-" + strconv.Itoa(i)
			meta := SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now}
			turns := []Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi " + sid}}
			if err := idx.UpsertSession(meta, turns); err != nil {
				errs <- err
				return
			}
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go writer(idxA, "proc-a", &wg, errs)
	go writer(idxB, "proc-b", &wg, errs)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer error: %v", err)
	}

	// The on-disk _meta.json must be valid JSON and contain every session
	// from both writers — nothing dropped, nothing torn.
	raw, err := os.ReadFile(idxA.metaPath())
	if err != nil {
		t.Fatalf("read _meta.json: %v", err)
	}
	var onDisk map[string]SessionMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("_meta.json is not valid JSON after concurrent writers: %v\ncontent: %s", err, raw)
	}

	want := 2 * perWriter
	if len(onDisk) != want {
		missingA, missingB := 0, 0
		for i := 0; i < perWriter; i++ {
			if _, ok := onDisk["proc-a-"+strconv.Itoa(i)]; !ok {
				missingA++
			}
			if _, ok := onDisk["proc-b-"+strconv.Itoa(i)]; !ok {
				missingB++
			}
		}
		t.Fatalf("_meta.json has %d sessions, want %d (missing from proc-a: %d, missing from proc-b: %d) — writers clobbered each other",
			len(onDisk), want, missingA, missingB)
	}
}

// TestCrossProcessDeleteSurvivesConcurrentUpsert verifies that a delete from
// one Index instance is not resurrected by a concurrent upsert wave from a
// second instance touching unrelated session keys — the pendingDeletes /
// merge-then-delete ordering in writeMetaFileLocked.
func TestCrossProcessDeleteSurvivesConcurrentUpsert(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	idxA, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex A: %v", err)
	}
	idxB, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex B: %v", err)
	}

	now := time.Now()
	doomed := SessionMeta{SessionID: "doomed", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now}
	if err := idxA.UpsertSession(doomed, []Turn{{UUID: "doomed", SessionID: "doomed", TurnIndex: 0, Role: "user", Timestamp: now, Text: "x"}}); err != nil {
		t.Fatalf("seed UpsertSession: %v", err)
	}
	// idxB has not yet Loaded, so it doesn't know about "doomed" — mirrors a
	// freshly-started peer process.
	if _, err := idxB.LoadIfChanged(); err != nil {
		t.Fatalf("idxB LoadIfChanged: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := idxA.DeleteSession("doomed"); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			sid := "other-" + strconv.Itoa(i)
			meta := SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now}
			if err := idxB.UpsertSession(meta, []Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "y"}}); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer error: %v", err)
	}

	raw, err := os.ReadFile(idxA.metaPath())
	if err != nil {
		t.Fatalf("read _meta.json: %v", err)
	}
	var onDisk map[string]SessionMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("_meta.json is not valid JSON: %v", err)
	}
	if _, stillThere := onDisk["doomed"]; stillThere {
		t.Fatalf("deleted session %q was resurrected by a concurrent peer upsert", "doomed")
	}
	if len(onDisk) != 20 {
		t.Fatalf("_meta.json has %d sessions, want 20 (the concurrent 'other-*' upserts)", len(onDisk))
	}
}

// TestPeerWriteDoesNotDesyncSessionsAndTurns is the regression test for the
// finding cog-review raised on an earlier version of this fix (PR #458):
// writeMetaFileLocked must never let idx.sessions gain a peer process's
// session key without idx.turns having that session's turns loaded, because
// idx.sessions backs ListSessions/GetMeta (used by cog_list_conversations)
// while idx.turns backs GetTurn/Search (used by cog_get_turn/cog_search) —
// a split between them means a session is listed but its content is
// unreachable.
//
// Drives two Index instances against the same projDir: idxB writes a session
// idxA never loads via Load/LoadIfChanged. idxA then performs its own,
// unrelated UpsertSession (writeMetaFileLocked observes idxB's session on
// disk while merging). The invariant checked is: for every session_id idxA's
// in-memory idx.sessions knows about, idx.turns must also know about it —
// i.e. UpsertSession must never introduce a sessions/turns split-brain as a
// side effect of coordinating with a peer's write.
func TestPeerWriteDoesNotDesyncSessionsAndTurns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	idxA, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex A: %v", err)
	}
	idxB, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex B: %v", err)
	}

	now := time.Now()

	// idxA establishes a baseline session so it has a non-empty in-memory
	// view before the peer write happens.
	if err := idxA.UpsertSession(
		SessionMeta{SessionID: "a-baseline", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
		[]Turn{{UUID: "a-baseline", SessionID: "a-baseline", TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi"}},
	); err != nil {
		t.Fatalf("idxA baseline UpsertSession: %v", err)
	}

	// idxB, standing in for a separate CLI process, writes a session idxA
	// never observes via Load/LoadIfChanged.
	if err := idxB.UpsertSession(
		SessionMeta{SessionID: "b-peer-only", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
		[]Turn{{UUID: "b-peer-only", SessionID: "b-peer-only", TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi from B"}},
	); err != nil {
		t.Fatalf("idxB UpsertSession: %v", err)
	}

	// idxA now does its own, unrelated write. Its writeMetaFileLocked call
	// will read _meta.json off disk (which now contains "b-peer-only") while
	// merging in its own delta.
	if err := idxA.UpsertSession(
		SessionMeta{SessionID: "a-second", TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
		[]Turn{{UUID: "a-second", SessionID: "a-second", TurnIndex: 0, Role: "user", Timestamp: now, Text: "hi again"}},
	); err != nil {
		t.Fatalf("idxA second UpsertSession: %v", err)
	}

	// Invariant: every session_id in idxA's in-memory idx.sessions must also
	// be present in idx.turns. "b-peer-only" must NOT appear in idxA's
	// idx.sessions unless idxA has actually loaded its turns (which it
	// hasn't, since it never called Load/LoadIfChanged after idxB's write).
	// Scoped explicitly (not deferred) so the RLock is released before the
	// LoadIfChanged call further down, which takes its own write lock.
	func() {
		idxA.mu.RLock()
		defer idxA.mu.RUnlock()
		for sid := range idxA.sessions {
			if _, ok := idxA.turns[sid]; !ok {
				t.Fatalf("idx.sessions contains %q but idx.turns does not — sessions/turns split-brain (peer-write desync)", sid)
			}
		}
		if _, ok := idxA.sessions["b-peer-only"]; ok {
			t.Fatalf("idxA.sessions unexpectedly contains peer-only session %q without ever loading its turns", "b-peer-only")
		}
	}()

	// Meanwhile the on-disk file must still contain all three sessions —
	// idxA's write must not have dropped idxB's peer session even though
	// idxA didn't adopt it into memory.
	raw, err := os.ReadFile(idxA.metaPath())
	if err != nil {
		t.Fatalf("read _meta.json: %v", err)
	}
	var onDisk map[string]SessionMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("_meta.json is not valid JSON: %v", err)
	}
	for _, want := range []string{"a-baseline", "b-peer-only", "a-second"} {
		if _, ok := onDisk[want]; !ok {
			t.Fatalf("_meta.json missing expected session %q after idxA's second write; got keys: %v", want, onDisk)
		}
	}

	// And idxA's own view is self-consistent when it does reload: turns for
	// everything idxA has ever upserted are still resolvable via GetTurn.
	if _, ok := idxA.GetTurn("a-baseline", 0); !ok {
		t.Fatalf("idxA lost its own baseline turn after the peer-write interleaving")
	}
	if _, ok := idxA.GetTurn("a-second", 0); !ok {
		t.Fatalf("idxA lost its own second turn after the peer-write interleaving")
	}

	// A subsequent LoadIfChanged must detect the peer's on-disk addition
	// (content differs from idxA's own lastMetaHash baseline) and perform a
	// full reload that backfills idx.turns for "b-peer-only" too.
	reloaded, err := idxA.LoadIfChanged()
	if err != nil {
		t.Fatalf("LoadIfChanged: %v", err)
	}
	if !reloaded {
		t.Fatalf("LoadIfChanged reported no change, but the on-disk file contains idxB's peer write that idxA never adopted")
	}
	if _, ok := idxA.GetMeta("b-peer-only"); !ok {
		t.Fatalf("after LoadIfChanged, idxA still doesn't know about peer session %q", "b-peer-only")
	}
	if _, ok := idxA.GetTurn("b-peer-only", 0); !ok {
		t.Fatalf("after LoadIfChanged, idxA knows about peer session %q via sessions but still can't resolve its turns", "b-peer-only")
	}
}
