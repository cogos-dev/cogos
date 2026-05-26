// router_test.go — SimpleRouter unit tests
package engine

import (
	"context"
	"path/filepath"
	"testing"
)

// ── Registration ──────────────────────────────────────────────────────────────

func TestRouterRegisterDeregister(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "a"})

	a := NewStubProvider("a", "from-a")
	b := NewStubProvider("b", "from-b")
	r.RegisterProvider(a)
	r.RegisterProvider(b)

	if len(r.providers) != 2 {
		t.Fatalf("providers len = %d; want 2", len(r.providers))
	}

	r.DeregisterProvider("a")
	if len(r.providers) != 1 {
		t.Fatalf("after deregister providers len = %d; want 1", len(r.providers))
	}
	if r.providers[0].Name() != "b" {
		t.Errorf("remaining provider = %q; want b", r.providers[0].Name())
	}
}

// ── Route ─────────────────────────────────────────────────────────────────────

func TestRouterSelectsDefault(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "alpha"})
	r.RegisterProvider(NewStubProvider("alpha", "reply"))

	req := &CompletionRequest{Metadata: RequestMetadata{RequestID: "r1"}}
	p, dec, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "alpha" {
		t.Errorf("selected = %q; want alpha", p.Name())
	}
	if dec.SelectedProvider != "alpha" {
		t.Errorf("decision provider = %q; want alpha", dec.SelectedProvider)
	}
	if dec.FallbackUsed {
		t.Error("FallbackUsed should be false for default selection")
	}
}

func TestRouterFallbackWhenPrimaryUnavailable(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{
		Default:       "primary",
		FallbackChain: []string{"primary", "backup"},
	})

	primary := NewStubProvider("primary", "")
	primary.available = false // simulate down

	backup := NewStubProvider("backup", "backup reply")
	r.RegisterProvider(primary)
	r.RegisterProvider(backup)

	p, dec, err := r.Route(context.Background(), &CompletionRequest{
		Metadata: RequestMetadata{RequestID: "r2"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "backup" {
		t.Errorf("selected = %q; want backup", p.Name())
	}
	if !dec.FallbackUsed {
		t.Error("FallbackUsed should be true")
	}
	if dec.FallbackFrom != "primary" {
		t.Errorf("FallbackFrom = %q; want primary", dec.FallbackFrom)
	}
}

func TestRouterErrorWhenNoneAvailable(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "p"})

	p := NewStubProvider("p", "")
	p.available = false
	r.RegisterProvider(p)

	_, _, err := r.Route(context.Background(), &CompletionRequest{
		Metadata: RequestMetadata{RequestID: "r3"},
	})
	if err == nil {
		t.Error("expected error when no provider is available")
	}
}

func TestRouterProcessStateOverride(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{
		Default: "cloud",
		ProcessStateRouting: map[string]string{
			"consolidating": "local",
		},
		FallbackChain: []string{"cloud", "local"},
	})

	cloud := NewStubProvider("cloud", "cloud reply")
	local := NewStubProvider("local", "local reply")
	r.RegisterProvider(cloud)
	r.RegisterProvider(local)

	req := &CompletionRequest{
		Metadata: RequestMetadata{
			RequestID:    "r4",
			ProcessState: "consolidating",
		},
	}
	p, _, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// "local" should be preferred for consolidating state.
	if p.Name() != "local" {
		t.Errorf("selected = %q; want local (process_state_routing override)", p.Name())
	}
}

func TestRouterCapabilityFilter(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "basic"})

	basic := NewStubProvider("basic", "")
	basic.capabilities = ProviderCapabilities{
		Capabilities: []Capability{CapJSON},
		IsLocal:      true,
	}
	full := NewStubProvider("full", "")
	full.capabilities = ProviderCapabilities{
		Capabilities: []Capability{CapJSON, CapToolUse},
		IsLocal:      true,
	}
	r.RegisterProvider(basic)
	r.RegisterProvider(full)

	req := &CompletionRequest{
		Metadata: RequestMetadata{
			RequestID:            "r5",
			RequiredCapabilities: []Capability{CapToolUse},
		},
	}
	p, _, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "full" {
		t.Errorf("selected = %q; want full (has tool_use)", p.Name())
	}
}

// ── Stats ─────────────────────────────────────────────────────────────────────

