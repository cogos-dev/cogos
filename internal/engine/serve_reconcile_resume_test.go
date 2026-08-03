// serve_reconcile_resume_test.go — outside-in httptest coverage for
// POST /v1/reconcile/{type}/resume.
//
// Covers, in the same style as TestServiceMutation_* (serve_services_mutations_test.go)
// / TestConfigMutation_* (serve_config_gate_test.go):
//
//  1. Gate — 403 when EnableReconcileControl=false (default).
//  2. Not-found — 404 for a provider type the daemon does not have.
//  3. No daemon wired — 404 when SetReconcileDaemon was never called.
//  4. Real effect — 200, and the quarantine the request claims to lift is
//     actually gone from the live daemon afterward (not just echoed back),
//     closing the exact gap cog-review flagged: `cogos reconcile <type>`
//     looked like it did this but never touched the running daemon.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newReconcileResumeTestServer returns an HTTP handler wired to daemon (may be
// nil, to exercise the no-daemon-wired path) with EnableReconcileControl set
// as requested.
func newReconcileResumeTestServer(t *testing.T, daemon *ReconcileDaemon, enableReconcileControl bool) http.Handler {
	t.Helper()
	root := t.TempDir()
	cfg := makeConfig(t, root)
	cfg.EnableReconcileControl = enableReconcileControl
	nucleus := makeNucleus("Test", "tester")
	proc := NewProcess(cfg, nucleus)

	srv := NewServer(cfg, nucleus, proc)
	if daemon != nil {
		srv.SetReconcileDaemon(daemon)
	}
	t.Cleanup(func() {
		if b := proc.Broker(); b != nil {
			_ = b.Close()
		}
	})
	return srv.Handler()
}

// postResume issues POST /v1/reconcile/{type}/resume.
func postResume(handler http.Handler, providerType string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/reconcile/"+providerType+"/resume", nil)
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeResumeResp(t *testing.T, rec *httptest.ResponseRecorder) reconcileResumeResponse {
	t.Helper()
	var resp reconcileResumeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode resume response: %v; body=%q", err, rec.Body.String())
	}
	return resp
}

// quarantinedTestDaemon builds a daemon over one always-failing provider and
// drives ticks until it is quarantined (mirrors
// TestReconcileDaemon_QuarantineDoesNotAlterHealthOrClearTheEpisode's setup
// in reconcile_daemon_backoff_test.go). Returns the daemon and the provider's
// type string.
func quarantinedTestDaemon(t *testing.T) (*ReconcileDaemon, string) {
	t.Helper()
	p := newFlakyReconcilable("resume-target")
	d := newBackoffTestDaemon(t, p, 3)

	ctx := context.Background()
	for i := 0; i < 80; i++ {
		d.runTick(ctx)
	}
	if !d.isQuarantined(p.Type()) {
		t.Fatalf("precondition failed: provider %q not quarantined after 80 ticks", p.Type())
	}
	return d, p.Type()
}

// TestReconcileResume_GateDisabled verifies 403 with a "disabled" error when
// EnableReconcileControl is false (the default) — matching the
// EnableSkillExec / EnableServiceControl / EnableConfigMutation convention.
func TestReconcileResume_GateDisabled(t *testing.T) {
	t.Parallel()
	d, providerType := quarantinedTestDaemon(t)
	handler := newReconcileResumeTestServer(t, d, false)

	rec := postResume(handler, providerType)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q; want 403", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q; want application/json", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%q", err, rec.Body.String())
	}
	if resp["error"] != "disabled" {
		t.Errorf("error=%q; want \"disabled\"", resp["error"])
	}
	if !strings.Contains(resp["detail"], "enable_reconcile_control") {
		t.Errorf("detail=%q; want mention of enable_reconcile_control", resp["detail"])
	}

	// The gate must reject before touching daemon state.
	if !d.isQuarantined(providerType) {
		t.Error("provider was un-quarantined despite the gate being closed")
	}
}

// TestReconcileResume_NotFound verifies 404 for a provider type the daemon
// does not have, with the gate open.
func TestReconcileResume_NotFound(t *testing.T) {
	t.Parallel()
	d, _ := quarantinedTestDaemon(t)
	handler := newReconcileResumeTestServer(t, d, true)

	rec := postResume(handler, "no-such-provider")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q; want 404", rec.Code, rec.Body.String())
	}
	resp := decodeResumeResp(t, rec)
	if resp.Success {
		t.Error("success=true for an unknown provider type")
	}
	if !strings.Contains(resp.Error, "no-such-provider") {
		t.Errorf("error=%q; want mention of the unknown provider type", resp.Error)
	}
}

// TestReconcileResume_NoDaemonWired verifies 404 when SetReconcileDaemon was
// never called — the same "nothing observed" default the GET convergence/
// coherence routes use, not a panic.
func TestReconcileResume_NoDaemonWired(t *testing.T) {
	t.Parallel()
	handler := newReconcileResumeTestServer(t, nil, true)

	rec := postResume(handler, "anything")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q; want 404", rec.Code, rec.Body.String())
	}
}

// TestReconcileResume_LiftsQuarantine is the load-bearing test: a resume
// request against a quarantined provider must actually lift the live
// daemon's quarantine (d.isQuarantined false afterward), not just report
// success. This is the gap cog-review found — `cogos reconcile <type>` looks
// like it does this but runs in an independent process and never touches
// d.quarantined at all.
func TestReconcileResume_LiftsQuarantine(t *testing.T) {
	t.Parallel()
	d, providerType := quarantinedTestDaemon(t)
	handler := newReconcileResumeTestServer(t, d, true)

	rec := postResume(handler, providerType)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q; want 200", rec.Code, rec.Body.String())
	}
	resp := decodeResumeResp(t, rec)
	if !resp.Success {
		t.Fatalf("success=false; want true: %+v", resp)
	}
	if resp.ProviderType != providerType {
		t.Errorf("provider_type=%q; want %q", resp.ProviderType, providerType)
	}
	if resp.Provider == nil {
		t.Fatal("provider field missing from response")
	}
	if resp.Provider.Quarantined {
		t.Error("response reports Quarantined=true right after resume; want false")
	}

	// The real assertion: the LIVE daemon's quarantine state actually
	// changed, not just the HTTP response's echo of it.
	if d.isQuarantined(providerType) {
		t.Error("daemon still reports the provider quarantined after resume")
	}
}
