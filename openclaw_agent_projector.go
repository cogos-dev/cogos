// openclaw_agent_projector.go
// Reconcilable provider that projects CogOS agent CRDs into OpenClaw's
// openclaw.json config. The agent definitions at .cog/bin/agents/definitions/
// are the single source of truth — this reconciler computes drift between
// declared state (CRDs) and live state (openclaw.json), then applies updates.
//
// Usage:
//   cog reconcile plan openclaw-agents
//   cog reconcile apply openclaw-agents

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

func init() {
	RegisterProvider("openclaw-agents", &OpenClawAgentProjector{})
}

// OpenClawAgentProjector implements Reconcilable for agent projection.
type OpenClawAgentProjector struct {
	configPath string // resolved path to openclaw.json
}

func (p *OpenClawAgentProjector) Type() string { return "openclaw-agents" }

// ─── Config types (declared state from CRDs) ────────────────────────────────

// openclawAgentConfig is the declared state: agent CRDs that have an OpenClaw shell.
type openclawAgentConfig struct {
	Agents []AgentCRD
}

// ─── Live types (current state in openclaw.json) ────────────────────────────

// openclawAgentLive represents the live agent entries from openclaw.json.
type openclawAgentLive struct {
	Agents []openclawAgentEntry
	// Full raw JSON for safe read-modify-write
	RawConfig map[string]json.RawMessage
}

// openclawAgentEntry is the shape of agents.list[] in openclaw.json.
type openclawAgentEntry struct {
	ID        string                 `json:"id"`
	Default   bool                   `json:"default,omitempty"`
	Name      string                 `json:"name"`
	Workspace string                 `json:"workspace,omitempty"`
	Identity  *openclawAgentIdentity `json:"identity,omitempty"`
	Tools     *openclawAgentTools    `json:"tools,omitempty"`
}

type openclawAgentIdentity struct {
	Name  string `json:"name,omitempty"`
	Emoji string `json:"emoji,omitempty"`
}

type openclawAgentTools struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// ─── CRD → OpenClaw ID mapping ─────────────────────────────────────────────

// crdToOpenClawID maps a CRD metadata.name to the OpenClaw agents.list[].id.
// The "whirl" agent maps to "main" (OpenClaw's default agent convention).
func crdToOpenClawID(crdName string) string {
	if crdName == "whirl" {
		return "main"
	}
	return crdName
}

// ─── CRD → OpenClaw entry projection ───────────────────────────────────────

// projectCRDToEntry converts a CRD into the OpenClaw agent entry it should produce.
func projectCRDToEntry(crd AgentCRD) openclawAgentEntry {
	entry := openclawAgentEntry{
		ID:        crdToOpenClawID(crd.Metadata.Name),
		Name:      crd.Spec.Identity.Name,
		Workspace: crd.Spec.Context.Workspace,
		Identity: &openclawAgentIdentity{
			Name:  crd.Spec.Identity.Name,
			Emoji: crd.Spec.Identity.Emoji,
		},
	}

	// Mark whirl as default
	if crd.Metadata.Name == "whirl" {
		entry.Default = true
	}

	// Project tool policy from CRD capabilities (not modelConfig — that's for Claude CLI)
	allow := crd.Spec.Capabilities.Tools.Allow
	deny := crd.Spec.Capabilities.Tools.Deny

	// Override with shell-specific tool policy if present
	if oc := crd.Spec.Runtime.Shells.OpenClaw; oc != nil {
		if len(oc.ToolPolicy.Allow) > 0 {
			allow = oc.ToolPolicy.Allow
		}
		if len(oc.ToolPolicy.Deny) > 0 {
			deny = oc.ToolPolicy.Deny
		}
	}

	// Only set tools if there's an actual restriction (["*"] = unrestricted = no tools block)
	if len(allow) > 0 && !(len(allow) == 1 && allow[0] == "*") {
		entry.Tools = &openclawAgentTools{
			Allow: allow,
		}
	}
	if len(deny) > 0 {
		if entry.Tools == nil {
			entry.Tools = &openclawAgentTools{}
		}
		entry.Tools.Deny = deny
	}

	return entry
}

// ─── Reconcilable implementation ────────────────────────────────────────────

