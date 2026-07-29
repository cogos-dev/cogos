package vitalsretention

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMaybeCompact_ConcurrentCallsTriggerAtMostOneCompaction is the #497
// regression test: a burst of concurrent HandleBusEvent calls must claim the
// single-flight compaction slot at most once, closing the check-then-act
// race on lastCompactAt flagged non-blocking in #493's final review.
func TestMaybeCompact_ConcurrentCallsTriggerAtMostOneCompaction(t *testing.T) {
	withWorkspace(t, "node-a")

	var calls int32
	release := make(chan struct{})
	restore := SetCompactHookForTest(func(r *Recorder, base, nodeKey string, cfg Config) error {
		atomic.AddInt32(&calls, 1)
		<-release
		return nil
	})
	t.Cleanup(restore)

	r := &Recorder{}
	ts := time.Now().UTC()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.HandleBusEvent(ProprioBusID, snapshotBlock(ts, 1, 1, 1, 0, 0, 0))
		}()
	}
	wg.Wait()
	close(release)

	// Give the single (if any) compaction goroutine time to finish and
	// record its result before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want exactly 1 compaction call across 20 concurrent HandleBusEvent calls, got %d", got)
	}
}

// TestHandleBusEvent_DoesNotBlockOnSlowCompaction is the #497 regression
// test for the tick-path latency requirement: HandleBusEvent (the bus
// handler dispatched synchronously inside BusSessionManager.AppendEvent,
// fired by the autonomic ticker) must return promptly even when a
// compaction pass is slow, because compaction now runs on its own
// goroutine rather than inline on the tick path.
func TestHandleBusEvent_DoesNotBlockOnSlowCompaction(t *testing.T) {
	withWorkspace(t, "node-a")

	started := make(chan struct{})
	release := make(chan struct{})
	restore := SetCompactHookForTest(func(r *Recorder, base, nodeKey string, cfg Config) error {
		close(started)
		<-release // never released during this test's HandleBusEvent call
		return nil
	})
	t.Cleanup(func() {
		restore()
	})

	r := &Recorder{}
	ts := time.Now().UTC()

	start := time.Now()
	r.HandleBusEvent(ProprioBusID, snapshotBlock(ts, 1, 1, 1, 0, 0, 0))
	elapsed := time.Since(start)

	const budget = 100 * time.Millisecond
	if elapsed > budget {
		t.Fatalf("HandleBusEvent took %v, want < %v — it must not wait on compaction I/O", elapsed, budget)
	}

	select {
	case <-started:
		// Confirms the slow compaction actually ran (in the background),
		// rather than the fast return being an artifact of maybeCompact
		// never firing at all.
	case <-time.After(2 * time.Second):
		t.Fatal("compaction goroutine never started")
	}

	close(release)
}
