package inference

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// testProvider is a minimal stub satisfying ProviderLike for resolver tests.
type testProvider struct {
	name      string
	model     string
	available bool
}

func (p *testProvider) Name() string                        { return p.name }
func (p *testProvider) Model() string                       { return p.model }
func (p *testProvider) Available(_ context.Context) bool    { return p.available }

// --- DefaultCoreInferenceConfig tests ---

func TestDefaultCoreInferenceConfig(t *testing.T) {
	cfg := DefaultCoreInferenceConfig()

	if len(cfg.Tiers) < 1 {
		t.Fatalf("expected at least 1 tier, got 0")
	}
	if cfg.Tiers[0].Kind != TierClaudeCodeProvider {
		t.Errorf("tier 0 kind: want %q, got %q", TierClaudeCodeProvider, cfg.Tiers[0].Kind)
	}
	if cfg.DefaultRole == "" {
		t.Error("DefaultRole must not be empty")
	}
	if cfg.Tiers[0].Model == "" {
		t.Error("tier 0 Model must not be empty")
	}
}

// --- Resolver tests ---

func TestResolveClaudeCodeAvailable(t *testing.T) {
	claude := &testProvider{name: "claude", model: "haiku", available: true}
	providers := map[string]ProviderLike{"claude": claude}

	r := NewResolver(DefaultCoreInferenceConfig(), providers)
	p, tierName, err := r.Resolve(context.Background(), "autonomic")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("Resolve: expected non-nil provider")
	}
	if p.Name() != "claude" {
		t.Errorf("expected provider name 'claude', got %q", p.Name())
	}
	if tierName != "haiku-via-max" {
		t.Errorf("expected tier name 'haiku-via-max', got %q", tierName)
	}
}

func TestResolveFallsThrough(t *testing.T) {
	// claude unavailable, ollama available — should fall through to tier 1
	claude := &testProvider{name: "claude", model: "haiku", available: false}
	ollama := &testProvider{name: "ollama", model: "gemma4:e4b", available: true}
	providers := map[string]ProviderLike{"claude": claude, "ollama": ollama}

	r := NewResolver(DefaultCoreInferenceConfig(), providers)
	p, tierName, err := r.Resolve(context.Background(), "autonomic")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if p.Name() != "ollama" {
		t.Errorf("expected fallback to 'ollama', got %q", p.Name())
	}
	if tierName != "local-gemma4" {
		t.Errorf("expected tier name 'local-gemma4', got %q", tierName)
	}
}

func TestResolveNoTiersAvailable(t *testing.T) {
	claude := &testProvider{name: "claude", model: "haiku", available: false}
	ollama := &testProvider{name: "ollama", model: "gemma4:e4b", available: false}
	providers := map[string]ProviderLike{"claude": claude, "ollama": ollama}

	r := NewResolver(DefaultCoreInferenceConfig(), providers)
	_, _, err := r.Resolve(context.Background(), "autonomic")
	if err == nil {
		t.Fatal("expected error when no tiers available, got nil")
	}
}

func TestResolveEmptyRoleUsesDefault(t *testing.T) {
	claude := &testProvider{name: "claude", model: "haiku", available: true}
	providers := map[string]ProviderLike{"claude": claude}

	r := NewResolver(DefaultCoreInferenceConfig(), providers)
	// empty role → should use DefaultRole = "autonomic"
	p, _, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve with empty role: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider for empty role")
	}
}

func TestResolveRoleNotServedByTier(t *testing.T) {
	// "foveal-scoring" is only in tier 1 (local-gemma4); tier 0 (claude) doesn't serve it.
	// With only claude available (and ollama unavailable), resolve should fail.
	claude := &testProvider{name: "claude", model: "haiku", available: true}
	ollama := &testProvider{name: "ollama", model: "gemma4:e4b", available: false}
	providers := map[string]ProviderLike{"claude": claude, "ollama": ollama}

	r := NewResolver(DefaultCoreInferenceConfig(), providers)
	_, _, err := r.Resolve(context.Background(), "foveal-scoring")
	if err == nil {
		t.Fatal("expected error: claude tier does not serve foveal-scoring and ollama is unavailable")
	}
}

// --- LoadCoreInferenceConfig tests ---

func TestLoadCoreInferenceConfigMissing(t *testing.T) {
	// A directory that definitely has no .cog/config/core-inference.yaml
	cfg, err := LoadCoreInferenceConfig("/tmp/no-such-dir-cogos-test-xyz")
	if err != nil {
		t.Fatalf("expected no error for missing config, got: %v", err)
	}
	// Should return the default
	if len(cfg.Tiers) == 0 {
		t.Error("expected default tiers from missing config")
	}
	if cfg.Tiers[0].Kind != TierClaudeCodeProvider {
		t.Errorf("expected default tier 0 to be claude-code, got %q", cfg.Tiers[0].Kind)
	}
}

func TestLoadCoreInferenceConfigValid(t *testing.T) {
	dir := t.TempDir()
	cogDir := filepath.Join(dir, ".cog", "config")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `tiers:
  - kind: claude-code
    name: haiku-custom
    model: haiku
    max_timeout_seconds: 60
    roles:
      - autonomic
      - abstract-generation
default_role: autonomic
`
	if err := os.WriteFile(filepath.Join(cogDir, "core-inference.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := LoadCoreInferenceConfig(dir)
	if err != nil {
		t.Fatalf("LoadCoreInferenceConfig: unexpected error: %v", err)
	}
	if len(cfg.Tiers) != 1 {
		t.Fatalf("expected 1 tier, got %d", len(cfg.Tiers))
	}
	if cfg.Tiers[0].Kind != TierClaudeCodeProvider {
		t.Errorf("tier kind: want %q, got %q", TierClaudeCodeProvider, cfg.Tiers[0].Kind)
	}
	if cfg.Tiers[0].Name != "haiku-custom" {
		t.Errorf("tier name: want 'haiku-custom', got %q", cfg.Tiers[0].Name)
	}
	if cfg.DefaultRole != "autonomic" {
		t.Errorf("default role: want 'autonomic', got %q", cfg.DefaultRole)
	}
}

func TestLoadCoreInferenceConfigEmptyTiers(t *testing.T) {
	// A file with valid YAML but no tiers should fall back to default.
	dir := t.TempDir()
	cogDir := filepath.Join(dir, ".cog", "config")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := `default_role: autonomic
tiers: []
`
	if err := os.WriteFile(filepath.Join(cogDir, "core-inference.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := LoadCoreInferenceConfig(dir)
	if err != nil {
		t.Fatalf("LoadCoreInferenceConfig: unexpected error: %v", err)
	}
	// Empty tiers → default
	if cfg.Tiers[0].Kind != TierClaudeCodeProvider {
		t.Errorf("expected default tier 0 to be claude-code, got %q", cfg.Tiers[0].Kind)
	}
}
