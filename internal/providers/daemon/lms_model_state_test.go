// lms_model_state_test.go — unit tests for the daemon-side lms-model-state provider.
//
// Whitebox (package daemon). Exercises the model_state YAML parser and the
// opt-in Health() gate. No live LM Studio; the network probe is not exercised
// here (Health() over a real backend lives in the engine-layer tests).
package daemon

import (
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

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
