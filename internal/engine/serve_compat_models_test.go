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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	// Respect caller cancellation like a real ctx-aware HTTP probe would, so a
	// cancelled/expired caller context makes this stub fail (skipped by
	// liveModelEntries) rather than returning ids unconditionally.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// compatModelIDs indexes a composed list by id for presence checks.
func compatModelIDs(ms []compatModel) map[string]bool {
	out := make(map[string]bool, len(ms))
	for _, m := range ms {
		out[m.ID] = true
	}
	return out
}

// TestComposeModelsList_CancelledCallerDoesNotPoisonCache pins the #441-class
// guard: a caller that cancels mid-compose cancels the in-flight per-provider
// probes (their ctx descends from the caller's), so a healthy provider is skipped
// and the composed list comes back incomplete. That incomplete list must NOT be
// written to the shared 45s TTL cache — the next caller with a healthy context
// must still receive the full menu. Without the guard, the first (cancelled) call
// would cache the degraded list and the second call would serve it for up to 45s.
func TestComposeModelsList_CancelledCallerDoesNotPoisonCache(t *testing.T) {
	// No t.Parallel: reasons about the per-router package cache entry. The entry
	// is keyed by this router alone, so it does not race other tests.
	lister := newListerStub("lmstudio-darkstar", true, "gemma-4-26b", "qwen-3-14b")
	frontier := newListerStub("claude-oauth", false, "claude-opus-4-8")

	router := NewSimpleRouter(RoutingConfig{Default: "claude-oauth"})
	router.RegisterProvider(lister)
	router.RegisterProvider(frontier)

	// First call under an already-cancelled caller context: the live probes see
	// ctx.Err() and are skipped, so the healthy lister's composite ids are absent.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	first := compatModelIDs(composeModelsList(cancelled, router))
	if first["lmstudio-darkstar/gemma-4-26b"] {
		t.Fatalf("precondition failed: cancelled-caller probe should have been skipped; got %v", first)
	}

	// Second call under a healthy context: because the cancelled build was NOT
	// cached, this must rebuild cleanly and surface the healthy lister's models.
	second := compatModelIDs(composeModelsList(context.Background(), router))
	for _, want := range []string{"lmstudio-darkstar/gemma-4-26b", "lmstudio-darkstar/qwen-3-14b"} {
		if !second[want] {
			t.Errorf("cache poisoned by cancelled caller: %q missing after healthy rebuild; got %v", want, second)
		}
	}
}

// ── #518: context_length propagation ────────────────────────────────────────

// contextListerStub is a StubProvider that implements ModelContextLister
// (rather than plain ModelLister) so /v1/models composition exercises the
// context_length-carrying path (#518). listings are returned verbatim by
// ListModelsWithContext; listErr, when set, makes it fail (graceful-skip path).
type contextListerStub struct {
	*StubProvider
	listings []ModelListing
	listErr  error
}

func newContextListerStub(name string, isLocal bool, listings ...ModelListing) *contextListerStub {
	sp := NewStubProvider(name, "resp")
	sp.capabilities.IsLocal = isLocal
	return &contextListerStub{StubProvider: sp, listings: listings}
}

func (l *contextListerStub) ListModelsWithContext(ctx context.Context) ([]ModelListing, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.listErr != nil {
		return nil, l.listErr
	}
	return l.listings, nil
}

// TestHandleModels_ContextLengthPropagated verifies the core #518 fix: a
// provider that reports a per-model context window via ModelContextLister has
// that window surfaced as `context_length` on the composed /v1/models entry.
func TestHandleModels_ContextLengthPropagated(t *testing.T) {
	t.Parallel()

	lister := newContextListerStub("lmstudio-eclipse", true,
		ModelListing{ID: "ornith-1.0-35b", ContextLength: 32768},
	)
	router := NewSimpleRouter(RoutingConfig{Default: "lmstudio-eclipse"})
	router.RegisterProvider(lister)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)
	byID := modelIDSet(resp)

	m, ok := byID["lmstudio-eclipse/ornith-1.0-35b"]
	if !ok {
		t.Fatalf("composite id missing; got %v", modelIDKeys(byID))
	}
	if m.ContextLength != 32768 {
		t.Errorf("ContextLength = %d; want 32768", m.ContextLength)
	}
}

