// Package inference implements the core inference model contract for CogOS kernel
// autonomic operations.
//
// The contract declares an ordered N-tier ladder of inference resources. Each tier
// specifies a kind (local/external/claude-code), a model, and the autonomic operation
// roles it serves. The Resolver walks the ladder in order and returns the first
// available provider that satisfies a requested role.
//
// Default configuration (lowest friction):
//
//	Tier 0: Haiku via claude-code provider (Max subscription) — zero infrastructure,
//	        works immediately on any authenticated Claude Code install.
//	Tier 1: local Ollama gemma4:e4b (kept-warm) — VRAM-committed, zero cold-start
//	        when kept warm, but subject to the 90s kernel cycle budget vs 110s cold-start gap.
//
// The contract abstracts tier shape from the autonomic logic: the kernel declares
// what role it needs; the resolver decides which tier satisfies it.
//
// Substrate refs:
//   - feedback_node_core_inference_model_contract.md — N-tier ladder spec
//   - feedback_claude_max_subscription_only.md — claude-code provider is canonical external path
//   - feedback_substrate_eval_constraints.md — 90s cycle budget, 110s cold-start
//   - feedback_models_always_swappable.md — model is parameter, not hardcoded
package inference

// TierKind identifies the shape of an inference tier.
type TierKind string

const (
	// TierKeptWarmLocal is a local model (e.g. Ollama gemma4:e4b) kept resident in VRAM.
	// Cost: persistent VRAM + Ollama process. Benefit: zero cold-start, deterministic latency.
	TierKeptWarmLocal TierKind = "kept-warm-local"

	// TierWarmWithColdStart is a local model that may be evicted under memory pressure,
	// but with a documented cold-start ceiling so callers can budget appropriately.
	// Note: the current 90s localHarnessCycleTimeout is shorter than gemma4's ~110s cold-start
	// on M4 Pro — operators must pre-warm or use TierClaudeCodeProvider as fallback.
	TierWarmWithColdStart TierKind = "warm-with-cold-start"

	// TierExternalSelfHosted is a self-hosted inference endpoint (vLLM, mlx-lm, LM Studio).
	// Cost: separate infrastructure. Benefit: no local VRAM cost on the kernel node.
	TierExternalSelfHosted TierKind = "external-self-hosted"

	// TierClaudeCodeProvider routes through the claude-code provider (claude -p subprocess)
	// using the operator's Claude Max subscription. Zero incremental cost for Max subscribers.
	// Zero infrastructure burden — works the moment Claude Code is installed and authenticated.
	TierClaudeCodeProvider TierKind = "claude-code"
)

// TierProfile declares one rung of the N-tier inference ladder.
type TierProfile struct {
	// Kind identifies the provider shape.
	Kind TierKind `yaml:"kind"`

	// Name is a human-readable label, e.g. "local-gemma4", "haiku-via-max".
	// Used in resolver logs and diagnostics.
	Name string `yaml:"name"`

	// Model is the model identifier passed to the provider (e.g. "gemma4:e4b", "haiku").
	// For claude-code tiers: "haiku", "sonnet", "opus".
	Model string `yaml:"model"`

	// Endpoint is the inference endpoint URL for local or self-hosted tiers.
	// Empty for TierClaudeCodeProvider (the claude binary handles routing).
	Endpoint string `yaml:"endpoint,omitempty"`

	// ColdStartCeilingSeconds documents the maximum cold-start latency for TierWarmWithColdStart.
	// 0 means not applicable (tier is always warm or network-latency-bound).
	ColdStartCeilingSeconds int `yaml:"cold_start_ceiling_seconds,omitempty"`

	// MaxTimeoutSeconds overrides the kernel dispatch timeout for calls routed through this tier.
	// 0 means use the kernel default (localHarnessCycleTimeout = 90s).
	// Set this to ≥ ColdStartCeilingSeconds for TierWarmWithColdStart tiers.
	MaxTimeoutSeconds int `yaml:"max_timeout_seconds,omitempty"`

	// Roles is the list of autonomic operation roles this tier serves.
	// When empty, the tier matches any role (universal fallback).
	// Example role names: "autonomic", "metabolic", "abstract-generation",
	// "foveal-scoring", "reconcilable-observer".
	Roles []string `yaml:"roles,omitempty"`
}

// CoreInferenceConfig is the node's declared inference contract.
//
// It is loaded from .cog/config/core-inference.yaml. If that file is absent,
// DefaultCoreInferenceConfig() is used.
type CoreInferenceConfig struct {
	// Tiers is the ordered N-tier ladder. Index 0 is the preferred tier (lowest cost,
	// highest availability for the operator profile). The Resolver walks this in order,
	// selecting the first available tier that matches the requested role.
	Tiers []TierProfile `yaml:"tiers"`

	// DefaultRole is the role assumed when Resolve is called with an empty role string.
	DefaultRole string `yaml:"default_role,omitempty"`
}

// DefaultCoreInferenceConfig returns the recommended default configuration.
//
// Default stack for a Max-subscriber node:
//   - Tier 0: Haiku via claude-code (lowest friction — works immediately)
//   - Tier 1: local Ollama gemma4:e4b (for operators who have it running)
//
// This is the "stupid easy" path: the operator only needs Claude Code installed
// and authenticated. No Ollama setup, no model downloads, no VRAM management required
// for the default to work. Tier 1 is additive for operators who prefer local inference.
func DefaultCoreInferenceConfig() CoreInferenceConfig {
	return CoreInferenceConfig{
		DefaultRole: "autonomic",
		Tiers: []TierProfile{
			{
				Kind:  TierClaudeCodeProvider,
				Name:  "haiku-via-max",
				Model: "haiku",
				// 90s matches the kernel's localHarnessCycleTimeout ceiling.
				// Network RTT is well under 90s; this budget is for the full
				// round-trip including model generation.
				MaxTimeoutSeconds: 90,
				Roles:             []string{"autonomic", "abstract-generation", "metabolic", "reconcilable-observer"},
			},
			{
				Kind:     TierKeptWarmLocal,
				Name:     "local-gemma4",
				Model:    "gemma4:e4b",
				Endpoint: "http://localhost:11434",
				// 90s matches localHarnessCycleTimeout. Cold-start on M4 Pro is ~110s
				// (exceeds this budget) — operators using this tier must pre-warm gemma4
				// or accept cold-start failures. See feedback_substrate_eval_constraints.md.
				MaxTimeoutSeconds: 90,
				Roles:             []string{"autonomic", "foveal-scoring"},
			},
		},
	}
}
