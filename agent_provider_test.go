// agent_provider_test.go
// Tests for the AgentProvider Reconcilable implementation.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

// setupAgentTestDir creates a temporary workspace with the standard agent directory
// structure and a minimal registry.yaml.
func setupAgentTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	agentsDir := filepath.Join(root, ".cog", "bin", "agents")
	dirs := []string{
		filepath.Join(agentsDir, "identities"),
		filepath.Join(agentsDir, "delegations", "coordinator"),
		filepath.Join(agentsDir, "delegations", "researcher"),
		filepath.Join(agentsDir, "fleets"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	return root
}

// writeIdentityCard writes a minimal identity card to the test workspace.
func writeIdentityCard(t *testing.T, root, name, role string, withFrontmatter bool) string {
	t.Helper()
	identDir := filepath.Join(root, ".cog", "bin", "agents", "identities")
	filePath := filepath.Join(identDir, fmt.Sprintf("identity_%s.md", name))

	var content string
	if withFrontmatter {
		content = fmt.Sprintf("---\nname: %s\nrole: %s\n---\n\n# Identity Card: %s\n", name, role, name)
	} else {
		content = fmt.Sprintf("# Identity Card: %s\n\nRole: %s\n", name, role)
	}

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("writing identity card: %v", err)
	}
	return filePath
}

// writeDelegationAgent writes a minimal delegation AGENT.md.
func writeDelegationAgent(t *testing.T, root, name, role string, layer int) string {
	t.Helper()
	delegDir := filepath.Join(root, ".cog", "bin", "agents", "delegations", name)
	if err := os.MkdirAll(delegDir, 0755); err != nil {
		t.Fatalf("mkdir delegation: %v", err)
	}

	filePath := filepath.Join(delegDir, "AGENT.md")
	content := fmt.Sprintf(`---
name: %s
role: %s
layer: %d
description: Test %s delegation

defaults:
  model: kimi-k2
  provider: openrouter
---

# %s Agent
`, titleCase(name), role, layer, name, titleCase(name))

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("writing delegation: %v", err)
	}
	return filePath
}

// writeFleetConfig writes a minimal fleet YAML file.
func writeFleetConfig(t *testing.T, root, name, mode string, models []string) string {
	t.Helper()
	fleetDir := filepath.Join(root, ".cog", "bin", "agents", "fleets")
	if err := os.MkdirAll(fleetDir, 0755); err != nil {
		t.Fatalf("mkdir fleets: %v", err)
	}

	filePath := filepath.Join(fleetDir, name+".yaml")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("name: %s\n", name))
	sb.WriteString(fmt.Sprintf("description: Test %s fleet\n", name))
	sb.WriteString(fmt.Sprintf("integration_mode: %s\n", mode))
	if len(models) > 0 {
		sb.WriteString("models:\n")
		for _, m := range models {
			sb.WriteString(fmt.Sprintf("  - id: %s\n", m))
		}
	}

	if err := os.WriteFile(filePath, []byte(sb.String()), 0644); err != nil {
		t.Fatalf("writing fleet: %v", err)
	}
	return filePath
}

