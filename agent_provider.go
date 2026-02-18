// agent_provider.go
// AgentProvider implements the Reconcilable interface for agent identity,
// delegation, and fleet reconciliation. It manages agent definitions declared
// in .cog/config/agents/agents.hcl against live state in .cog/bin/agents/.
//
// Resource subtypes:
//   - identity: persona definitions in .cog/bin/agents/identities/
//   - delegation: task agents in .cog/bin/agents/delegations/
//   - fleet: multi-agent teams in .cog/bin/agents/fleets/

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// titleCase capitalizes the first letter of a string. Used instead of
// the deprecated strings.Title.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ─── Config types ─────────────────────────────────────────────────────────────

// AgentConfig is the normalized representation of declared agent configuration.
// It can be loaded from HCL (agents.hcl) or from the registry (registry.yaml).
type AgentConfig struct {
	Reconciler  AgentReconcilerConfig `yaml:"-"`
	Identities  []AgentIdentity       `yaml:"-"`
	Delegations []AgentDelegation     `yaml:"-"`
	Fleets      []AgentFleet          `yaml:"-"`
	ConfigPath  string                `yaml:"-"`
}

// AgentReconcilerConfig controls reconciler behavior.
type AgentReconcilerConfig struct {
	DryRun             bool `yaml:"dry_run"`
	PruneUnmanaged     bool `yaml:"prune_unmanaged"`
	RespectUserManaged bool `yaml:"respect_user_managed"`
	PreserveBody       bool `yaml:"preserve_body"`
}

// AgentIdentity represents a declared identity-class agent.
type AgentIdentity struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	Description string `yaml:"description,omitempty"`
	Path        string `yaml:"path,omitempty"` // relative to .cog/bin/agents/
}

// AgentDelegation represents a declared delegation-class agent.
type AgentDelegation struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	Description string `yaml:"description,omitempty"`
	Model       string `yaml:"model,omitempty"`
	Layer       int    `yaml:"layer,omitempty"`
	Path        string `yaml:"path,omitempty"`
}

// AgentFleet represents a declared fleet configuration.
type AgentFleet struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Mode        string   `yaml:"mode,omitempty"`
	Models      []string `yaml:"models,omitempty"`
	Path        string   `yaml:"path,omitempty"`
}

// ─── Live state types ─────────────────────────────────────────────────────────

// AgentLiveState is the aggregated live state from the filesystem.
type AgentLiveState struct {
	Identities  []LiveIdentity
	Delegations []LiveDelegation
	Fleets      []LiveFleet
}

// LiveIdentity represents an identity agent found on disk.
type LiveIdentity struct {
	Name           string   `yaml:"name"`
	Role           string   `yaml:"role"`
	ContextPlugin  string   `yaml:"context_plugin,omitempty"`
	MemoryPath     string   `yaml:"memory_path,omitempty"`
	MemoryNamespace string  `yaml:"memory_namespace,omitempty"`
	DerivesFrom    string   `yaml:"derives_from,omitempty"`
	Dependencies   []string `yaml:"dependencies,omitempty"`
	FilePath       string   `yaml:"-"` // absolute path on disk
	HasFrontmatter bool     `yaml:"-"`
}

// LiveDelegation represents a delegation agent found on disk.
type LiveDelegation struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	Layer       int    `yaml:"layer,omitempty"`
	Description string `yaml:"description,omitempty"`
	Model       string `yaml:"-"`
	Provider    string `yaml:"-"`
	FilePath    string `yaml:"-"`
}

// delegationFrontmatter is the raw YAML frontmatter for delegation AGENT.md files.
type delegationFrontmatter struct {
	Name        string `yaml:"name"`
	Role        string `yaml:"role"`
	Layer       int    `yaml:"layer"`
	Description string `yaml:"description"`
	Defaults    struct {
		Model    string `yaml:"model"`
		Provider string `yaml:"provider"`
	} `yaml:"defaults"`
}

// LiveFleet represents a fleet configuration found on disk.
type LiveFleet struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Mode        string   `yaml:"integration_mode"`
	Models      []string `yaml:"-"` // extracted from the models list
	FilePath    string   `yaml:"-"`
}

