// router.go — SimpleRouter + BuildRouter
//
// SimpleRouter implements the Router interface with rule-based provider selection:
//  1. Check process-state routing overrides
//  2. Try preferred provider first, then fallback chain
//  3. Filter by availability + required capabilities
//  4. Score local > cloud (sovereignty gradient)
//  5. Record every routing decision for future sentinel training
//
// BuildRouter reads .cog/config/providers.yaml and instantiates enabled providers.
// Falls back to a default Ollama config when no providers.yaml is present.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

var toolCallRejectionsByProvider sync.Map // map[string]*atomic.Int64

// SimpleRouter implements Router with rule-based provider selection.
type SimpleRouter struct {
	mu        sync.RWMutex
	providers []Provider // ordered by registration sequence
	byName    map[string]Provider

	cfg RoutingConfig

	// Atomics for lock-free stats.
	totalRequests atomic.Int64
	escalations   atomic.Int64
	fallbacks     atomic.Int64
	byProvider    sync.Map // map[string]*atomic.Int64
}

// NewSimpleRouter creates an empty router with the given routing config.
func NewSimpleRouter(cfg RoutingConfig) *SimpleRouter {
	return &SimpleRouter{
		cfg:    cfg,
		byName: make(map[string]Provider),
	}
}

// RegisterProvider adds a provider to the pool.
func (r *SimpleRouter) RegisterProvider(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[p.Name()] = p
	r.providers = append(r.providers, p)
}

// DeregisterProvider removes a provider by name.
func (r *SimpleRouter) DeregisterProvider(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byName, name)
	updated := r.providers[:0]
	for _, p := range r.providers {
		if p.Name() != name {
			updated = append(updated, p)
		}
	}
	r.providers = updated
}

// Route selects the best available provider for the request.
func (r *SimpleRouter) Route(ctx context.Context, req *CompletionRequest) (Provider, *RoutingDecision, error) {
	start := time.Now()
	r.totalRequests.Add(1)

	r.mu.RLock()
	providers := make([]Provider, len(r.providers))
	copy(providers, r.providers)
	cfg := r.cfg
	r.mu.RUnlock()

	// Provider preference: explicit > process-state > default.
	preferred := req.Metadata.PreferProvider
	if preferred == "" && req.Metadata.ProcessState != "" {
		preferred = cfg.ProcessStateRouting[req.Metadata.ProcessState]
	}
	if preferred == "" {
		preferred = cfg.Default
	}

	// Build a priority-ordered candidate list:
	// 1. preferred provider, 2. fallback chain, 3. remaining providers.
	ordered := r.buildCandidateOrder(providers, preferred, cfg.FallbackChain)

	var scores []ProviderScore
	var selected Provider
	escalated := false
	fallbackUsed := false
	fallbackFrom := ""

	for i, p := range ordered {
		caps := p.Capabilities()
		capsMet := caps.HasAllCapabilities(req.Metadata.RequiredCapabilities)
		avail := p.Available(ctx)

		score := ProviderScore{
			Provider:        p.Name(),
			RawScore:        computeScore(p, req),
			Available:       avail,
			CapabilitiesMet: capsMet,
		}
		if !caps.IsLocal {
			score.SwapPenalty = 0.10
		}
		score.AdjustedScore = score.RawScore - score.SwapPenalty
		scores = append(scores, score)

		if avail && capsMet && selected == nil {
			selected = p
			if i > 0 {
				fallbackUsed = true
				fallbackFrom = ordered[0].Name()
			}
			if !caps.IsLocal {
				escalated = true
				r.escalations.Add(1)
			}
		}
	}

	if selected == nil {
		return nil, nil, fmt.Errorf("router: no available provider for request %s", req.Metadata.RequestID)
	}

	// Track per-provider count.
	counter, _ := r.byProvider.LoadOrStore(selected.Name(), &atomic.Int64{})
	counter.(*atomic.Int64).Add(1)
	if fallbackUsed {
		r.fallbacks.Add(1)
	}

	decision := &RoutingDecision{
		RequestID:        req.Metadata.RequestID,
		SelectedProvider: selected.Name(),
		Scores:           scores,
		Reason:           routeReason(escalated, fallbackUsed),
		Escalated:        escalated,
		FallbackUsed:     fallbackUsed,
		FallbackFrom:     fallbackFrom,
		Timestamp:        time.Now().UTC(),
		LatencyNs:        time.Since(start).Nanoseconds(),
	}

	slog.Debug("router: selected",
		"provider", selected.Name(),
		"escalated", escalated,
		"fallback", fallbackUsed,
		"latency_us", time.Since(start).Microseconds())

	return selected, decision, nil
}