// writeRegistry writes a registry.yaml to the test workspace.
func writeRegistry(t *testing.T, root string, content string) {
	t.Helper()
	registryPath := filepath.Join(root, ".cog", "bin", "agents", "registry.yaml")
	if err := os.WriteFile(registryPath, []byte(content), 0644); err != nil {
		t.Fatalf("writing registry: %v", err)
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestAgentProviderType(t *testing.T) {
	p := &AgentProvider{}
	if p.Type() != "agent" {
		t.Errorf("Type() = %q, want %q", p.Type(), "agent")
	}
}

func TestAgentProviderRegistered(t *testing.T) {
	if !HasProvider("agent") {
		t.Fatal("agent provider not registered")
	}
	p, err := GetProvider("agent")
	if err != nil {
		t.Fatalf("GetProvider(agent) failed: %v", err)
	}
	if p.Type() != "agent" {
		t.Errorf("registered provider Type() = %q, want %q", p.Type(), "agent")
	}
}

func TestAgentProviderLoadConfig(t *testing.T) {
	root := setupAgentTestDir(t)
	writeRegistry(t, root, `
version: "1.0"
identities:
  cog:
    path: identities/identity_cog.md
    role: "Workspace Guardian"
    description: "Test cog agent"
  dev:
    path: identities/identity_dev.md
    role: "Implementation Specialist"
    description: "Test dev agent"
  _template:
    path: identities/identity_template.md
    role: "Template"
    description: "Should be skipped"
delegations:
  coordinator:
    path: delegations/coordinator/AGENT.md
    role: "Task Orchestrator"
    layer: 1
    model: kimi-k2
    description: "Test coordinator"
fleets:
  research_team:
    path: fleets/research_team.yaml
    description: "Test research team"
    models: [kimi-k2, gpt-4o-mini]
    mode: collaborative
`)

	p := &AgentProvider{}
	raw, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	cfg, ok := raw.(*AgentConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *AgentConfig", raw)
	}

	// Should have 2 identities (template filtered out)
	if len(cfg.Identities) != 2 {
		t.Errorf("Identities count = %d, want 2", len(cfg.Identities))
	}

	// Check sorted order
	if len(cfg.Identities) >= 2 {
		if cfg.Identities[0].Name != "cog" {
			t.Errorf("Identities[0].Name = %q, want %q", cfg.Identities[0].Name, "cog")
		}
		if cfg.Identities[1].Name != "dev" {
			t.Errorf("Identities[1].Name = %q, want %q", cfg.Identities[1].Name, "dev")
		}
	}

	if len(cfg.Delegations) != 1 {
		t.Errorf("Delegations count = %d, want 1", len(cfg.Delegations))
	}
	if len(cfg.Delegations) > 0 && cfg.Delegations[0].Name != "coordinator" {
		t.Errorf("Delegations[0].Name = %q, want %q", cfg.Delegations[0].Name, "coordinator")
	}

	if len(cfg.Fleets) != 1 {
		t.Errorf("Fleets count = %d, want 1", len(cfg.Fleets))
	}
}

func TestAgentProviderLoadConfigMissing(t *testing.T) {
	root := t.TempDir()
	p := &AgentProvider{}
	_, err := p.LoadConfig(root)
	if err == nil {
		t.Error("expected error when registry.yaml is missing")
	}
}

func TestAgentProviderFetchLive(t *testing.T) {
	root := setupAgentTestDir(t)
	writeRegistry(t, root, `version: "1.0"
identities: {}
delegations: {}
fleets: {}
`)

	// Create test files on disk
	writeIdentityCard(t, root, "cog", "Workspace Guardian", true)
	writeIdentityCard(t, root, "dev", "Implementation Specialist", true)
	writeDelegationAgent(t, root, "coordinator", "Task Orchestrator", 1)
	writeFleetConfig(t, root, "research_team", "collaborative", []string{"kimi-k2", "gpt-4o-mini"})

	p := &AgentProvider{Root: root}
	cfg := &AgentConfig{} // minimal config for type assertion

	raw, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive failed: %v", err)
	}

	live, ok := raw.(*AgentLiveState)
	if !ok {
		t.Fatalf("FetchLive returned %T, want *AgentLiveState", raw)
	}

	if len(live.Identities) != 2 {
		t.Errorf("Identities count = %d, want 2", len(live.Identities))
	}

	// Verify identity fields parsed from frontmatter
	foundCog := false
	for _, ident := range live.Identities {
		if ident.Name == "cog" {
			foundCog = true
			if ident.Role != "Workspace Guardian" {
				t.Errorf("cog.Role = %q, want %q", ident.Role, "Workspace Guardian")
			}
			if !ident.HasFrontmatter {
				t.Error("cog.HasFrontmatter = false, want true")
			}
		}
	}
	if !foundCog {
		t.Error("cog identity not found in live state")
	}

	if len(live.Delegations) != 1 {
		t.Errorf("Delegations count = %d, want 1", len(live.Delegations))
	}
	if len(live.Delegations) > 0 {
		if live.Delegations[0].Name != "Coordinator" {
			t.Errorf("Delegation[0].Name = %q, want %q", live.Delegations[0].Name, "Coordinator")
		}
		if live.Delegations[0].Model != "kimi-k2" {
			t.Errorf("Delegation[0].Model = %q, want %q", live.Delegations[0].Model, "kimi-k2")
		}
	}

	if len(live.Fleets) != 1 {
		t.Errorf("Fleets count = %d, want 1", len(live.Fleets))
	}
	if len(live.Fleets) > 0 {
		if live.Fleets[0].Name != "research_team" {
			t.Errorf("Fleet[0].Name = %q, want %q", live.Fleets[0].Name, "research_team")
		}
		if live.Fleets[0].Mode != "collaborative" {
			t.Errorf("Fleet[0].Mode = %q, want %q", live.Fleets[0].Mode, "collaborative")
		}
		if len(live.Fleets[0].Models) != 2 {
			t.Errorf("Fleet[0].Models count = %d, want 2", len(live.Fleets[0].Models))
		}
	}
}

func TestAgentProviderFetchLiveWrongType(t *testing.T) {
	p := &AgentProvider{Root: t.TempDir()}
	_, err := p.FetchLive(context.Background(), "not a config")
	if err == nil {
		t.Error("expected error for wrong config type")
	}
}

func TestAgentProviderFetchLiveEmptyDir(t *testing.T) {
	root := setupAgentTestDir(t)
	writeRegistry(t, root, `version: "1.0"`)

	p := &AgentProvider{Root: root}
	raw, err := p.FetchLive(context.Background(), &AgentConfig{})
	if err != nil {
		t.Fatalf("FetchLive failed on empty dirs: %v", err)
	}
	live := raw.(*AgentLiveState)
	if len(live.Identities) != 0 || len(live.Delegations) != 0 || len(live.Fleets) != 0 {
		t.Errorf("expected empty live state, got %d identities, %d delegations, %d fleets",
			len(live.Identities), len(live.Delegations), len(live.Fleets))
	}
}

func TestAgentProviderComputePlan(t *testing.T) {
	cfg := &AgentConfig{
		Identities: []AgentIdentity{
			{Name: "cog", Role: "Workspace Guardian"},
			{Name: "dev", Role: "Implementation Specialist"},
			{Name: "newagent", Role: "New Agent"},
		},
		Delegations: []AgentDelegation{
			{Name: "coordinator", Role: "Task Orchestrator", Layer: 1},
		},
		Fleets: []AgentFleet{
			{Name: "research_team", Description: "Research team", Mode: "collaborative"},
		},
	}

	live := &AgentLiveState{
		Identities: []LiveIdentity{
			{Name: "cog", Role: "Workspace Guardian"},
			{Name: "dev", Role: "Implementation Specialist"},
		},
		Delegations: []LiveDelegation{
			{Name: "Coordinator", Role: "Task Orchestrator", Layer: 1},
		},
		Fleets: []LiveFleet{
			{Name: "research_team", Description: "Research team", Mode: "collaborative"},
		},
	}

	p := &AgentProvider{}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}

	if plan.ResourceType != "agent" {
		t.Errorf("ResourceType = %q, want %q", plan.ResourceType, "agent")
	}

	// Should have 1 create (newagent) and 4 skips (cog, dev, coordinator, research_team)
	if plan.Summary.Creates != 1 {
		t.Errorf("Summary.Creates = %d, want 1", plan.Summary.Creates)
	}
	if plan.Summary.Skipped != 4 {
		t.Errorf("Summary.Skipped = %d, want 4", plan.Summary.Skipped)
	}
	if plan.Summary.Updates != 0 {
		t.Errorf("Summary.Updates = %d, want 0", plan.Summary.Updates)
	}
	if plan.Summary.Deletes != 0 {
		t.Errorf("Summary.Deletes = %d, want 0", plan.Summary.Deletes)
	}

	// Verify the create action is for newagent
	foundCreate := false
	for _, a := range plan.Actions {
		if a.Action == ActionCreate && a.Name == "newagent" {
			foundCreate = true
			if a.ResourceType != "identity" {
				t.Errorf("create action ResourceType = %q, want %q", a.ResourceType, "identity")
			}
		}
	}
	if !foundCreate {
		t.Error("expected create action for newagent")
	}
}