// fleetYAML is the raw YAML structure for fleet files.
type fleetYAML struct {
	Name            string `yaml:"name"`
	Description     string `yaml:"description"`
	IntegrationMode string `yaml:"integration_mode"`
	Models          []struct {
		ID string `yaml:"id"`
	} `yaml:"models"`
}

// ─── Registry YAML types ──────────────────────────────────────────────────────

// agentRegistry is the parsed structure of registry.yaml.
type agentRegistry struct {
	Version     string                       `yaml:"version"`
	Updated     string                       `yaml:"updated"`
	Identities  map[string]registryEntry     `yaml:"identities"`
	Delegations map[string]registryDelegEntry `yaml:"delegations"`
	Fleets      map[string]registryFleetEntry `yaml:"fleets"`
}

type registryEntry struct {
	Path        string `yaml:"path"`
	Role        string `yaml:"role"`
	Description string `yaml:"description"`
}

type registryDelegEntry struct {
	Path        string `yaml:"path"`
	Role        string `yaml:"role"`
	Layer       int    `yaml:"layer"`
	Model       string `yaml:"model"`
	Description string `yaml:"description"`
}

type registryFleetEntry struct {
	Path        string   `yaml:"path"`
	Description string   `yaml:"description"`
	Models      []string `yaml:"models"`
	Mode        string   `yaml:"mode"`
}

// ─── Provider ─────────────────────────────────────────────────────────────────

// AgentProvider implements Reconcilable for agent management.
type AgentProvider struct {
	Root string
}

// Type returns "agent".
func (a *AgentProvider) Type() string { return "agent" }

// LoadConfig loads the declared agent configuration.
// Strategy: parse registry.yaml as the config source, since it is the
// existing single-source-of-truth listing of all agents. HCL parsing will
// be added in a future iteration.
func (a *AgentProvider) LoadConfig(root string) (any, error) {
	a.Root = root

	registryPath := filepath.Join(root, ".cog", "bin", "agents", "registry.yaml")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("agent: reading registry: %w", err)
	}

	var reg agentRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("agent: parsing registry: %w", err)
	}

	cfg := &AgentConfig{
		ConfigPath: registryPath,
		Reconciler: AgentReconcilerConfig{
			PreserveBody:       true,
			RespectUserManaged: true,
		},
	}

	// Convert registry identities to config (skip entries starting with _)
	for name, entry := range reg.Identities {
		if strings.HasPrefix(name, "_") {
			continue
		}
		cfg.Identities = append(cfg.Identities, AgentIdentity{
			Name:        name,
			Role:        entry.Role,
			Description: entry.Description,
			Path:        entry.Path,
		})
	}
	sort.Slice(cfg.Identities, func(i, j int) bool {
		return cfg.Identities[i].Name < cfg.Identities[j].Name
	})

	// Convert registry delegations
	for name, entry := range reg.Delegations {
		cfg.Delegations = append(cfg.Delegations, AgentDelegation{
			Name:        name,
			Role:        entry.Role,
			Description: entry.Description,
			Model:       entry.Model,
			Layer:       entry.Layer,
			Path:        entry.Path,
		})
	}
	sort.Slice(cfg.Delegations, func(i, j int) bool {
		return cfg.Delegations[i].Name < cfg.Delegations[j].Name
	})

	// Convert registry fleets
	for name, entry := range reg.Fleets {
		cfg.Fleets = append(cfg.Fleets, AgentFleet{
			Name:        name,
			Description: entry.Description,
			Mode:        entry.Mode,
			Models:      entry.Models,
			Path:        entry.Path,
		})
	}
	sort.Slice(cfg.Fleets, func(i, j int) bool {
		return cfg.Fleets[i].Name < cfg.Fleets[j].Name
	})

	return cfg, nil
}

