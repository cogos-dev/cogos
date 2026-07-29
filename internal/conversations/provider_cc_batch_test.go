// provider_cc_batch_test.go — regression tests for the sibling-path finding
// cog-review raised on PR #495's first pass: issue #494 remedy 1 (batch the
// meta write) originally only covered the normalized-ingest path
// (applyIngestSource). The CC (source_dirs) path in ApplyPlan's action loop
// called idx.UpsertSession/idx.DeleteSession once per action individually,
// reproducing the identical O(N) per-cycle write-amplification defect for N
// actively-used CC sessions drifting in the same reconcile cycle. These
// tests cover the fix: pendingCCUpserts/pendingCCDeletes batch the whole
// cycle's CC actions into one idx.UpsertSessions/idx.DeleteSessions call.
package conversations

import (
	"context"
	"os"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// TestApplyPlan_CCUpsertsAreBatched writes several distinct CC session JSONL
// files in one source_dirs directory, reconciles once, and asserts the whole
// cycle's worth of CC creates commits via exactly ONE writeMetaFileLocked
// call — not one per session.
func TestApplyPlan_CCUpsertsAreBatched(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	const n = 5
	sessionIDs := make([]string, n)
	for i := 0; i < n; i++ {
		sid := sessionUUIDN(i)
		sessionIDs[i] = sid
		lines := []string{
			makeAITitleRecord(sid, "CC Batch Session"),
			makeUserRecord("uuid-u-"+sid, "", sid, "hello from "+sid, "2026-05-01T10:00:00Z"),
		}
		writeJSONLFixture(t, srcDir, sid, lines)
	}
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
	if plan.Summary.Creates != n {
		t.Fatalf("expected %d creates, got %d (summary=%+v)", n, plan.Summary.Creates, plan.Summary)
	}

	var results []reconcile.Result
	writes := countMetaWrites(t, func() {
		var applyErr error
		results, applyErr = p.ApplyPlan(ctx, plan)
		if applyErr != nil {
			t.Fatalf("ApplyPlan: %v", applyErr)
		}
	})

	if writes != 1 {
		t.Fatalf("ApplyPlan for %d CC creates in one cycle caused %d meta writes, want exactly 1 (batched)", n, writes)
	}
	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}
	for _, r := range results {
		if r.Status != reconcile.ApplySucceeded {
			t.Errorf("action %s/%s did not succeed: %s", r.Action, r.Name, r.Error)
		}
	}

	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	ls := liveAny2.(*liveState)
	for _, sid := range sessionIDs {
		if _, ok := ls.Entries[sid]; !ok {
			t.Errorf("session %s missing from live state after batched CC upsert", sid)
		}
	}
}

// TestApplyPlan_CCDeletesAreBatched seeds several CC sessions, removes most
// of their source files, and asserts the resulting cycle's deletes commit
// via exactly ONE writeMetaFileLocked call.
func TestApplyPlan_CCDeletesAreBatched(t *testing.T) {
	p, root := newTestProvider(t)
	srcDir := t.TempDir()

	const n = 5
	const deleted = 3
	sessionIDs := make([]string, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		sid := sessionUUIDN(i)
		sessionIDs[i] = sid
		lines := []string{
			makeAITitleRecord(sid, "CC Batch Delete Session"),
			makeUserRecord("uuid-u-"+sid, "", sid, "hello from "+sid, "2026-05-01T10:00:00Z"),
		}
		paths[i] = writeJSONLFixture(t, srcDir, sid, lines)
	}
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
	if _, err := p.ApplyPlan(ctx, plan); err != nil {
		t.Fatalf("initial ApplyPlan: %v", err)
	}

	// Remove `deleted` of the n source files.
	for i := 0; i < deleted; i++ {
		if err := os.Remove(paths[i]); err != nil {
			t.Fatalf("remove source file: %v", err)
		}
	}

	cfgAny2, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig 2: %v", err)
	}
	liveAny2, err := p.FetchLive(ctx, cfgAny2)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	plan2, err := p.ComputePlan(cfgAny2, liveAny2, nil)
	if err != nil {
		t.Fatalf("ComputePlan 2: %v", err)
	}
	if plan2.Summary.Deletes != deleted {
		t.Fatalf("expected %d deletes, got %d (summary=%+v)", deleted, plan2.Summary.Deletes, plan2.Summary)
	}

	writes := countMetaWrites(t, func() {
		results, applyErr := p.ApplyPlan(ctx, plan2)
		if applyErr != nil {
			t.Fatalf("ApplyPlan 2: %v", applyErr)
		}
		for _, r := range results {
			if r.Status == reconcile.ApplyFailed {
				t.Errorf("action %s/%s failed: %s", r.Action, r.Name, r.Error)
			}
		}
	})
	if writes != 1 {
		t.Fatalf("ApplyPlan for %d CC deletes in one cycle caused %d meta writes, want exactly 1 (batched)", deleted, writes)
	}

	liveAny3, err := p.FetchLive(ctx, cfgAny2)
	if err != nil {
		t.Fatalf("FetchLive 3: %v", err)
	}
	ls := liveAny3.(*liveState)
	for i := 0; i < deleted; i++ {
		if _, ok := ls.Entries[sessionIDs[i]]; ok {
			t.Errorf("session %s still present after batched CC delete", sessionIDs[i])
		}
	}
	for i := deleted; i < n; i++ {
		if _, ok := ls.Entries[sessionIDs[i]]; !ok {
			t.Errorf("session %s missing after unrelated batched delete cycle", sessionIDs[i])
		}
	}
}

// sessionUUIDN derives a distinct, deterministic pseudo-UUID-shaped session
// ID for index i, so multiple CC fixture files in the same test don't
// collide. Not a real UUID — the provider only requires the source
// filename's stem to match the session_id field in its records.
func sessionUUIDN(i int) string {
	return "00000000-0000-0000-0000-" + padTo12(i)
}

func padTo12(i int) string {
	s := "000000000000"
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if len(digits) == 0 {
		digits = []byte{'0'}
	}
	return s[:len(s)-len(digits)] + string(digits)
}
