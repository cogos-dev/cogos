// state_lock_test.go — cross-process write-race regression test for the
// generic reconcile framework's state file (pkg/substrate/reconcile/state.go),
// the sibling of issue #449's _meta.json race flagged by cog-review on PR
// #458 (fifth review pass, head 9d0aa2e): LoadState/WriteState back every
// resource-provider type via the identical
// LoadState → ComputePlan → ApplyPlan → BuildState → WriteState cycle run by
// both `cogos reconcile <type>` and the daemon's own reconcile loop, with no
// cross-process coordination prior to this fix.
//
// Unlike the conversations index fix, BuildState fully re-derives Resources
// from live state each cycle rather than applying a delta on top of a fresh
// disk read, so there is no sound delta-merge here — the fix is
// AcquireStateLock/Release serializing the whole cycle, verified below by
// simulating N concurrent "reconcile cycles" (each acquiring the lock,
// loading state, incrementing a resource's revision, writing state, then
// releasing) and asserting no cycle's update is silently lost to a
// last-write-wins race.
package reconcile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// TestAcquireStateLock_SerializesConcurrentCycles drives N goroutines, each
// standing in for an independent process's full reconcile cycle
// (AcquireStateLock → LoadState → mutate → WriteState → Release) against the
// same resourceType, and asserts every cycle's contribution survives on disk.
// Without the lock, two cycles' WriteState calls can interleave: cycle B
// reads state before cycle A's WriteState lands, so B's WriteState (built
// from the pre-A snapshot) discards A's Serial bump and Resources entry — a
// classic last-writer-wins race. With the lock serializing LoadState through
// WriteState, no cycle can observe a stale read.
func TestAcquireStateLock_SerializesConcurrentCycles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const resourceType = "test-resource"
	const numCycles = 50

	var wg sync.WaitGroup
	errs := make(chan error, numCycles)
	wg.Add(numCycles)
	for i := 0; i < numCycles; i++ {
		i := i
		go func() {
			defer wg.Done()
			lock, err := AcquireStateLock(root, resourceType)
			if err != nil {
				errs <- err
				return
			}
			defer lock.Release()

			state, err := LoadState(root, resourceType)
			if err != nil {
				errs <- err
				return
			}
			if state == nil {
				state = NewState(resourceType)
				state.Serial = 0 // WriteState increments; NewState's Serial isn't persisted yet
			}
			state.Resources = append(state.Resources, Resource{
				Address: "test." + strconv.Itoa(i),
				Type:    "test",
				Mode:    ModeManaged,
				Name:    "resource-" + strconv.Itoa(i),
			})
			if err := WriteState(root, resourceType, state); err != nil {
				errs <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("cycle error: %v", err)
	}

	final, err := LoadState(root, resourceType)
	if err != nil {
		t.Fatalf("LoadState final: %v", err)
	}
	if final == nil {
		t.Fatal("final state is nil")
	}
	if final.Serial != numCycles {
		t.Errorf("Serial = %d, want %d — a concurrent cycle's WriteState was silently lost", final.Serial, numCycles)
	}
	if len(final.Resources) != numCycles {
		missing := 0
		seen := map[string]bool{}
		for _, r := range final.Resources {
			seen[r.Address] = true
		}
		for i := 0; i < numCycles; i++ {
			if !seen["test."+strconv.Itoa(i)] {
				missing++
			}
		}
		t.Fatalf("Resources count = %d, want %d (missing %d entries) — cycles clobbered each other", len(final.Resources), numCycles, missing)
	}

	// The on-disk file must be well-formed JSON — no torn writes.
	raw, err := os.ReadFile(StatePath(root, resourceType))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var onDisk State
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("state file is not valid JSON after concurrent cycles: %v\ncontent: %s", err, raw)
	}
}

// TestStateLockPath_IsSiblingOfStateFile verifies the lock file lives beside
// the state file, not on top of it — the lock's own open/lock/unlock
// lifecycle must never touch the JSON content file LoadState reads (mirrors
// metaLockPath's contract in internal/conversations/index.go).
func TestStateLockPath_IsSiblingOfStateFile(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	statePath := StatePath(root, "discord")
	lockPath := StateLockPath(root, "discord")
	if lockPath == statePath {
		t.Fatalf("StateLockPath must differ from StatePath, both are %q", statePath)
	}
	if lockPath != statePath+".lock" {
		t.Errorf("StateLockPath = %q, want %q", lockPath, statePath+".lock")
	}
}

// TestAcquireStateLock_ContendsOnSameResourceType verifies a second
// AcquireStateLock call for the same resourceType genuinely blocks while a
// peer holds the lock (simulated by acquiring StateLockPath directly, the
// way a separate OS process would), and that the block is scoped to that
// resourceType — a concurrent AcquireStateLock for a different resourceType
// is unaffected. Mirrors the contention-shape convention in
// TestReadsNotBlockedByPeerHoldingCrossProcessLock
// (internal/conversations/index_multiwriter_test.go): probe with a budget
// well under StateLockTimeout rather than waiting out the full 5s timeout.
func TestAcquireStateLock_ContendsOnSameResourceType(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lockPath := StateLockPath(root, "held-resource")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	peerLock, err := filelock.Acquire(lockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("peer filelock.Acquire: %v", err)
	}
	defer peerLock.Release()

	contended := make(chan error, 1)
	go func() {
		lock, err := AcquireStateLock(root, "held-resource")
		if err == nil {
			lock.Release()
		}
		contended <- err
	}()

	const probeBudget = 300 * time.Millisecond
	select {
	case err := <-contended:
		t.Fatalf("AcquireStateLock for the same resourceType returned (err=%v) while a peer held the lock — contention was not enforced", err)
	case <-time.After(probeBudget):
		// Expected: still blocked waiting on the peer's held lock.
	}

	// A different resourceType must not contend — its lock file is distinct.
	other, err := AcquireStateLock(root, "other-resource")
	if err != nil {
		t.Fatalf("acquire unrelated resourceType should not contend: %v", err)
	}
	other.Release()

	// Release the peer lock so the contended goroutine's Acquire can
	// succeed and the test can exit cleanly instead of leaking a goroutine
	// blocked until StateLockTimeout.
	peerLock.Release()
	if err := <-contended; err != nil {
		t.Fatalf("AcquireStateLock after peer released: %v", err)
	}
}
