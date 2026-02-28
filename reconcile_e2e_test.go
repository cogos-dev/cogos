// reconcile_e2e_test.go
// End-to-end test: CRD agent definition → OpenClaw convergence.
// Verifies the full pipeline: agent CRD → openclaw-agents provider → openclaw.json.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestE2E_CRDToOpenClawConvergence(t *testing.T) {
	// 1. Temp workspace with agent CRD
	root := t.TempDir()
	crdDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if err := os.MkdirAll(crdDir, 0755); err != nil {
		t.Fatalf("mkdir CRD dir: %v", err)
	}

	crd := `apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: test-agent
spec:
  type: autonomous
  identity:
    name: TestBot
    emoji: "🤖"
  context:
    workspace: test-workspace
  runtime:
    shells:
      openclaw:
        enabled: true
        toolPolicy:
          allow: ["web_search"]
  capabilities:
    tools:
      allow: ["*"]
`
	if err := os.WriteFile(filepath.Join(crdDir, "test-agent.agent.yaml"), []byte(crd), 0644); err != nil {
		t.Fatalf("write CRD: %v", err)
	}

	// Registry file needed for CRD loading
	registryDir := filepath.Join(root, ".cog", "bin", "agents")
	registryContent := `agents:
  test-agent:
    definition: definitions/test-agent.agent.yaml
`
	if err := os.WriteFile(filepath.Join(registryDir, "registry.yaml"), []byte(registryContent), 0644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	// 2. Temp openclaw.json with empty agents
	ocDir := t.TempDir()
	ocPath := filepath.Join(ocDir, "openclaw.json")
	initialOC := map[string]any{
		"meta": map[string]any{
			"version": "test",
		},
		"agents": map[string]any{
			"list": []any{},
		},
	}
	ocData, _ := json.MarshalIndent(initialOC, "", "  ")
	if err := os.WriteFile(ocPath, ocData, 0644); err != nil {
		t.Fatalf("write openclaw.json: %v", err)
	}

	// 3. Create provider with test config path and run reconcile
	provider := &OpenClawAgentProjector{configPath: ocPath}

	cfg, err := provider.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agentCfg := cfg.(*openclawAgentConfig)
	if len(agentCfg.Agents) == 0 {
		t.Fatal("expected at least 1 agent from CRD")
	}

	// 4. Fetch live, compute plan, apply
	ctx := t
	_ = ctx

	live, err := provider.FetchLive(nil, cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	plan, err := provider.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Creates != 1 {
		t.Fatalf("expected 1 create, got %d", plan.Summary.Creates)
	}

	results, err := provider.ApplyPlan(nil, plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	for _, r := range results {
		if r.Status == ApplyFailed {
			t.Errorf("apply failed for %s: %s", r.Name, r.Error)
		}
	}

	// 5. Assert: openclaw.json now contains the agent
	data, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("re-read openclaw.json: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	var agentsSection struct {
		List []openclawAgentEntry `json:"list"`
	}
	if err := json.Unmarshal(result["agents"], &agentsSection); err != nil {
		t.Fatalf("parse agents: %v", err)
	}

	if len(agentsSection.List) != 1 {
		t.Fatalf("expected 1 agent in list, got %d", len(agentsSection.List))
	}

	agent := agentsSection.List[0]
	if agent.ID != "test-agent" {
		t.Errorf("expected ID 'test-agent', got %q", agent.ID)
	}
	if agent.Name != "TestBot" {
		t.Errorf("expected name 'TestBot', got %q", agent.Name)
	}
	if agent.Identity == nil || agent.Identity.Emoji != "🤖" {
		t.Error("expected emoji from CRD")
	}

	// 6. Modify CRD (change emoji)
	crdUpdated := `apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: test-agent
spec:
  type: autonomous
  identity:
    name: TestBot
    emoji: "🔬"
  context:
    workspace: test-workspace
  runtime:
    shells:
      openclaw:
        enabled: true
        toolPolicy:
          allow: ["web_search"]
  capabilities:
    tools:
      allow: ["*"]
`
	if err := os.WriteFile(filepath.Join(crdDir, "test-agent.agent.yaml"), []byte(crdUpdated), 0644); err != nil {
		t.Fatalf("write updated CRD: %v", err)
	}

	// 7. Re-reconcile
	cfg2, _ := provider.LoadConfig(root)
	live2, _ := provider.FetchLive(nil, cfg2)
	plan2, err := provider.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan (round 2): %v", err)
	}

	if plan2.Summary.Updates != 1 {
		t.Fatalf("expected 1 update, got %d (creates=%d, skipped=%d)",
			plan2.Summary.Updates, plan2.Summary.Creates, plan2.Summary.Skipped)
	}

	results2, err := provider.ApplyPlan(nil, plan2)
	if err != nil {
		t.Fatalf("ApplyPlan (round 2): %v", err)
	}

	for _, r := range results2 {
		if r.Status == ApplyFailed {
			t.Errorf("apply failed (round 2): %s: %s", r.Name, r.Error)
		}
	}

	// 8. Assert: openclaw.json updated with new emoji
	data2, _ := os.ReadFile(ocPath)
	var result2 map[string]json.RawMessage
	json.Unmarshal(data2, &result2)

	var agentsSection2 struct {
		List []openclawAgentEntry `json:"list"`
	}
	json.Unmarshal(result2["agents"], &agentsSection2)

	if len(agentsSection2.List) != 1 {
		t.Fatalf("expected 1 agent after update, got %d", len(agentsSection2.List))
	}
	if agentsSection2.List[0].Identity == nil || agentsSection2.List[0].Identity.Emoji != "🔬" {
		t.Error("expected updated emoji '🔬'")
	}
}

func TestE2E_MetaReconcileWaveOrder(t *testing.T) {
	// Verify that meta-reconciler respects wave ordering
	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "openclaw-agents", Wave: 1, Interval: "5m", DependsOn: []string{"agent"}},
			{Name: "agent", Wave: 0, Interval: "5m"},
		},
	}

	levels, err := resolveOrder(cfg.Resources)
	if err != nil {
		t.Fatalf("resolveOrder: %v", err)
	}

	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}

	if levels[0][0].Name != "agent" {
		t.Errorf("expected agent first, got %s", levels[0][0].Name)
	}
	if levels[1][0].Name != "openclaw-agents" {
		t.Errorf("expected openclaw-agents second, got %s", levels[1][0].Name)
	}
}
