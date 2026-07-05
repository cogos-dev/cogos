// dispatch_jobs_test.go — coverage for the async job-handle registry
// (dispatch_jobs.go) and the async path through toolDispatchToHarness /
// toolPollDispatch.
package engine

import (
	"context"
	"testing"
	"time"
)

// --- DispatchJobRegistry unit tests -----------------------------------------

func TestDispatchJobRegistry_LifecyclePendingToDone(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	jobID := reg.Create("cycle-1")

	rec, ok := reg.Get(jobID)
	if !ok {
		t.Fatalf("Get(%q): not found immediately after Create", jobID)
	}
	if rec.State != DispatchJobPending {
		t.Errorf("initial state = %q, want %q", rec.State, DispatchJobPending)
	}
	if rec.CycleID != "cycle-1" {
		t.Errorf("CycleID = %q, want %q", rec.CycleID, "cycle-1")
	}

	reg.MarkRunning(jobID)
	rec, _ = reg.Get(jobID)
	if rec.State != DispatchJobRunning {
		t.Errorf("state after MarkRunning = %q, want %q", rec.State, DispatchJobRunning)
	}

	result := &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "ok"}}}
	reg.Complete(jobID, result)
	rec, _ = reg.Get(jobID)
	if rec.State != DispatchJobDone {
		t.Errorf("state after Complete = %q, want %q", rec.State, DispatchJobDone)
	}
	if rec.Result == nil || len(rec.Result.Results) != 1 || rec.Result.Results[0].Content != "ok" {
		t.Errorf("Result not stored correctly: %+v", rec.Result)
	}
}

func TestDispatchJobRegistry_Fail(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	jobID := reg.Create("")
	reg.MarkRunning(jobID)
	reg.Fail(jobID, "boom")

	rec, ok := reg.Get(jobID)
	if !ok {
		t.Fatal("job not found after Fail")
	}
	if rec.State != DispatchJobFailed {
		t.Errorf("state = %q, want %q", rec.State, DispatchJobFailed)
	}
	if rec.Err != "boom" {
		t.Errorf("Err = %q, want %q", rec.Err, "boom")
	}
}

func TestDispatchJobRegistry_UnknownJobNotFound(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	if _, ok := reg.Get("does-not-exist"); ok {
		t.Fatal("expected unknown job id to be not-found")
	}
}

// TestDispatchJobRegistry_GetReturnsDefensiveCopy guards against a caller
// mutating the record it got back and corrupting the registry's internal
// state for other readers.
func TestDispatchJobRegistry_GetReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	jobID := reg.Create("")

	rec, _ := reg.Get(jobID)
	rec.State = DispatchJobFailed // mutate the caller's copy

	rec2, _ := reg.Get(jobID)
	if rec2.State != DispatchJobPending {
		t.Fatalf("registry state leaked caller mutation: got %q, want %q", rec2.State, DispatchJobPending)
	}
}

// TestDispatchJobRegistry_LazyGCReclaimsExpiredTerminalJobs verifies the TTL
// sweep: a terminal job older than the TTL is removed on the next Create
// call (lazy GC, no background ticker).
func TestDispatchJobRegistry_LazyGCReclaimsExpiredTerminalJobs(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	reg.ttl = 10 * time.Millisecond

	fakeNow := time.Now()
	reg.now = func() time.Time { return fakeNow }

	oldJob := reg.Create("")
	reg.Complete(oldJob, &DispatchBatchResult{})

	// Advance fake time past the TTL, then create a new job — this should
	// trigger gcLocked and evict oldJob.
	fakeNow = fakeNow.Add(1 * time.Second)
	_ = reg.Create("")

	if _, ok := reg.Get(oldJob); ok {
		t.Fatal("expected expired terminal job to be GC'd, but it is still present")
	}
}