func (p *OpenClawAgentProjector) LoadConfig(root string) (any, error) {
	crds, err := ListAgentCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("openclaw-agents: load CRDs: %w", err)
	}

	// Filter to agents with an OpenClaw shell config OR interactive type (whirl)
	var relevant []AgentCRD
	for _, crd := range crds {
		if crd.Spec.Runtime.Shells.OpenClaw != nil || crd.Spec.Type == "interactive" {
			relevant = append(relevant, crd)
		}
	}

	return &openclawAgentConfig{Agents: relevant}, nil
}

func (p *OpenClawAgentProjector) FetchLive(ctx context.Context, config any) (any, error) {
	configPath := p.resolveConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &openclawAgentLive{}, nil
		}
		return nil, fmt.Errorf("openclaw-agents: read config: %w", err)
	}

	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, fmt.Errorf("openclaw-agents: parse config: %w", err)
	}

	agentsRaw, ok := rawConfig["agents"]
	if !ok {
		return &openclawAgentLive{RawConfig: rawConfig}, nil
	}

	var agentsSection struct {
		List []openclawAgentEntry `json:"list"`
	}
	if err := json.Unmarshal(agentsRaw, &agentsSection); err != nil {
		return nil, fmt.Errorf("openclaw-agents: parse agents.list: %w", err)
	}

	return &openclawAgentLive{
		Agents:    agentsSection.List,
		RawConfig: rawConfig,
	}, nil
}

func (p *OpenClawAgentProjector) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	cfg := config.(*openclawAgentConfig)
	liveState := live.(*openclawAgentLive)

	plan := &ReconcilePlan{
		ResourceType: "openclaw-agents",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	// Index live agents by ID
	liveByID := make(map[string]openclawAgentEntry)
	for _, entry := range liveState.Agents {
		liveByID[entry.ID] = entry
	}

	// Check each declared CRD against live state
	for _, crd := range cfg.Agents {
		desired := projectCRDToEntry(crd)
		liveEntry, exists := liveByID[desired.ID]

		if !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "openclaw-agent",
				Name:         desired.ID,
				Details: map[string]any{
					"agent_name": desired.Name,
					"workspace":  desired.Workspace,
					"payload":    desired,
				},
			})
			plan.Summary.Creates++
		} else if !agentEntriesEqual(desired, liveEntry) {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "openclaw-agent",
				Name:         desired.ID,
				Details: map[string]any{
					"agent_name": desired.Name,
					"payload":    desired,
				},
			})
			plan.Summary.Updates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "openclaw-agent",
				Name:         desired.ID,
				Details: map[string]any{
					"agent_name": desired.Name,
				},
			})
			plan.Summary.Skipped++
		}

		delete(liveByID, desired.ID)
	}

	// Remaining live agents are unmanaged — skip
	for id, entry := range liveByID {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionSkip,
			ResourceType: "openclaw-agent",
			Name:         id,
			Details: map[string]any{
				"agent_name": entry.Name,
				"reason":     "no CRD — unmanaged",
			},
		})
		plan.Summary.Skipped++
	}

	return plan, nil
}