// TestHandleModels_ContextLengthAbsentIsOmitted verifies the design
// constraint from #518: a model with no known context window must have the
// `context_length` JSON key OMITTED entirely, never emitted as 0 or a guessed
// default — a client that sees no field can fall back sanely; one that sees a
// wrong number cannot tell it apart from a real answer.
func TestHandleModels_ContextLengthAbsentIsOmitted(t *testing.T) {
	t.Parallel()

	// A plain ModelLister (no context metadata available at all) alongside a
	// ModelContextLister entry that itself reports an unknown (0) window —
	// both must omit the field on the wire.
	plain := newListerStub("lmstudio-darkstar", true, "gemma-4-26b")
	unknownCtx := newContextListerStub("some-vllm", true,
		ModelListing{ID: "mystery-model", ContextLength: 0},
	)
	router := NewSimpleRouter(RoutingConfig{Default: "lmstudio-darkstar"})
	router.RegisterProvider(plain)
	router.RegisterProvider(unknownCtx)

	srv := freshModelsServer(t, router)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	srv.handleModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleModels: status = %d; want 200", w.Code)
	}

	// Decode into a generic map so a present-but-zero key is distinguishable
	// from an absent key (unlike decoding into a typed struct field).
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	for _, entry := range raw.Data {
		id, _ := entry["id"].(string)
		if id != "lmstudio-darkstar/gemma-4-26b" && id != "some-vllm/mystery-model" {
			continue
		}
		if _, present := entry["context_length"]; present {
			t.Errorf("entry %q: context_length key present in JSON with no known window; want omitted entirely; entry=%+v", id, entry)
		}
	}
}

// TestOpenAICompatListModelsWithContext_PrefersLoadedOverMax verifies the
// #518 design constraint at the provider layer: when LM Studio's
// /api/v0/models reports both a loaded_context_length and a larger
// max_context_length, ListModelsWithContext must report the LOADED value —
// advertising the checkpoint's theoretical max when a smaller context is
// actually loaded is exactly the bug #518 exists to fix.
func TestOpenAICompatListModelsWithContext_PrefersLoadedOverMax(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":                    "ornith-1.0-35b",
					"state":                 "loaded",
					"loaded_context_length": 32768,
					"max_context_length":    262144,
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "ornith-1.0-35b")
	listings, err := p.ListModelsWithContext(context.Background())
	if err != nil {
		t.Fatalf("ListModelsWithContext: %v", err)
	}
	if len(listings) != 1 {
		t.Fatalf("listings = %v; want 1 entry", listings)
	}
	if listings[0].ContextLength != 32768 {
		t.Errorf("ContextLength = %d; want 32768 (loaded, not the 262144 max)", listings[0].ContextLength)
	}
}

// TestOpenAICompatListModelsWithContext_FallsBackToMaxWhenNotLoaded verifies
// the fallback half: a model reported by /api/v0/models with no
// loaded_context_length (not currently loaded) still yields a usable —
// though approximate — context_length from max_context_length, rather than
// silently dropping the field.
func TestOpenAICompatListModelsWithContext_FallsBackToMaxWhenNotLoaded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":                 "gemma-4-26b",
					"state":              "not-loaded",
					"max_context_length": 131072,
				},
			},
		})
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "gemma-4-26b")
	listings, err := p.ListModelsWithContext(context.Background())
	if err != nil {
		t.Fatalf("ListModelsWithContext: %v", err)
	}
	if len(listings) != 1 || listings[0].ContextLength != 131072 {
		t.Fatalf("listings = %v; want 1 entry with ContextLength 131072", listings)
	}
}

// TestOpenAICompatListModelsWithContext_FallsBackToPlainListingOnNonLMStudio
// verifies graceful degradation against a non-LM-Studio OpenAI-compat server
// (vLLM, llama.cpp): /api/v0/models 404s, so ListModelsWithContext must fall
// back to the standard /v1/models id-only listing rather than erroring the
// provider out of the /v1/models menu entirely.
func TestOpenAICompatListModelsWithContext_FallsBackToPlainListingOnNonLMStudio(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			http.NotFound(w, r)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("llama-3-8b"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "llama-3-8b")
	listings, err := p.ListModelsWithContext(context.Background())
	if err != nil {
		t.Fatalf("ListModelsWithContext: %v", err)
	}
	if len(listings) != 1 || listings[0].ID != "llama-3-8b" {
		t.Fatalf("listings = %v; want 1 entry with id llama-3-8b", listings)
	}
	if listings[0].ContextLength != 0 {
		t.Errorf("ContextLength = %d; want 0 (unknown — non-LM-Studio server has no comparable field)", listings[0].ContextLength)
	}
}