// TestDispatchJobRegistry_LazyGCDoesNotReclaimPending guards against GC
// evicting an in-flight (non-terminal) job just because it's old — only
// terminal (done/failed) records are TTL-eligible.
func TestDispatchJobRegistry_LazyGCDoesNotReclaimPending(t *testing.T) {
	t.Parallel()
	reg := NewDispatchJobRegistry()
	reg.ttl = 10 * time.Millisecond

	fakeNow := time.Now()
	reg.now = func() time.Time { return fakeNow }

	pendingJob := reg.Create("")

	fakeNow = fakeNow.Add(1 * time.Second)
	_ = reg.Create("")

	if _, ok := reg.Get(pendingJob); !ok {
		t.Fatal("pending job was reclaimed by GC; only terminal jobs should be TTL-eligible")
	}
}

// --- toolDispatchToHarness async path / toolPollDispatch --------------------

// TestToolDispatchToHarness_AsyncReturnsImmediateJobHandle is the test that
// fails without the change: without Async support, dispatching with
// Async=true would either be ignored (falling through to the synchronous
// path and blocking until fakeAgentDispatcher's canned result is available
// — which for a fast fake wouldn't distinguish the paths) or the field
// wouldn't exist at all. The meaningful assertion is that the response is a
// job handle shape ({job_id, status:"pending"}), not a DispatchBatchResult.
func TestToolDispatchToHarness_AsyncReturnsImmediateJobHandle(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))

	block := make(chan struct{})
	disp := &blockingDispatcher{
		release: block,
		canned:  &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "done"}}},
	}
	server := NewMCPServerWithAgentController(cfg, makeNucleus("Cog", "tester"), process, disp)

	result, _, err := server.toolDispatchToHarness(context.Background(), nil, dispatchToHarnessInput{
		Task:  "do the thing",
		Async: true,
	})
	if err != nil {
		t.Fatalf("toolDispatchToHarness (async): %v", err)
	}

	var receipt dispatchJobReceipt
	decodeMCPJSONForAgentTests(t, result, &receipt)
	if receipt.JobID == "" {
		t.Fatal("expected non-empty job_id in async receipt")
	}
	if receipt.Status != string(DispatchJobPending) {
		t.Errorf("initial status = %q, want %q", receipt.Status, DispatchJobPending)
	}

	// Release the blocked dispatcher, then wait for the background goroutine
	// to actually reach a terminal state before the test returns — otherwise
	// t.TempDir()'s cleanup can race the goroutine's ledger write to
	// .cog/ledger/ under the workspace root (the goroutine outlives the
	// synchronous part of the test by design; this is the async path's whole
	// point, so the test must wait it out rather than fire-and-forget).
	close(block)
	waitForTerminalJob(t, server, receipt.JobID)
}

// TestToolDispatchToHarness_AsyncPollTransitionsToDone exercises the full
// async lifecycle: dispatch async, observe pending, unblock the fake
// dispatcher, poll until done, and confirm the polled result matches what a
// synchronous call would have returned for the same input.
func TestToolDispatchToHarness_AsyncPollTransitionsToDone(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))

	release := make(chan struct{})
	canned := &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "the answer"}}}
	disp := &blockingDispatcher{release: release, canned: canned}
	server := NewMCPServerWithAgentController(cfg, makeNucleus("Cog", "tester"), process, disp)

	result, _, err := server.toolDispatchToHarness(context.Background(), nil, dispatchToHarnessInput{
		Task:  "do the thing",
		Async: true,
	})
	if err != nil {
		t.Fatalf("toolDispatchToHarness (async): %v", err)
	}
	var receipt dispatchJobReceipt
	decodeMCPJSONForAgentTests(t, result, &receipt)

	// Poll immediately: still pending/running since the fake dispatcher is
	// blocked on `release`.
	pollResult, _, err := server.toolPollDispatch(context.Background(), nil, pollDispatchInput{JobID: receipt.JobID})
	if err != nil {
		t.Fatalf("toolPollDispatch (pre-release): %v", err)
	}
	var status dispatchJobStatusResponse
	decodeMCPJSONForAgentTests(t, pollResult, &status)
	if status.Status != string(DispatchJobPending) && status.Status != string(DispatchJobRunning) {
		t.Fatalf("status before release = %q, want pending or running", status.Status)
	}

	close(release)

	final := waitForTerminalJob(t, server, receipt.JobID)
	if final.Status != string(DispatchJobDone) {
		t.Fatalf("final status = %q, want %q (error=%q)", final.Status, DispatchJobDone, final.Error)
	}
	if final.Result == nil || len(final.Result.Results) != 1 || final.Result.Results[0].Content != "the answer" {
		t.Fatalf("polled result does not match the synchronous canned result: %+v", final.Result)
	}
}

