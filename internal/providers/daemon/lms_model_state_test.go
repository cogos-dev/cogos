// lms_model_state_test.go — unit tests for the daemon-side lms-model-state provider.
//
// Whitebox (package daemon). Exercises the model_state YAML parser, the opt-in
// Health() gate, and the /api/v0/models probe (loading→progressing, prefer the
// loaded row over a not-loaded duplicate) via httptest.
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ── probeModelStateEntry: HTTP path (loading + duplicate-row ordering) ───────────

type msRow struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Ctx   *int   `json:"loaded_context_length"`
}

func msIntp(n int) *int { return &n }

func msModelsServer(t *testing.T, rows ...msRow) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeModelStateEntry_LoadingIsProgressingNotError(t *testing.T) {
	// A model mid-load (state=="loading", nil context) must report progressing,
	// NOT a Degraded-inducing error — a high-context load can take minutes.
	srv := msModelsServer(t, msRow{ID: "target", State: "loading", Ctx: nil})
	e := modelStateEntry{name: "b", endpoint: srv.URL, model: "target", contextLength: 262144}
	progressing, err := probeModelStateEntry(context.Background(), e)
	if err != nil {
		t.Fatalf("loading must not be an error, got %v", err)
	}
	if !progressing {
		t.Fatalf("loading must report progressing=true")
	}
}

func TestProbeModelStateEntry_PrefersLoadedOverDuplicate(t *testing.T) {
	// A not-loaded duplicate ordered BEFORE the loaded row must not shadow it.
	srv := msModelsServer(t,
		msRow{ID: "target", State: "not-loaded", Ctx: nil},
		msRow{ID: "target", State: "loaded", Ctx: msIntp(262144)},
	)
	e := modelStateEntry{name: "b", endpoint: srv.URL, model: "target", contextLength: 262144}
	progressing, err := probeModelStateEntry(context.Background(), e)
	if err != nil {
		t.Fatalf("loaded duplicate must satisfy the probe, got %v", err)
	}
	if progressing {
		t.Fatalf("loaded-at-target must not report progressing")
	}
}

func TestProbeModelStateEntry_WrongContextIsError(t *testing.T) {
	srv := msModelsServer(t, msRow{ID: "target", State: "loaded", Ctx: msIntp(65536)})
	e := modelStateEntry{name: "b", endpoint: srv.URL, model: "target", contextLength: 262144}
	if _, err := probeModelStateEntry(context.Background(), e); err == nil {
		t.Fatalf("wrong loaded context must be an error")
	}
}

// ── parseModelStateEntriesFromYAML ──────────────────────────────────────────────

func TestParseModelStateEntries_Empty(t *testing.T) {
	t.Parallel()
	if got := parseModelStateEntriesFromYAML([]byte("")); len(got) != 0 {
		t.Errorf("empty YAML: got %d; want 0", len(got))
	}
}

func TestParseModelStateEntries_NoOptIn(t *testing.T) {
	t.Parallel()
	// A backend with no model_state, and one with manage:false — neither opts in.
	yaml := `providers:
  lmstudio:
    type: lmstudio
    endpoint: http://localhost:1234
    model: some-model
  eclipse:
    type: openai
    endpoint: http://192.168.10.191:1234
    options:
      model_state:
        manage: false
        model: ornith-1.0-35b
`
	got := parseModelStateEntriesFromYAML([]byte(yaml))
	if len(got) != 0 {
		t.Errorf("no opt-in: got %d; want 0", len(got))
	}
}

func TestParseModelStateEntries_OptIn(t *testing.T) {
	t.Parallel()
	yaml := `providers:
  eclipse:
    type: openai
    endpoint: http://192.168.10.191:1234
    api_key_env: ECLIPSE_API_KEY
    options:
      model_state:
        manage: true
        model: ornith-1.0-35b
        context_length: 262144
`
	got := parseModelStateEntriesFromYAML([]byte(yaml))
	if len(got) != 1 {
		t.Fatalf("got %d; want 1", len(got))
	}
	e := got[0]
	if e.name != "eclipse" {
		t.Errorf("name: got %q", e.name)
	}
	if e.endpoint != "http://192.168.10.191:1234" {
		t.Errorf("endpoint: got %q", e.endpoint)
	}
	if e.apiKeyEnv != "ECLIPSE_API_KEY" {
		t.Errorf("apiKeyEnv: got %q", e.apiKeyEnv)
	}
	if e.model != "ornith-1.0-35b" {
		t.Errorf("model: got %q", e.model)
	}
	if e.contextLength != 262144 {
		t.Errorf("contextLength: got %d; want 262144", e.contextLength)
	}
}

func TestParseModelStateEntries_MalformedYAML(t *testing.T) {
	t.Parallel()
	if got := parseModelStateEntriesFromYAML([]byte("::: not yaml :::")); len(got) != 0 {
		t.Errorf("malformed YAML: got %d; want 0", len(got))
	}
}

// ── Health opt-in gate ──────────────────────────────────────────────────────────

// TestHealthSuspendedWhenNoWorkspace: with no workspace root configured, the
// stub reports Missing (workspace not configured) — a distinct pre-condition.
// With a workspace root but no opted-in backend, it reports Suspended.
func TestHealthSuspendedWhenNoOptIn(t *testing.T) {
	// Set an empty temp workspace (no providers files) so resolveRoot passes but
	// loadModelStateEntries finds nothing.
	SetWorkspaceRoot(t.TempDir())
	t.Cleanup(func() { SetWorkspaceRoot("") })

	p := &lmsModelStateProvider{stubMethods: stubMethods{name: "lms-model-state"}}
	h := p.Health()
	if h.Health != reconcile.HealthSuspended {
		t.Fatalf("no opt-in ⇒ Suspended; got %s (%s)", h.Health, h.Message)
	}
}

func TestType(t *testing.T) {
	t.Parallel()
	p := &lmsModelStateProvider{stubMethods: stubMethods{name: "lms-model-state"}}
	if p.Type() != "lms-model-state" {
		t.Errorf("Type: got %q", p.Type())
	}
}