func TestRouterStats(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "p"})
	r.RegisterProvider(NewStubProvider("p", "reply"))

	req := &CompletionRequest{Metadata: RequestMetadata{RequestID: "s1"}}
	for range 3 {
		if _, _, err := r.Route(context.Background(), req); err != nil {
			t.Fatalf("Route: %v", err)
		}
	}

	stats := r.Stats()
	if stats.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d; want 3", stats.TotalRequests)
	}
	if stats.RequestsByProvider["p"] != 3 {
		t.Errorf("RequestsByProvider[p] = %d; want 3", stats.RequestsByProvider["p"])
	}
	if stats.SovereigntyRatio != 1.0 {
		t.Errorf("SovereigntyRatio = %f; want 1.0 (local only)", stats.SovereigntyRatio)
	}
}

// ── makeProvider ─────────────────────────────────────────────────────────────

func TestMakeProviderOllama(t *testing.T) {
	t.Parallel()
	p, err := makeProvider("ollama", ProviderConfig{Type: "ollama", Model: "qwen2.5:9b"}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	if p.Name() != "ollama" {
		t.Errorf("name = %q; want ollama", p.Name())
	}
}

func TestMakeProviderStub(t *testing.T) {
	t.Parallel()
	p, err := makeProvider("stub", ProviderConfig{Type: "stub"}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	if p.Name() != "stub" {
		t.Errorf("name = %q; want stub", p.Name())
	}
}

func TestMakeProviderUnknown(t *testing.T) {
	t.Parallel()
	_, err := makeProvider("x", ProviderConfig{Type: "unknown_type"}, nil)
	if err == nil {
		t.Error("expected error for unknown provider type")
	}
}

func TestMakeProviderInfersTypeFromName(t *testing.T) {
	t.Parallel()
	// No Type field — should infer "ollama" from name.
	p, err := makeProvider("ollama", ProviderConfig{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	if _, ok := p.(*OllamaProvider); !ok {
		t.Errorf("expected OllamaProvider, got %T", p)
	}
}

// TestMakeProviderVLLM verifies that type "vllm" is registered and routes to
// the OpenAI-compatible provider implementation. vLLM exposes the OpenAI
// /v1/chat/completions and /v1/models contract; no dedicated provider type
// is required for the unsupervised case.
func TestMakeProviderVLLM(t *testing.T) {
	t.Parallel()
	p, err := makeProvider("vllm-local", ProviderConfig{
		Type:     "vllm",
		Endpoint: "http://localhost:8000",
		Model:    "gemma4:e4b",
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider(vllm): %v", err)
	}
	if _, ok := p.(*OpenAICompatProvider); !ok {
		t.Errorf("vllm should resolve to *OpenAICompatProvider, got %T", p)
	}
	if p.Name() != "vllm-local" {
		t.Errorf("Name() = %q; want vllm-local", p.Name())
	}
}

// ── defaultProvidersConfig ────────────────────────────────────────────────────

func TestDefaultProvidersConfig(t *testing.T) {
	t.Parallel()
	pcfg := defaultProvidersConfig(defaultOllamaModel)
	// defaultProvidersConfig probes LM Studio (:1234) then Ollama (:11434) and
	// selects whichever is reachable. On machines with neither running it
	// returns the no-local-model placeholder. Any of these three outcomes is valid.
	switch {
	case pcfg.Providers["ollama"].Type == "ollama":
		if pcfg.Providers["ollama"].Model != defaultOllamaModel {
			t.Errorf("ollama model = %q; want %q", pcfg.Providers["ollama"].Model, defaultOllamaModel)
		}
		if pcfg.Routing.Default != "ollama" {
			t.Errorf("routing default = %q; want ollama", pcfg.Routing.Default)
		}
	case pcfg.Providers["lmstudio"].Type == "openai-compat":
		if pcfg.Routing.Default != "lmstudio" {
			t.Errorf("routing default = %q; want lmstudio", pcfg.Routing.Default)
		}
	case pcfg.Providers["no-local-model"].Type == "stub":
		// No local backend reachable — placeholder config is correct.
	default:
		t.Errorf("unexpected default providers config: %+v", pcfg.Providers)
	}
}

func TestLoadProvidersConfigAppliesExplicitLocalModel(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "kernel.yaml"), "local_model: gemma4:e2b\n")
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    enabled: true
    endpoint: "http://localhost:11434"
    model: "qwen3.5:9b"
    timeout: 60
routing:
  default: ollama
  fallback_chain:
    - ollama
`)

	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	pcfg, err := loadProvidersConfig(cfg)
	if err != nil {
		t.Fatalf("loadProvidersConfig: %v", err)
	}

	if pcfg.Providers["ollama"].Model != "gemma4:e2b" {
		t.Errorf("ollama model = %q; want gemma4:e2b", pcfg.Providers["ollama"].Model)
	}
}

// TestLoadProvidersConfigDeepMergesLocalYAML verifies that providers.local.yaml
// is deep-merged over providers.yaml — node-specific endpoints, env-var-backed
// API keys, and additional providers all land in the merged config without
// requiring the user to copy the entire defaults file.
func TestLoadProvidersConfigDeepMergesLocalYAML(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    enabled: true
    endpoint: "http://localhost:11434"
    model: "gemma4:e4b"
    timeout: 60
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 300
routing:
  default: claude-code
  fallback_chain: [claude-code, ollama]
  process_state_routing:
    active: claude-code
    receptive: ollama
`)

	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.local.yaml"), `providers:
  ollama:
    context_window: 32768
  lmstudio-mlx:
    type: openai
    endpoint: http://localhost:1234
    model: gemma-mlx-id
    api_key_env: LMS_API_KEY
    options:
      is_local: true
routing:
  fallback_chain: [claude-code, lmstudio-mlx, ollama]
  process_state_routing:
    consolidating: lmstudio-mlx
`)

	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	pcfg, err := loadProvidersConfig(cfg)
	if err != nil {
		t.Fatalf("loadProvidersConfig: %v", err)
	}

	// Existing provider field-level merge: ollama keeps base type/endpoint,
	// gains context_window from overlay.
	ollama := pcfg.Providers["ollama"]
	if ollama.Type != "ollama" {
		t.Errorf("ollama type = %q; want preserved 'ollama'", ollama.Type)
	}
	if ollama.Endpoint != "http://localhost:11434" {
		t.Errorf("ollama endpoint changed unexpectedly: %q", ollama.Endpoint)
	}
	if ollama.ContextWindow != 32768 {
		t.Errorf("ollama context_window = %d; want 32768 from overlay", ollama.ContextWindow)
	}

	// New provider added wholesale.
	lms, ok := pcfg.Providers["lmstudio-mlx"]
	if !ok {
		t.Fatalf("lmstudio-mlx not added by overlay; providers=%v", keysOf(pcfg.Providers))
	}
	if lms.Type != "openai" || lms.Endpoint != "http://localhost:1234" || lms.APIKeyEnv != "LMS_API_KEY" {
		t.Errorf("lmstudio-mlx fields wrong: %+v", lms)
	}
	if v, ok := lms.Options["is_local"].(bool); !ok || !v {
		t.Errorf("lmstudio-mlx options.is_local not preserved: %v", lms.Options)
	}

	// Routing merged: fallback_chain replaced, process_state_routing extended
	// (active/receptive preserved from base, consolidating added by overlay).
	if got := pcfg.Routing.FallbackChain; len(got) != 3 || got[1] != "lmstudio-mlx" {
		t.Errorf("fallback_chain = %v; want overlay value", got)
	}
	if pcfg.Routing.ProcessStateRouting["active"] != "claude-code" {
		t.Error("process_state_routing.active lost during merge")
	}
	if pcfg.Routing.ProcessStateRouting["consolidating"] != "lmstudio-mlx" {
		t.Errorf("process_state_routing.consolidating = %q; want overlay value",
			pcfg.Routing.ProcessStateRouting["consolidating"])
	}

	// Untouched provider preserved.
	if pcfg.Providers["claude-code"].Type != "claude-code" {
		t.Error("claude-code provider lost during merge")
	}
}

// TestLoadProvidersConfigSkipsMissingLocalYAML verifies that a missing
// providers.local.yaml is not an error — the base config is returned as-is.
func TestLoadProvidersConfigSkipsMissingLocalYAML(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    model: gemma4:e4b
routing:
  default: ollama
`)
	// Deliberately do NOT write providers.local.yaml.

	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	pcfg, err := loadProvidersConfig(cfg)
	if err != nil {
		t.Fatalf("loadProvidersConfig: %v", err)
	}
	if pcfg.Providers["ollama"].Model != "gemma4:e4b" {
		t.Errorf("base config not returned cleanly: %+v", pcfg.Providers["ollama"])
	}
}

func keysOf(m map[string]ProviderConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── ProviderForName / ProviderForModel ───────────────────────────────────────

// modelStub wraps a StubProvider but overrides Model() to return a fixed string.
// Used to test ProviderForModel resolution without modifying StubProvider.
type modelStub struct {
	*StubProvider
	model string
}

func (m *modelStub) Model() string { return m.model }

// TestProviderForName_AliasHit verifies that when req.Model matches a registered
// provider's Name(), ProviderForName returns (name, true) so the caller knows
// NOT to set a ModelOverride — the provider already knows its configured model.
func TestProviderForName_AliasHit(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "mlx-gemma"})
	r.RegisterProvider(NewStubProvider("mlx-gemma", ""))

	name, ok := r.ProviderForName("mlx-gemma")
	if !ok {
		t.Fatal("ProviderForName: expected hit, got miss")
	}
	if name != "mlx-gemma" {
		t.Errorf("ProviderForName: got %q; want mlx-gemma", name)
	}
}