func TestAgentProviderComputePlanDrift(t *testing.T) {
	cfg := &AgentConfig{
		Identities: []AgentIdentity{
			{Name: "cog", Role: "Updated Guardian Role"},
		},
	}

	live := &AgentLiveState{
		Identities: []LiveIdentity{
			{Name: "cog", Role: "Workspace Guardian"},
		},
	}

	p := &AgentProvider{}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}

	if plan.Summary.Updates != 1 {
		t.Errorf("Summary.Updates = %d, want 1 (role drift)", plan.Summary.Updates)
	}

	for _, a := range plan.Actions {
		if a.Action == ActionUpdate && a.Name == "cog" {
			if a.Details["role_declared"] != "Updated Guardian Role" {
				t.Errorf("update details role_declared = %v, want %q", a.Details["role_declared"], "Updated Guardian Role")
			}
			if a.Details["role_live"] != "Workspace Guardian" {
				t.Errorf("update details role_live = %v, want %q", a.Details["role_live"], "Workspace Guardian")
			}
		}
	}
}

func TestAgentProviderComputePlanPrune(t *testing.T) {
	cfg := &AgentConfig{
		Reconciler: AgentReconcilerConfig{
			PruneUnmanaged: true,
		},
		Identities: []AgentIdentity{
			{Name: "cog", Role: "Guardian"},
		},
	}

	live := &AgentLiveState{
		Identities: []LiveIdentity{
			{Name: "cog", Role: "Guardian"},
			{Name: "orphan", Role: "Orphan Agent"},
		},
	}

	p := &AgentProvider{}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}

	if plan.Summary.Deletes != 1 {
		t.Errorf("Summary.Deletes = %d, want 1", plan.Summary.Deletes)
	}

	foundDelete := false
	for _, a := range plan.Actions {
		if a.Action == ActionDelete && a.Name == "orphan" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Error("expected delete action for orphan agent")
	}
}