// FetchLive retrieves the current agent state from the filesystem.
// Scans .cog/bin/agents/ for identity cards, delegation specs, and fleet configs.
func (a *AgentProvider) FetchLive(ctx context.Context, config any) (any, error) {
	_, ok := config.(*AgentConfig)
	if !ok {
		return nil, fmt.Errorf("agent: expected *AgentConfig, got %T", config)
	}

	root := a.Root
	agentsDir := filepath.Join(root, ".cog", "bin", "agents")

	live := &AgentLiveState{}

	// --- Scan identities ---
	identDir := filepath.Join(agentsDir, "identities")
	if entries, err := os.ReadDir(identDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			filePath := filepath.Join(identDir, entry.Name())
			ident, err := parseIdentityLive(filePath)
			if err != nil {
				continue // skip unparseable files
			}
			live.Identities = append(live.Identities, *ident)
		}
	}
	sort.Slice(live.Identities, func(i, j int) bool {
		return live.Identities[i].Name < live.Identities[j].Name
	})

	// --- Scan delegations ---
	delegDir := filepath.Join(agentsDir, "delegations")
	if entries, err := os.ReadDir(delegDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			agentFile := filepath.Join(delegDir, entry.Name(), "AGENT.md")
			deleg, err := parseDelegationLive(agentFile)
			if err != nil {
				continue
			}
			live.Delegations = append(live.Delegations, *deleg)
		}
	}
	sort.Slice(live.Delegations, func(i, j int) bool {
		return live.Delegations[i].Name < live.Delegations[j].Name
	})

	// --- Scan fleets ---
	fleetDir := filepath.Join(agentsDir, "fleets")
	if entries, err := os.ReadDir(fleetDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			filePath := filepath.Join(fleetDir, entry.Name())
			fleet, err := parseFleetLive(filePath)
			if err != nil {
				continue
			}
			live.Fleets = append(live.Fleets, *fleet)
		}
	}
	sort.Slice(live.Fleets, func(i, j int) bool {
		return live.Fleets[i].Name < live.Fleets[j].Name
	})

	return live, nil
}

// ComputePlan compares declared config against live state to produce a plan.
func (a *AgentProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	cfg, ok := config.(*AgentConfig)
	if !ok {
		return nil, fmt.Errorf("agent: expected *AgentConfig, got %T", config)
	}
	liveState, ok := live.(*AgentLiveState)
	if !ok {
		return nil, fmt.Errorf("agent: expected *AgentLiveState, got %T", live)
	}

	plan := &ReconcilePlan{
		ResourceType: "agent",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   cfg.ConfigPath,
		Metadata:     map[string]any{},
	}

	// --- Diff identities ---
	liveIdentMap := make(map[string]*LiveIdentity, len(liveState.Identities))
	for i := range liveState.Identities {
		liveIdentMap[liveState.Identities[i].Name] = &liveState.Identities[i]
	}

	for _, declared := range cfg.Identities {
		liveIdent, exists := liveIdentMap[declared.Name]
		if !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "identity",
				Name:         declared.Name,
				Details: map[string]any{
					"role":        declared.Role,
					"description": declared.Description,
					"path":        declared.Path,
				},
			})
			plan.Summary.Creates++
		} else {
			// Check for drift on reconciled fields
			drifted := false
			details := map[string]any{}
			if declared.Role != "" && declared.Role != liveIdent.Role {
				drifted = true
				details["role_declared"] = declared.Role
				details["role_live"] = liveIdent.Role
			}
			if drifted {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionUpdate,
					ResourceType: "identity",
					Name:         declared.Name,
					Details:      details,
				})
				plan.Summary.Updates++
			} else {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionSkip,
					ResourceType: "identity",
					Name:         declared.Name,
					Details:      map[string]any{"reason": "in sync"},
				})
				plan.Summary.Skipped++
			}
			delete(liveIdentMap, declared.Name)
		}
	}

	// Remaining live identities not in config
	for name := range liveIdentMap {
		if strings.HasPrefix(name, "_") || name == "" {
			continue
		}
		if cfg.Reconciler.PruneUnmanaged {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "identity",
				Name:         name,
				Details:      map[string]any{"reason": "not in config, prune enabled"},
			})
			plan.Summary.Deletes++
		} else {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("identity %q exists on disk but not in config (unmanaged)", name))
		}
	}

	// --- Diff delegations ---
	liveDelegMap := make(map[string]*LiveDelegation, len(liveState.Delegations))
	for i := range liveState.Delegations {
		// Normalize delegation name to lowercase for matching
		liveDelegMap[strings.ToLower(liveState.Delegations[i].Name)] = &liveState.Delegations[i]
	}

	for _, declared := range cfg.Delegations {
		_, exists := liveDelegMap[strings.ToLower(declared.Name)]
		if !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "delegation",
				Name:         declared.Name,
				Details: map[string]any{
					"role":  declared.Role,
					"model": declared.Model,
					"layer": declared.Layer,
				},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "delegation",
				Name:         declared.Name,
				Details:      map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
			delete(liveDelegMap, strings.ToLower(declared.Name))
		}
	}

	for name := range liveDelegMap {
		if cfg.Reconciler.PruneUnmanaged {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "delegation",
				Name:         name,
				Details:      map[string]any{"reason": "not in config, prune enabled"},
			})
			plan.Summary.Deletes++
		}
	}

	// --- Diff fleets ---
	liveFleetMap := make(map[string]*LiveFleet, len(liveState.Fleets))
	for i := range liveState.Fleets {
		liveFleetMap[liveState.Fleets[i].Name] = &liveState.Fleets[i]
	}

	for _, declared := range cfg.Fleets {
		_, exists := liveFleetMap[declared.Name]
		if !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "fleet",
				Name:         declared.Name,
				Details: map[string]any{
					"description": declared.Description,
					"mode":        declared.Mode,
					"models":      declared.Models,
				},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "fleet",
				Name:         declared.Name,
				Details:      map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
			delete(liveFleetMap, declared.Name)
		}
	}

	for name := range liveFleetMap {
		if cfg.Reconciler.PruneUnmanaged {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionDelete,
				ResourceType: "fleet",
				Name:         name,
				Details:      map[string]any{"reason": "not in config, prune enabled"},
			})
			plan.Summary.Deletes++
		}
	}

	// Sort actions for deterministic output
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].ResourceType != plan.Actions[j].ResourceType {
			return plan.Actions[i].ResourceType < plan.Actions[j].ResourceType
		}
		if plan.Actions[i].Action != plan.Actions[j].Action {
			return plan.Actions[i].Action < plan.Actions[j].Action
		}
		return plan.Actions[i].Name < plan.Actions[j].Name
	})

	return plan, nil
}

