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
	// #432: default raised from 30s to dispatchTimeoutDefault (240s) — a 30s
	// ceiling canceled every request under realistic local-model latency
	// (2-4 min under prefill load), and the non-streaming call underneath
	// didn't even abort server-side on that cancel. Assert against the named
	// constant so this test tracks intentional future changes to the default
	// rather than re-pinning a magic number.
	if got.TimeoutSeconds != dispatchTimeoutDefault {
		t.Errorf("TimeoutSeconds default not applied, got %d, want %d", got.TimeoutSeconds, dispatchTimeoutDefault)
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

// TestQueryDispatchToHarness_ExplicitModelPreservedAsRequestedModel covers
// issue #430: a model string that isn't one of the recognized routing enum
// values ("", "e4b", "26b") must be preserved as the caller's explicit
// request, not silently collapsed to "e4b". Prior behavior (pre-#430) lost
// the caller's intent right here, before routing ever saw it.
func TestQueryDispatchToHarness_ExplicitModelPreservedAsRequestedModel(t *testing.T) {
	disp := &fakeAgentDispatcher{cannedOk: true}
	req := DispatchRequest{Task: "x", Model: DispatchModel("ornith-1.0-35b")}
	if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := disp.lastReq.RequestedModel; got != "ornith-1.0-35b" {
		t.Errorf("explicit model not preserved in RequestedModel, got %q", got)
	}
}

// TestQueryDispatchToHarness_RecognizedEnumValuesLeaveRequestedModelEmpty
// guards backward compatibility: the legacy "e4b"/"26b"/"" routing values
// must NOT populate RequestedModel — they continue to route through the
// legacy local-LLM probe (Path 3) exactly as before.
func TestQueryDispatchToHarness_RecognizedEnumValuesLeaveRequestedModelEmpty(t *testing.T) {
	for _, m := range []DispatchModel{"", DispatchModelE4B, DispatchModel26B} {
		disp := &fakeAgentDispatcher{cannedOk: true}
		req := DispatchRequest{Task: "x", Model: m}
		if _, err := QueryDispatchToHarness(context.Background(), disp, req); err != nil {
			t.Fatalf("query(model=%q): %v", m, err)
		}
		if got := disp.lastReq.RequestedModel; got != "" {
			t.Errorf("model=%q: expected empty RequestedModel, got %q", m, got)
		}
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