func TestAgentProviderComputePlanNoPrune(t *testing.T) {
	cfg := &AgentConfig{
		Reconciler: AgentReconcilerConfig{
			PruneUnmanaged: false,
		},
		Identities: []AgentIdentity{
			{Name: "cog", Role: "Guardian"},
		},
	}

	live := &AgentLiveState{
		Identities: []LiveIdentity{
			{Name: "cog", Role: "Guardian"},
			{Name: "orphan", Role: "Orphan Agent"},
		},
	}

	p := &AgentProvider{}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan failed: %v", err)
	}

	if plan.Summary.Deletes != 0 {
		t.Errorf("Summary.Deletes = %d, want 0 (prune disabled)", plan.Summary.Deletes)
	}

	if len(plan.Warnings) == 0 {
		t.Error("expected warning about unmanaged agent, got none")
	}
}

func TestAgentProviderComputePlanWrongTypes(t *testing.T) {
	p := &AgentProvider{}

	_, err := p.ComputePlan("bad", nil, nil)
	if err == nil {
		t.Error("expected error for wrong config type")
	}

	_, err = p.ComputePlan(&AgentConfig{}, "bad", nil)
	if err == nil {
		t.Error("expected error for wrong live type")
	}
}

func TestAgentProviderHealth(t *testing.T) {
	t.Run("no root", func(t *testing.T) {
		p := &AgentProvider{}
		h := p.Health()
		if h.Health != HealthMissing {
			t.Errorf("Health = %s, want Missing", h.Health)
		}
	})

	t.Run("missing agents dir", func(t *testing.T) {
		p := &AgentProvider{Root: t.TempDir()}
		h := p.Health()
		if h.Health != HealthMissing {
			t.Errorf("Health = %s, want Missing", h.Health)
		}
	})

	t.Run("agents dir exists but no registry", func(t *testing.T) {
		root := setupAgentTestDir(t)
		p := &AgentProvider{Root: root}
		h := p.Health()
		if h.Health != HealthDegraded {
			t.Errorf("Health = %s, want Degraded (no registry.yaml)", h.Health)
		}
	})

	t.Run("healthy", func(t *testing.T) {
		root := setupAgentTestDir(t)
		writeRegistry(t, root, `version: "1.0"`)
		p := &AgentProvider{Root: root}
		h := p.Health()
		if h.Health != HealthHealthy {
			t.Errorf("Health = %s, want Healthy", h.Health)
		}
	})
}

