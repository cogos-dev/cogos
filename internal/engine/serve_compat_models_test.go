// serve_compat_models_test.go — tests for the live /v1/models composition
// (handleModels / composeModelsList / liveModelEntries / modelEntryFor) and the
// admission-parity invariant it must uphold with resolve.go.
//
// THE ADMISSION-PARITY INVARIANT under test: every id handleModels advertises
// must satisfy IsKnownModel(router, id) == true, else a client selecting it gets
// a boundary 400 (advertise-then-reject). Each composition test that asserts an
// id is present also asserts IsKnownModel for it.
//
// Covers spec Section 6:
//   - composite ids from an OpenAI-compat ModelLister stub
//   - bare claude ids from a claude-oauth ModelLister stub
//   - a ModelLister whose ListModels errors is SKIPPED (still 200, others present)
//   - a non-lister provider contributes no live ids (only its static/alias path)
//   - embedding-tagged ids
//   - RangeProviders visits every registered provider exactly once
package engine

import (
	"context"
	"errors"
	"testing"
)

// listerStub is a StubProvider that also implements ModelLister, so it is
// live-enumerated by handleModels. ids are returned by ListModels; listErr, when
// set, makes ListModels fail (exercising the graceful-skip path). isLocal picks
// composite (local) vs bare (frontier, when named accordingly) emission.
type listerStub struct {
	*StubProvider
	ids     []string
	listErr error
	calls   int
}

func newListerStub(name string, isLocal bool, ids ...string) *listerStub {
	sp := NewStubProvider(name, "resp")
	sp.capabilities.IsLocal = isLocal
	return &listerStub{StubProvider: sp, ids: ids}
}

func (l *listerStub) ListModels(ctx context.Context) ([]string, error) {
	l.calls++
	if l.listErr != nil {
		return nil, l.listErr
	}
	return l.ids, nil
}

// modelIDs returns the set of ids present in a /v1/models response.
func modelIDSet(resp modelsResponse) map[string]modelsResponseModel {
	m := make(map[string]modelsResponseModel, len(resp.Data))
	for _, e := range resp.Data {
		m[e.ID] = e
	}
	return m
}

// freshModelsServer builds a Server on the given router with an empty models
// cache so each test observes a live composition (the package-level TTL cache is
// keyed per-router; a freshly-constructed router is always cold).
func freshModelsServer(t *testing.T, router Router) *Server {
	t.Helper()
	return newTestServerWithRouter(t, router)
}

// TestHandleModels_CompositeAndBare exercises the core live-composition menu:
// an OpenAI-compat lister → composite ids; a claude-oauth lister → bare claude
// ids; a plain non-lister provider contributes no live id. Every advertised id
// must admit (IsKnownModel).
func TestHandleModels_CompositeAndBare(t *testing.T) {
	t.Parallel()

	// OpenAI-compat local lister serving two ids → composite "<name>/<id>".
	openai := newListerStub("lmstudio-darkstar", true, "gemma-4-26b", "qwen-3-14b")
	// Frontier lister (claude-oauth) → bare claude ids.
	claude := newListerStub("claude-oauth", false, "claude-opus-4-8", "claude-haiku-4-5-20251001")
	// A non-lister local provider — contributes only via alias/static paths.
	plain := NewStubProvider("mlx-lm", "resp")

	router := NewSimpleRouter(RoutingConfig{Default: "claude-oauth"})
	router.RegisterProvider(openai)
	router.RegisterProvider(claude)
	router.RegisterProvider(plain)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	// Composite ids for the OpenAI-compat lister.
	for _, want := range []string{"lmstudio-darkstar/gemma-4-26b", "lmstudio-darkstar/qwen-3-14b"} {
		m, ok := byID[want]
		if !ok {
			t.Errorf("composite id %q missing; got ids %v", want, modelIDKeys(byID))
			continue
		}
		if m.OwnedBy != "cogos:lmstudio-darkstar" {
			t.Errorf("%q owned_by = %q; want cogos:lmstudio-darkstar", want, m.OwnedBy)
		}
		if !IsKnownModel(router, want) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", want)
		}
	}

	// Bare claude ids for the frontier lister.
	for _, want := range []string{"claude-opus-4-8", "claude-haiku-4-5-20251001"} {
		m, ok := byID[want]
		if !ok {
			t.Errorf("bare claude id %q missing; got ids %v", want, modelIDKeys(byID))
			continue
		}
		if m.OwnedBy != "anthropic" {
			t.Errorf("%q owned_by = %q; want anthropic", want, m.OwnedBy)
		}
		if !IsKnownModel(router, want) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", want)
		}
	}

	// The non-lister provider must not have produced any "mlx-lm/..." id.
	for id := range byID {
		if len(id) > len("mlx-lm/") && id[:len("mlx-lm/")] == "mlx-lm/" {
			t.Errorf("non-lister provider produced a live id %q", id)
		}
	}

	// Full admission-parity sweep: every advertised id must admit.
	for _, e := range resp.Data {
		if !IsKnownModel(router, e.ID) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", e.ID)
		}
	}
}