// ApplyPlan executes the planned changes against the filesystem.
func (a *AgentProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("agent: nil plan")
	}

	root := a.Root
	agentsDir := filepath.Join(root, ".cog", "bin", "agents")
	var results []ReconcileResult

	for _, action := range plan.Actions {
		result := ReconcileResult{
			Phase:  action.ResourceType,
			Action: string(action.Action),
			Name:   action.Name,
		}

		switch action.Action {
		case ActionSkip:
			result.Status = ApplySkipped
		case ActionCreate:
			err := a.applyCreate(agentsDir, action)
			if err != nil {
				result.Status = ApplyFailed
				result.Error = err.Error()
			} else {
				result.Status = ApplySucceeded
			}
		case ActionUpdate:
			err := a.applyUpdate(agentsDir, action)
			if err != nil {
				result.Status = ApplyFailed
				result.Error = err.Error()
			} else {
				result.Status = ApplySucceeded
			}
		case ActionDelete:
			err := a.applyDelete(agentsDir, action)
			if err != nil {
				result.Status = ApplyFailed
				result.Error = err.Error()
			} else {
				result.Status = ApplySucceeded
			}
		default:
			result.Status = ApplySkipped
		}

		results = append(results, result)
	}

	return results, nil
}

// BuildState constructs state from live data for snapshot/import.
func (a *AgentProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	_, ok := config.(*AgentConfig)
	if !ok {
		return nil, fmt.Errorf("agent: expected *AgentConfig, got %T", config)
	}
	liveState, ok := live.(*AgentLiveState)
	if !ok {
		return nil, fmt.Errorf("agent: expected *AgentLiveState, got %T", live)
	}

	serial := 1
	lineage := "agent-provider"
	if existing != nil {
		serial = existing.Serial + 1
		lineage = existing.Lineage
	}

	state := &ReconcileState{
		Version:      1,
		Lineage:      lineage,
		Serial:       serial,
		ResourceType: "agent",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Resources:    []ReconcileResource{},
		Metadata:     map[string]any{},
	}

	// Add identity resources
	for _, ident := range liveState.Identities {
		attrs := map[string]any{
			"role": ident.Role,
		}
		if ident.ContextPlugin != "" {
			attrs["context_plugin"] = ident.ContextPlugin
		}
		if ident.MemoryPath != "" {
			attrs["memory_path"] = ident.MemoryPath
		}
		if ident.MemoryNamespace != "" {
			attrs["memory_namespace"] = ident.MemoryNamespace
		}
		if ident.DerivesFrom != "" {
			attrs["derives_from"] = ident.DerivesFrom
		}

		state.Resources = append(state.Resources, ReconcileResource{
			Address:       fmt.Sprintf("agent.%s", ident.Name),
			Type:          "identity",
			Mode:          ModeManaged,
			ExternalID:    ident.FilePath,
			Name:          ident.Name,
			Attributes:    attrs,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		})
	}

	// Add delegation resources
	for _, deleg := range liveState.Delegations {
		attrs := map[string]any{
			"role":  deleg.Role,
			"layer": deleg.Layer,
		}
		if deleg.Model != "" {
			attrs["model"] = deleg.Model
		}
		if deleg.Description != "" {
			attrs["description"] = deleg.Description
		}

		state.Resources = append(state.Resources, ReconcileResource{
			Address:       fmt.Sprintf("delegation.%s", strings.ToLower(deleg.Name)),
			Type:          "delegation",
			Mode:          ModeManaged,
			ExternalID:    deleg.FilePath,
			Name:          strings.ToLower(deleg.Name),
			Attributes:    attrs,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		})
	}

	// Add fleet resources
	for _, fleet := range liveState.Fleets {
		attrs := map[string]any{
			"description": fleet.Description,
			"mode":        fleet.Mode,
		}
		if len(fleet.Models) > 0 {
			attrs["models"] = fleet.Models
		}

		state.Resources = append(state.Resources, ReconcileResource{
			Address:       fmt.Sprintf("fleet.%s", fleet.Name),
			Type:          "fleet",
			Mode:          ModeManaged,
			ExternalID:    fleet.FilePath,
			Name:          fleet.Name,
			Attributes:    attrs,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		})
	}

	return state, nil
}