func TestAgentProviderBuildState(t *testing.T) {
	cfg := &AgentConfig{}
	live := &AgentLiveState{
		Identities: []LiveIdentity{
			{
				Name:           "cog",
				Role:           "Workspace Guardian",
				ContextPlugin:  ".cog/hooks/context-plugins/cog.sh",
				MemoryPath:     ".cog/mem/identities/cog/",
				MemoryNamespace: "cog://",
				DerivesFrom:    "cog://ontology/crystal",
				FilePath:       "/test/identities/identity_cog.md",
			},
			{
				Name:     "dev",
				Role:     "Implementation Specialist",
				FilePath: "/test/identities/identity_dev.md",
			},
		},
		Delegations: []LiveDelegation{
			{
				Name:        "Coordinator",
				Role:        "coordinator",
				Layer:       1,
				Model:       "kimi-k2",
				Description: "Test coordinator",
				FilePath:    "/test/delegations/coordinator/AGENT.md",
			},
		},
		Fleets: []LiveFleet{
			{
				Name:        "research_team",
				Description: "Test research team",
				Mode:        "collaborative",
				Models:      []string{"kimi-k2", "gpt-4o-mini"},
				FilePath:    "/test/fleets/research_team.yaml",
			},
		},
	}

	p := &AgentProvider{}
	state, err := p.BuildState(cfg, live, nil)
	if err != nil {
		t.Fatalf("BuildState failed: %v", err)
	}

	if state.Version != 1 {
		t.Errorf("Version = %d, want 1", state.Version)
	}
	if state.ResourceType != "agent" {
		t.Errorf("ResourceType = %q, want %q", state.ResourceType, "agent")
	}
	if state.Serial != 1 {
		t.Errorf("Serial = %d, want 1", state.Serial)
	}

	// 2 identities + 1 delegation + 1 fleet = 4 resources
	if len(state.Resources) != 4 {
		t.Fatalf("Resources count = %d, want 4", len(state.Resources))
	}

	// Verify identity resource
	cogRes := findResource(state.Resources, "agent.cog")
	if cogRes == nil {
		t.Fatal("resource agent.cog not found")
	}
	if cogRes.Type != "identity" {
		t.Errorf("agent.cog Type = %q, want identity", cogRes.Type)
	}
	if cogRes.Attributes["role"] != "Workspace Guardian" {
		t.Errorf("agent.cog role = %v, want Workspace Guardian", cogRes.Attributes["role"])
	}
	if cogRes.Attributes["context_plugin"] != ".cog/hooks/context-plugins/cog.sh" {
		t.Errorf("agent.cog context_plugin = %v", cogRes.Attributes["context_plugin"])
	}

	// Verify delegation resource
	coordRes := findResource(state.Resources, "delegation.coordinator")
	if coordRes == nil {
		t.Fatal("resource delegation.coordinator not found")
	}
	if coordRes.Type != "delegation" {
		t.Errorf("delegation.coordinator Type = %q, want delegation", coordRes.Type)
	}

	// Verify fleet resource
	fleetRes := findResource(state.Resources, "fleet.research_team")
	if fleetRes == nil {
		t.Fatal("resource fleet.research_team not found")
	}
	if fleetRes.Type != "fleet" {
		t.Errorf("fleet.research_team Type = %q, want fleet", fleetRes.Type)
	}
}

