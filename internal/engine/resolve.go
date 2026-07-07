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
	"strings"
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
	// Repointed from the decommissioned "ollama" provider to "lmstudio-darkstar"
	// (the live local backend, per PR #417). On a stock install no "ollama"
	// provider is registered, so the old value resolved to nothing; the "ollama"
	// alias key is kept as a convenience spelling that now routes to the live
	// local provider. Installs that still declare an "ollama" provider in
	// providers.yaml are unaffected — they select it by its provider name, not
	// via this alias default.
	"ollama":       {PreferProvider: "lmstudio-darkstar", InjectKernelTools: true},
	"kernel-agent": {PreferProvider: "lmstudio-darkstar", InjectKernelTools: true},
}

// resolveLiveCatalog resolves the two id families that the live GET /v1/models
// endpoint advertises beyond the static alias table, so an advertised id never
// gets a boundary 400 (the admission-parity invariant). It is the single source
// of truth shared by ResolveModelRequest and IsKnownModel.
//
//   - Composite "<provider>/<model>": split on the FIRST "/". When the prefix is
//     a REGISTERED provider name, resolve to {PreferProvider: prefix,
//     ModelOverride: suffix}. When the prefix is not a registered provider (e.g.
//     an "openrouter/x/y"-style id), report no match so the caller falls through
//     to the existing logic — this guard prevents mis-splitting such ids.
//   - Live claude ids: when a frontier provider is registered (see
//     frontierProviderName — the same predicate isFrontierConfigured emits on)
//     AND the id has the "claude-" prefix, resolve to that provider with
//     ModelOverride=model. This makes newly-shipped Anthropic ids (not in
//     intentAliases) both admissible and routable — matching the "claude-oauth
//     is model-agnostic" design intent — and keeps emit/admit in lockstep so a
//     bare claude id like claude-haiku-4-5-20251001 is never advertised-then-
//     rejected.
//
// Returns (resolution, true) on a match; (zero, false) otherwise. A nil router
// yields no match (the caller handles the nil-router path).
func resolveLiveCatalog(router Router, model string) (ModelResolution, bool) {
	if router == nil || model == "" {
		return ModelResolution{}, false
	}

	// Composite "<provider>/<model>": only when the prefix is a registered
	// provider name; otherwise fall through so multi-segment ids aren't
	// mis-split.
	if i := strings.Index(model, "/"); i > 0 && i < len(model)-1 {
		prefix, suffix := model[:i], model[i+1:]
		if name, ok := router.ProviderForName(prefix); ok {
			return ModelResolution{PreferProvider: name, ModelOverride: suffix}, true
		}
	}

	// Live claude ids → the registered frontier provider (claude-oauth first).
	if strings.HasPrefix(model, "claude-") {
		if name, ok := frontierProviderName(router); ok {
			return ModelResolution{PreferProvider: name, ModelOverride: model}, true
		}
	}

	return ModelResolution{}, false
}

