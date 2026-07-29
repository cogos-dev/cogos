// index_batch_shared_deadline_test.go — regression test for cog-review's
// fourth-pass finding on PR #495: UpsertSessions'/DeleteSessions' phase-1
// per-session-independent lock acquisition (fixed in the third pass to
// restore fault isolation) removed the early-exit an all-or-nothing loop
// would have had, so a batch with K simultaneously-contended sessionIDs
// could each independently poll out a full metaLockTimeout, compounding to
// K x metaLockTimeout for one batch call — undermining metaLockTimeout's
// documented "a wedged peer doesn't hang the caller indefinitely" bound,
// and (since remedy 4's applyMu is held for ApplyPlan's whole duration)
// stalling every other reconcile activity for the provider for the same
// window.
//
// The fix (acquireSessionLocks in index.go): a single deadline computed
// once and shared across every session's filelock.Acquire call bounds the
// WHOLE phase-1 loop's wall-clock time to one metaLockTimeout, regardless
// of how many sessions in the batch are simultaneously contended.
package conversations

import (
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// TestAcquireSessionLocksBoundsTotalWaitToOneSharedDeadline seeds a batch
// where THREE sessions are simultaneously peer-locked. Before the fix, each
// would independently poll out its own full metaLockTimeout, so total wall
// time would approach 3x the timeout. After the fix, total wall time must
// stay close to ONE timeout, regardless of how many sessions are contended.
func TestAcquireSessionLocksBoundsTotalWaitToOneSharedDeadline(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	const timeout = 300 * time.Millisecond
	prev := metaLockTimeout
	metaLockTimeout = timeout
	defer func() { metaLockTimeout = prev }()

	contendedIDs := []string{"sess-contended-a", "sess-contended-b", "sess-contended-c"}
	var peers []*filelock.FileLock
	for _, sid := range contendedIDs {
		peer, err := filelock.Acquire(idx.turnsLockPath(sid), 2*time.Second)
		if err != nil {
			t.Fatalf("peer filelock.Acquire(%s): %v", sid, err)
		}
		peers = append(peers, peer)
	}
	defer func() {
		for _, p := range peers {
			p.Release()
		}
	}()

	// The uncontended session is placed LAST, after the three contended
	// ones have already consumed most of the shared deadline — this is
	// what actually exercises "an uncontended lock still succeeds via its
	// one free tryLock attempt even once the nominal budget has run out"
	// (see acquireSessionLocks' doc comment).
	ids := append(append([]string{}, contendedIDs...), "sess-uncontended")

	start := time.Now()
	held, failed := idx.acquireSessionLocks(ids)
	elapsed := time.Since(start)
	defer func() {
		for _, h := range held {
			h.lock.Release()
		}
	}()

	// The actual regression check: total wall time for THREE contended
	// sessions must stay close to ONE metaLockTimeout, not compound to
	// ~3x it. A generous margin (1.5x the timeout) comfortably separates
	// "bounded to one shared deadline" from "one full timeout per
	// contended session" (which would be ~3x here).
	maxAllowed := timeout + timeout/2
	if elapsed > maxAllowed {
		t.Fatalf("acquireSessionLocks with 3 simultaneously-contended sessions took %v, want <= %v (one shared metaLockTimeout budget, not one per contended session — got what looks like the pre-fix per-session-timeout compounding)", elapsed, maxAllowed)
	}

	// All three contended sessions must still be reported as failed (the
	// shared deadline bounds total WAIT, it doesn't magically grant locks).
	for _, sid := range contendedIDs {
		if _, ok := failed[sid]; !ok {
			t.Errorf("expected %s to fail (peer-held) but it does not appear in failed", sid)
		}
	}
	// The uncontended session must still succeed — the shared budget is not
	// so aggressively front-loaded that an easy, uncontended lock gets
	// starved when it's tried after contended ones.
	foundUncontended := false
	for _, h := range held {
		if h.sid == "sess-uncontended" {
			foundUncontended = true
		}
	}
	if !foundUncontended {
		t.Error("sess-uncontended (never peer-locked) failed to acquire its lock")
	}
}