// TestProviderForName_Miss verifies that a string that is not a registered
// provider name returns ("", false), so the caller falls through to ModelOverride.
func TestProviderForName_Miss(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{})
	r.RegisterProvider(NewStubProvider("mlx-gemma", ""))

	_, ok := r.ProviderForName("gemma-3-4b-it")
	if ok {
		t.Error("ProviderForName: expected miss for non-name model string, got hit")
	}
}

// TestProviderForModel_HitSetsPreferProvider verifies that when req.Model is not
// a provider alias (ProviderForName miss) but matches a provider's Model() string,
// ProviderForModel returns (name, true) — the caller should set PreferProvider AND
// ModelOverride so the targeted provider gets the explicit model id.
func TestProviderForModel_HitSetsPreferProvider(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{Default: "cloud"})
	cloud := &modelStub{StubProvider: NewStubProvider("cloud", ""), model: "claude-opus-4"}
	r.RegisterProvider(cloud)

	name, ok := r.ProviderForModel("claude-opus-4")
	if !ok {
		t.Fatal("ProviderForModel: expected hit, got miss")
	}
	if name != "cloud" {
		t.Errorf("ProviderForModel: got %q; want cloud", name)
	}
}

// TestProviderForModel_Miss verifies ("", false) when no provider's Name or Model
// matches — the caller should route via defaults with ModelOverride only.
func TestProviderForModel_Miss(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{})
	r.RegisterProvider(NewStubProvider("ollama", ""))

	_, ok := r.ProviderForModel("gpt-5")
	if ok {
		t.Error("ProviderForModel: expected miss for unknown model, got hit")
	}
}

