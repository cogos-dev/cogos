// procmgr_test.go — map-eviction + concurrency coverage for ProcessManager.
//
// The eviction tests pin the manager to a fake (never-started) *exec.Cmd so no
// real subprocess is spawned; only the bookkeeping is exercised. The
// concurrency test is meant to run under `go test -race`.
package engine

import (
	"os/exec"
	"sync"
	"testing"
	"time"
)

// trackFake registers a process without starting a real subprocess.
func trackFake(pm *ProcessManager, kind ProcessKind) *ManagedProcess {
	return pm.Track(exec.Command("true"), ManagedProcessOpts{Kind: kind, Source: "test"})
}

// TestFinishEvictsAgedBackgroundEntries is the regression test for the leak:
// background processes used to stay in the map forever after Finish.
func TestFinishEvictsAgedBackgroundEntries(t *testing.T) {
	oldRetention := finishedRetention
	finishedRetention = 0 // any finished entry is immediately past retention
	defer func() { finishedRetention = oldRetention }()

	pm := NewProcessManager(ProcessManagerConfig{})

	// A finished background process from a prior cycle...
	old := trackFake(pm, ProcessBackground)
	pm.Finish(old.ID) // marks terminal; reapLocked runs but `old` is the newest

	// ...is evicted when the next Finish triggers a reap (retention == 0 means
	// the previously-finished `old` is now strictly older than retention).
	next := trackFake(pm, ProcessBackground)
	pm.Finish(next.ID)

	pm.mu.RLock()
	_, oldPresent := pm.processes[old.ID]
	total := len(pm.processes)
	pm.mu.RUnlock()

	if oldPresent {
		t.Errorf("aged finished background entry %s was not evicted", old.ID[:8])
	}
	if total > 1 {
		t.Errorf("map retained %d entries, want <= 1 (only the just-finished one)", total)
	}
}

// TestRemoveDeletesBackgroundEntry covers the failed-start path, where Remove is
// called on a background process and must drop it (it used to only delete
// foreground kinds, leaking the entry).
func TestRemoveDeletesBackgroundEntry(t *testing.T) {
	pm := NewProcessManager(ProcessManagerConfig{})
	proc := trackFake(pm, ProcessBackground)

	pm.Remove(proc.ID)

	pm.mu.RLock()
	_, present := pm.processes[proc.ID]
	pm.mu.RUnlock()
	if present {
		t.Errorf("background entry %s not removed", proc.ID[:8])
	}
}

// TestReapHardCap verifies the hard cap drops oldest-finished entries even when
// they are within the retention window.
func TestReapHardCap(t *testing.T) {
	oldCap := maxTrackedProcesses
	oldRetention := finishedRetention
	maxTrackedProcesses = 10
	finishedRetention = time.Hour // age sweep keeps everything; only the cap bites
	defer func() {
		maxTrackedProcesses = oldCap
		finishedRetention = oldRetention
	}()

	pm := NewProcessManager(ProcessManagerConfig{MaxGlobal: 1000})
	for i := 0; i < 50; i++ {
		p := trackFake(pm, ProcessBackground)
		pm.Finish(p.ID)
	}

	pm.mu.RLock()
	total := len(pm.processes)
	pm.mu.RUnlock()
	if total > maxTrackedProcesses {
		t.Errorf("map has %d entries, want <= hard cap %d", total, maxTrackedProcesses)
	}
}

// TestProcessManagerShutdownClosesChannel verifies Shutdown closes shutdownCh
// (so kill-escalation goroutines can abort) and that a repeat Shutdown does not
// panic on a double close (guarded by shutdownOnce).
func TestProcessManagerShutdownClosesChannel(t *testing.T) {
	pm := NewProcessManager(ProcessManagerConfig{})
	pm.Shutdown(10 * time.Millisecond) // no running procs → returns immediately

	select {
	case <-pm.shutdownCh:
		// closed as expected
	default:
		t.Fatal("shutdownCh not closed after Shutdown")
	}

	// Idempotent: a second Shutdown must not panic on a double close.
	pm.Shutdown(10 * time.Millisecond)
}

// TestProcMgrConcurrentAccess drives Track/Finish/Remove/List/Stats/CanSpawn
// from many goroutines so `go test -race` flags any unsynchronized field
// access (e.g. reading proc.Status without proc.mu).
func TestProcMgrConcurrentAccess(t *testing.T) {
	pm := NewProcessManager(ProcessManagerConfig{MaxGlobal: 100000})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				p := trackFake(pm, ProcessBackground)
				if i%2 == 0 {
					pm.Finish(p.ID)
				} else {
					pm.Remove(p.ID)
				}
			}
		}()
	}
	// Concurrent readers.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = pm.List()
				_ = pm.Stats()
				_ = pm.CanSpawn("node-x")
			}
		}()
	}
	wg.Wait()
}

// TestProcMgrKillVsReaders is the targeted regression for the proc.Status data
// race: Kill writes proc.Status under proc.mu while List/Stats/KillBy* read it.
// Before the loadStatus/snapshot fix, `go test -race` flagged this interleaving.
// We register processes whose cmd was never started (cmd.Process == nil) so
// Kill's escalation goroutine is skipped and no real signals are sent — only
// the bookkeeping (and its locking) is exercised.
func TestProcMgrKillVsReaders(t *testing.T) {
	pm := NewProcessManager(ProcessManagerConfig{MaxGlobal: 100000})

	ids := make([]string, 0, 256)
	for i := 0; i < 256; i++ {
		p := pm.Track(exec.Command("true"), ManagedProcessOpts{
			Kind:     ProcessForeground,
			Source:   "sess-a",
			Identity: "node-1",
		})
		ids = append(ids, p.ID)
	}

	var wg sync.WaitGroup
	// Killers mutate Status under proc.mu.
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(off int) {
			defer wg.Done()
			for i := off; i < len(ids); i += 8 {
				pm.Kill(ids[i])
			}
		}(g)
	}
	// Source/identity sweeps also flip status via Kill on matching procs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = pm.KillBySource("sess-a")
			_ = pm.KillByIdentity("node-1")
		}
	}()
	// Readers race the killers.
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_ = pm.List()
				_ = pm.Stats()
			}
		}()
	}
	wg.Wait()
}
