// dispatch_capability_gating_test.go — coverage for the harness-loop
// capability gating added to dispatchSlot (local_agent_harness.go). The
// sketch's original "thread identity through the harness" deliverable was
// already ~60% shipped (attribution: DispatchIdentity, subject resolution,
// ledger attribution, per-tool-call trace attribution); the genuine gap
// closed here is honoring the wired capabilityGater when
// IdentityNakedDefault is true — the file's own prior comment named this
// exact gap ("No capability gating here; this is observability metadata
// only").
//
// Test matrix:
//  1. flag on + gater present + bound subject + gater denies a tool
//     -> the denied tool is absent from the tools list sent to the provider.
//  2. flag off (default) -> tools list unchanged regardless of gater/subject
//     (byte-for-byte parity with pre-change behavior, mirroring the chat
//     path's own stated contract).
//  3. flag on + no gater wired (capResolver nil, the honest default — see
//     mcp_sessions_identity.go: SetCapabilityResolver has zero production
//     callers) -> permit-by-default, tools list unchanged.
package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

// This file reuses the fakeCapabilityGater / newFakeCapabilityGater test
// double already defined in mcp_sessions_g2_test.go (same package, PART C
// capability gating tests) rather than redeclaring an equivalent fake.

// toolsFromChatCompletionRequest decodes the "tools" field of an OpenAI-
// compat /v1/chat/completions request body and returns the function names,
// in the order the provider sent them.
func toolsFromChatCompletionRequest(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode chat completion request: %v (body=%s)", err, body)
	}
	names := make([]string, len(payload.Tools))
	for i, tl := range payload.Tools {
		names[i] = tl.Function.Name
	}
	return names
}

// toolCapture is a mutex-guarded recorder for the tools array seen on each
// /v1/chat/completions request, safe for the httptest handler goroutine to
// write to and the test goroutine to read from.
type toolCapture struct {
	mu    sync.Mutex
	calls [][]string
}

func (c *toolCapture) add(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, names)
}

func (c *toolCapture) snapshot() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]string, len(c.calls))
	copy(out, c.calls)
	return out
}

