// openclaw_gateway_provider_test.go
// Tests for the OpenClawGatewayProvider Reconcilable implementation.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func setupGatewayTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".cog", "config", "openclaw-gateway")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func writeGatewayConfig(t *testing.T, root, content string) {
	t.Helper()
	cfgPath := filepath.Join(root, ".cog", "config", "openclaw-gateway", "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeOpenClawJSON(t *testing.T, path string, data map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("marshal openclaw.json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write openclaw.json: %v", err)
	}
}

// ─── Tests ──────────────────────────────────────────────────────────────────

func TestOpenClawGatewayLoadConfig(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    cogos:
      baseUrl: "http://localhost:5100/v1"
      apiKey: local
channels:
  telegram:
    enabled: true
`)

	provider := &OpenClawGatewayProvider{}
	cfg, err := provider.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	declared := cfg.(*gatewayDeclaredConfig)
	if declared.Models == nil {
		t.Fatal("expected models section")
	}
	if declared.Channels == nil {
		t.Fatal("expected channels section")
	}

	providers := extractMapKey(declared.Models, "providers")
	if _, ok := providers["cogos"]; !ok {
		t.Error("expected cogos provider in models.providers")
	}
}

func TestOpenClawGatewayLoadConfig_Missing(t *testing.T) {
	root := t.TempDir()
	provider := &OpenClawGatewayProvider{}
	_, err := provider.LoadConfig(root)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestOpenClawGatewayComputePlan_NoChanges(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    cogos:
      baseUrl: "http://localhost:5100/v1"
      apiKey: local
channels:
  telegram:
    enabled: true
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"models": map[string]any{
			"providers": map[string]any{
				"cogos": map[string]any{
					"baseUrl": "http://localhost:5100/v1",
					"apiKey":  "local",
				},
			},
		},
		"channels": map[string]any{
			"telegram": map[string]any{
				"enabled": true,
			},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)

	plan, err := provider.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.HasChanges() {
		t.Errorf("expected no changes, got creates=%d updates=%d deletes=%d",
			plan.Summary.Creates, plan.Summary.Updates, plan.Summary.Deletes)
	}
}

func TestOpenClawGatewayComputePlan_ModelDrift(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    cogos:
      baseUrl: "http://localhost:5100/v1"
      apiKey: local
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"models": map[string]any{
			"providers": map[string]any{
				"cogos": map[string]any{
					"baseUrl": "http://localhost:9999/v1",
					"apiKey":  "changed",
				},
			},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)

	plan, err := provider.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Updates == 0 {
		t.Error("expected update action for drifted model provider")
	}
}

func TestOpenClawGatewayComputePlan_NewProvider(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    newprovider:
      baseUrl: "http://new:8080/v1"
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"models": map[string]any{
			"providers": map[string]any{},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)

	plan, err := provider.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Creates != 1 {
		t.Errorf("expected 1 create, got %d", plan.Summary.Creates)
	}
}

func TestOpenClawGatewayApplyPlan(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    cogos:
      baseUrl: "http://localhost:5100/v1"
      apiKey: local
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"models": map[string]any{
			"providers": map[string]any{
				"cogos": map[string]any{
					"baseUrl": "http://old:9999/v1",
					"apiKey":  "old",
				},
			},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)
	plan, _ := provider.ComputePlan(cfg, live, nil)

	results, err := provider.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	for _, r := range results {
		if r.Status == ApplyFailed {
			t.Errorf("action %s %s failed: %s", r.Action, r.Name, r.Error)
		}
	}

	// Verify the file was updated
	data, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	models := result["models"].(map[string]any)
	providers := models["providers"].(map[string]any)
	cogos := providers["cogos"].(map[string]any)
	if cogos["baseUrl"] != "http://localhost:5100/v1" {
		t.Errorf("expected baseUrl updated, got %v", cogos["baseUrl"])
	}
}

func TestOpenClawGatewayBuildState(t *testing.T) {
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
models:
  providers:
    cogos:
      baseUrl: "http://localhost:5100/v1"
channels:
  telegram:
    enabled: true
`)

	provider := &OpenClawGatewayProvider{}
	cfg, _ := provider.LoadConfig(root)

	live := &gatewayLiveConfig{
		Sections: map[string]map[string]any{
			"models": {
				"providers": map[string]any{
					"cogos": map[string]any{"baseUrl": "http://localhost:5100/v1"},
				},
			},
			"channels": {
				"telegram": map[string]any{"enabled": true},
			},
		},
	}

	state, err := provider.BuildState(cfg, live, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}

	if len(state.Resources) != 2 {
		t.Errorf("expected 2 resources, got %d", len(state.Resources))
	}
}

func TestOpenClawGatewaySubsetComparison(t *testing.T) {
	// Declared config is a subset of live (live has extra fields like botToken).
	// Plan should show no changes because subset comparison ignores unmanaged fields.
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
channels:
  telegram:
    enabled: true
    dmPolicy: pairing
  discord:
    enabled: true
    groupPolicy: allowlist
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"channels": map[string]any{
			"telegram": map[string]any{
				"enabled":  true,
				"dmPolicy": "pairing",
				"botToken": "secret-token-here",  // not in declared → ignored
			},
			"discord": map[string]any{
				"enabled":      true,
				"groupPolicy":  "allowlist",
				"token":        "discord-token",    // not in declared → ignored
				"allowFrom":    []any{"123456789"},  // not in declared → ignored
			},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)

	plan, err := provider.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.HasChanges() {
		for _, a := range plan.Actions {
			if a.Action != ActionSkip {
				t.Errorf("unexpected %s on %s", a.Action, a.Name)
			}
		}
	}
}

