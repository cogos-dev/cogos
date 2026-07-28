// health_backpressure_test.go — regression tests for #482.
//
// A self-sustaining feedback loop kept the conversations provider permanently
// Degraded on a live node:
//
//	reconcile daemon (30s poll, 31-244s cycle) overlaps itself
//	→ autonomic self-heal calls ApplyPlan on the SAME provider
//	→ both contend on .cog/state/conversations/_meta.json.lock
//	→ the loser gets filelock.ErrLockTimeout
//	→ the timeout lands in p.lastErrors → Health() == Degraded
//	→ Degraded satisfies needsHeal → self-heal fires again → repeat forever
//
// The invariant these tests pin: losing a race for an on-disk index lock is
// BACKPRESSURE, not corruption. It must not set Health=Degraded and must not
// count as an unresolved-work failure, because doing so re-arms the very loop
// that caused the contention.
package conversations

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// isLockBackpressure must recognise a filelock timeout through arbitrary
// wrapping depth (the real path wraps it twice: UpsertSession →
// writeMetaFileLocked → filelock.Acquire), and must NOT match genuine errors.
func TestIsLockBackpressure_ClassifiesSentinel(t *testing.T) {
	wrapped := fmt.Errorf("upsert session x: %w",
		fmt.Errorf("conversations/index: acquire meta lock: %w",
			fmt.Errorf("%w: /tmp/_meta.json.lock", filelock.ErrLockTimeout)))

	if !isLockBackpressure(wrapped) {
		t.Fatalf("wrapped filelock.ErrLockTimeout not classified as backpressure: %v", wrapped)
	}
	if isLockBackpressure(nil) {
		t.Fatal("nil must not be backpressure")
	}
	if isLockBackpressure(fmt.Errorf("parse foo.jsonl: invalid character 'x'")) {
		t.Fatal("a genuine parse error must not be classified as backpressure")
	}
}

// Health must stay Healthy when the only thing that went wrong was lock
// contention, and must still report Degraded for genuine indexing errors.
func TestHealth_LockContentionIsNotDegraded(t *testing.T) {
	p := NewProvider()

	p.mu.Lock()
	p.planApplied = true
	p.applyBackpressure = 3
	p.lastErrors = nil
	p.applyFailures = 0
	p.mu.Unlock()

	h := p.Health()
	if h.Health != reconcile.HealthHealthy {
		t.Fatalf("lock contention only: want Healthy, got %s (msg=%q) — "+
			"this is #482: self-contention reported as degradation, which "+
			"re-arms self-heal and regenerates the contention", h.Health, h.Message)
	}
	if h.Sync != reconcile.SyncStatusSynced {
		t.Fatalf("lock contention only: want Synced, got %s", h.Sync)
	}

	// Genuine errors must still degrade.
	p.mu.Lock()
	p.lastErrors = []string{"parse /x.jsonl: invalid character"}
	p.mu.Unlock()
	if got := p.Health().Health; got != reconcile.HealthDegraded {
		t.Fatalf("genuine indexing error: want Degraded, got %s", got)
	}
}

// End-to-end: hold the on-disk session lock a real ApplyPlan needs, run
// ApplyPlan, and assert the action fails (it genuinely could not write) while
// the provider stays Healthy and Synced. This is the exact production shape.
func TestApplyPlan_LockTimeoutDoesNotDegradeProvider(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	lines := []string{
		makeAITitleRecord(sessionUUID, "Contended Session"),
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

	// Simulate the peer (daemon or self-heal) that already holds the lock.
	// UpsertSession takes the per-session turns lock first, so holding that
	// one is what a losing racer actually blocks on.
	peer, err := filelock.Acquire(p.index.turnsLockPath(sessionUUID), 2*time.Second)
	if err != nil {
		t.Fatalf("peer filelock.Acquire: %v", err)
	}
	defer peer.Release()

	results, err := p.ApplyPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyPlan returned a hard error: %v", err)
	}

	sawTimeout := false
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed &&
			strings.Contains(r.Error, filelock.ErrLockTimeout.Error()) {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Fatalf("expected at least one lock-timeout failure in results, got %+v", results)
	}

	h := p.Health()
	if h.Health == reconcile.HealthDegraded {
		t.Fatalf("after lock-timeout-only apply: want Healthy, got Degraded (msg=%q) — "+
			"#482 feedback loop: Degraded re-arms self-heal, self-heal races the "+
			"reconcile daemon, the race produces another lock timeout, forever", h.Message)
	}
	if h.Sync == reconcile.SyncStatusOutOfSync {
		t.Fatalf("after lock-timeout-only apply: want Synced, got OutOfSync (msg=%q)", h.Message)
	}
}
