// agent_crd.go
// Loads agent CRD definitions from .cog/bin/agents/definitions/*.agent.yaml.
// These are the single source of truth for agent bounded contexts — capabilities,
// access, identity, model config, and shell projections.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── Access Control Types ───────────────────────────────────────────────────────

// AgentCRDAccess defines access control for agents and users.
// Supports both legacy flat format (map[string]string) and the structured format
// with separate agents/users blocks. The custom UnmarshalYAML detects which
// shape is present and populates accordingly.
type AgentCRDAccess struct {
	// Agent-to-agent permissions (backward-compatible with legacy flat map)
	Agents map[string]string `yaml:"agents,omitempty"`
	// Per-user access rules
	Users map[string]AgentCRDUserAccess `yaml:"users,omitempty"`
	// Default access level for unlisted users: "none", "ro", "rw", "admin"
	DefaultLevel string `yaml:"defaultLevel,omitempty"`
}

// AgentCRDUserAccess defines per-user access to an agent.
type AgentCRDUserAccess struct {
	Level       string `yaml:"level"`                 // "admin", "rw", "ro", "none"
	MemoryScope string `yaml:"memoryScope,omitempty"` // user-specific memory path prefix
}

// UnmarshalYAML implements custom unmarshaling for AgentCRDAccess.
// It tries the structured format first (with agents/users/defaultLevel keys),
// then falls back to the legacy flat map[string]string format where all
// top-level keys are agent names mapped to permission strings.
func (a *AgentCRDAccess) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try structured format first
	type structured AgentCRDAccess
	var s structured
	if err := unmarshal(&s); err == nil && (s.Agents != nil || s.Users != nil || s.DefaultLevel != "") {
		*a = AgentCRDAccess(s)
		return nil
	}

	// Fall back to legacy flat map (agent→permission)
	var flat map[string]string
	if err := unmarshal(&flat); err == nil {
		a.Agents = flat
		return nil
	}

	return fmt.Errorf("cannot parse access: expected structured (agents/users/defaultLevel) or flat map[string]string")
}

// ─── CRD Types ──────────────────────────────────────────────────────────────────

// AgentCRD is the top-level Kubernetes-style agent definition.
type AgentCRD struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   AgentCRDMeta   `yaml:"metadata"`
	Spec       AgentCRDSpec   `yaml:"spec"`
}

type AgentCRDMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type AgentCRDSpec struct {
	Type         string                 `yaml:"type"` // interactive, declarative, headless
	Identity     AgentCRDIdentity       `yaml:"identity,omitempty"`
	Context      AgentCRDContext        `yaml:"context,omitempty"`
	Capabilities AgentCRDCapabilities   `yaml:"capabilities,omitempty"`
	Access       AgentCRDAccess         `yaml:"access,omitempty"`
	ModelConfig  AgentCRDModelConfig    `yaml:"modelConfig,omitempty"`
	Runtime      AgentCRDRuntime        `yaml:"runtime,omitempty"`
	Scheduling   AgentCRDScheduling     `yaml:"scheduling,omitempty"`
	Bus          AgentCRDBus            `yaml:"bus,omitempty"`
}

type AgentCRDIdentity struct {
	Card  string `yaml:"card,omitempty"`
	Name  string `yaml:"name,omitempty"`
	Emoji string `yaml:"emoji,omitempty"`
	Role  string `yaml:"role,omitempty"`
}

type AgentCRDContext struct {
	Memory       AgentCRDMemory `yaml:"memory,omitempty"`
	SystemPrompt string         `yaml:"systemPrompt,omitempty"`
	Workspace    string         `yaml:"workspace,omitempty"`
}

type AgentCRDMemory struct {
	Sector string   `yaml:"sector,omitempty"`
	Scope  []string `yaml:"scope,omitempty"`
}

type AgentCRDCapabilities struct {
	Tools     AgentCRDToolPolicy `yaml:"tools,omitempty"`
	MCPServers []AgentCRDMCP     `yaml:"mcpServers,omitempty"`
	Advertise bool               `yaml:"advertise,omitempty"`
}

type AgentCRDToolPolicy struct {
	Allow []string `yaml:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty"`
}

type AgentCRDMCP struct {
	Name      string   `yaml:"name"`
	URL       string   `yaml:"url,omitempty"`
	Protocol  string   `yaml:"protocol,omitempty"`
	ToolNames []string `yaml:"toolNames,omitempty"`
}

type AgentCRDModelConfig struct {
	Provider     string   `yaml:"provider,omitempty"`
	Model        string   `yaml:"model,omitempty"`
	Fallbacks    []string `yaml:"fallbacks,omitempty"`
	AllowedTools []string `yaml:"allowedTools,omitempty"`
	Temperature  *float64 `yaml:"temperature,omitempty"`
	MaxTokens    *int     `yaml:"maxTokens,omitempty"`
}

type AgentCRDRuntime struct {
	Sandbox AgentCRDSandbox `yaml:"sandbox,omitempty"`
	Shells  AgentCRDShells  `yaml:"shells,omitempty"`
}