// Stats returns current routing statistics.
func (r *SimpleRouter) Stats() RouterStats {
	stats := RouterStats{
		TotalRequests:                r.totalRequests.Load(),
		EscalationCount:              r.escalations.Load(),
		FallbackCount:                r.fallbacks.Load(),
		RequestsByProvider:           make(map[string]int64),
		ToolCallRejectionsByProvider: make(map[string]int64),
	}
	r.byProvider.Range(func(k, v any) bool {
		stats.RequestsByProvider[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	toolCallRejectionsByProvider.Range(func(k, v any) bool {
		stats.ToolCallRejectionsByProvider[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	if stats.TotalRequests > 0 {
		var local int64
		r.mu.RLock()
		for _, p := range r.providers {
			if p.Capabilities().IsLocal {
				if n, ok := stats.RequestsByProvider[p.Name()]; ok {
					local += n
				}
			}
		}
		r.mu.RUnlock()
		stats.SovereigntyRatio = float64(local) / float64(stats.TotalRequests)
	}
	return stats
}

func recordToolCallRejection(providerName string) {
	if providerName == "" {
		return
	}
	counter, _ := toolCallRejectionsByProvider.LoadOrStore(providerName, &atomic.Int64{})
	counter.(*atomic.Int64).Add(1)
}

// buildCandidateOrder returns providers ordered by routing preference.
func (r *SimpleRouter) buildCandidateOrder(all []Provider, preferred string, chain []string) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ordered []Provider
	seen := map[string]bool{}

	for _, name := range append([]string{preferred}, chain...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if p, ok := r.byName[name]; ok {
			ordered = append(ordered, p)
		}
	}
	// Append providers not in the explicit lists.
	for _, p := range all {
		if !seen[p.Name()] {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// computeScore returns a raw fitness score [0.0, 1.0] for a provider.
func computeScore(p Provider, req *CompletionRequest) float64 {
	score := 0.5
	caps := p.Capabilities()
	if caps.IsLocal {
		score += 0.4
	}
	if req.Metadata.PreferLocal && caps.IsLocal {
		score += 0.1
	}
	return score
}

func routeReason(escalated, fallback bool) string {
	switch {
	case fallback:
		return "fallback: primary provider unavailable"
	case escalated:
		return "escalated: no local provider available"
	default:
		return "local: best available provider"
	}
}

// ── BuildRouter ───────────────────────────────────────────────────────────────

// BuildRouter constructs a Router from workspace configuration.
// Reads .cog/config/providers.yaml; falls back to a default Ollama config.
func BuildRouter(cfg *Config, opts ...BuildRouterOption) (Router, error) {
	var bro buildRouterOpts
	for _, o := range opts {
		o(&bro)
	}

	pcfg, err := loadProvidersConfig(cfg)
	if err != nil {
		slog.Warn("router: providers.yaml not found, using default (ollama)", "err", err)
		pcfg = defaultProvidersConfig(cfg.LocalModel)
	}

	router := NewSimpleRouter(pcfg.Routing)

	for name, pc := range pcfg.Providers {
		if !pc.IsEnabled() {
			continue
		}
		p, err := makeProvider(name, pc, bro.procMgr)
		if err != nil {
			slog.Warn("router: skipping provider", "name", name, "err", err)
			continue
		}
		router.RegisterProvider(p)
		slog.Info("router: registered", "name", name, "model", pc.Model)
	}

	// Auto-discover OpenAI-compatible servers on well-known ports.
	autoDiscoverOpenAICompat(router, pcfg)

	return router, nil
}

type buildRouterOpts struct {
	procMgr *ProcessManager
}

// BuildRouterOption configures BuildRouter.
type BuildRouterOption func(*buildRouterOpts)

// WithProcessManager provides a ProcessManager for providers that spawn subprocesses.
func WithProcessManager(pm *ProcessManager) BuildRouterOption {
	return func(o *buildRouterOpts) { o.procMgr = pm }
}

func loadProvidersConfig(cfg *Config) (ProvidersConfig, error) {
	path := filepath.Join(cfg.CogDir, "config", "providers.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return ProvidersConfig{}, err
	}
	var pcfg ProvidersConfig
	if err := yaml.Unmarshal(data, &pcfg); err != nil {
		return ProvidersConfig{}, fmt.Errorf("parse providers.yaml: %w", err)
	}
	applyLocalModelConfig(cfg, &pcfg)
	return pcfg, nil
}

func applyLocalModelConfig(cfg *Config, pcfg *ProvidersConfig) {
	if cfg == nil || pcfg == nil || cfg.LocalModel == "" {
		return
	}
	for name, pc := range pcfg.Providers {
		providerType := pc.Type
		if providerType == "" {
			providerType = name
		}
		if providerType != "ollama" {
			continue
		}
		if pc.Model == "" || cfg.localModelConfigured {
			pc.Model = cfg.LocalModel
			pcfg.Providers[name] = pc
		}
	}
}

// makeProvider instantiates a Provider from a ProviderConfig.
// The provider type is inferred from the name if Type is empty.
func makeProvider(name string, pc ProviderConfig, procMgr *ProcessManager) (Provider, error) {
	t := pc.Type
	if t == "" {
		t = name
	}
	switch t {
	case "ollama":
		return NewOllamaProvider(name, pc), nil
	case "anthropic":
		return NewAnthropicProvider(name, pc), nil
	case "openai-compat", "openai", "lmstudio", "vllm", "llamacpp":
		return NewOpenAICompatProvider(name, pc), nil
	case "claude-code":
		if procMgr == nil {
			procMgr = NewProcessManager(ProcessManagerConfig{})
		}
		return NewClaudeCodeProvider(name, pc, procMgr), nil
	case "codex":
		return NewCodexProvider(name, pc), nil
	case "stub":
		return NewStubProvider(name, "stub response"), nil
	default:
		return nil, fmt.Errorf("unknown provider type %q", t)
	}
}

// defaultProvidersConfig returns a minimal config pointing at local Ollama.
func defaultProvidersConfig(localModel string) ProvidersConfig {
	enabled := true
	if localModel == "" {
		localModel = defaultOllamaModel
	}
	return ProvidersConfig{
		Providers: map[string]ProviderConfig{
			"ollama": {
				Type:     "ollama",
				Enabled:  &enabled,
				Endpoint: "http://localhost:11434",
				Model:    localModel,
				Timeout:  60,
			},
		},
		Routing: RoutingConfig{
			Default:        "ollama",
			LocalThreshold: 0.8,
			FallbackChain:  []string{"ollama"},
		},
	}
}

// ── Auto-discovery ───────────────────────────────────────────────────────────

// openaiCompatProbeEndpoint defines a well-known local endpoint to auto-discover.
type openaiCompatProbeEndpoint struct {
	name     string
	endpoint string
}

// openaiCompatWellKnownEndpoints lists endpoints to probe on startup.
// Ollama (localhost:11434) is handled separately; these are OpenAI-compatible servers.
var openaiCompatWellKnownEndpoints = []openaiCompatProbeEndpoint{
	{name: "lmstudio", endpoint: "http://localhost:1234"},
}

// autoDiscoverOpenAICompat probes well-known local ports for OpenAI-compatible
// servers and registers any that respond. Skips endpoints already configured
// in providers.yaml to avoid duplicates.
func autoDiscoverOpenAICompat(router *SimpleRouter, pcfg ProvidersConfig) {
	// Build a set of already-configured endpoints to avoid duplicates.
	configuredEndpoints := map[string]bool{}
	configuredNames := map[string]bool{}
	for name, pc := range pcfg.Providers {
		if pc.Endpoint != "" {
			configuredEndpoints[strings.TrimRight(pc.Endpoint, "/")] = true
		}
		configuredNames[name] = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, probe := range openaiCompatWellKnownEndpoints {
		endpoint := strings.TrimRight(probe.endpoint, "/")
		if configuredEndpoints[endpoint] || configuredNames[probe.name] {
			continue
		}

		p := NewOpenAICompatProvider(probe.name, ProviderConfig{
			Type:     "openai-compat",
			Endpoint: endpoint,
			Timeout:  5,
		})
		if p.Available(ctx) {
			router.RegisterProvider(p)
			slog.Info("router: auto-discovered", "name", probe.name, "endpoint", endpoint)
		}
	}
}