func TestAgentProviderBuildStateIncrementsSerial(t *testing.T) {
	cfg := &AgentConfig{}
	live := &AgentLiveState{}
	existing := &ReconcileState{
		Serial:  5,
		Lineage: "existing-lineage",
	}

	p := &AgentProvider{}
	state, err := p.BuildState(cfg, live, existing)
	if err != nil {
		t.Fatalf("BuildState failed: %v", err)
	}

	if state.Serial != 6 {
		t.Errorf("Serial = %d, want 6", state.Serial)
	}
	if state.Lineage != "existing-lineage" {
		t.Errorf("Lineage = %q, want %q", state.Lineage, "existing-lineage")
	}
}

func TestAgentProviderBuildStateWrongTypes(t *testing.T) {
	p := &AgentProvider{}

	_, err := p.BuildState("bad", nil, nil)
	if err == nil {
		t.Error("expected error for wrong config type")
	}

	_, err = p.BuildState(&AgentConfig{}, "bad", nil)
	if err == nil {
		t.Error("expected error for wrong live type")
	}
}

func TestAgentProviderApplyPlanCreate(t *testing.T) {
	root := setupAgentTestDir(t)
	p := &AgentProvider{Root: root}

	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionCreate,
				ResourceType: "identity",
				Name:         "testagent",
				Details: map[string]any{
					"role": "Test Agent Role",
				},
			},
		},
		Summary: ReconcileSummary{Creates: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Results count = %d, want 1", len(results))
	}
	if results[0].Status != ApplySucceeded {
		t.Errorf("Result status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	// Verify file was created
	createdPath := filepath.Join(root, ".cog", "bin", "agents", "identities", "identity_testagent.md")
	data, err := os.ReadFile(createdPath)
	if err != nil {
		t.Fatalf("created file not found: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "name: testagent") {
		t.Error("created file missing name in frontmatter")
	}
	if !strings.Contains(content, "role: Test Agent Role") {
		t.Error("created file missing role in frontmatter")
	}
	if !strings.Contains(content, "# Identity Card:") {
		t.Error("created file missing markdown body")
	}
}

func TestAgentProviderApplyPlanSkip(t *testing.T) {
	root := setupAgentTestDir(t)
	p := &AgentProvider{Root: root}

	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionSkip,
				ResourceType: "identity",
				Name:         "cog",
				Details:      map[string]any{"reason": "in sync"},
			},
		},
		Summary: ReconcileSummary{Skipped: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("Results count = %d, want 1", len(results))
	}
	if results[0].Status != ApplySkipped {
		t.Errorf("Result status = %q, want skipped", results[0].Status)
	}
}

func TestAgentProviderApplyPlanUpdate(t *testing.T) {
	root := setupAgentTestDir(t)
	writeIdentityCard(t, root, "cog", "Old Role", true)

	p := &AgentProvider{Root: root}
	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionUpdate,
				ResourceType: "identity",
				Name:         "cog",
				Details: map[string]any{
					"role_declared": "New Guardian Role",
					"role_live":     "Old Role",
				},
			},
		},
		Summary: ReconcileSummary{Updates: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Result status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	// Read updated file
	updatedPath := filepath.Join(root, ".cog", "bin", "agents", "identities", "identity_cog.md")
	data, err := os.ReadFile(updatedPath)
	if err != nil {
		t.Fatalf("reading updated file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "New Guardian Role") {
		t.Errorf("updated file should contain new role; got:\n%s", content)
	}
	// Body should be preserved
	if !strings.Contains(content, "# Identity Card: cog") {
		t.Errorf("updated file body should be preserved; got:\n%s", content)
	}
}

func TestAgentProviderApplyPlanDelete(t *testing.T) {
	root := setupAgentTestDir(t)
	writeIdentityCard(t, root, "orphan", "Orphan", true)

	p := &AgentProvider{Root: root}
	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionDelete,
				ResourceType: "identity",
				Name:         "orphan",
			},
		},
		Summary: ReconcileSummary{Deletes: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Result status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	// Verify file was deleted
	deletedPath := filepath.Join(root, ".cog", "bin", "agents", "identities", "identity_orphan.md")
	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Error("deleted file still exists on disk")
	}
}

