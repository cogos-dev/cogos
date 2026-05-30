// resolve.go — shared model-string → provider resolution.
//
// ResolveModelRequest is the single source of truth that maps an OpenAI-compat
// `model` string to:
//   - preferProvider: the registered provider name to prefer (empty = default
//     routing)
//   - modelOverride:  the model id forwarded to the provider (empty = provider
//     uses its own configured model)
//   - injectKernelTools: true when the caller should inject the kernel's MCP
//     tool surface (only for the "kernel-agent"/"ollama" alias).
//
// Alias table (intent → provider + model override):
//
//	foreground        → claude-oauth, claude-sonnet-4-6  (interactive, full capability)
//	deliberation      → claude-oauth, claude-opus-4-7    (heavier reasoning)
//	haiku/sonnet/opus → claude-oauth, <model id>         (per-model selection)
//	claude            → claude-oauth, ""                 (generic managed alias)
//	codex             → codex,        ""
//	ollama            → ollama,       ""                 (+ injectKernelTools)
//	kernel-agent      → ollama,       ""                 (+ injectKernelTools)
//	local             → first local provider, ""
//	""                → default routing (all fields empty)
//
// For any other string the function falls through to the router:
//  1. ProviderForName — exact provider-name match (no ModelOverride)
//  2. ProviderForModel — model-string match (sets ModelOverride = model)
//
// The function is intentionally side-effect-free: callers own logging and
// any per-request mutation on CompletionRequest.
package engine

import (
	"log/slog"
)

// ModelResolution is the output of ResolveModelRequest.
type ModelResolution struct {
	PreferProvider    string // empty = use router default
	ModelOverride     string // empty = provider uses its configured model
	InjectKernelTools bool   // caller should inject kernel MCP tools
}

// intentAliases maps fixed model-string tokens to (provider, modelOverride).
// Entries here are the canonical alias table shared by the gateway and dispatch.
// Keep in sync with the /v1/models menu in serve_compat.go.
var intentAliases = map[string]ModelResolution{
	// Managed-frontier aliases → claude-oauth (Anthropic via the Max-subscription
	// OAuth bearer, direct /v1/messages, no CLI subprocess). claude-code stays
	// registered as the router fallback and as claude-oauth's internal 429
	// fallback, so nothing is stranded if the OAuth path is unavailable.
	"foreground":   {PreferProvider: "claude-oauth", ModelOverride: "claude-sonnet-4-6"},
	"deliberation": {PreferProvider: "claude-oauth", ModelOverride: "claude-opus-4-7"},
	// Raw model IDs that should route to claude-oauth.
	"claude-sonnet-4-6": {PreferProvider: "claude-oauth", ModelOverride: "claude-sonnet-4-6"},
	"claude-opus-4-7":   {PreferProvider: "claude-oauth", ModelOverride: "claude-opus-4-7"},
	// Short convenience aliases — per-model selection (haiku / sonnet / opus).
	// claude-oauth is model-agnostic (effectiveModel honours ModelOverride), so a
	// single provider serves the whole family; any other raw claude-* id falls
	// through to default routing (claude-oauth) with ModelOverride preserved.
	"claude": {PreferProvider: "claude-oauth"},
	"haiku":  {PreferProvider: "claude-oauth", ModelOverride: "claude-haiku-4-5-20251001"},
	"sonnet": {PreferProvider: "claude-oauth", ModelOverride: "claude-sonnet-4-6"},
	"opus":   {PreferProvider: "claude-oauth", ModelOverride: "claude-opus-4-7"},
	// Other provider aliases.
	"codex": {PreferProvider: "codex"},
	// Local/kernel aliases — injectKernelTools tells the gateway to wire tools.
	"ollama":       {PreferProvider: "ollama", InjectKernelTools: true},
	"kernel-agent": {PreferProvider: "ollama", InjectKernelTools: true},
}

// ResolveModelRequest maps a model string to a ModelResolution using the
// shared alias table followed by live router probes when router != nil.
//
// Gateway callers pass the live SimpleRouter. Dispatch callers pass nil for
// the router — alias-table resolution (which covers all managed-frontier
// targets) is still available; unknown-model strings that would need a router
// probe fall through to empty (no resolution).
//
// Managed-frontier strings now resolve to claude-oauth (see the alias table
// above); all other strings are unchanged:
//
//	""            → {} (empty, default routing)
//	"local"       → resolved via router.FirstLocalProvider / ProviderForName("ollama")
//	"claude"      → {claude-oauth, ""}
//	"codex"       → {codex, ""}
//	"ollama"      → {ollama, "", injectKernelTools}
//	"kernel-agent"→ {ollama, "", injectKernelTools}
//	named provider→ {name, ""} (ProviderForName)
//	model id      → {provider, model} (ProviderForModel + ModelOverride)
func ResolveModelRequest(router Router, model string, requestID string) ModelResolution {
	if model == "" {
		return ModelResolution{}
	}

	// "local" has dynamic logic that requires a live router.
	if model == "local" {
		if router == nil {
			return ModelResolution{}
		}
		if name, ok := router.ProviderForName("ollama"); ok {
			return ModelResolution{PreferProvider: name}
		}
		if name, ok := router.FirstLocalProvider(); ok {
			return ModelResolution{PreferProvider: name}
		}
		slog.Warn("chat: model=local requested but no local provider registered; falling back to default routing",
			"request_id", requestID,
		)
		return ModelResolution{}
	}

	// Alias table lookup — covers fixed managed-frontier aliases, raw model IDs
	// for known upstream providers, and the kernel-local aliases.
	if res, ok := intentAliases[model]; ok {
		return res
	}

	// Router-based fallback: live provider name / model match.
	if router == nil {
		return ModelResolution{}
	}
	if name, ok := router.ProviderForName(model); ok {
		return ModelResolution{PreferProvider: name}
	}
	// Model-ID pass-through: set ModelOverride and, if a provider serves this
	// model, also set PreferProvider.
	res := ModelResolution{ModelOverride: model}
	if name, ok := router.ProviderForModel(model); ok {
		res.PreferProvider = name
	}
	return res
}