// TestOpenAICompatListModelsWithContext_SlowProbeDoesNotStarveFallback is the
// #519 review regression: liveModelEntries hands ListModelsWithContext a
// single bounded ctx meant to cover ONE real upstream call (the pre-#518
// contract every OpenAI-compat provider relied on). Before the budget-split
// fix, the /api/v0/models probe and its /v1/models fallback shared that same
// ctx, so a non-LM-Studio backend that is merely SLOW to 404 (a loaded proxy,
// a remote host — exactly who the fallback exists to serve) could burn most
// of the budget on the failed probe alone, leaving the real fallback call too
// little time to complete and silently dropping the provider from /v1/models
// (logged at slog.Debug, where nobody looks).
//
// This test reproduces exactly that shape with a ctx budget generous enough
// to complete a NORMAL fallback call, but too tight to survive a slow probe
// PLUS the fallback both drawing from the same pool:
//   - total ctx budget:            1200ms
//   - /api/v0/models (the probe):  sleeps 1000ms, then 404s
//   - /v1/models (the fallback):   sleeps 500ms, then succeeds
//
// Pre-fix: the probe alone consumes ~1000ms of the shared 1200ms ctx, leaving
// ~200ms for a fallback call that needs 500ms — it times out and
// ListModelsWithContext returns an error, exactly the "silently vanishes"
// failure the review flagged. Post-fix: the probe is capped at its own
// apiV0ModelsProbeTimeout sub-budget (modelsPerProviderTimeout/4 = 500ms in
// production), so it is canceled well before its fake 1000ms sleep completes;
// the fallback then runs against the ORIGINAL ctx, which still has the
// majority of the 1200ms left — comfortably more than the 500ms it needs.
func TestOpenAICompatListModelsWithContext_SlowProbeDoesNotStarveFallback(t *testing.T) {
	t.Parallel()

	const (
		totalBudget  = 1200 * time.Millisecond
		probeDelay   = 1000 * time.Millisecond
		fallbackWait = 500 * time.Millisecond
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			// Simulate a non-LM-Studio server that is slow to answer 404 (a
			// loaded proxy, a remote/off-LAN host) rather than an instant one.
			select {
			case <-time.After(probeDelay):
			case <-r.Context().Done():
				return
			}
			http.NotFound(w, r)
		case "/v1/models":
			select {
			case <-time.After(fallbackWait):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(openaiModelsResponseJSON("llama-3-8b"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newTestOpenAIProvider(t, srv.URL, "llama-3-8b")

	ctx, cancel := context.WithTimeout(context.Background(), totalBudget)
	defer cancel()

	listings, err := p.ListModelsWithContext(ctx)
	if err != nil {
		t.Fatalf("ListModelsWithContext: %v; the slow probe starved the fallback's share of the shared %s budget (probe delay %s, fallback needs %s)",
			err, totalBudget, probeDelay, fallbackWait)
	}
	if len(listings) != 1 || listings[0].ID != "llama-3-8b" {
		t.Fatalf("listings = %v; want 1 entry with id llama-3-8b", listings)
	}
}

// TestHandleModels_AllEntriesCarryContextLength is the #518 completion
// regression: on a production-shaped router (frontier claude-oauth lister +
// local context-lister), EVERY advertised entry — intent aliases (foreground,
// deliberation, local), static frontier ids, and live-enumerated ids — must
// carry a context_length. Anthropic entries get the shared 200k constant;
// the "local" alias inherits the loaded window of the provider it resolves to.
func TestHandleModels_AllEntriesCarryContextLength(t *testing.T) {
	t.Parallel()

	claude := newListerStub("claude-oauth", false, "claude-opus-4-8", "claude-sonnet-5")
	local := newContextListerStub("lmstudio-darkstar", true,
		ModelListing{ID: "gemma-4-26b", ContextLength: 32768},
	)
	// The "local" alias resolves to lmstudio-darkstar (resolve.go); its
	// configured model determines which live listing the alias inherits.
	local.capabilities.ModelsAvailable = []string{"gemma-4-26b"}

	router := NewSimpleRouter(RoutingConfig{Default: "claude-oauth"})
	router.RegisterProvider(claude)
	router.RegisterProvider(local)

	srv := freshModelsServer(t, router)
	resp := fetchModels(t, srv)

	for _, e := range resp.Data {
		if e.ContextLength <= 0 {
			t.Errorf("entry %q: no context_length advertised (#518)", e.ID)
		}
	}

	byID := modelIDSet(resp)
	for _, id := range []string{"foreground", "deliberation", "claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001", "claude-opus-4-8", "claude-sonnet-5"} {
		if m, ok := byID[id]; !ok || m.ContextLength != 200_000 {
			t.Errorf("entry %q: ContextLength = %d (present=%v); want 200000", id, m.ContextLength, ok)
		}
	}
	if m, ok := byID["local"]; !ok || m.ContextLength != 32768 {
		t.Errorf("local alias: ContextLength = %d (present=%v); want 32768 (loaded window of resolved provider)", m.ContextLength, ok)
	}
}