// TestProviderForModel_DeterministicTiebreak verifies that when two providers
// declare the same Model() string, ProviderForModel always resolves to the same
// one. Providers are sorted by Name() on registration, so the alphabetically first
// name wins consistently regardless of registration order.
func TestProviderForModel_DeterministicTiebreak(t *testing.T) {
	t.Parallel()

	const sharedModel = "gemma-3-4b-it"

	// Register in reverse-alpha order to confirm sort, not insertion, governs.
	r := NewSimpleRouter(RoutingConfig{Default: "zebra"})
	r.RegisterProvider(&modelStub{StubProvider: NewStubProvider("zebra", ""), model: sharedModel})
	r.RegisterProvider(&modelStub{StubProvider: NewStubProvider("alpha", ""), model: sharedModel})
	r.RegisterProvider(&modelStub{StubProvider: NewStubProvider("mango", ""), model: sharedModel})

	name, ok := r.ProviderForModel(sharedModel)
	if !ok {
		t.Fatal("ProviderForModel: expected hit, got miss")
	}
	// "alpha" sorts first — that should always win.
	if name != "alpha" {
		t.Errorf("ProviderForModel: got %q; want alpha (alphabetically first)", name)
	}

	// Call multiple times to confirm stability.
	for range 10 {
		got, _ := r.ProviderForModel(sharedModel)
		if got != name {
			t.Errorf("ProviderForModel: non-deterministic — got %q on repeat, want %q", got, name)
		}
	}
}