// newRecordingLLMServer returns an httptest server that behaves like a
// minimal OpenAI-compat endpoint (models + chat/completions; honors the
// request's stream field like a real server, per the #432-era harness test
// convention in local_agent_harness_test.go), always responding with plain
// text content (no tool_calls) so the tool loop terminates after one turn.
// Records the tools array from every /v1/chat/completions request into cap.
func newRecordingLLMServer(t *testing.T, model string, cap *toolCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": model, "object": "model"}},
			})
		case "/v1/chat/completions":
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read chat completion request body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			cap.add(toolsFromChatCompletionRequest(t, bodyBytes))

			var body struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(bodyBytes, &body)

			content := "done"
			if body.Stream {
				writeSSECompletion(t, w, content)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "chatcmpl-gate-test",
				"object": "chat.completion",
				"model":  model,
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// dispatchGatingHarness builds a LocalHarnessController wired to a recording
// fake LLM server, with providers.yaml pointing dispatch at it directly (so
// no state-routing ambiguity). Returns the controller and the tool capture.
func dispatchGatingHarness(t *testing.T) (*LocalHarnessController, *toolCapture, *httptest.Server) {
	t.Helper()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	model := "gemma-4-e4b"
	captured := &toolCapture{}
	llm := newRecordingLLMServer(t, model, captured)
	t.Cleanup(llm.Close)

	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  test-harness:
    type: openai
    endpoint: `+llm.URL+`
    model: `+model+`
`)

	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}
	return ctrl, captured, llm
}

// TestDispatchSlot_CapabilityGating_FlagOn_DeniedToolFiltered is the test
// that fails without the change: before capabilityGater() / the gating
// block in dispatchSlot existed, a denied tool would still appear in the
// tools list sent to the provider even with IdentityNakedDefault=true and a
// gater wired, because the identity fields were observability-only.
func TestDispatchSlot_CapabilityGating_FlagOn_DeniedToolFiltered(t *testing.T) {
	ctrl, captured, _ := dispatchGatingHarness(t)
	ctrl.cfg.IdentityNakedDefault = true
	gater := newFakeCapabilityGater()
	gater.deny["bound-user/cog_emit_event"] = struct{}{}
	ctrl.mcpSrv.SetCapabilityResolver(gater)

	batch, err := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "do a thing",
		Provider:       "test-harness",
		N:              1,
		TimeoutSeconds: 10,
		Identity:       DispatchIdentity{Sub: "bound-user"},
	})
	if err != nil {
		t.Fatalf("DispatchToHarness: %v", err)
	}
	if len(batch.Results) != 1 || !batch.Results[0].Success {
		t.Fatalf("dispatch did not succeed: %+v", batch.Results)
	}
	calls := captured.snapshot()
	if len(calls) == 0 {
		t.Fatal("provider was never called; nothing captured")
	}
	tools := calls[0]
	for _, name := range tools {
		if name == "cog_emit_event" {
			t.Fatalf("denied tool cog_emit_event present in tools sent to provider: %v", tools)
		}
	}
	if len(tools) == 0 {
		t.Fatal("expected some tools to remain after filtering (only one of eleven was denied)")
	}
}

// TestDispatchSlot_CapabilityGating_FlagOff_Unchanged mirrors the chat
// path's own stated contract: IdentityNakedDefault=false (the default) must
// leave tool injection byte-for-byte unchanged, even with a gater wired that
// would otherwise deny a tool and a bound subject present.
func TestDispatchSlot_CapabilityGating_FlagOff_Unchanged(t *testing.T) {
	ctrl, captured, _ := dispatchGatingHarness(t)
	// IdentityNakedDefault left at its zero value (false).
	gater := newFakeCapabilityGater()
	gater.deny["bound-user/cog_emit_event"] = struct{}{}
	ctrl.mcpSrv.SetCapabilityResolver(gater)

	batch, err := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "do a thing",
		Provider:       "test-harness",
		N:              1,
		TimeoutSeconds: 10,
		Identity:       DispatchIdentity{Sub: "bound-user"},
	})
	if err != nil {
		t.Fatalf("DispatchToHarness: %v", err)
	}
	calls := captured.snapshot()
	if len(calls) == 0 {
		t.Fatal("provider was never called")
	}
	tools := calls[0]
	found := false
	for _, name := range tools {
		if name == "cog_emit_event" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cog_emit_event to remain present with the dark flag off, got %v", tools)
	}
	_ = batch
}

// TestDispatchSlot_CapabilityGating_NoGaterWired_PermitByDefault covers the
// honest-default case the plan calls out: SetCapabilityResolver has zero
// production callers today, so capResolver is nil at runtime everywhere.
// Flag on + nil gater must permit everything (same as flag off).
func TestDispatchSlot_CapabilityGating_NoGaterWired_PermitByDefault(t *testing.T) {
	ctrl, captured, _ := dispatchGatingHarness(t)
	ctrl.cfg.IdentityNakedDefault = true
	// No SetCapabilityResolver call — capResolver stays nil.

	_, err := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "do a thing",
		Provider:       "test-harness",
		N:              1,
		TimeoutSeconds: 10,
		Identity:       DispatchIdentity{Sub: "bound-user"},
	})
	if err != nil {
		t.Fatalf("DispatchToHarness: %v", err)
	}
	calls := captured.snapshot()
	if len(calls) == 0 {
		t.Fatal("provider was never called")
	}
	tools := calls[0]
	found := false
	for _, name := range tools {
		if name == "cog_emit_event" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected permit-by-default (no gater wired) to leave cog_emit_event present, got %v", tools)
	}
}

// TestDispatchSlot_CapabilityGating_AnonymousSubjectNotGated covers the
// third gate condition: an anonymous dispatch (no Identity.Sub) has no
// envelope to check and must not be gated even with the flag on and a
// gater wired, mirroring the chat path's bound.Bound check.
func TestDispatchSlot_CapabilityGating_AnonymousSubjectNotGated(t *testing.T) {
	ctrl, captured, _ := dispatchGatingHarness(t)
	ctrl.cfg.IdentityNakedDefault = true
	gater := newFakeCapabilityGater()
	gater.deny["anonymous/cog_emit_event"] = struct{}{}
	ctrl.mcpSrv.SetCapabilityResolver(gater)

	// No Identity.Sub supplied -> subject resolves to "anonymous" inside
	// DispatchToHarness (local_agent_harness.go). Even though the fake gater
	// would deny cog_emit_event for "anonymous", the gating block's own
	// subject != "anonymous" condition must skip filtering entirely.
	_, err := ctrl.DispatchToHarness(context.Background(), DispatchRequest{
		Task:           "do a thing",
		Provider:       "test-harness",
		N:              1,
		TimeoutSeconds: 10,
	})
	if err != nil {
		t.Fatalf("DispatchToHarness: %v", err)
	}
	calls := captured.snapshot()
	if len(calls) == 0 {
		t.Fatal("provider was never called")
	}
	tools := calls[0]
	found := false
	for _, name := range tools {
		if name == "cog_emit_event" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected anonymous subject to bypass gating, got %v", tools)
	}
}
