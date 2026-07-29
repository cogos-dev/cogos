// serve_agents_test.go — coverage for the HTTP dispatch-job surface added
// alongside the async job-handle registry (dispatch_jobs.go): GET
// /v1/dispatch-jobs/{id} (handleDispatchJobGet) and the async=true branch of
// POST /v1/agents/{id}/dispatch (handleAgentDispatch). Both were previously
// untested despite being called out in the PR description as the "primary"
// poll surface (HTTP callers without MCP access use this; cog_poll_dispatch
// is the MCP-side mirror already covered by dispatch_jobs_test.go).
//
// Tests route through srv.Handler() (the real corsMiddleware(mux) stack) so
// path values ({id}) are resolved exactly as they are in production, rather
// than calling the handler methods directly and faking r.PathValue.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- GET /v1/dispatch-jobs/{id} ---------------------------------------------

// TestHandleDispatchJobGet_HappyPathReturnsJobState drives a real async
// dispatch through the MCP tool (so a genuine job lands in the registry) and
// then polls it via the HTTP surface, confirming the shape matches the
// MCP-side poll response (dispatchJobStatusResponse — same struct, same
// json tags) and eventually reflects the terminal result.
func TestHandleDispatchJobGet_HappyPathReturnsJobState(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	block := make(chan struct{})
	disp := &blockingDispatcher{
		release: block,
		canned:  &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "the answer"}}},
	}
	srv.SetAgentController(disp)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer wired; dispatch job registry cannot be reached")
	}

	receipt := srv.mcpServer.startAsyncDispatch(context.Background(), DispatchRequest{Task: "do the thing"})
	if receipt.JobID == "" {
		t.Fatal("startAsyncDispatch returned an empty job id")
	}

	// Poll while still in flight (fake dispatcher blocked on `release`).
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/dispatch-jobs/"+receipt.JobID, nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("in-flight poll status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	var inFlight dispatchJobStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&inFlight); err != nil {
		t.Fatalf("decode in-flight body: %v", err)
	}
	if inFlight.JobID != receipt.JobID {
		t.Errorf("in-flight JobID = %q, want %q", inFlight.JobID, receipt.JobID)
	}
	if inFlight.Status != string(DispatchJobPending) && inFlight.Status != string(DispatchJobRunning) {
		t.Errorf("in-flight Status = %q, want pending or running", inFlight.Status)
	}

	// Release the fake dispatcher and poll again until terminal.
	close(block)
	final := pollHTTPUntilTerminal(t, srv, receipt.JobID)
	if final.Status != string(DispatchJobDone) {
		t.Fatalf("final Status = %q, want %q (error=%q)", final.Status, DispatchJobDone, final.Error)
	}
	if final.Result == nil || len(final.Result.Results) != 1 || final.Result.Results[0].Content != "the answer" {
		t.Fatalf("final Result mismatch: %+v", final.Result)
	}
	if final.CreatedAt == "" || final.UpdatedAt == "" {
		t.Errorf("expected non-empty CreatedAt/UpdatedAt timestamps, got %+v", final)
	}
}

// TestHandleDispatchJobGet_UnknownIDReturns404 guards the not-found path:
// an id that was never registered (or has been GC'd past its TTL) must come
// back as 404 with a JSON error body carrying code="not_found", matching
// writeAgentHTTPError's AgentControllerError{Code:"not_found"} contract used
// elsewhere on this surface (e.g. handleAgentGet).
func TestHandleDispatchJobGet_UnknownIDReturns404(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/dispatch-jobs/no-such-job", nil)
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != "not_found" {
		t.Errorf(`body["code"] = %v, want "not_found"`, body["code"])
	}
	if body["error"] == nil || body["error"] == "" {
		t.Error("expected a non-empty error message")
	}
}

// TestHandleDispatchJobGet_MCPServerNilReturns503 covers the defensive guard
// for a Server built without registerMCPRoutes ever wiring s.mcpServer
// (mirrors handleAgentDispatch's identical guard a few lines above it in
// serve_agents.go).
func TestHandleDispatchJobGet_MCPServerNilReturns503(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := &Server{cfg: cfg, nucleus: makeNucleus("Cog", "tester"), process: process}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/dispatch-jobs/anything", nil)
	req.SetPathValue("id", "anything")
	srv.handleDispatchJobGet(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503; body=%s", w.Code, w.Body.String())
	}
}

