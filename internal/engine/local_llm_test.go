package engine

import (
	"path/filepath"
	"testing"
)

// ── normalizeModelNameForOllama ───────────────────────────────────────────────

func TestNormalizeModelNameForOllama(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		// LMS-style vendor-prefixed names → Ollama tags
		{"google/gemma-4-26b-a4b", "gemma4:26b"},
		{"google/gemma-4-e4b", "gemma4:e4b"},
		{"google/gemma-4-E4B", "gemma4:e4b"},
		{"google/gemma-4-27b-something", "gemma4:27b"},
		// Already-Ollama names are returned unchanged
		{"gemma4:e4b", "gemma4:e4b"},
		{"gemma4:26b", "gemma4:26b"},
		{"llama3.2:1b", "llama3.2:1b"},
		// Bare gemma-4 without vendor prefix
		{"gemma-4-26b-a4b", "gemma4:26b"},
		{"gemma-4-e4b-mlx", "gemma4:e4b"},
		// Empty stays empty
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizeModelNameForOllama(tc.in)
		if got != tc.want {
			t.Errorf("normalizeModelNameForOllama(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// ── resolvePreferredLocalModel ────────────────────────────────────────────────

func TestResolvePreferredLocalModelExactMatch(t *testing.T) {
	t.Parallel()
	models := []string{"gemma4:e4b", "gemma4:26b", "llama3.2:1b"}
	got := resolvePreferredLocalModel(models, "gemma4:e4b")
	if got != "gemma4:e4b" {
		t.Errorf("exact match: got %q; want %q", got, "gemma4:e4b")
	}
}

func TestResolvePreferredLocalModelPrefixMatch(t *testing.T) {
	t.Parallel()
	models := []string{"gemma4:26b-instruct", "gemma4:e4b", "llama3.2:1b"}
	got := resolvePreferredLocalModel(models, "gemma4:26b")
	if got != "gemma4:26b-instruct" {
		t.Errorf("prefix match: got %q; want %q", got, "gemma4:26b-instruct")
	}
}

// TestResolvePreferredLocalModelLMSStyleName verifies that a name in LM-Studio
// format ("google/gemma-4-26b-a4b") matches the equivalent Ollama model
// ("gemma4:26b") via normalizeModelNameForOllama rather than falling through to
// an arbitrary models[0].
func TestResolvePreferredLocalModelLMSStyleName(t *testing.T) {
	t.Parallel()
	// The models list mirrors what Ollama would report; no "google/..." entry.
	models := []string{"gemma4:26b", "gemma4:e4b", "llama3.2:1b"}
	got := resolvePreferredLocalModel(models, "google/gemma-4-26b-a4b")
	if got != "gemma4:26b" {
		t.Errorf("LMS-style name: got %q; want %q", got, "gemma4:26b")
	}
}

// TestResolvePreferredLocalModelFallsToE4B verifies that when the preferred
// model is absent and the LMS-style normalization also fails to match, the
// function returns defaultOllamaModel ("gemma4:e4b") rather than an arbitrary
// small model (e.g. "llama3.2:1b").
func TestResolvePreferredLocalModelFallsToE4B(t *testing.T) {
	t.Parallel()
	// "configured-but-not-loaded" is not present in models; neither is the
	// normalized form; E4B is available.
	models := []string{"llama3.2:1b", "gemma4:e4b"}
	got := resolvePreferredLocalModel(models, "configured-but-not-loaded")
	if got != defaultOllamaModel {
		t.Errorf("fallback to E4B floor: got %q; want %q (defaultOllamaModel)", got, defaultOllamaModel)
	}
}

// TestResolvePreferredLocalModelE4BNotAvailableFallsToFirst verifies that when
// neither the preferred model nor defaultOllamaModel is available, the function
// returns models[0] as a last resort (preserving existing behavior for
// edge-case deployments).
func TestResolvePreferredLocalModelE4BNotAvailableFallsToFirst(t *testing.T) {
	t.Parallel()
	models := []string{"llama3.2:1b", "mistral:7b"}
	got := resolvePreferredLocalModel(models, "configured-but-not-loaded")
	if got != "llama3.2:1b" {
		t.Errorf("last-resort first model: got %q; want %q", got, "llama3.2:1b")
	}
}

// ── resolveDispatchLocalModel ─────────────────────────────────────────────────

// TestResolveDispatchLocalModelE4BExact: configured model is present → no note.
func TestResolveDispatchLocalModelE4BExact(t *testing.T) {
	t.Parallel()
	models := []string{"gemma4:e4b", "llama3.2:1b"}
	model, route, note := resolveDispatchLocalModel(models, "gemma4:e4b", DispatchModelE4B)
	if model != "gemma4:e4b" {
		t.Errorf("model: got %q; want %q", model, "gemma4:e4b")
	}
	if route != DispatchModelE4B {
		t.Errorf("route: got %q; want %q", route, DispatchModelE4B)
	}
	if note != "" {
		t.Errorf("note: got %q; want empty", note)
	}
}

// TestResolveDispatchLocalModelLMSNameResolvesToOllama verifies defect 1 fix:
// when kernel.yaml has local_model="google/gemma-4-26b-a4b" and Ollama has
// "gemma4:26b", resolveDispatchLocalModel should resolve to the Ollama name
// rather than falling back.
func TestResolveDispatchLocalModelLMSNameResolvesToOllama(t *testing.T) {
	t.Parallel()
	models := []string{"gemma4:26b", "gemma4:e4b"}
	// localModelHint() would return "google/gemma-4-26b-a4b" from kernel.yaml
	model, route, note := resolveDispatchLocalModel(models, "google/gemma-4-26b-a4b", DispatchModelE4B)
	if model != "gemma4:26b" {
		t.Errorf("LMS name resolved to wrong model: got %q; want %q", model, "gemma4:26b")
	}
	if route != DispatchModelE4B {
		t.Errorf("route: got %q; want %q", route, DispatchModelE4B)
	}
	// Normalized match → no mismatch note expected.
	if note != "" {
		t.Errorf("note: got %q; want empty (normalized match)", note)
	}
}

// TestResolveDispatchLocalModelFallsToE4BNotLlama verifies defect 2 fix:
// when the configured local model is absent, the fallback must be
// defaultOllamaModel ("gemma4:e4b"), NOT whatever happens to be models[0]
// (e.g. "llama3.2:1b").
func TestResolveDispatchLocalModelFallsToE4BNotLlama(t *testing.T) {
	t.Parallel()
	// models list matches the scenario from the bug report: llama3.2:1b sorts
	// first (or is the only model besides e4b). Configured model not present.
	models := []string{"llama3.2:1b", "gemma4:e4b"}
	model, route, note := resolveDispatchLocalModel(models, "google/gemma-4-26b-a4b", DispatchModelE4B)
	if model != defaultOllamaModel {
		t.Errorf("wrong fallback: got %q; want %q (defaultOllamaModel, not llama3.2:1b)", model, defaultOllamaModel)
	}
	if route != DispatchModelE4B {
		t.Errorf("route: got %q; want %q", route, DispatchModelE4B)
	}
	if note == "" {
		t.Error("note: expected a mismatch note, got empty")
	}
}

// TestResolveDispatchLocalModel26BDegradesToE4B verifies that the 26B path,
// when no large model is loaded, degrades to E4B via the preferred-model chain
// rather than returning an arbitrary first model.
func TestResolveDispatchLocalModel26BDegradesToE4B(t *testing.T) {
	t.Parallel()
	// No model with ≥26B parameter count; E4B is present alongside a small model.
	models := []string{"llama3.2:1b", "gemma4:e4b"}
	model, route, note := resolveDispatchLocalModel(models, "gemma4:e4b", DispatchModel26B)
	if model != "gemma4:e4b" {
		t.Errorf("26B fallback: got %q; want %q (E4B floor)", model, "gemma4:e4b")
	}
	if route != DispatchModelE4B {
		t.Errorf("route: got %q; want %q", route, DispatchModelE4B)
	}
	if note == "" {
		t.Error("note: expected degradation warning, got empty")
	}
}

// TestResolveDispatchLocalModelNoModels verifies the empty-models sentinel.
func TestResolveDispatchLocalModelNoModels(t *testing.T) {
	t.Parallel()
	model, route, note := resolveDispatchLocalModel(nil, "gemma4:e4b", DispatchModelE4B)
	if model != "" {
		t.Errorf("no models: got %q; want empty", model)
	}
	if route != DispatchModelE4B {
		t.Errorf("route: got %q; want %q", route, DispatchModelE4B)
	}
	if note == "" {
		t.Error("note: expected error note, got empty")
	}
}

// TestResolveLocalProviderTimeoutNilConfig: nil cfg yields 0 (caller falls
// back to localProviderDefaultTimeoutSec). Important for unit-test paths and
// the brief window during construction before cfg is wired.
func TestResolveLocalProviderTimeoutNilConfig(t *testing.T) {
	t.Parallel()
	if got := resolveLocalProviderTimeout(nil); got != 0 {
		t.Errorf("nil cfg: got %d; want 0", got)
	}
}

// TestResolveLocalProviderTimeoutMissingFiles: a Config rooted at a workspace
// without providers.yaml returns 0 (loadProvidersConfig errors are swallowed
// — the dispatch path then falls back to the default timeout).
func TestResolveLocalProviderTimeoutMissingFiles(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 0 {
		t.Errorf("missing providers.yaml: got %d; want 0", got)
	}
}

// TestResolveLocalProviderTimeoutPrefersOllama: when an "ollama" provider is
// configured, its Timeout wins regardless of other entries' values. This is
// the common-case operator setup.
func TestResolveLocalProviderTimeoutPrefersOllama(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 600
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: ollama
  fallback_chain: [ollama]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 600 {
		t.Errorf("ollama timeout: got %d; want 600", got)
	}
}

// TestResolveLocalProviderTimeoutLocalYAMLOverlay: providers.local.yaml's
// timeout overrides providers.yaml via the existing deep-merge, so operator
// node-specific overrides take effect without copying the entire defaults
// file. This is the path the bug originally hid: hardcoded 120s shadowed
// whatever operators configured here.
func TestResolveLocalProviderTimeoutLocalYAMLOverlay(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 60
routing:
  default: ollama
  fallback_chain: [ollama]
`)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.local.yaml"), `providers:
  ollama:
    timeout: 480
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 480 {
		t.Errorf("local-yaml overlay: got %d; want 480", got)
	}
}

// TestResolveLocalProviderTimeoutFallsBackToOtherLocal: if no "ollama" entry
// exists (operator removed it; runs MLX or LM Studio only) the resolver falls
// back to any other localhost-pointed provider's timeout.
func TestResolveLocalProviderTimeoutFallsBackToOtherLocal(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  lmstudio-mlx:
    type: openai
    endpoint: http://localhost:1234
    model: gemma-mlx
    timeout: 240
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: claude-code
  fallback_chain: [claude-code, lmstudio-mlx]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 240 {
		t.Errorf("local fallback: got %d; want 240", got)
	}
}

// TestResolveLocalProviderTimeoutSkipsRemoteProviders: a remote provider's
// timeout must not be picked up as the local-tier timeout. Otherwise a
// 30-second claude-code timeout would shadow whatever Ollama needs.
func TestResolveLocalProviderTimeoutSkipsRemoteProviders(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  claude-code:
    type: claude-code
    model: sonnet
    timeout: 30
routing:
  default: claude-code
  fallback_chain: [claude-code]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 0 {
		t.Errorf("only-remote providers: got %d; want 0 (signal default)", got)
	}
}

// TestResolveLocalProviderTimeoutSkipsDisabledOllama: an explicitly disabled
// "ollama" entry must not contribute its timeout. Ensures the IsEnabled gate
// matches BuildRouter's behavior — operators expect "enabled: false" to
// remove a provider entirely from consideration.
func TestResolveLocalProviderTimeoutSkipsDisabledOllama(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	writeTestFile(t, filepath.Join(root, ".cog", "config", "providers.yaml"), `providers:
  ollama:
    type: ollama
    enabled: false
    endpoint: http://localhost:11434
    model: gemma4:e4b
    timeout: 60
  lmstudio-mlx:
    type: openai
    endpoint: http://localhost:1234
    model: gemma-mlx
    timeout: 240
routing:
  default: lmstudio-mlx
  fallback_chain: [lmstudio-mlx]
`)
	cfg, err := LoadConfig(root, 0)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := resolveLocalProviderTimeout(cfg); got != 240 {
		t.Errorf("disabled ollama should be skipped: got %d; want 240 (lmstudio fallback)", got)
	}
}

// TestBuildLocalProviderUsesProvidedTimeout: pass-through verification —
// if buildLocalProvider's caller supplies a timeout, that's what the
// constructed OllamaProvider's HTTP client sees.
func TestBuildLocalProviderUsesProvidedTimeout(t *testing.T) {
	t.Parallel()
	target := LocalLLMTarget{
		BaseURL: "http://localhost:11434",
		Backend: LocalLLMBackendOllama,
	}
	p := buildLocalProvider(target, "gemma4:e4b", 480)
	op, ok := p.(*OllamaProvider)
	if !ok {
		t.Fatalf("expected *OllamaProvider, got %T", p)
	}
	if got := int(op.timeout.Seconds()); got != 480 {
		t.Errorf("ollama HTTP timeout: got %ds; want 480s", got)
	}
}

// TestBuildLocalProviderDefaultsTo300: when caller passes 0 (no config
// override available) buildLocalProvider must apply the 300s default — that's
// the literal value the original 120s bug was raised against. Covers both
// negative (treated as zero) and zero inputs.
func TestBuildLocalProviderDefaultsTo300(t *testing.T) {
	t.Parallel()
	target := LocalLLMTarget{
		BaseURL: "http://localhost:11434",
		Backend: LocalLLMBackendOllama,
	}
	for _, in := range []int{0, -1} {
		p := buildLocalProvider(target, "gemma4:e4b", in)
		op := p.(*OllamaProvider)
		if got := int(op.timeout.Seconds()); got != localProviderDefaultTimeoutSec {
			t.Errorf("input %d: got %ds; want %ds", in, got, localProviderDefaultTimeoutSec)
		}
	}
}
