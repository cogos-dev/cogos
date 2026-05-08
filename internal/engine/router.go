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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/myrgic/cogos/pkg/reconcile"
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
// Providers are kept sorted by Name() so that ProviderForModel iteration
// is deterministic when multiple providers share the same Model() string.
func (r *SimpleRouter) RegisterProvider(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[p.Name()] = p
	r.providers = append(r.providers, p)
	sort.Slice(r.providers, func(i, j int) bool {
		return r.providers[i].Name() < r.providers[j].Name()
	})
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

// ProviderForName returns the registered provider name when `name` is an
// exact match for a provider's Name(). Used to detect provider aliases so
// callers can target a specific provider without forwarding the alias as a
// ModelOverride. Returns ("", false) when no provider matches.
func (r *SimpleRouter) ProviderForName(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byName[name]; ok {
		return p.Name(), true
	}
	return "", false
}

// ProviderForModel returns the registered provider name whose Name() or
// Model() matches `model`. Name match takes precedence over model match so
// callers can target a specific provider instance even when multiple
// providers serve the same underlying model. Returns ("", false) when no
// provider matches.
func (r *SimpleRouter) ProviderForModel(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byName[model]; ok {
		return p.Name(), true
	}
	for _, p := range r.providers {
		if p.Model() == model {
			return p.Name(), true
		}
	}
	return "", false
}

// FirstLocalProvider returns the name of the first registered provider whose
// Capabilities().IsLocal is true. Providers are iterated in registration
// order (alphabetically sorted by Name). Returns ("", false) when none is
// registered.
func (r *SimpleRouter) FirstLocalProvider() (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.Capabilities().IsLocal {
			return p.Name(), true
		}
	}
	return "", false
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

func recordToolCallRejection(providerName string) { //nolint:unused // called from tool_loop.go (mcpserver build tag)
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
	basePath := filepath.Join(cfg.CogDir, "config", "providers.yaml")
	data, err := os.ReadFile(basePath)
	if err != nil {
		return ProvidersConfig{}, err
	}
	var pcfg ProvidersConfig
	if err := yaml.Unmarshal(data, &pcfg); err != nil {
		return ProvidersConfig{}, fmt.Errorf("parse providers.yaml: %w", err)
	}

	// Deep-merge providers.local.yaml on top if present. The local file is
	// gitignored and holds node-specific endpoints, API key env-var names, and
	// fallback overrides. Documented since the file shipped; this is the
	// implementation that backs the documentation.
	localPath := filepath.Join(cfg.CogDir, "config", "providers.local.yaml")
	if localData, lerr := os.ReadFile(localPath); lerr == nil {
		var local ProvidersConfig
		if perr := yaml.Unmarshal(localData, &local); perr != nil {
			slog.Warn("router: providers.local.yaml parse error, ignoring overlay", "err", perr)
		} else {
			pcfg = mergeProvidersConfig(pcfg, local)
			slog.Info("router: providers.local.yaml merged",
				"providers_added", len(local.Providers),
				"path", localPath,
			)
		}
	} else if !os.IsNotExist(lerr) {
		slog.Warn("router: providers.local.yaml read error, ignoring overlay", "err", lerr)
	}

	applyLocalModelConfig(cfg, &pcfg)
	return pcfg, nil
}

// mergeProvidersConfig deep-merges overlay onto base and returns the result.
// Per-provider entries are field-level merged (overlay non-zero values win,
// zero values keep base). Routing uses the same shape. New providers in the
// overlay are added; the base map is preserved for keys not in overlay.
func mergeProvidersConfig(base, overlay ProvidersConfig) ProvidersConfig {
	if overlay.Providers != nil {
		if base.Providers == nil {
			base.Providers = make(map[string]ProviderConfig, len(overlay.Providers))
		}
		for name, oc := range overlay.Providers {
			if existing, ok := base.Providers[name]; ok {
				base.Providers[name] = mergeProviderConfig(existing, oc)
			} else {
				base.Providers[name] = oc
			}
		}
	}
	base.Routing = mergeRoutingConfig(base.Routing, overlay.Routing)
	return base
}

func mergeProviderConfig(base, overlay ProviderConfig) ProviderConfig {
	if overlay.Type != "" {
		base.Type = overlay.Type
	}
	if overlay.APIKeyEnv != "" {
		base.APIKeyEnv = overlay.APIKeyEnv
	}
	if overlay.Endpoint != "" {
		base.Endpoint = overlay.Endpoint
	}
	if overlay.Model != "" {
		base.Model = overlay.Model
	}
	if overlay.ContextWindow != 0 {
		base.ContextWindow = overlay.ContextWindow
	}
	if overlay.MaxTokens != 0 {
		base.MaxTokens = overlay.MaxTokens
	}
	if overlay.Timeout != 0 {
		base.Timeout = overlay.Timeout
	}
	if overlay.Enabled != nil {
		base.Enabled = overlay.Enabled
	}
	if len(overlay.Headers) > 0 {
		if base.Headers == nil {
			base.Headers = make(map[string]string, len(overlay.Headers))
		}
		for k, v := range overlay.Headers {
			base.Headers[k] = v
		}
	}
	if len(overlay.Options) > 0 {
		if base.Options == nil {
			base.Options = make(map[string]interface{}, len(overlay.Options))
		}
		for k, v := range overlay.Options {
			base.Options[k] = v
		}
	}
	return base
}

func mergeRoutingConfig(base, overlay RoutingConfig) RoutingConfig {
	if overlay.Default != "" {
		base.Default = overlay.Default
	}
	if overlay.LocalThreshold != 0 {
		base.LocalThreshold = overlay.LocalThreshold
	}
	// FallbackChain is intentionally replaced wholesale (not element-merged) so the overlay
	// can reorder or shrink the chain; element-wise merge would make removal impossible.
	if len(overlay.FallbackChain) > 0 {
		base.FallbackChain = overlay.FallbackChain
	}
	if overlay.MaxCostPerDayUSD != 0 {
		base.MaxCostPerDayUSD = overlay.MaxCostPerDayUSD
	}
	if len(overlay.ProcessStateRouting) > 0 {
		if base.ProcessStateRouting == nil {
			base.ProcessStateRouting = make(map[string]string, len(overlay.ProcessStateRouting))
		}
		for k, v := range overlay.ProcessStateRouting {
			base.ProcessStateRouting[k] = v
		}
	}
	return base
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
	case "pi":
		if procMgr == nil {
			procMgr = NewProcessManager(ProcessManagerConfig{})
		}
		return NewPiProvider(name, pc, procMgr), nil
	case "stub":
		return NewStubProvider(name, "stub response"), nil
	case mlxSupervisedType:
		supervisor := ServiceSupervisor(NewLaunchctlController())
		p, err := newMLXSupervisedProvider(name, pc, supervisor)
		if err != nil {
			return nil, fmt.Errorf("mlx-supervised %q: %w", name, err)
		}
		// Register in the global reconcile registry so the autonomic ticker can
		// run the full plan/apply self-heal cycle. UpsertProvider is used instead
		// of RegisterProvider to allow safe re-registration (e.g. on config reload
		// or in tests that call BuildRouter multiple times).
		reconcile.UpsertProvider(mlxSupervisedType+"/"+name, p)
		slog.Info("router: registered mlx-supervised provider in reconcile registry", "name", name)
		return p, nil
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
