// provider_apply_serialization_test.go — regression tests for issue #494
// remedy 4: an in-process apply mutex on *Provider prevents the reconcile
// daemon and the autonomic ticker's self-heal from ever concurrently
// entering ApplyPlan for the same provider instance.
//
// Background (see the issue and health_backpressure_test.go's header for
// #482, the earlier, incomplete fix): both callers eventually reach
// writeMetaFileLocked's filelock.Acquire(metaLockPath, metaLockTimeout).
// flock(2) locks attach to the open file description, not the owning
// process, so two os.OpenFile calls from the SAME process still block each
// other. #482 stopped that contention from reporting Degraded health, but
// did nothing to prevent the contention itself — the loser still burned a
// full metaLockTimeout (5s) and its action still came back ApplyFailed,
// which (via planApplied staying false) kept the provider OutOfSync and
// re-armed the next self-heal tick. Remedy 4 removes the race entirely: a
// second ApplyPlan call for the same Provider while one is already running
// returns near-instantly with a single ApplySkipped result instead of
// contending for the on-disk lock at all.
package conversations

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// TestApplyPlan_HeldApplyMuIsSkippedImmediately drives ApplyPlan while
// applyMu is already held (simulating "another ApplyPlan call for this
// Provider is in flight," the same technique
// TestApplyPlan_LockTimeoutDoesNotDegradeProvider uses to simulate a held
// on-disk lock) and asserts it returns immediately with exactly one
// ApplySkipped result, touching neither the index nor provider bookkeeping.
func TestApplyPlan_HeldApplyMuIsSkippedImmediately(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	lines := []string{
		makeAITitleRecord(sessionUUID, "Serialization Test Session"),
		makeUserRecord("uuid-u1", "", sessionUUID, "hello", "2026-05-01T10:00:00Z"),
		makeAssistantRecord("uuid-a1", "uuid-u1", sessionUUID, "hi", "2026-05-01T10:01:00Z"),
	}
	writeJSONLFixture(t, srcDir, sessionUUID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	liveAny, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if !plan.Summary.HasChanges() {
		t.Fatalf("expected a plan with changes, got %+v", plan.Summary)
	}

	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	start := time.Now()
	results, err := p.ApplyPlan(ctx, plan)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ApplyPlan returned a hard error while applyMu was held: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("ApplyPlan took %v while applyMu was held — want a near-instant TryLock skip, not a block", elapsed)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySkipped {
		t.Fatalf("results = %+v, want exactly one ApplySkipped result", results)
	}

	// The skipped call must not have touched the index: the session from the
	// plan must still be entirely unindexed (no partial/racing write).
	if _, ok := p.index.GetMeta(sessionUUID); ok {
		t.Fatalf("skipped ApplyPlan call indexed %s anyway", sessionUUID)
	}
}

// TestApplyPlan_ConcurrentCallsNeverBothProceed is the closest reproduction
// of the live production interleaving: two goroutines call p.ApplyPlan on the
// SAME Provider concurrently. The first is paused (via writeMetaFileLockedHook,
// the same synchronization point index_batch_test.go's countMetaWrites uses)
// partway through its apply so the second genuinely overlaps it, rather than
// relying on a timing-sensitive race. Before remedy 4, the second call would
// eventually reach filelock.Acquire(metaLockPath, ...) and either wait out
// metaLockTimeout contending with the first (same-process flock
// self-collision) or interleave unsafely; after the fix it must return
// immediately with ApplySkipped and never surface a filelock timeout.
func TestApplyPlan_ConcurrentCallsNeverBothProceed(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	lines := []string{
		makeAITitleRecord(sessionUUID, "Concurrent Apply Session"),
		makeUserRecord("uuid-u1", "", sessionUUID, "hello", "2026-05-01T10:00:00Z"),
		makeAssistantRecord("uuid-a1", "uuid-u1", sessionUUID, "hi", "2026-05-01T10:01:00Z"),
	}
	writeJSONLFixture(t, srcDir, sessionUUID, lines)
	writeObservatoryConfig(t, root, []string{srcDir})

	ctx := context.Background()
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	liveAny, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if !plan.Summary.HasChanges() {
		t.Fatalf("expected a plan with changes, got %+v", plan.Summary)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var entryClosed bool

	metaWriteHookMu.Lock()
	writeMetaFileLockedHook = func() {
		if !entryClosed {
			entryClosed = true
			close(entered)
		}
		<-release
	}
	defer func() {
		writeMetaFileLockedHook = nil
		metaWriteHookMu.Unlock()
	}()

	type applyOutcome struct {
		results []reconcile.Result
		err     error
	}
	firstDone := make(chan applyOutcome, 1)
	go func() {
		res, err := p.ApplyPlan(ctx, plan)
		firstDone <- applyOutcome{res, err}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first ApplyPlan never reached writeMetaFileLocked — test setup is broken")
	}

	secondStart := time.Now()
	secondResults, secondErr := p.ApplyPlan(ctx, plan)
	secondElapsed := time.Since(secondStart)
	close(release)

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first (in-flight) ApplyPlan returned a hard error: %v", first.err)
	}
	for _, r := range first.results {
		if r.Status == reconcile.ApplyFailed {
			t.Fatalf("first (in-flight) ApplyPlan reported ApplyFailed: %+v — the concurrent second call should have been skipped, not contended", r)
		}
	}

	if secondErr != nil {
		t.Fatalf("second (contending) ApplyPlan returned a hard error: %v", secondErr)
	}
	// metaLockTimeout is 5s; a properly-skipped call returns in microseconds.
	// A generous 500ms bound comfortably separates "skipped" from "blocked
	// toward metaLockTimeout" even on a loaded CI runner.
	if secondElapsed > 500*time.Millisecond {
		t.Fatalf("second ApplyPlan took %v while the first was mid-apply — want a near-instant TryLock skip, not a block toward metaLockTimeout (5s)", secondElapsed)
	}
	if len(secondResults) != 1 || secondResults[0].Status != reconcile.ApplySkipped {
		t.Fatalf("second (contending) ApplyPlan results = %+v, want exactly one ApplySkipped result", secondResults)
	}
	for _, r := range secondResults {
		if strings.Contains(r.Error, filelock.ErrLockTimeout.Error()) {
			t.Fatalf("second ApplyPlan surfaced a flock timeout instead of an apply-mutex skip: %+v — this is the exact self-collision issue #494 reports", r)
		}
	}

	// The winning (first) call must have actually indexed the session.
	if _, ok := p.index.GetMeta(sessionUUID); !ok {
		t.Fatalf("the winning ApplyPlan call did not index %s", sessionUUID)
	}
}
