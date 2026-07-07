// provider_lms_model_state_test.go — unit tests for LMSModelStateProvider.
//
// Test discipline (mirrors provider_mlx_supervised_test.go):
//   - Package engine (whitebox)
//   - t.TempDir() for filesystem isolation
//   - No sleeping; no live LM Studio; no real load/unload
//   - The actuator is replaced with a FAKE script that records argv+env to a
//     file so ApplyPlan tests assert the exact invocation WITHOUT loading a model
//     (GUARDRAIL 1).
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

// modelsJSON is a raw /api/v0/models body builder. Each entry is (id, state,
// loadedCtx, maxCtx); a negative loadedCtx omits the field entirely (the
// not-loaded/null case).
type modelFixture struct {
	id, state string
	loadedCtx int // <0 ⇒ omit loaded_context_length
	maxCtx    int
	typ       string
}

func modelsHandler(fixtures ...modelFixture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			http.NotFound(w, r)
			return
		}
		var data []map[string]any
		for _, f := range fixtures {
			row := map[string]any{
				"id":                 f.id,
				"state":              f.state,
				"max_context_length": f.maxCtx,
			}
			if f.typ != "" {
				row["type"] = f.typ
			}
			if f.loadedCtx >= 0 {
				row["loaded_context_length"] = f.loadedCtx
			}
			// else: omit the key entirely — the null/not-loaded case.
			data = append(data, row)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "object": "list"})
	})
}

// makeLMSProvider builds a managed provider pointed at endpoint with a target.
func makeLMSProvider(t *testing.T, endpoint, model string, ctxLen int) *LMSModelStateProvider {
	t.Helper()
	cfg := ProviderConfig{
		Type:     "lmstudio",
		Endpoint: endpoint,
		Options: map[string]interface{}{
			"model_state": map[string]interface{}{
				"manage":         true,
				"model":          model,
				"context_length": ctxLen,
			},
		},
	}
	p, err := newLMSModelStateProvider("lms-test", cfg, "tok", t.TempDir())
	if err != nil {
		t.Fatalf("newLMSModelStateProvider: %v", err)
	}
	// Point the actuator at a fake so Health()'s install-check passes and
	// ApplyPlan runs the fake. Individual tests may override.
	p.actuatorScript = writeFakeActuator(t)
	p.nodeBin = "/bin/sh" // run the fake as a shell script
	p.local = false       // force SDK-actuator path (not lms CLI)
	return p
}

// writeFakeActuator writes a shell script that records argv + the token env var
// to <dir>/actuator-calls.log and exits 0. It NEVER contacts a backend.
func writeFakeActuator(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "actuator-calls.log")
	script := "#!/bin/sh\n" +
		"echo \"ARGV: $@\" >> \"" + logPath + "\"\n" +
		"echo \"TOKEN: ${LMS_ACTUATOR_TOKEN}\" >> \"" + logPath + "\"\n" +
		"echo '{\"ok\":true}'\n"
	path := filepath.Join(dir, "fake-actuator.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake actuator: %v", err)
	}
	return path
}

func readActuatorLog(t *testing.T, actuatorScript string) string {
	t.Helper()
	logPath := filepath.Join(filepath.Dir(actuatorScript), "actuator-calls.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "" // no calls
	}
	return string(data)
}

// ── FetchLive: parsing incl. the null loaded_context_length case ────────────────

func TestFetchLiveParsesNullContext(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "loaded-model", state: "loaded", loadedCtx: 262144, maxCtx: 262144},
		modelFixture{id: "cold-model", state: "not-loaded", loadedCtx: -1, maxCtx: 262144}, // null
	))
	defer srv.Close()

	p := makeLMSProvider(t, srv.URL, "loaded-model", 262144)
	live, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	rows, ok := live.([]lmsModelRow)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %#v", live)
	}
	// Loaded row has a non-nil pointer.
	loaded := findModelRow(rows, "loaded-model")
	if loaded == nil || loaded.LoadedContextLength == nil || *loaded.LoadedContextLength != 262144 {
		t.Errorf("loaded row context: got %v, want 262144", loaded)
	}
	// not-loaded row's loaded_context_length must be nil (not 0) — the null case.
	cold := findModelRow(rows, "cold-model")
	if cold == nil || cold.LoadedContextLength != nil {
		t.Errorf("not-loaded row should have nil LoadedContextLength, got %v", cold.LoadedContextLength)
	}
}

