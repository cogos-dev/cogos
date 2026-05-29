// index_race_test.go — concurrency regression test for the in-memory Index.
//
// Reproduces the crash-loop root cause: the reconcile daemon and the autonomic
// ticker both drive Provider.FetchLive concurrently, which calls Index.Load
// (a map writer), while MCP tool handlers concurrently read the index via
// Search / ListSessions / GetTurn. Before the mutex was added this raced and
// the kernel died with "fatal error: concurrent map writes".
//
// Run with: go test -race ./internal/conversations/...
package conversations

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestIndexConcurrentLoadAndRead exercises concurrent Load (write path) and
// readers against a single Index, mirroring the two-daemon FetchLive pattern.
// It must pass under -race.
func TestIndexConcurrentLoadAndRead(t *testing.T) {
	t.Parallel()

	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	// Seed some sessions so Load has files to read back and readers have data.
	const nSessions = 8
	now := time.Now()
	for i := 0; i < nSessions; i++ {
		sid := "session-" + strconv.Itoa(i)
		meta := SessionMeta{
			SessionID:   sid,
			TurnCount:   2,
			FirstTurnAt: now,
			LastTurnAt:  now,
		}
		turns := []Turn{
			{UUID: sid + "-0", SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "hello world " + sid},
			{UUID: sid + "-1", SessionID: sid, TurnIndex: 1, Role: "assistant", Timestamp: now, Text: "reply " + sid},
		}
		if err := idx.UpsertSession(meta, turns); err != nil {
			t.Fatalf("seed UpsertSession: %v", err)
		}
	}

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup

	// Writers: concurrent Load (the FetchLive path from daemon + ticker).
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := idx.Load(); err != nil {
					t.Errorf("Load: %v", err)
					return
				}
			}
		}()
	}

	// Writers: concurrent UpsertSession (the ApplyPlan path).
	for g := 0; g < goroutines/2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				sid := "writer-" + strconv.Itoa(g) + "-" + strconv.Itoa(i%4)
				_ = idx.UpsertSession(
					SessionMeta{SessionID: sid, TurnCount: 1, FirstTurnAt: now, LastTurnAt: now},
					[]Turn{{UUID: sid, SessionID: sid, TurnIndex: 0, Role: "user", Timestamp: now, Text: "x " + sid}},
				)
			}
		}(g)
	}

	// Readers: Search / ListSessions / GetTurn (the MCP tool path).
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var zero time.Time
			for i := 0; i < iterations; i++ {
				_ = idx.Search("hello", zero, zero, "", "", 0)
				_ = idx.ListSessions(zero, zero, "")
				_, _ = idx.GetTurn("session-0", 0)
				_, _ = idx.GetMeta("session-1")
			}
		}()
	}

	wg.Wait()
}