func TestOpenClawGatewayApplyPreservesSecrets(t *testing.T) {
	// When the declared config updates a field (e.g. groupPolicy) but does
	// NOT declare token/botToken (because they are redacted on export),
	// the apply must preserve those secrets in the live config.
	root := setupGatewayTestWorkspace(t)
	writeGatewayConfig(t, root, `
channels:
  discord:
    enabled: true
    groupPolicy: allowlist
`)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"channels": map[string]any{
			"discord": map[string]any{
				"enabled":     true,
				"groupPolicy": "none",                // will drift → triggers update
				"token":       "secret-discord-token", // not in declared
				"allowFrom":   []any{"123456789"},     // not in declared
			},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	cfg, _ := provider.LoadConfig(root)
	live, _ := provider.FetchLive(context.Background(), cfg)
	plan, _ := provider.ComputePlan(cfg, live, nil)

	results, err := provider.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	for _, r := range results {
		if r.Status == ApplyFailed {
			t.Errorf("action %s %s failed: %s", r.Action, r.Name, r.Error)
		}
	}

	// Re-read and verify
	data, _ := os.ReadFile(ocPath)
	var result map[string]any
	json.Unmarshal(data, &result)

	discord := result["channels"].(map[string]any)["discord"].(map[string]any)

	// Updated field should reflect declared value
	if discord["groupPolicy"] != "allowlist" {
		t.Errorf("expected groupPolicy=allowlist, got %v", discord["groupPolicy"])
	}
	// Secret must survive the merge
	if discord["token"] != "secret-discord-token" {
		t.Errorf("token was clobbered: got %v", discord["token"])
	}
	// Other unmanaged fields must also survive
	if discord["allowFrom"] == nil {
		t.Error("allowFrom was clobbered")
	}
}

func TestOpenClawGatewayExportConfig(t *testing.T) {
	root := setupGatewayTestWorkspace(t)

	ocPath := filepath.Join(root, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{
		"models": map[string]any{
			"mode": "merge",
			"providers": map[string]any{
				"cogos": map[string]any{
					"baseUrl": "http://localhost:5100/v1",
					"apiKey":  "secret-key",
				},
			},
		},
		"channels": map[string]any{
			"telegram": map[string]any{
				"enabled":  true,
				"botToken": "secret-bot-token",
			},
			"discord": map[string]any{
				"enabled": true,
				"token":   "secret-discord-token",
			},
		},
		"auth": map[string]any{
			"token": "should-not-appear",
		},
		"agents": map[string]any{
			"list": []any{},
		},
	})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	if err := provider.ExportConfig(root); err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}

	// Read the generated config
	cfgPath := filepath.Join(root, ".cog", "config", "openclaw-gateway", "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read exported config: %v", err)
	}

	content := string(data)

	// Should NOT contain secrets
	if strings.Contains(content, "secret-key") {
		t.Error("exported config contains apiKey secret")
	}
	if strings.Contains(content, "secret-bot-token") {
		t.Error("exported config contains botToken secret")
	}
	if strings.Contains(content, "secret-discord-token") {
		t.Error("exported config contains discord token")
	}

	// Should NOT contain unmanaged sections (auth, agents)
	if strings.Contains(content, "should-not-appear") {
		t.Error("exported config contains auth token")
	}

	// Should contain non-secret managed fields
	if !strings.Contains(content, "localhost:5100") {
		t.Error("exported config missing baseUrl")
	}
	if !strings.Contains(content, "merge") {
		t.Error("exported config missing models.mode")
	}
	if !strings.Contains(content, "enabled") {
		t.Error("exported config missing channel enabled fields")
	}
}

func TestDeepSubsetEqual(t *testing.T) {
	tests := []struct {
		name     string
		declared any
		live     any
		want     bool
	}{
		{
			name:     "exact match",
			declared: map[string]any{"a": float64(1)},
			live:     map[string]any{"a": float64(1)},
			want:     true,
		},
		{
			name:     "subset match",
			declared: map[string]any{"a": float64(1)},
			live:     map[string]any{"a": float64(1), "b": float64(2)},
			want:     true,
		},
		{
			name:     "value mismatch",
			declared: map[string]any{"a": float64(1)},
			live:     map[string]any{"a": float64(2), "b": float64(3)},
			want:     false,
		},
		{
			name:     "missing from live",
			declared: map[string]any{"a": float64(1)},
			live:     map[string]any{"b": float64(2)},
			want:     false,
		},
		{
			name:     "nested subset",
			declared: map[string]any{"a": map[string]any{"x": float64(1)}},
			live:     map[string]any{"a": map[string]any{"x": float64(1), "y": float64(2)}, "b": "extra"},
			want:     true,
		},
		{
			name:     "scalar match",
			declared: "hello",
			live:     "hello",
			want:     true,
		},
		{
			name:     "scalar mismatch",
			declared: "hello",
			live:     "world",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deepSubsetEqual(tt.declared, tt.live)
			if got != tt.want {
				t.Errorf("deepSubsetEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenClawGatewayHealth_Missing(t *testing.T) {
	provider := &OpenClawGatewayProvider{configPath: "/nonexistent/openclaw.json"}
	health := provider.Health()
	if health.Health != HealthMissing {
		t.Errorf("expected Missing health, got %s", health.Health)
	}
}

func TestOpenClawGatewayHealth_Present(t *testing.T) {
	tmp := t.TempDir()
	ocPath := filepath.Join(tmp, "openclaw.json")
	writeOpenClawJSON(t, ocPath, map[string]any{"meta": map[string]any{}})

	provider := &OpenClawGatewayProvider{configPath: ocPath}
	health := provider.Health()
	if health.Health != HealthHealthy {
		t.Errorf("expected Healthy, got %s", health.Health)
	}
}
