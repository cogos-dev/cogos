// provider_prune_batch_test.go — regression tests for issue #494 remedies 1
// (batch the meta write, including the prune path) and 2 (hoist
// idx.ListSessions out of the per-source prune loop) as they interact inside
// applyIngestSource/ApplyPlan.
package conversations

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// newIngestAction builds the reconcile.Action shape ComputePlan produces for
// an ingest source create/update, matching applyIngestSource's expectations
// (Details["is_ingest"], ["source_dir"], ["ingest_files"]).
func newIngestAction(source, sourceDir string, files []string) reconcile.Action {
	return reconcile.Action{
		Action:       reconcile.ActionUpdate,
		ResourceType: "conversations",
		Name:         source,
		Details: map[string]any{
			"is_ingest":    true,
			"source_dir":   sourceDir,
			"ingest_files": files,
		},
	}
}

// TestApplyIngestSourcePruneBatchesMetaWrite drives applyIngestSource
// directly (bypassing ApplyPlan/ComputePlan) so the prune path can be
// exercised deterministically: a source is first indexed with 5 sessions,
// then the underlying file is rewritten to contain only 2 of them, and
// applyIngestSource is called again with the pre-rewrite session IDs as
// existingSourceSessionIDs (exactly what ApplyPlan's hoisted
// idx.SessionIDsBySource() call would have produced).
//
// Before the #494 fix, the prune loop called idx.DeleteSession once per
// stale session — 3 full _meta.json rewrites here. The fix must produce
// exactly ONE writeMetaFileLocked call for the prune batch (on top of the
// one for the upsert batch), regardless of how many sessions are pruned.
func TestApplyIngestSourcePruneBatchesMetaWrite(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	const source = "prune-test-source"
	ingestDir := t.TempDir()
	filePath := filepath.Join(ingestDir, "run1.jsonl")

	allIDs := []string{"a", "b", "c", "d", "e"}
	writeLines := func(ids []string) {
		var lines []string
		for _, sid := range ids {
			lines = append(lines, makeIngestRecord(source, sid, "user", "hello "+sid, "2026-06-01T10:00:00Z", nil))
		}
		content := ""
		for _, l := range lines {
			content += l + "\n"
		}
		if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
			t.Fatalf("write ingest file: %v", err)
		}
	}

	// Initial parse: all 5 sessions.
	writeLines(allIDs)
	action := newIngestAction(source, ingestDir, []string{filePath})

	writes := countMetaWrites(t, func() {
		if err := applyIngestSource(idx, action, nil, nil, nil, nil); err != nil {
			t.Fatalf("applyIngestSource (initial): %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("initial applyIngestSource caused %d meta writes, want 1", writes)
	}

	bySource := idx.SessionIDsBySource()
	got := bySource[source]
	if len(got) != len(allIDs) {
		t.Fatalf("SessionIDsBySource()[%q] = %v, want %d entries", source, got, len(allIDs))
	}

	// Rewrite the source file to contain only 2 of the 5 sessions — 3 must
	// be pruned. This is deliberately not a realistic append-only observer
	// run (the code comment calls this "rare — defensive only"), but it's
	// exactly the path that must not regress to per-session meta rewrites.
	writeLines([]string{"a", "b"})

	writes = countMetaWrites(t, func() {
		if err := applyIngestSource(idx, action, nil, nil, nil, bySource[source]); err != nil {
			t.Fatalf("applyIngestSource (prune): %v", err)
		}
	})
	// One write for the upsert batch (a, b) + one for the prune batch
	// (c, d, e) = 2, never 1 (upsert only, wrong) or 4 (1 upsert + 3
	// per-session deletes, the pre-fix behavior).
	if writes != 2 {
		t.Fatalf("prune-cycle applyIngestSource caused %d meta writes, want exactly 2 (one upsert batch + one prune batch)", writes)
	}

	remaining := idx.SessionIDsBySource()[source]
	if len(remaining) != 2 {
		t.Fatalf("SessionIDsBySource()[%q] after prune = %v, want exactly [a b] (composite keys)", source, remaining)
	}
	// Ingest sessions are keyed as "<source>/<session_id>" (see
	// indexKeyForIngest / SessionMeta.Source's doc comment) — not the bare
	// record session_id used in writeLines above.
	for _, want := range []string{"a", "b"} {
		key := indexKeyForIngest(source, want)
		if _, ok := idx.GetMeta(key); !ok {
			t.Fatalf("session %q missing after prune cycle", key)
		}
	}
	for _, gone := range []string{"c", "d", "e"} {
		key := indexKeyForIngest(source, gone)
		if _, ok := idx.GetMeta(key); ok {
			t.Fatalf("session %q still present after prune cycle, want pruned", key)
		}
		if _, statErr := os.Stat(idx.turnsPath(key)); statErr == nil {
			t.Fatalf("turns file for pruned session %q still on disk", key)
		}
	}
}

// TestApplyPlanPruneBatchesMetaWriteEndToEnd exercises the same prune
// scenario through the full LoadConfig→FetchLive→ComputePlan→ApplyPlan
// pipeline (rather than calling applyIngestSource directly), confirming
// ApplyPlan's hoisted idx.SessionIDsBySource() call (remedy 2) is correctly
// threaded through to applyIngestSource and that the batched writes (remedy
// 1) still happen when driven by a real reconcile cycle.
func TestApplyPlanPruneBatchesMetaWriteEndToEnd(t *testing.T) {
	p, root := newTestProvider(t)
	ingestRoot := t.TempDir()
	const source = "prune-e2e-source"

	writeIngestDir(t, ingestRoot, source, "run1", []string{
		makeIngestRecord(source, "s1", "user", "hi 1", "2026-06-01T10:00:00Z", nil),
		makeIngestRecord(source, "s2", "user", "hi 2", "2026-06-01T10:00:01Z", nil),
		makeIngestRecord(source, "s3", "user", "hi 3", "2026-06-01T10:00:02Z", nil),
	})
	writeObservatoryConfigFull(t, root, nil, []string{ingestRoot})

	reconcileOnce(t, p, root)

	// Rewrite the ingest file, dropping s3 — triggers a size-based drift
	// detection on the next cycle and a defensive prune of s3.
	writeIngestDir(t, ingestRoot, source, "run1", []string{
		makeIngestRecord(source, "s1", "user", "hi 1", "2026-06-01T10:00:00Z", nil),
		makeIngestRecord(source, "s2", "user", "hi 2", "2026-06-01T10:00:01Z", nil),
	})

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
	if plan.Summary.Updates == 0 {
		t.Fatalf("expected an update action after rewriting the ingest file, got summary %+v", plan.Summary)
	}

	writes := countMetaWrites(t, func() {
		results, err := p.ApplyPlan(ctx, plan)
		if err != nil {
			t.Fatalf("ApplyPlan: %v", err)
		}
		for _, r := range results {
			if r.Status == reconcile.ApplyFailed {
				t.Fatalf("apply action %s/%s failed: %s", r.Action, r.Name, r.Error)
			}
		}
	})
	if writes != 2 {
		t.Fatalf("end-to-end prune cycle caused %d meta writes, want exactly 2 (one upsert batch + one prune batch)", writes)
	}

	liveAny2, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		t.Fatalf("FetchLive 2: %v", err)
	}
	ls := liveAny2.(*liveState)
	if _, ok := ls.Entries[source+"/s3"]; ok {
		t.Fatalf("s3 still present in live state after being dropped from the source file")
	}
	if _, ok := ls.Entries[source+"/s1"]; !ok {
		t.Fatalf("s1 missing from live state after prune cycle")
	}
	if _, ok := ls.Entries[source+"/s2"]; !ok {
		t.Fatalf("s2 missing from live state after prune cycle")
	}
}
