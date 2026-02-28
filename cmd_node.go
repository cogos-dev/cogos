// cmd_node.go — CLI commands for node identity and shell registration.
// Commands: info, shells, help.
//
// Reads from ~/.cog/node/identity.yaml and ~/.cog/node/shells.yaml,
// which are generated on first shell init.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// NodeIdentity represents ~/.cog/node/identity.yaml.
type NodeIdentity struct {
	Version string           `yaml:"version"`
	Node    NodeInfo         `yaml:"node"`
	Caps    NodeCapabilities `yaml:"capabilities"`
}

// NodeInfo is the machine-level identity.
type NodeInfo struct {
	ID          string `yaml:"id"`
	Hostname    string `yaml:"hostname"`
	MachineUUID string `yaml:"machine_uuid"`
	Created     string `yaml:"created"`
	Type        string `yaml:"type"`
	OS          string `yaml:"os"`
	Arch        string `yaml:"arch"`
}

// NodeCapabilities declares what this node can do.
type NodeCapabilities struct {
	Shell     bool `yaml:"shell"`
	Inference bool `yaml:"inference"`
	GUI       bool `yaml:"gui"`
	Docker    bool `yaml:"docker"`
}

// ShellRegistration represents ~/.cog/node/shells.yaml.
type ShellRegistration struct {
	Version string                 `yaml:"version"`
	Shells  map[string]ShellConfig `yaml:"shells"`
}

// ShellConfig describes a single registered shell.
type ShellConfig struct {
	Type         string        `yaml:"type"`
	Root         string        `yaml:"root"`
	Sessions     ShellSessions `yaml:"sessions,omitempty"`
	Hooks        ShellHooks    `yaml:"hooks,omitempty"`
	Capabilities []string      `yaml:"capabilities"`
	Status       string        `yaml:"status"`
}

// ShellSessions describes where a shell stores session transcripts.
type ShellSessions struct {
	Pattern string `yaml:"pattern"`
	Format  string `yaml:"format"`
}

// ShellHooks describes how a shell exposes lifecycle hooks.
type ShellHooks struct {
	Mechanism string   `yaml:"mechanism"`
	Events    []string `yaml:"events"`
}

// ─── Loaders ────────────────────────────────────────────────────────────────────

// nodeDir returns the path to ~/.cog/node/.
func nodeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cog", "node")
	}
	return filepath.Join(home, ".cog", "node")
}

// LoadNodeIdentity reads ~/.cog/node/identity.yaml.
func LoadNodeIdentity() (*NodeIdentity, error) {
	path := filepath.Join(nodeDir(), "identity.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read node identity: %w", err)
	}
	var ident NodeIdentity
	if err := yaml.Unmarshal(data, &ident); err != nil {
		return nil, fmt.Errorf("parse node identity: %w", err)
	}
	return &ident, nil
}

// LoadShellRegistration reads ~/.cog/node/shells.yaml.
func LoadShellRegistration() (*ShellRegistration, error) {
	path := filepath.Join(nodeDir(), "shells.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shell registration: %w", err)
	}
	var reg ShellRegistration
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse shell registration: %w", err)
	}
	return &reg, nil
}

// ─── Command dispatcher ─────────────────────────────────────────────────────────

// cmdNode dispatches node subcommands.
func cmdNode(args []string) error {
	if len(args) == 0 {
		return cmdNodeInfo()
	}

	switch args[0] {
	case "info":
		return cmdNodeInfo()
	case "shells":
		return cmdNodeShells()
	case "help", "-h", "--help":
		return cmdNodeHelp()
	default:
		return fmt.Errorf("unknown node command: %s", args[0])
	}
}

// ─── info ───────────────────────────────────────────────────────────────────────

func cmdNodeInfo() error {
	ident, err := LoadNodeIdentity()
	if err != nil {
		return fmt.Errorf("load node identity (run shell init first): %w", err)
	}

	// Capabilities
	var caps []string
	if ident.Caps.Shell {
		caps = append(caps, "shell")
	}
	if ident.Caps.Inference {
		caps = append(caps, "inference")
	}
	if ident.Caps.GUI {
		caps = append(caps, "gui")
	}
	if ident.Caps.Docker {
		caps = append(caps, "docker")
	}

	// Shells
	shells, _ := LoadShellRegistration()
	var shellNames []string
	if shells != nil {
		for name, cfg := range shells.Shells {
			shellNames = append(shellNames, fmt.Sprintf("%s (%s)", name, cfg.Status))
		}
	}

	// Cluster status
	clusterStatus := "disabled"
	if root, _, err := ResolveWorkspace(); err == nil {
		provider := NewBEPProvider(root)
		if cfg, err := provider.LoadConfig(); err == nil && cfg.Enabled {
			clusterStatus = "enabled"
		}
	}

	// Workspaces
	var workspaces []string
	if globalCfg, err := loadGlobalConfig(); err == nil {
		for name := range globalCfg.Workspaces {
			workspaces = append(workspaces, name)
		}
	}

	fmt.Printf("Node: %s (%s)\n", ident.Node.ID, ident.Node.Type)
	fmt.Printf("  OS:      %s (%s)\n", ident.Node.OS, ident.Node.Arch)
	fmt.Printf("  Caps:    %s\n", strings.Join(caps, ", "))
	if len(shellNames) > 0 {
		fmt.Printf("  Shells:  %s\n", strings.Join(shellNames, ", "))
	}
	fmt.Printf("  Cluster: %s\n", clusterStatus)
	if len(workspaces) > 0 {
		fmt.Printf("  Workspaces: %s\n", strings.Join(workspaces, ", "))
	}

	return nil
}

// ─── shells ─────────────────────────────────────────────────────────────────────

func cmdNodeShells() error {
	reg, err := LoadShellRegistration()
	if err != nil {
		return err
	}

	if len(reg.Shells) == 0 {
		fmt.Println("No shells registered.")
		return nil
	}

	for name, cfg := range reg.Shells {
		fmt.Printf("  %-15s type=%-12s status=%-8s caps=%s\n",
			name, cfg.Type, cfg.Status, strings.Join(cfg.Capabilities, ","))
	}
	return nil
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdNodeHelp() error {
	fmt.Println("Usage: cog node <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  info      Show node identity, capabilities, and cluster status")
	fmt.Println("  shells    List registered shells with status and capabilities")
	fmt.Println()
	fmt.Println("Node identity is stored at ~/.cog/node/identity.yaml")
	fmt.Println("Shell registrations at ~/.cog/node/shells.yaml")
	return nil
}
