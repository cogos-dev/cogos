// agent_dispatch_query_test.go — coverage for the engine-side validation,
// normalization, and controller-availability checks. The concrete dispatcher
// lives in the root package; here we exercise only the contract that engine
// callers can rely on.
package engine

import (
	"context"
	"strings"
	"testing"
)

// fakeAgentDispatcher is a fakeAgentController extension that satisfies
// AgentDispatcher. It records the last DispatchRequest it observed and
// returns a canned batch.
type fakeAgentDispatcher struct {
	fakeAgentController
	lastReq  DispatchRequest
	canned   *DispatchBatchResult
	cannedOk bool
}

func (f *fakeAgentDispatcher) DispatchToHarness(_ context.Context, req DispatchRequest) (*DispatchBatchResult, error) {
	f.lastReq = req
	if f.canned != nil {
		return f.canned, nil
	}
	if !f.cannedOk {
		return nil, &AgentControllerError{Code: "internal", Message: "no canned response set"}
	}
	return &DispatchBatchResult{Results: []DispatchResult{{Index: 0, Success: true}}}, nil
}

func TestQueryDispatchToHarness_NormalizationDefaults(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "do thing"}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	got := disp.lastReq
	if got.AgentID != DefaultAgentID {
		t.Errorf("AgentID default not applied, got %q", got.AgentID)
	}
	if got.Model != DispatchModelE4B {
		t.Errorf("Model default not applied, got %q", got.Model)
	}
	if got.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds default not applied, got %d", got.TimeoutSeconds)
	}
	if got.N != 1 {
		t.Errorf("N default not applied, got %d", got.N)
	}
}

func TestQueryDispatchToHarness_ClampsRanges(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "x", N: 99, TimeoutSeconds: 99999}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	if disp.lastReq.N != 4 {
		t.Errorf("N not clamped to 4, got %d", disp.lastReq.N)
	}
	if disp.lastReq.TimeoutSeconds != 300 {
		t.Errorf("TimeoutSeconds not clamped to 300, got %d", disp.lastReq.TimeoutSeconds)
	}
}

func TestQueryDispatchToHarness_UnknownModelDefaultsToE4B(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "x", Model: DispatchModel("frobnicate")}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	if disp.lastReq.Model != DispatchModelE4B {
		t.Errorf("unknown model not normalized to e4b, got %q", disp.lastReq.Model)
	}
}

func TestQueryDispatchToHarness_EmptyTaskRejected(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	if _, err := QueryDispatchToHarness(context.Background(), disp, DispatchRequest{Task: ""}); err == nil {
		t.Fatal("expected error for empty task")
	}
	if _, err := QueryDispatchToHarness(context.Background(), disp, DispatchRequest{Task: "   "}); err == nil {
		t.Fatal("expected error for whitespace-only task")
	}
}

func TestQueryDispatchToHarness_NilControllerUnavailable(t *testing.T) {
	_, err := QueryDispatchToHarness(context.Background(), nil, DispatchRequest{Task: "x"})
	if err == nil {
		t.Fatal("expected ErrAgentUnavailable")
	}
	ace, ok := err.(*AgentControllerError)
	if !ok || ace.Code != "unavailable" {
		t.Errorf("expected unavailable error, got %v", err)
	}
}

func TestQueryDispatchToHarness_ControllerWithoutDispatcher(t *testing.T) {
	// Plain fakeAgentController doesn't satisfy AgentDispatcher; the
	// query helper should report unavailable rather than panic.
	plain := &fakeAgentController{}
	_, err := QueryDispatchToHarness(context.Background(), plain, DispatchRequest{Task: "x"})
	if err == nil {
		t.Fatal("expected error for non-dispatch controller")
	}
	if !strings.Contains(err.Error(), "does not support dispatch") {
		t.Errorf("expected does-not-support message, got %v", err)
	}
}

// RFC-0007 Layer 1: Provider field flows through Normalize without
// mutation. The Normalize step only trims; unknown-name validation is
// deferred to the dispatcher, which holds the live ProviderResolver.
func TestQueryDispatchToHarness_ProviderFieldFlowsThrough(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "do thing", Provider: "  desktop  "}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := disp.lastReq.Provider; got != "desktop" {
		t.Errorf("Provider not trimmed/preserved, got %q", got)
	}
}

func TestQueryDispatchToHarness_EmptyProviderUnchanged(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "do thing"}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := disp.lastReq.Provider; got != "" {
		t.Errorf("Provider should default empty, got %q", got)
	}
}

func TestDispatchRequest_NormalizeDedupesTools(t *testing.T) {
	req := DispatchRequest{Task: "x", Tools: []string{"a", " ", "a", "b", "", "b"}}
	if err := req.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got, want := req.Tools, []string{"a", "b"}; !equalStrings(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