// frontierProviderName returns the registered frontier provider to route
// managed-Claude ids to. It MUST admit under exactly the same conditions that
// serve_compat.go's isFrontierConfigured emits the bare claude ids, or the
// endpoint advertises a claude id that IsKnownModel rejects (advertise-then-
// reject; the admission-parity invariant). isFrontierConfigured matches by
// provider name {claude-oauth, anthropic, claude-code} OR by a provider serving
// claude-sonnet-4-6 / claude-opus-4-7 under any name — so this helper checks the
// same predicates and returns a routable provider name for each.
//
// Preference order mirrors the model-agnostic frontier failover: the direct-API
// providers (claude-oauth, then anthropic) first, then the claude-code CLI, then
// whatever provider serves the representative sonnet/opus model strings.
func frontierProviderName(router Router) (string, bool) {
	if router == nil {
		return "", false
	}
	for _, name := range []string{"claude-oauth", "anthropic", "claude-code"} {
		if n, ok := router.ProviderForName(name); ok {
			return n, true
		}
	}
	for _, model := range []string{"claude-sonnet-4-6", "claude-opus-4-7"} {
		if n, ok := router.ProviderForModel(model); ok {
			return n, true
		}
	}
	return "", false
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
//	"local"       → resolved via router.FirstLocalProvider / ProviderForName("lmstudio-darkstar")
//	"claude"      → {claude-oauth, ""}
//	"codex"       → {codex, ""}
//	"ollama"      → {lmstudio-darkstar, "", injectKernelTools}
//	"kernel-agent"→ {lmstudio-darkstar, "", injectKernelTools}
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
		// Prefer the live local backend (lmstudio-darkstar, per PR #417); fall
		// back to a still-declared "ollama" provider for installs that kept it,
		// then to any registered local provider.
		if name, ok := router.ProviderForName("lmstudio-darkstar"); ok {
			return ModelResolution{PreferProvider: name}
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

	// Live-catalog parity: composite "<provider>/<model>" and newly-shipped
	// claude-* ids advertised by GET /v1/models must both admit (IsKnownModel)
	// and route. Checked before the generic ProviderForModel fallback so a
	// composite id resolves to its named provider with the suffix as override.
	if res, ok := resolveLiveCatalog(router, model); ok {
		return res
	}
	// Model-ID pass-through: set ModelOverride and, if a provider serves this
	// model, also set PreferProvider.
	res := ModelResolution{ModelOverride: model}
	if name, ok := router.ProviderForModel(model); ok {
		res.PreferProvider = name
	}
	return res
}

// IsKnownModel reports whether a non-empty model string resolves to a real
// routing target: an intent alias, the dynamic "local" alias, a registered
// provider name, or a model served by some registered provider. It is the
// kernel-boundary admission check used by the gateway to reject unknown model
// ids with HTTP 400 instead of forwarding them to the default provider (which
// would POST a bogus id upstream and surface an opaque 500-wrapped 404).
//
// The empty string is treated as KNOWN (it means "default routing", which is a
// valid request — see ResolveModelRequest). A nil router can only validate the
// static alias table; callers on the nil-router path (dispatch) must not use
// this as a hard gate, because provider-name / provider-model matches require a
// live router. The gateway always passes a live router.
//
// Like ResolveModelRequest, this function is side-effect-free.
func IsKnownModel(router Router, model string) bool {
	if model == "" {
		return true // default routing is always valid
	}
	if _, ok := intentAliases[model]; ok {
		return true
	}
	if model == "local" {
		// "local" is a valid alias; whether a local provider is actually
		// registered is handled by ResolveModelRequest's fallback-to-default
		// behaviour, so do not reject it here.
		return true
	}
	if router == nil {
		// Without a live router only the static alias table is knowable.
		return false
	}
	if _, ok := router.ProviderForName(model); ok {
		return true
	}
	if _, ok := router.ProviderForModel(model); ok {
		return true
	}
	// Live-catalog parity: composite "<provider>/<model>" ids and newly-shipped
	// claude-* ids advertised at GET /v1/models are admissible even when they are
	// not in the static alias table nor served by a ProviderForModel match. Same
	// helper ResolveModelRequest uses, so admission and routing never diverge.
	if _, ok := resolveLiveCatalog(router, model); ok {
		return true
	}
	return false
}

// AvailableModelIDs returns the menu of model ids the kernel accepts, in a
// stable order: the intent aliases ("local" included), the raw frontier model
// ids exposed at GET /v1/models, then any registered provider names not already
// listed. Used to build the 400 response body when an unknown model is
// rejected so the caller sees exactly what they may request. Side-effect-free.
func AvailableModelIDs(router Router) []string {
	// Intent aliases + the dynamic "local" alias, in a deterministic order
	// that mirrors the /v1/models menu's intent-first layout.
	ids := []string{
		"foreground", "deliberation", "local",
		"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001",
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	// Append remaining static intent aliases (claude, codex, ollama, etc.)
	// that are not already in the menu, for completeness.
	for alias := range intentAliases {
		if !seen[alias] {
			ids = append(ids, alias)
			seen[alias] = true
		}
	}
	// Append registered provider names (e.g. claude-oauth, ollama, lmstudio).
	if sr, ok := router.(*SimpleRouter); ok && sr != nil {
		sr.mu.RLock()
		for _, p := range sr.providers {
			if name := p.Name(); name != "" && !seen[name] {
				ids = append(ids, name)
				seen[name] = true
			}
		}
		sr.mu.RUnlock()
	}
	return ids
}