func TestFetchLiveNullDoesNotShadowLoaded(t *testing.T) {
	// A not-loaded duplicate id must not shadow the loaded one when matching.
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "dup", state: "not-loaded", loadedCtx: -1, maxCtx: 262144},
		modelFixture{id: "dup", state: "loaded", loadedCtx: 262144, maxCtx: 262144},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "dup", 262144)
	live, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	rows := live.([]lmsModelRow)
	r := findModelRow(rows, "dup")
	if r == nil || r.State != "loaded" {
		t.Errorf("findModelRow must prefer the loaded row, got %#v", r)
	}
}

// ── ComputePlan: drift states ──────────────────────────────────────────────────

func TestComputePlanLoadWhenAbsent(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	rows := []lmsModelRow{{ID: "other", State: "loaded", LoadedContextLength: ip(4096)}}
	plan, err := p.ComputePlan(&p.target, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || !strings.HasSuffix(plan.Actions[0].Name, "/load") {
		t.Fatalf("expected one /load action, got %#v", plan.Actions)
	}
}

func TestComputePlanContextDriftUnloadReload(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	rows := []lmsModelRow{{ID: "target", State: "loaded", LoadedContextLength: ip(65536)}}
	plan, err := p.ComputePlan(&p.target, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || !strings.HasSuffix(plan.Actions[0].Name, "/context") {
		t.Fatalf("expected one /context action, got %#v", plan.Actions)
	}
}

func TestComputePlanSyncedEmptyPlan(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	rows := []lmsModelRow{{ID: "target", State: "loaded", LoadedContextLength: ip(262144)}}
	plan, err := p.ComputePlan(&p.target, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("expected empty plan when at target, got %#v", plan.Actions)
	}
}

func TestComputePlanJITEvict(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	p.target.JITEvict = true
	rows := []lmsModelRow{
		{ID: "target", State: "loaded", LoadedContextLength: ip(262144)},
		{ID: "crowder", State: "loaded", LoadedContextLength: ip(8192)},
	}
	plan, err := p.ComputePlan(&p.target, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawUnload bool
	for _, a := range plan.Actions {
		if strings.HasSuffix(a.Name, "/unload") && a.Details["model"] == "crowder" {
			sawUnload = true
		}
	}
	if !sawUnload {
		t.Fatalf("expected /unload of crowder under jit_evict, got %#v", plan.Actions)
	}
}

func TestComputePlanOptOutEmpty(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	p.target.Manage = false
	rows := []lmsModelRow{{ID: "other", State: "loaded", LoadedContextLength: ip(4096)}}
	plan, err := p.ComputePlan(&p.target, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("opt-out must yield empty plan, got %#v", plan.Actions)
	}
}

// ── Health: three-axis mapping ─────────────────────────────────────────────────

func TestHealthSyncedWhenLoadedAtTarget(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "target", state: "loaded", loadedCtx: 262144, maxCtx: 262144},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "target", 262144)
	mustFetch(t, p)
	h := p.Health()
	assertHealth(t, h, reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

func TestHealthDegradedOnWrongContext(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "target", state: "loaded", loadedCtx: 65536, maxCtx: 262144},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "target", 262144)
	mustFetch(t, p)
	h := p.Health()
	assertHealth(t, h, reconcile.SyncStatusOutOfSync, reconcile.HealthDegraded)
}

func TestHealthProgressingWhenLoading(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "target", state: "loading", loadedCtx: -1, maxCtx: 262144},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "target", 262144)
	mustFetch(t, p)
	h := p.Health()
	if h.Health != reconcile.HealthProgressing {
		t.Fatalf("loading ⇒ Progressing; got %s", h.Health)
	}
}

func TestHealthMissingWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "other", state: "loaded", loadedCtx: 4096, maxCtx: 4096},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "target", 262144)
	mustFetch(t, p)
	h := p.Health()
	assertHealth(t, h, reconcile.SyncStatusOutOfSync, reconcile.HealthMissing)
}

func TestHealthSuspendedWhenNotManaged(t *testing.T) {
	p := makeLMSProvider(t, "http://x", "target", 262144)
	p.target.Manage = false
	h := p.Health()
	if h.Health != reconcile.HealthSuspended {
		t.Fatalf("unmanaged ⇒ Suspended; got %s", h.Health)
	}
}

func TestHealthSuspendedWhenUnreachable(t *testing.T) {
	// Point at a closed port — FetchLive errors, Health maps to Suspended (NOT
	// Degraded): don't self-heal an unreachable box.
	p := makeLMSProvider(t, "http://127.0.0.1:1", "target", 262144)
	_, err := p.FetchLive(context.Background(), nil)
	if err == nil {
		t.Fatal("expected FetchLive to fail against closed port")
	}
	h := p.Health()
	if h.Health != reconcile.HealthSuspended {
		t.Fatalf("unreachable ⇒ Suspended (not Degraded); got %s (%s)", h.Health, h.Message)
	}
}

func TestHealthSuspendedWhenActuatorMissing(t *testing.T) {
	srv := httptest.NewServer(modelsHandler(
		modelFixture{id: "target", state: "loaded", loadedCtx: 262144, maxCtx: 262144},
	))
	defer srv.Close()
	p := makeLMSProvider(t, srv.URL, "target", 262144)
	p.actuatorScript = filepath.Join(t.TempDir(), "does-not-exist.mjs")
	p.local = false
	p.lmsCLI = "" // no CLI fallback
	mustFetch(t, p)
	h := p.Health()
	if h.Health != reconcile.HealthSuspended {
		t.Fatalf("missing actuator ⇒ Suspended; got %s (%s)", h.Health, h.Message)
	}
}

// ── ApplyPlan: invokes the FAKE actuator with correct argv+env, no real load ──

func TestApplyPlanInvokesActuatorWithArgvAndEnv(t *testing.T) {
	p := makeLMSProvider(t, "http://192.168.10.191:1234", "target-model", 262144)
	plan := &reconcile.Plan{
		ResourceType: lmsModelStateType,
		Actions: []reconcile.Action{{
			Action:       reconcile.ActionUpdate,
			ResourceType: lmsModelStateType,
			Name:         p.name + "/load",
			Details: map[string]any{
				"model":          "target-model",
				"context_length": 262144,
			},
		}},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplySucceeded {
		t.Fatalf("expected one succeeded result, got %#v", results)
	}
	log := readActuatorLog(t, p.actuatorScript)
	// argv assertions: op, host, port, model, context-length.
	// NOTE: --parallel is intentionally absent: LM Studio's SDK load config has no
	// per-load parallelism knob, so the actuator does not accept/forward it.
	for _, want := range []string{
		"load", "--host 192.168.10.191", "--port 1234",
		"--model target-model", "--context-length 262144",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("actuator argv missing %q; log:\n%s", want, log)
		}
	}
	// Token must arrive via ENV, never argv.
	if !strings.Contains(log, "TOKEN: tok") {
		t.Errorf("token not passed via LMS_ACTUATOR_TOKEN env; log:\n%s", log)
	}
	if strings.Contains(log, "tok") && strings.Contains(log, "ARGV:") {
		argvLine := firstLine(log, "ARGV:")
		if strings.Contains(argvLine, "tok") {
			t.Errorf("token leaked into argv: %q", argvLine)
		}
	}
}

