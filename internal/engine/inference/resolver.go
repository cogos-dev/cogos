package inference

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// ProviderLike is the subset of the engine.Provider interface that the Resolver
// requires. It is defined here (rather than importing internal/engine) to avoid
// a circular import — the inference package lives under internal/engine/ but
// must not import its parent package.
//
// engine.Provider is a superset of ProviderLike; all engine.Provider values
// satisfy this interface and can be passed directly to NewResolver.
type ProviderLike interface {
	// Name returns the provider identifier (e.g. "ollama", "claude-code").
	Name() string
	// Model returns the configured model identifier (e.g. "gemma4:e4b", "haiku").
	Model() string
	// Available reports whether the provider is ready to serve requests.
	Available(ctx context.Context) bool
}

// Resolver resolves autonomic inference targets from the N-tier contract.
//
// It is constructed once by the kernel (after providers are registered) and
// shared across all autonomic operations. The Resolver does NOT own provider
// lifecycle — it only selects from the map of already-registered providers.
type Resolver struct {
	cfg       CoreInferenceConfig
	providers map[string]ProviderLike
}

// NewResolver creates a Resolver from a CoreInferenceConfig and a provider map.
// The provider map is keyed by provider name (matching Provider.Name()).
// Typically this is the same map the engine router uses.
func NewResolver(cfg CoreInferenceConfig, providers map[string]ProviderLike) *Resolver {
	return &Resolver{cfg: cfg, providers: providers}
}

// Resolve returns the first available provider that satisfies the given role.
//
// Algorithm:
//  1. If role is empty, use cfg.DefaultRole.
//  2. Walk cfg.Tiers in order (tier 0 is preferred).
//  3. For each tier: check roleMatches, then find a registered provider for that tier.
//  4. If a provider is found and reports Available, return it.
//  5. If no tier satisfies the role with an available provider, return an error.
//
// Returns: (provider, selectedTierName, error).
func (r *Resolver) Resolve(ctx context.Context, role string) (ProviderLike, string, error) {
	if role == "" {
		role = r.cfg.DefaultRole
	}
	if role == "" {
		role = "autonomic" // ultimate fallback
	}

	var tried []string
	for _, tier := range r.cfg.Tiers {
		if !roleMatches(tier, role) {
			continue
		}
		p := findProvider(r.providers, tier)
		if p == nil {
			tried = append(tried, fmt.Sprintf("%s(no-registered-provider)", tier.Name))
			continue
		}
		if !p.Available(ctx) {
			tried = append(tried, fmt.Sprintf("%s(unavailable)", tier.Name))
			continue
		}
		return p, tier.Name, nil
	}

	if len(tried) == 0 {
		return nil, "", fmt.Errorf("inference resolver: no tier configured for role %q", role)
	}
	return nil, "", fmt.Errorf("inference resolver: no available provider for role %q (tried: %s)",
		role, strings.Join(tried, ", "))
}

// ResolveForOperation is a convenience wrapper that logs the selection decision.
// operationName is a human-readable label for the autonomic operation (used in logs).
func (r *Resolver) ResolveForOperation(ctx context.Context, operationName, role string) (ProviderLike, string, error) {
	p, tierName, err := r.Resolve(ctx, role)
	if err != nil {
		slog.Warn("inference resolver: no provider selected",
			"operation", operationName,
			"role", role,
			"error", err,
		)
		return nil, "", err
	}
	slog.Info("inference resolver: tier selected",
		"operation", operationName,
		"role", role,
		"tier", tierName,
		"provider", p.Name(),
		"model", p.Model(),
	)
	return p, tierName, nil
}

// roleMatches reports whether tier serves the requested role.
// An empty Roles list means the tier matches any role.
func roleMatches(tier TierProfile, role string) bool {
	if len(tier.Roles) == 0 {
		return true // universal tier
	}
	for _, r := range tier.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// findProvider searches the providers map for a provider that matches the tier's
// kind and model. Returns nil if no match is found.
//
// Matching heuristics (by TierKind):
//   - TierClaudeCodeProvider: Name contains "claude" OR Model matches tier.Model
//   - TierKeptWarmLocal / TierWarmWithColdStart: Name contains "ollama"
//   - TierExternalSelfHosted: Name contains "vllm" or "mlx"
func findProvider(providers map[string]ProviderLike, tier TierProfile) ProviderLike {
	for _, p := range providers {
		if matchesTier(p, tier) {
			return p
		}
	}
	return nil
}

// matchesTier reports whether provider p is a candidate for the given tier.
func matchesTier(p ProviderLike, tier TierProfile) bool {
	name := strings.ToLower(p.Name())
	switch tier.Kind {
	case TierClaudeCodeProvider:
		return strings.Contains(name, "claude") || p.Model() == tier.Model
	case TierKeptWarmLocal, TierWarmWithColdStart:
		return strings.Contains(name, "ollama")
	case TierExternalSelfHosted:
		return strings.Contains(name, "vllm") || strings.Contains(name, "mlx")
	default:
		return false
	}
}