type AgentCRDSandbox struct {
	Mode      string `yaml:"mode,omitempty"`      // off, non-main, all, scoped
	Workspace string `yaml:"workspace,omitempty"` // none, ro, rw
	Scope     string `yaml:"scope,omitempty"`     // session, agent, shared
}

type AgentCRDShells struct {
	OpenClaw  *AgentCRDShellOpenClaw  `yaml:"openclaw,omitempty"`
	ClaudeCode *AgentCRDShellClaude   `yaml:"claude-code,omitempty"`
}

type AgentCRDShellOpenClaw struct {
	Channel        string             `yaml:"channel,omitempty"`
	Channels       []string           `yaml:"channels,omitempty"`
	RequireMention *bool              `yaml:"requireMention,omitempty"`
	AutoThread     *bool              `yaml:"autoThread,omitempty"`
	Sandbox        string             `yaml:"sandbox,omitempty"`
	ToolPolicy     AgentCRDToolPolicy `yaml:"toolPolicy,omitempty"`
}

type AgentCRDShellClaude struct {
	AllowedTools              []string `yaml:"allowedTools,omitempty"`
	DangerouslySkipPermissions *bool   `yaml:"dangerouslySkipPermissions,omitempty"`
}

type AgentCRDScheduling struct {
	Cron               []AgentCRDCronEntry `yaml:"cron,omitempty"`
	EventSubscriptions []AgentCRDEvent     `yaml:"eventSubscriptions,omitempty"`
}

type AgentCRDCronEntry struct {
	Schedule string `yaml:"schedule"`
	Task     string `yaml:"task"`
	Channel  string `yaml:"channel,omitempty"`
}

type AgentCRDEvent struct {
	Type    string `yaml:"type"`
	Filter  string `yaml:"filter,omitempty"`
	Channel string `yaml:"channel,omitempty"`
}

type AgentCRDBus struct {
	Endpoint  string   `yaml:"endpoint,omitempty"`
	Subscribe []string `yaml:"subscribe,omitempty"`
	Publish   []string `yaml:"publish,omitempty"`
}

// ─── Loader ─────────────────────────────────────────────────────────────────────

// agentCRDDir returns the path to the agent definitions directory.
func agentCRDDir(root string) string {
	return filepath.Join(root, ".cog", "bin", "agents", "definitions")
}

// LoadAgentCRD loads a single agent CRD by name.
// Looks for {root}/.cog/bin/agents/definitions/{name}.agent.yaml.
func LoadAgentCRD(root, name string) (*AgentCRD, error) {
	path := filepath.Join(agentCRDDir(root), name+".agent.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load agent CRD %q: %w", name, err)
	}

	var crd AgentCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return nil, fmt.Errorf("parse agent CRD %q: %w", name, err)
	}

	if crd.APIVersion != "cog.os/v1alpha1" || crd.Kind != "Agent" {
		return nil, fmt.Errorf("agent CRD %q: unexpected apiVersion=%q kind=%q",
			name, crd.APIVersion, crd.Kind)
	}

	return &crd, nil
}

// ListAgentCRDs loads all agent CRDs from the definitions directory.
func ListAgentCRDs(root string) ([]AgentCRD, error) {
	dir := agentCRDDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list agent CRDs: %w", err)
	}

	var crds []AgentCRD
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".agent.yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".agent.yaml")
		crd, err := LoadAgentCRD(root, name)
		if err != nil {
			return nil, err
		}
		crds = append(crds, *crd)
	}
	return crds, nil
}

// GetAgentCRDToolPolicy returns the tool policy for an agent from its CRD.
// Returns nil if no CRD is found (backward-compatible — no restriction).
func GetAgentCRDToolPolicy(root, agentName string) (*AgentCRDToolPolicyResult, error) {
	crd, err := LoadAgentCRD(root, agentName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // No CRD = no policy = unrestricted
		}
		return nil, err
	}

	result := &AgentCRDToolPolicyResult{
		AllowedTools: crd.Spec.ModelConfig.AllowedTools,
		DenyTools:    crd.Spec.Capabilities.Tools.Deny,
	}

	// Shell-specific override: claude-code allowedTools takes precedence
	if cc := crd.Spec.Runtime.Shells.ClaudeCode; cc != nil && len(cc.AllowedTools) > 0 {
		result.AllowedTools = cc.AllowedTools
	}

	// DangerouslySkipPermissions defaults to false
	if cc := crd.Spec.Runtime.Shells.ClaudeCode; cc != nil && cc.DangerouslySkipPermissions != nil {
		result.DangerouslySkipPermissions = *cc.DangerouslySkipPermissions
	}

	return result, nil
}

// AgentCRDToolPolicyResult contains the resolved tool policy for an agent.
type AgentCRDToolPolicyResult struct {
	AllowedTools              []string // Claude CLI --allowed-tools patterns
	DenyTools                 []string // Tools explicitly denied
	DangerouslySkipPermissions bool    // Whether to pass --dangerously-skip-permissions
}