func TestAgentProviderApplyPlanNil(t *testing.T) {
	p := &AgentProvider{Root: t.TempDir()}
	_, err := p.ApplyPlan(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil plan")
	}
}

func TestAgentProviderApplyPlanCreateDelegation(t *testing.T) {
	root := setupAgentTestDir(t)
	p := &AgentProvider{Root: root}

	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionCreate,
				ResourceType: "delegation",
				Name:         "newdeleg",
				Details: map[string]any{
					"role":  "New Delegation",
					"model": "gpt-4o-mini",
				},
			},
		},
		Summary: ReconcileSummary{Creates: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Result status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	createdPath := filepath.Join(root, ".cog", "bin", "agents", "delegations", "newdeleg", "AGENT.md")
	data, err := os.ReadFile(createdPath)
	if err != nil {
		t.Fatalf("created delegation file not found: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "model: gpt-4o-mini") {
		t.Error("created delegation missing model")
	}
}

func TestAgentProviderApplyPlanCreateFleet(t *testing.T) {
	root := setupAgentTestDir(t)
	p := &AgentProvider{Root: root}

	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionCreate,
				ResourceType: "fleet",
				Name:         "newfleet",
				Details: map[string]any{
					"description": "New test fleet",
					"mode":        "parallel",
					"models":      []string{"model-a", "model-b"},
				},
			},
		},
		Summary: ReconcileSummary{Creates: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Result status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	createdPath := filepath.Join(root, ".cog", "bin", "agents", "fleets", "newfleet.yaml")
	data, err := os.ReadFile(createdPath)
	if err != nil {
		t.Fatalf("created fleet file not found: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "integration_mode: parallel") {
		t.Errorf("created fleet missing mode; got:\n%s", content)
	}
	if !strings.Contains(content, "model-a") {
		t.Error("created fleet missing model-a")
	}
}

func TestAgentProviderApplyPlanDeleteFleet(t *testing.T) {
	root := setupAgentTestDir(t)
	writeFleetConfig(t, root, "dead_fleet", "parallel", nil)

	p := &AgentProvider{Root: root}
	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionDelete,
				ResourceType: "fleet",
				Name:         "dead_fleet",
			},
		},
		Summary: ReconcileSummary{Deletes: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	deletedPath := filepath.Join(root, ".cog", "bin", "agents", "fleets", "dead_fleet.yaml")
	if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
		t.Error("deleted fleet file still exists")
	}
}

func TestAgentProviderApplyPlanDeleteDelegation(t *testing.T) {
	root := setupAgentTestDir(t)
	writeDelegationAgent(t, root, "dead_deleg", "Old Role", 2)

	p := &AgentProvider{Root: root}
	plan := &ReconcilePlan{
		ResourceType: "agent",
		Actions: []ReconcileAction{
			{
				Action:       ActionDelete,
				ResourceType: "delegation",
				Name:         "dead_deleg",
			},
		},
		Summary: ReconcileSummary{Deletes: 1},
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan failed: %v", err)
	}

	if results[0].Status != ApplySucceeded {
		t.Errorf("Status = %q, want succeeded; error: %s", results[0].Status, results[0].Error)
	}

	deletedDir := filepath.Join(root, ".cog", "bin", "agents", "delegations", "dead_deleg")
	if _, err := os.Stat(deletedDir); !os.IsNotExist(err) {
		t.Error("deleted delegation directory still exists")
	}
}

// ─── Identity parsing edge cases ──────────────────────────────────────────────

func TestParseIdentityLiveNoFrontmatter(t *testing.T) {
	root := setupAgentTestDir(t)
	writeIdentityCard(t, root, "plain", "Some Role", false)

	filePath := filepath.Join(root, ".cog", "bin", "agents", "identities", "identity_plain.md")
	ident, err := parseIdentityLive(filePath)
	if err != nil {
		t.Fatalf("parseIdentityLive failed: %v", err)
	}

	if ident.HasFrontmatter {
		t.Error("HasFrontmatter should be false for card without frontmatter")
	}

	// Name should be derived from filename
	if ident.Name != "plain" {
		t.Errorf("Name = %q, want %q (derived from filename)", ident.Name, "plain")
	}
}