func (p *OpenClawAgentProjector) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	configPath := p.resolveConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("openclaw-agents: read config for apply: %w", err)
	}

	var fullConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &fullConfig); err != nil {
		return nil, fmt.Errorf("openclaw-agents: parse config for apply: %w", err)
	}

	var agentsSection map[string]json.RawMessage
	if raw, ok := fullConfig["agents"]; ok {
		if err := json.Unmarshal(raw, &agentsSection); err != nil {
			return nil, fmt.Errorf("openclaw-agents: parse agents section: %w", err)
		}
	} else {
		agentsSection = make(map[string]json.RawMessage)
	}

	var agentList []openclawAgentEntry
	if raw, ok := agentsSection["list"]; ok {
		if err := json.Unmarshal(raw, &agentList); err != nil {
			return nil, fmt.Errorf("openclaw-agents: parse agents.list: %w", err)
		}
	}

	// Index by ID for mutation
	listByID := make(map[string]int)
	for i, entry := range agentList {
		listByID[entry.ID] = i
	}

	var results []ReconcileResult
	modified := false

	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			results = append(results, ReconcileResult{
				Action: string(ActionSkip),
				Name:   action.Name,
				Status: ApplySkipped,
			})
			continue
		}

		// Extract payload from Details
		payloadRaw, ok := action.Details["payload"]
		if !ok {
			results = append(results, ReconcileResult{
				Action: string(action.Action),
				Name:   action.Name,
				Status: ApplyFailed,
				Error:  "missing payload in action details",
			})
			continue
		}
		desired, ok := payloadRaw.(openclawAgentEntry)
		if !ok {
			results = append(results, ReconcileResult{
				Action: string(action.Action),
				Name:   action.Name,
				Status: ApplyFailed,
				Error:  "invalid payload type",
			})
			continue
		}

		switch action.Action {
		case ActionCreate:
			agentList = append(agentList, desired)
			listByID[desired.ID] = len(agentList) - 1
			modified = true
			log.Printf("[openclaw-agents] Created agent %q", desired.ID)
			results = append(results, ReconcileResult{
				Action: string(ActionCreate),
				Name:   desired.ID,
				Status: ApplySucceeded,
			})

		case ActionUpdate:
			if idx, ok := listByID[desired.ID]; ok {
				agentList[idx] = desired
				modified = true
				log.Printf("[openclaw-agents] Updated agent %q", desired.ID)
				results = append(results, ReconcileResult{
					Action: string(ActionUpdate),
					Name:   desired.ID,
					Status: ApplySucceeded,
				})
			}

		case ActionDelete:
			if idx, ok := listByID[desired.ID]; ok {
				agentList = append(agentList[:idx], agentList[idx+1:]...)
				modified = true
				log.Printf("[openclaw-agents] Deleted agent %q", desired.ID)
				results = append(results, ReconcileResult{
					Action: string(ActionDelete),
					Name:   desired.ID,
					Status: ApplySucceeded,
				})
			}
		}
	}

	if !modified {
		return results, nil
	}

	// Write back
	listJSON, err := json.Marshal(agentList)
	if err != nil {
		return results, fmt.Errorf("openclaw-agents: marshal list: %w", err)
	}
	agentsSection["list"] = listJSON

	agentsJSON, err := json.Marshal(agentsSection)
	if err != nil {
		return results, fmt.Errorf("openclaw-agents: marshal agents: %w", err)
	}
	fullConfig["agents"] = agentsJSON

	output, err := json.MarshalIndent(fullConfig, "", "  ")
	if err != nil {
		return results, fmt.Errorf("openclaw-agents: marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, output, 0644); err != nil {
		return results, fmt.Errorf("openclaw-agents: write config: %w", err)
	}

	log.Printf("[openclaw-agents] Wrote %d bytes to %s", len(output), configPath)
	return results, nil
}

func (p *OpenClawAgentProjector) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState := live.(*openclawAgentLive)

	state := &ReconcileState{
		Version:      1,
		Serial:       1,
		Lineage:      "openclaw-agents",
		ResourceType: "openclaw-agents",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	for _, entry := range liveState.Agents {
		attrs := map[string]any{
			"name":      entry.Name,
			"workspace": entry.Workspace,
		}
		if entry.Identity != nil {
			attrs["emoji"] = entry.Identity.Emoji
		}
		if entry.Tools != nil {
			attrs["tools_allow"] = entry.Tools.Allow
		}

		state.Resources = append(state.Resources, ReconcileResource{
			Address:       "agents.list." + entry.ID,
			Type:          "openclaw-agent",
			Mode:          ModeManaged,
			ExternalID:    entry.ID,
			Name:          entry.Name,
			Attributes:    attrs,
			LastRefreshed: time.Now().Format(time.RFC3339),
		})
	}

	return state, nil
}

func (p *OpenClawAgentProjector) Health() ResourceStatus {
	configPath := p.resolveConfigPath()
	if _, err := os.Stat(configPath); err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func (p *OpenClawAgentProjector) resolveConfigPath() string {
	if p.configPath != "" {
		return p.configPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "openclaw.json")
}

// agentEntriesEqual compares two agent entries for drift.
func agentEntriesEqual(desired, live openclawAgentEntry) bool {
	if desired.Name != live.Name || desired.Workspace != live.Workspace || desired.Default != live.Default {
		return false
	}

	// Compare identity
	if desired.Identity != nil && live.Identity != nil {
		if desired.Identity.Name != live.Identity.Name || desired.Identity.Emoji != live.Identity.Emoji {
			return false
		}
	} else if (desired.Identity == nil) != (live.Identity == nil) {
		return false
	}

	// Compare tools
	if desired.Tools != nil && live.Tools != nil {
		if !reflect.DeepEqual(desired.Tools.Allow, live.Tools.Allow) ||
			!reflect.DeepEqual(desired.Tools.Deny, live.Tools.Deny) {
			return false
		}
	} else if (desired.Tools == nil) != (live.Tools == nil) {
		return false
	}

	return true
}