// A future actuator footgun: prints {"ok":false} but exits 0 (dropped await /
// swallowed catch). ApplyPlan must NOT report ApplySucceeded — the result-line
// parse folds the actuator's error into an ApplyFailed.
func TestApplyPlanFailsOnOkFalseWithZeroExit(t *testing.T) {
	p := makeLMSProvider(t, "http://192.168.10.191:1234", "target-model", 262144)
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo '{\"ok\":false,\"error\":\"load timed out\"}'\n" +
		"exit 0\n"
	path := filepath.Join(dir, "fake-actuator-okfalse.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake actuator: %v", err)
	}
	p.actuatorScript = path

	plan := &reconcile.Plan{
		ResourceType: lmsModelStateType,
		Actions: []reconcile.Action{{
			Action:  reconcile.ActionUpdate,
			Name:    p.name + "/load",
			Details: map[string]any{"model": "target-model", "context_length": 262144},
		}},
	}
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != reconcile.ApplyFailed {
		t.Fatalf("expected ApplyFailed on ok=false/exit-0, got %#v", results)
	}
	if !strings.Contains(results[0].Error, "load timed out") {
		t.Errorf("expected actuator error folded into result, got %q", results[0].Error)
	}
}

func TestApplyPlanContextActionUsesSetContext(t *testing.T) {
	p := makeLMSProvider(t, "http://192.168.10.191:1234", "target-model", 262144)
	plan := &reconcile.Plan{
		ResourceType: lmsModelStateType,
		Actions: []reconcile.Action{{
			Action: reconcile.ActionUpdate,
			Name:   p.name + "/context",
			Details: map[string]any{
				"model":          "target-model",
				"context_length": 262144,
			},
		}},
	}
	if _, err := p.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	log := readActuatorLog(t, p.actuatorScript)
	if !strings.Contains(log, "set-context") {
		t.Errorf("context action should invoke set-context; log:\n%s", log)
	}
}

// ── construction / parsing ─────────────────────────────────────────────────────

func TestParseModelStateOptions(t *testing.T) {
	opts := map[string]interface{}{
		"model_state": map[string]interface{}{
			"manage":         true,
			"model":          "m",
			"context_length": 262144,
			"parallel":       4,
			"keep_warm":      true,
			"jit_evict":      false,
		},
	}
	c := parseModelStateOptions(opts)
	if !c.Manage || c.Model != "m" || c.ContextLength != 262144 || c.Parallel != 4 || !c.KeepWarm || c.JITEvict {
		t.Fatalf("parse mismatch: %#v", c)
	}
}

func TestLocalDetection(t *testing.T) {
	cfg := ProviderConfig{Endpoint: "http://127.0.0.1:1234", Options: map[string]interface{}{
		"model_state": map[string]interface{}{"manage": true, "model": "m"},
	}}
	p, _ := newLMSModelStateProvider("x", cfg, "", "")
	if !p.local {
		t.Errorf("127.0.0.1 should be detected as local")
	}
	cfg.Endpoint = "http://192.168.10.191:1234"
	p2, _ := newLMSModelStateProvider("x", cfg, "", "")
	if p2.local {
		t.Errorf("192.168.10.191 should NOT be local")
	}
	if p2.host != "192.168.10.191" || p2.port != 1234 {
		t.Errorf("host/port parse: got %s:%d", p2.host, p2.port)
	}
	if p2.wsURL != "ws://192.168.10.191:1234" {
		t.Errorf("wsURL: got %s", p2.wsURL)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func ip(v int) *int { return &v }

func mustFetch(t *testing.T, p *LMSModelStateProvider) {
	t.Helper()
	if _, err := p.FetchLive(context.Background(), nil); err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
}

func assertHealth(t *testing.T, h reconcile.ResourceStatus, sync reconcile.SyncStatus, health reconcile.HealthStatus) {
	t.Helper()
	if h.Sync != sync || h.Health != health {
		t.Fatalf("health: got (%s,%s), want (%s,%s): %s", h.Sync, h.Health, sync, health, h.Message)
	}
}

func firstLine(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// Guard: this file must compile on all platforms (no darwin-only deps).
var _ = runtime.GOOS
var _ = fmt.Sprint