// --- POST /v1/agents/{id}/dispatch (async=true branch) ----------------------

// TestHandleAgentDispatch_AsyncReturnsImmediateReceipt is the primary
// regression test for the untested HTTP async dispatch branch: posting with
// "async":true must return 202 Accepted with a job receipt immediately —
// not block until the (slow/blocked) dispatcher finishes — and the job must
// be pollable via GET /v1/dispatch-jobs/{id} afterwards.
func TestHandleAgentDispatch_AsyncReturnsImmediateReceipt(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	block := make(chan struct{})
	disp := &blockingDispatcher{
		release: block,
		canned:  &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true, Content: "done"}}},
	}
	srv.SetAgentController(disp)

	body := `{"task":"do the thing","async":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+DefaultAgentID+"/dispatch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// This call must return promptly even though the fake dispatcher is
	// blocked on `release` — if the async branch didn't fire (e.g. the
	// async field were ignored and fell through to the synchronous
	// QueryDispatchToHarness call), this ServeHTTP call would hang until the
	// test's own deadline, which is exactly the regression this test would
	// catch.
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 Accepted; body=%s", w.Code, w.Body.String())
	}
	var receipt dispatchJobReceipt
	if err := json.NewDecoder(w.Body).Decode(&receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if receipt.JobID == "" {
		t.Fatal("expected non-empty job_id in async receipt")
	}
	if receipt.Status != string(DispatchJobPending) {
		t.Errorf("receipt.Status = %q, want %q", receipt.Status, DispatchJobPending)
	}

	// Confirm it is reachable via the poll surface, then let it finish so
	// the goroutine doesn't race t.TempDir() cleanup.
	pollW := httptest.NewRecorder()
	pollReq := httptest.NewRequest(http.MethodGet, "/v1/dispatch-jobs/"+receipt.JobID, nil)
	srv.Handler().ServeHTTP(pollW, pollReq)
	if pollW.Code != http.StatusOK {
		t.Fatalf("poll status = %d; want 200; body=%s", pollW.Code, pollW.Body.String())
	}

	close(block)
	final := pollHTTPUntilTerminal(t, srv, receipt.JobID)
	if final.Status != string(DispatchJobDone) {
		t.Fatalf("final Status = %q, want %q (error=%q)", final.Status, DispatchJobDone, final.Error)
	}
}

// TestHandleAgentDispatch_AsyncWithoutMCPServerReturns503 covers
// handleAgentDispatch's guard: async=true with no MCP server wired (job
// registry unreachable) must fail loudly with 503, not silently fall through
// to a synchronous dispatch or panic on a nil s.mcpServer dereference.
func TestHandleAgentDispatch_AsyncWithoutMCPServerReturns503(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	process := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := &Server{cfg: cfg, nucleus: makeNucleus("Cog", "tester"), process: process}
	srv.agentController = &fakeAgentDispatcher{cannedOk: true}

	body := `{"task":"do the thing","async":true}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+DefaultAgentID+"/dispatch", strings.NewReader(body))
	req.SetPathValue("id", DefaultAgentID)
	req.Header.Set("Content-Type", "application/json")
	srv.handleAgentDispatch(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleAgentDispatch_SyncPathUnaffected is the flag-off sibling: async
// omitted (defaults false) must behave as a normal synchronous dispatch,
// returning 200 with a DispatchBatchResult body — confirming the new async
// branch is additive and doesn't disturb the existing behavior this route
// already had.
func TestHandleAgentDispatch_SyncPathUnaffected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	disp := &fakeAgentDispatcher{canned: &DispatchBatchResult{
		Results: []DispatchResult{{Index: 0, Success: true, Content: "sync-result"}},
	}}
	srv.SetAgentController(disp)

	body := `{"task":"do the thing"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+DefaultAgentID+"/dispatch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	var batch DispatchBatchResult
	if err := json.NewDecoder(w.Body).Decode(&batch); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Content != "sync-result" {
		t.Fatalf("sync result mismatch: %+v", batch)
	}
}

// --- POST /v1/agents/{id}/dispatch (target_node cluster routing) -----------
//
// Regression coverage for #490: handleAgentDispatch called the un-routed
// QueryDispatchToHarness (router hard-coded nil) instead of
// QueryDispatchToHarnessRouted(..., s.bepEngine, ...), so a target_node
// dispatch over HTTP always failed cluster_disabled even when the BEP engine
// was up and the MCP surface (toolDispatchToHarness) routed the identical
// request fine. Mirrors cluster_dispatch_test.go's router-nil case, but
// exercised through the real HTTP handler rather than
// QueryDispatchToHarnessRouted directly.

// TestHandleAgentDispatch_TargetNodeWithoutClusterReturnsClusterDisabled
// covers the defect: a Server with no BEP engine wired (s.bepEngine == nil,
// the default for every test server and any node with cluster.enabled=false)
// must still fail fast with code="cluster_disabled" for a target_node
// request over HTTP, matching the MCP tool's behavior — not hang, not panic
// on a nil-pointer RemoteDispatch call, and not silently run the dispatch
// locally.
func TestHandleAgentDispatch_TargetNodeWithoutClusterReturnsClusterDisabled(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	disp := &fakeAgentDispatcher{cannedOk: true}
	srv.SetAgentController(disp)

	if srv.bepEngine != nil {
		t.Fatal("test server unexpectedly has a bepEngine wired; test assumes cluster is dark")
	}

	body := `{"task":"do the thing","target_node":"nodeB"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+DefaultAgentID+"/dispatch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(w, req)

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["code"] != "cluster_disabled" {
		t.Fatalf(`body["code"] = %v, want "cluster_disabled" (status=%d, body=%v)`, resp["code"], w.Code, resp)
	}
	// The local dispatcher must never have been invoked for a target_node
	// request — a nil router is a hard fail-fast, not a silent local fallback.
	if disp.lastReq.Task != "" {
		t.Errorf("local dispatcher was invoked (lastReq=%+v); target_node request should fail before reaching it", disp.lastReq)
	}
}

// TestHandleAgentDispatch_EmptyTargetNodeUsesLocalPath is the sibling
// regression guard: omitting target_node must keep using the local dispatch
// path exactly as before the #490 fix — the new router plumbing must not
// change behavior for the (overwhelmingly common) non-cluster request shape.
func TestHandleAgentDispatch_EmptyTargetNodeUsesLocalPath(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	disp := &fakeAgentDispatcher{canned: &DispatchBatchResult{
		Results: []DispatchResult{{Index: 0, Success: true, Content: "local-result"}},
	}}
	srv.SetAgentController(disp)

	body := `{"task":"do the thing"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/"+DefaultAgentID+"/dispatch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	var batch DispatchBatchResult
	if err := json.NewDecoder(w.Body).Decode(&batch); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Content != "local-result" {
		t.Fatalf("local result mismatch: %+v", batch)
	}
	if disp.lastReq.TargetNode != "" {
		t.Errorf("lastReq.TargetNode = %q, want empty", disp.lastReq.TargetNode)
	}
}

// pollHTTPUntilTerminal polls GET /v1/dispatch-jobs/{id} through srv.Handler()
// until the job reaches a terminal state (done/failed) or a 5s deadline
// elapses. Mirrors dispatch_jobs_test.go's waitForTerminalJob (MCP side) —
// duplicated rather than shared because it drives a different transport
// (real HTTP handler vs. direct tool-method call).
func pollHTTPUntilTerminal(t *testing.T, srv *Server, jobID string) dispatchJobStatusResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var status dispatchJobStatusResponse
	for time.Now().Before(deadline) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/dispatch-jobs/"+jobID, nil)
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("poll status = %d; want 200; body=%s", w.Code, w.Body.String())
		}
		if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
			t.Fatalf("decode poll body: %v", err)
		}
		if status.Status == string(DispatchJobDone) || status.Status == string(DispatchJobFailed) {
			return status
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach a terminal state within 5s (last status=%q)", jobID, status.Status)
	return status
}