// Health returns the current three-axis status for the Agent provider.
func (a *AgentProvider) Health() ResourceStatus {
	root := a.Root
	if root == "" {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "workspace root not set",
		}
	}

	agentsDir := filepath.Join(root, ".cog", "bin", "agents")
	info, err := os.Stat(agentsDir)
	if err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   fmt.Sprintf("agents directory missing: %v", err),
		}
	}
	if !info.IsDir() {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthDegraded,
			Operation: OperationIdle,
			Message:   "agents path exists but is not a directory",
		}
	}

	// Check for registry.yaml
	registryPath := filepath.Join(agentsDir, "registry.yaml")
	if _, err := os.Stat(registryPath); err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthDegraded,
			Operation: OperationIdle,
			Message:   "registry.yaml missing",
		}
	}

	return ResourceStatus{
		Sync:      SyncStatusUnknown,
		Health:    HealthHealthy,
		Operation: OperationIdle,
		Message:   fmt.Sprintf("agents directory readable (%s)", agentsDir),
	}
}

// ─── Filesystem parsers ───────────────────────────────────────────────────────

// frontmatterPattern matches YAML frontmatter delimited by ---
var agentFrontmatterPattern = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)$`)

// parseIdentityLive reads an identity card from disk and extracts its frontmatter.
func parseIdentityLive(filePath string) (*LiveIdentity, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	ident := &LiveIdentity{
		FilePath: filePath,
	}

	matches := agentFrontmatterPattern.FindStringSubmatch(content)
	if matches != nil {
		ident.HasFrontmatter = true
		var fm IdentityFrontmatter
		if err := yaml.Unmarshal([]byte(matches[1]), &fm); err == nil {
			ident.Name = fm.Name
			ident.Role = fm.Role
			ident.ContextPlugin = fm.ContextPlugin
			ident.MemoryPath = fm.MemoryPath
			ident.MemoryNamespace = fm.MemoryNamespace
			ident.DerivesFrom = fm.DerivesFrom
			ident.Dependencies = fm.Dependencies
		}
	}

	// If no name from frontmatter, derive from filename
	if ident.Name == "" {
		base := filepath.Base(filePath)
		base = strings.TrimSuffix(base, ".md")
		// Strip common prefixes: "identity_", "advisor_identity-"
		name := strings.TrimPrefix(base, "identity_")
		// Handle patterns like "identity_cog_interface" -> "cog"
		// Take the first segment after removing "identity_"
		parts := strings.SplitN(name, "_", 2)
		if len(parts) > 0 {
			ident.Name = parts[0]
		}
	}

	return ident, nil
}

// parseDelegationLive reads a delegation AGENT.md and extracts its frontmatter.
func parseDelegationLive(filePath string) (*LiveDelegation, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	deleg := &LiveDelegation{
		FilePath: filePath,
	}

	matches := agentFrontmatterPattern.FindStringSubmatch(content)
	if matches == nil {
		return nil, fmt.Errorf("no frontmatter in delegation file %s", filePath)
	}

	var fm delegationFrontmatter
	if err := yaml.Unmarshal([]byte(matches[1]), &fm); err != nil {
		return nil, fmt.Errorf("parsing delegation frontmatter: %w", err)
	}

	deleg.Name = fm.Name
	deleg.Role = fm.Role
	deleg.Layer = fm.Layer
	deleg.Description = fm.Description
	deleg.Model = fm.Defaults.Model
	deleg.Provider = fm.Defaults.Provider

	return deleg, nil
}

// parseFleetLive reads a fleet YAML file and extracts its configuration.
func parseFleetLive(filePath string) (*LiveFleet, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var fy fleetYAML
	if err := yaml.Unmarshal(data, &fy); err != nil {
		return nil, fmt.Errorf("parsing fleet YAML: %w", err)
	}

	fleet := &LiveFleet{
		Name:        fy.Name,
		Description: fy.Description,
		Mode:        fy.IntegrationMode,
		FilePath:    filePath,
	}

	for _, m := range fy.Models {
		if m.ID != "" {
			fleet.Models = append(fleet.Models, m.ID)
		}
	}

	return fleet, nil
}

// ─── Apply helpers ────────────────────────────────────────────────────────────

// applyCreate creates a new agent file on disk.
func (a *AgentProvider) applyCreate(agentsDir string, action ReconcileAction) error {
	switch action.ResourceType {
	case "identity":
		return a.createIdentity(agentsDir, action)
	case "delegation":
		return a.createDelegation(agentsDir, action)
	case "fleet":
		return a.createFleet(agentsDir, action)
	default:
		return fmt.Errorf("unknown resource type: %s", action.ResourceType)
	}
}

// applyUpdate updates an existing agent file on disk.
func (a *AgentProvider) applyUpdate(agentsDir string, action ReconcileAction) error {
	switch action.ResourceType {
	case "identity":
		return a.updateIdentity(agentsDir, action)
	default:
		// For delegation and fleet updates, create is equivalent to update
		// since we regenerate from config
		return a.applyCreate(agentsDir, action)
	}
}

// applyDelete removes an agent file from disk.
func (a *AgentProvider) applyDelete(agentsDir string, action ReconcileAction) error {
	switch action.ResourceType {
	case "identity":
		// Find the file to delete
		identDir := filepath.Join(agentsDir, "identities")
		entries, err := os.ReadDir(identDir)
		if err != nil {
			return fmt.Errorf("reading identities dir: %w", err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), action.Name) {
				return os.Remove(filepath.Join(identDir, entry.Name()))
			}
		}
		return fmt.Errorf("identity file for %q not found", action.Name)
	case "delegation":
		delegDir := filepath.Join(agentsDir, "delegations", action.Name)
		return os.RemoveAll(delegDir)
	case "fleet":
		fleetFile := filepath.Join(agentsDir, "fleets", action.Name+".yaml")
		return os.Remove(fleetFile)
	default:
		return fmt.Errorf("unknown resource type: %s", action.ResourceType)
	}
}

// createIdentity generates a minimal identity card.
func (a *AgentProvider) createIdentity(agentsDir string, action ReconcileAction) error {
	role, _ := action.Details["role"].(string)
	name := action.Name

	identDir := filepath.Join(agentsDir, "identities")
	if err := os.MkdirAll(identDir, 0755); err != nil {
		return err
	}

	fileName := fmt.Sprintf("identity_%s.md", name)
	filePath := filepath.Join(identDir, fileName)

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString(fmt.Sprintf("name: %s\n", name))
	buf.WriteString(fmt.Sprintf("role: %s\n", role))
	buf.WriteString("---\n\n")
	buf.WriteString(fmt.Sprintf("# Identity Card: %s\n\n", titleCase(name)))
	buf.WriteString("## Name\n")
	buf.WriteString(fmt.Sprintf("**%s**\n\n", titleCase(name)))
	buf.WriteString("## Role\n")
	buf.WriteString(fmt.Sprintf("%s\n", role))

	return os.WriteFile(filePath, buf.Bytes(), 0644)
}

// updateIdentity updates the frontmatter of an existing identity card,
// preserving the markdown body.
func (a *AgentProvider) updateIdentity(agentsDir string, action ReconcileAction) error {
	identDir := filepath.Join(agentsDir, "identities")

	// Find the existing file
	entries, err := os.ReadDir(identDir)
	if err != nil {
		return fmt.Errorf("reading identities dir: %w", err)
	}

	var targetPath string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), action.Name) && strings.HasSuffix(entry.Name(), ".md") {
			targetPath = filepath.Join(identDir, entry.Name())
			break
		}
	}

	if targetPath == "" {
		// File not found, create instead
		return a.createIdentity(agentsDir, action)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		return err
	}

	content := string(data)
	matches := agentFrontmatterPattern.FindStringSubmatch(content)
	if matches == nil {
		// No frontmatter to update, create a new file
		return a.createIdentity(agentsDir, action)
	}

	// Parse existing frontmatter
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(matches[1]), &fm); err != nil {
		fm = map[string]any{}
	}

	// Apply updates from action details
	if role, ok := action.Details["role_declared"].(string); ok {
		fm["role"] = role
	}

	// Re-serialize frontmatter
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return fmt.Errorf("serializing frontmatter: %w", err)
	}

	body := matches[2]
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n")
	buf.WriteString(body)

	return os.WriteFile(targetPath, buf.Bytes(), 0644)
}

// createDelegation generates a delegation AGENT.md.
func (a *AgentProvider) createDelegation(agentsDir string, action ReconcileAction) error {
	name := action.Name
	role, _ := action.Details["role"].(string)
	model, _ := action.Details["model"].(string)
	if model == "" {
		model = "kimi-k2"
	}

	delegDir := filepath.Join(agentsDir, "delegations", name)
	if err := os.MkdirAll(delegDir, 0755); err != nil {
		return err
	}

	filePath := filepath.Join(delegDir, "AGENT.md")

	layer := 2
	if l, ok := action.Details["layer"].(int); ok {
		layer = l
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString(fmt.Sprintf("name: %s\n", titleCase(name)))
	buf.WriteString(fmt.Sprintf("role: %s\n", role))
	buf.WriteString(fmt.Sprintf("layer: %d\n", layer))
	buf.WriteString(fmt.Sprintf("\ndefaults:\n  model: %s\n  provider: openrouter\n", model))
	buf.WriteString("---\n\n")
	buf.WriteString(fmt.Sprintf("# %s Agent\n\n", titleCase(name)))
	buf.WriteString(fmt.Sprintf("You are a %s agent.\n", role))

	return os.WriteFile(filePath, buf.Bytes(), 0644)
}

// createFleet generates a fleet YAML file.
func (a *AgentProvider) createFleet(agentsDir string, action ReconcileAction) error {
	name := action.Name
	description, _ := action.Details["description"].(string)
	mode, _ := action.Details["mode"].(string)
	if mode == "" {
		mode = "collaborative"
	}

	fleetDir := filepath.Join(agentsDir, "fleets")
	if err := os.MkdirAll(fleetDir, 0755); err != nil {
		return err
	}

	filePath := filepath.Join(fleetDir, name+".yaml")

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("name: %s\n", name))
	buf.WriteString(fmt.Sprintf("description: %s\n", description))
	buf.WriteString(fmt.Sprintf("integration_mode: %s\n", mode))

	if modelsRaw, ok := action.Details["models"]; ok {
		if models, ok := modelsRaw.([]string); ok && len(models) > 0 {
			buf.WriteString("models:\n")
			for _, m := range models {
				buf.WriteString(fmt.Sprintf("  - id: %s\n", m))
			}
		}
	}

	return os.WriteFile(filePath, buf.Bytes(), 0644)
}

// ─── Registration ─────────────────────────────────────────────────────────────

func init() {
	RegisterProvider("agent", &AgentProvider{})
}