// TestHandleModels_FrontierNonClaudeIDFallsToComposite verifies the parity
// guard in modelEntryFor: a frontier provider that enumerates a NON-claude id
// (defensive — Anthropic returns only claude-* ids today) must emit it composite
// (which admits via ProviderForName), never bare (which would 400 — no admission
// path for a bare non-claude id).
func TestHandleModels_FrontierNonClaudeIDFallsToComposite(t *testing.T) {
	t.Parallel()

	// claude-oauth is frontier-by-name but here reports an odd non-claude id.
	fr := newListerStub("claude-oauth", false, "weird-legacy-id", "claude-opus-4-8")
	router := NewSimpleRouter(RoutingConfig{Default: "claude-oauth"})
	router.RegisterProvider(fr)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	// The claude-prefixed id emits bare.
	if _, ok := byID["claude-opus-4-8"]; !ok {
		t.Errorf("claude-opus-4-8 should emit bare; got %v", modelIDKeys(byID))
	}
	// The non-claude id must NOT be bare.
	if _, bare := byID["weird-legacy-id"]; bare {
		t.Error("non-claude id from frontier provider emitted BARE with no admission path")
	}
	// It must be composite instead.
	if _, ok := byID["claude-oauth/weird-legacy-id"]; !ok {
		t.Errorf("non-claude frontier id should fall back to composite; got %v", modelIDKeys(byID))
	}
	// Admission parity for every advertised id.
	for _, e := range resp.Data {
		if !IsKnownModel(router, e.ID) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", e.ID)
		}
	}
}

// TestHandleModels_ListerErrorSkipped verifies graceful degradation: a provider
// whose ListModels errors is skipped, the endpoint still returns 200, and the
// other providers' ids are still present.
func TestHandleModels_ListerErrorSkipped(t *testing.T) {
	t.Parallel()

	good := newListerStub("lmstudio-darkstar", true, "gemma-4-26b")
	bad := newListerStub("lmstudio-eclipse", true, "ornith-1.0-35b")
	bad.listErr = errors.New("boom: upstream 401")

	router := NewSimpleRouter(RoutingConfig{Default: "lmstudio-darkstar"})
	router.RegisterProvider(good)
	router.RegisterProvider(bad)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv) // fetchModels already asserts 200
	byID := modelIDSet(resp)

	if _, ok := byID["lmstudio-darkstar/gemma-4-26b"]; !ok {
		t.Errorf("healthy provider's id missing after peer error; got %v", modelIDKeys(byID))
	}
	if _, ok := byID["lmstudio-eclipse/ornith-1.0-35b"]; ok {
		t.Error("errored provider's id should have been skipped")
	}
	if bad.calls == 0 {
		t.Error("errored lister was never probed")
	}
}

// TestHandleModels_EmbeddingTagged verifies an embedding model id is listed with
// description "embedding" rather than dropped or presented as a chat model.
func TestHandleModels_EmbeddingTagged(t *testing.T) {
	t.Parallel()

	openai := newListerStub("lmstudio-darkstar", true, "text-embedding-nomic", "gemma-4-26b")
	router := NewSimpleRouter(RoutingConfig{Default: "lmstudio-darkstar"})
	router.RegisterProvider(openai)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	emb, ok := byID["lmstudio-darkstar/text-embedding-nomic"]
	if !ok {
		t.Fatalf("embedding id missing; got %v", modelIDKeys(byID))
	}
	if emb.Description != "embedding" {
		t.Errorf("embedding id description = %q; want %q", emb.Description, "embedding")
	}
	// Non-embedding id must NOT be tagged.
	chat, ok := byID["lmstudio-darkstar/gemma-4-26b"]
	if !ok {
		t.Fatalf("chat id missing; got %v", modelIDKeys(byID))
	}
	if chat.Description == "embedding" {
		t.Errorf("chat id wrongly tagged as embedding")
	}
}

// TestHandleModels_FrontierNameGatesBareEmission verifies the Finding-3 fix: a
// non-local, non-claude ModelLister must emit COMPOSITE ids (which always admit
// via the ProviderForName prefix guard), never bare — because bare ids from a
// non-claude provider would have no admission path.
func TestHandleModels_FrontierNameGatesBareEmission(t *testing.T) {
	t.Parallel()

	// A non-local lister with a non-claude name (the fragile !IsLocal case).
	rogue := newListerStub("some-cloud", false, "mystery-model-1")
	router := NewSimpleRouter(RoutingConfig{Default: "some-cloud"})
	router.RegisterProvider(rogue)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	// Must be emitted composite (owned_by cogos:some-cloud), not bare.
	if _, bare := byID["mystery-model-1"]; bare {
		t.Error("non-claude provider emitted a BARE id with no admission path (advertise-then-reject)")
	}
	composite := "some-cloud/mystery-model-1"
	m, ok := byID[composite]
	if !ok {
		t.Fatalf("expected composite id %q; got %v", composite, modelIDKeys(byID))
	}
	if m.OwnedBy != "cogos:some-cloud" {
		t.Errorf("%q owned_by = %q; want cogos:some-cloud", composite, m.OwnedBy)
	}
	// Admission parity for every advertised id.
	for _, e := range resp.Data {
		if !IsKnownModel(router, e.ID) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", e.ID)
		}
	}
}

