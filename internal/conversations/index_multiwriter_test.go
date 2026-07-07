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