func TestParseIdentityLiveWithFrontmatter(t *testing.T) {
	root := setupAgentTestDir(t)
	identDir := filepath.Join(root, ".cog", "bin", "agents", "identities")
	filePath := filepath.Join(identDir, "identity_test_agent.md")

	content := `---
name: test_agent
role: Test Role
context_plugin: .cog/hooks/test.sh
memory_path: .cog/mem/identities/test/
memory_namespace: test://
derives_from: cog://ontology/crystal
---

# Identity Card: Test Agent
Body content here.
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	ident, err := parseIdentityLive(filePath)
	if err != nil {
		t.Fatalf("parseIdentityLive failed: %v", err)
	}

	if !ident.HasFrontmatter {
		t.Error("HasFrontmatter should be true")
	}
	if ident.Name != "test_agent" {
		t.Errorf("Name = %q, want %q", ident.Name, "test_agent")
	}
	if ident.Role != "Test Role" {
		t.Errorf("Role = %q, want %q", ident.Role, "Test Role")
	}
	if ident.ContextPlugin != ".cog/hooks/test.sh" {
		t.Errorf("ContextPlugin = %q", ident.ContextPlugin)
	}
	if ident.MemoryNamespace != "test://" {
		t.Errorf("MemoryNamespace = %q", ident.MemoryNamespace)
	}
	if ident.DerivesFrom != "cog://ontology/crystal" {
		t.Errorf("DerivesFrom = %q", ident.DerivesFrom)
	}
}

func TestParseDelegationLive(t *testing.T) {
	root := setupAgentTestDir(t)
	writeDelegationAgent(t, root, "testdeleg", "expert", 2)

	filePath := filepath.Join(root, ".cog", "bin", "agents", "delegations", "testdeleg", "AGENT.md")
	deleg, err := parseDelegationLive(filePath)
	if err != nil {
		t.Fatalf("parseDelegationLive failed: %v", err)
	}

	if deleg.Name != "Testdeleg" {
		t.Errorf("Name = %q, want %q", deleg.Name, "Testdeleg")
	}
	if deleg.Role != "expert" {
		t.Errorf("Role = %q, want %q", deleg.Role, "expert")
	}
	if deleg.Layer != 2 {
		t.Errorf("Layer = %d, want 2", deleg.Layer)
	}
	if deleg.Model != "kimi-k2" {
		t.Errorf("Model = %q, want %q", deleg.Model, "kimi-k2")
	}
}

func TestParseFleetLive(t *testing.T) {
	root := setupAgentTestDir(t)
	writeFleetConfig(t, root, "testfleet", "dialectic", []string{"model-a", "model-b", "model-c"})

	filePath := filepath.Join(root, ".cog", "bin", "agents", "fleets", "testfleet.yaml")
	fleet, err := parseFleetLive(filePath)
	if err != nil {
		t.Fatalf("parseFleetLive failed: %v", err)
	}

	if fleet.Name != "testfleet" {
		t.Errorf("Name = %q, want %q", fleet.Name, "testfleet")
	}
	if fleet.Mode != "dialectic" {
		t.Errorf("Mode = %q, want %q", fleet.Mode, "dialectic")
	}
	if len(fleet.Models) != 3 {
		t.Errorf("Models count = %d, want 3", len(fleet.Models))
	}
}

func TestTitleCase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "A"},
		{"hello", "Hello"},
		{"Hello", "Hello"},
		{"coordinator", "Coordinator"},
	}
	for _, tt := range tests {
		if got := titleCase(tt.in); got != tt.want {
			t.Errorf("titleCase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func findResource(resources []ReconcileResource, address string) *ReconcileResource {
	for i := range resources {
		if resources[i].Address == address {
			return &resources[i]
		}
	}
	return nil
}