// TestHandleModels_HaikuAdmitsUnderClaudeCodeOnly verifies the Finding-1 fix:
// when the frontier tier is present ONLY via a provider named "claude-code"
// (not claude-oauth/anthropic), the static bare claude-haiku-4-5-20251001 id is
// both emitted and admissible — no advertise-then-reject.
func TestHandleModels_HaikuAdmitsUnderClaudeCodeOnly(t *testing.T) {
	t.Parallel()

	cc := newCloudStub("claude-code", "cc resp") // frontier by name, no ModelLister
	router := NewSimpleRouter(RoutingConfig{Default: "claude-code"})
	router.RegisterProvider(cc)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	// The static frontier ids are emitted because isFrontierConfigured matches
	// the claude-code name.
	if _, ok := byID["claude-haiku-4-5-20251001"]; !ok {
		t.Fatalf("claude-haiku-4-5-20251001 not advertised under claude-code-only config; got %v", modelIDKeys(byID))
	}
	// ...and every one of them must admit.
	for _, id := range []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"} {
		if !IsKnownModel(router, id) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false under claude-code-only config", id)
		}
	}
	// Routing parity: the id resolves to the claude-code provider with the id as
	// override.
	res := ResolveModelRequest(router, "claude-haiku-4-5-20251001", "t")
	if res.PreferProvider != "claude-code" {
		t.Errorf("claude-haiku-4-5-20251001 PreferProvider = %q; want claude-code", res.PreferProvider)
	}
	if res.ModelOverride != "claude-haiku-4-5-20251001" {
		t.Errorf("claude-haiku-4-5-20251001 ModelOverride = %q; want the bare id", res.ModelOverride)
	}
}

// TestHandleModels_HaikuAdmitsUnderNonCanonicalSonnetProvider verifies the other
// Finding-1 branch: frontier tier present only via a provider serving
// claude-sonnet-4-6 under a NON-canonical name still admits the static haiku id.
func TestHandleModels_HaikuAdmitsUnderNonCanonicalSonnetProvider(t *testing.T) {
	t.Parallel()

	// Provider named "my-anthropic" that SERVES claude-sonnet-4-6.
	fr := newCloudStub("my-anthropic", "resp")
	fr.model = "claude-sonnet-4-6"
	router := NewSimpleRouter(RoutingConfig{Default: "my-anthropic"})
	router.RegisterProvider(fr)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	if _, ok := byID["claude-haiku-4-5-20251001"]; !ok {
		t.Fatalf("haiku id not advertised via non-canonical sonnet provider; got %v", modelIDKeys(byID))
	}
	for _, e := range resp.Data {
		if !IsKnownModel(router, e.ID) {
			t.Errorf("ADMISSION PARITY: advertised %q but IsKnownModel=false", e.ID)
		}
	}
	// Routing target is the non-canonical provider (frontierProviderName falls
	// through to the ProviderForModel(sonnet) match).
	res := ResolveModelRequest(router, "claude-haiku-4-5-20251001", "t")
	if res.PreferProvider != "my-anthropic" {
		t.Errorf("haiku PreferProvider = %q; want my-anthropic", res.PreferProvider)
	}
}

// TestRangeProviders_VisitsEachOnce asserts RangeProviders visits every
// registered provider exactly once, in Name() order.
func TestRangeProviders_VisitsEachOnce(t *testing.T) {
	t.Parallel()

	router := NewSimpleRouter(RoutingConfig{})
	names := []string{"charlie", "alpha", "bravo"}
	for _, n := range names {
		router.RegisterProvider(NewStubProvider(n, "r"))
	}

	seen := map[string]int{}
	var order []string
	router.RangeProviders(func(p Provider) {
		seen[p.Name()]++
		order = append(order, p.Name())
	})

	if len(order) != len(names) {
		t.Fatalf("RangeProviders visited %d providers; want %d (%v)", len(order), len(names), order)
	}
	for _, n := range names {
		if seen[n] != 1 {
			t.Errorf("provider %q visited %d times; want 1", n, seen[n])
		}
	}
	// Name() order (SimpleRouter sorts on registration).
	want := []string{"alpha", "bravo", "charlie"}
	for i, n := range want {
		if order[i] != n {
			t.Errorf("visit order[%d] = %q; want %q (%v)", i, order[i], n, order)
		}
	}
}

func modelIDKeys(m map[string]modelsResponseModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
