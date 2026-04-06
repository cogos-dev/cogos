// router_test.go — SimpleRouter unit tests
package main

import (
	"context"
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

// ── defaultProvidersConfig ────────────────────────────────────────────────────

func TestDefaultProvidersConfig(t *testing.T) {
	t.Parallel()
	pcfg := defaultProvidersConfig()
	if _, ok := pcfg.Providers["ollama"]; !ok {
		t.Error("default config should have ollama provider")
	}
	if pcfg.Routing.Default != "ollama" {
		t.Errorf("default routing = %q; want ollama", pcfg.Routing.Default)
	}
}