// TestToolPollDispatch_UnknownJobIDReturnsClearError guards the not-found
// path — an unknown or expired job_id must not panic or return an empty
// success response.
func TestToolPollDispatch_UnknownJobIDReturnsClearError(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	server := NewMCPServer(cfg, makeNucleus("Cog", "tester"), process)

	result, _, err := server.toolPollDispatch(context.Background(), nil, pollDispatchInput{JobID: "no-such-job"})
	if err != nil {
		t.Fatalf("toolPollDispatch: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected a fallback error result for unknown job id")
	}
}

// TestQueryDispatchToHarness_SyncPathUnaffectedByAsyncField is the flag-off
// mirror: Async=false (the zero value / default) must behave byte-for-byte
// like before this change — synchronous, returning a DispatchBatchResult
// directly rather than a job receipt.
func TestQueryDispatchToHarness_SyncPathUnaffectedByAsyncField(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	disp := &fakeAgentDispatcher{canned: &DispatchBatchResult{
		Results: []DispatchResult{{Index: 0, Success: true, Content: "sync-result"}},
	}}
	server := NewMCPServerWithAgentController(cfg, makeNucleus("Cog", "tester"), process, disp)

	result, _, err := server.toolDispatchToHarness(context.Background(), nil, dispatchToHarnessInput{
		Task: "do the thing",
		// Async omitted -> false.
	})
	if err != nil {
		t.Fatalf("toolDispatchToHarness (sync): %v", err)
	}
	var batch DispatchBatchResult
	decodeMCPJSONForAgentTests(t, result, &batch)
	if len(batch.Results) != 1 || batch.Results[0].Content != "sync-result" {
		t.Fatalf("sync path result mismatch: %+v", batch)
	}
}

// waitForTerminalJob polls jobID via toolPollDispatch until it reaches a
// terminal state (done/failed) or a 5s deadline elapses (test failure on
// timeout). Callers that spawn a background dispatch goroutine (Async=true)
// MUST wait it out this way before returning — t.TempDir()'s cleanup runs as
// soon as the test function returns, and a still-running goroutine writing
// to the workspace's .cog/ledger/ races that cleanup (see the comment in
// TestToolDispatchToHarness_AsyncReturnsImmediateJobHandle).
func waitForTerminalJob(t *testing.T, server *MCPServer, jobID string) dispatchJobStatusResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var status dispatchJobStatusResponse
	for time.Now().Before(deadline) {
		pollResult, _, err := server.toolPollDispatch(context.Background(), nil, pollDispatchInput{JobID: jobID})
		if err != nil {
			t.Fatalf("toolPollDispatch: %v", err)
		}
		decodeMCPJSONForAgentTests(t, pollResult, &status)
		if status.Status == string(DispatchJobDone) || status.Status == string(DispatchJobFailed) {
			return status
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach a terminal state within 5s (last status=%q)", jobID, status.Status)
	return status
}

// --- test doubles ------------------------------------------------------------

// blockingDispatcher is an AgentController+AgentDispatcher fake whose
// DispatchToHarness call blocks on release before returning canned. Used to
// deterministically observe the "pending" window of an async dispatch
// before letting the underlying call complete.
type blockingDispatcher struct {
	fakeAgentController
	release chan struct{}
	canned  *DispatchBatchResult
}

func (b *blockingDispatcher) DispatchToHarness(ctx context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.canned, nil
}

// decodeMCPJSONForAgentTests is defined in agent_state_query_test.go (same
// package) and reused here rather than duplicated.
