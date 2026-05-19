// cmd_node.go — CLI commands for node identity, shell registration, and
// multi-node deployment (ADR-063).
//
// Commands: info, shells, init, start, stop, status, help.
//
// Reads from ~/.cog/node/identity.yaml and ~/.cog/node/shells.yaml,
// which are generated on first shell init.
//
// Multi-node commands use .cog/config/node/node.json within the workspace.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"strconv"
	"strings"
	"time"

	"os/exec"

	"github.com/myrgic/cogos/envspec"
	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// NodeIdentity represents ~/.cog/node/identity.yaml.
//
// This is the Layer 3 machine-registration record — hostname, OS, capability
// flags. It is NOT a cryptographic identity and does NOT contain key material.
//
// Three-layer node identity taxonomy (ADR-099):
//
//	L1: Constellation mesh identity — ECDSA P-256, NodeID = hex(sha256(DER(pubkey)))
//	    Repo: github.com/myrgic/constellation — peer heartbeat signing, ledger events
//	L2: Workspace operational identity — Ed25519, stored in .cog/identity.json
//	    Scope: cog workspace CLI (cog/.cog/ package main), NOT the cogos kernel
//	    Deprecated-pending-migration; see ADR-099 for retirement preconditions
//	L3: Node machine-registration record (this type) — no keys, YAML
//	    Scope: cogos kernel, multi-node deployment (ADR-062, ADR-063)
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
	case "init":
		return cmdNodeInit(args[1:])
	case "start":
		return cmdNodeStart(args[1:])
	case "stop":
		return cmdNodeStop(args[1:])
	case "status":
		return cmdNodeStatus(args[1:])
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

// ─── init ───────────────────────────────────────────────────────────────────────

// initWizardState holds all wizard selections before generating config.
type initWizardState struct {
	nodeName       string
	workspaceID    string
	tier           string
	syncProfile    string
	inferenceMode  string // "claude-code", "api-key", "openrouter"
	channels       []string // "discord", "telegram", "signal", etc.
	enableVault    bool
	enableTTS      bool
	bindAddr       string
	kernelPort     int
	gatewayPort    int
	mdnsEnabled    bool
	claudeAuthPath string // path to existing ~/.claude/ for volume mount
}

func cmdNodeInit(args []string) error {
	// Parse flags
	nonInteractive := false
	tier := ""
	workspaceID := ""
	yes := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tier", "-t":
			if i+1 < len(args) {
				tier = args[i+1]
				i++
			}
		case "--workspace", "-w":
			if i+1 < len(args) {
				workspaceID = args[i+1]
				i++
			}
		case "--non-interactive", "--ni":
			nonInteractive = true
		case "--yes", "-y":
			yes = true
			nonInteractive = true
		}
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found: %w", err)
	}

	// Check if already initialized
	if _, err := LoadNodeConfig(root); err == nil {
		if !yes {
			fmt.Println("Node already initialized. Reinitialize? (y/N)")
			if !promptConfirm() {
				return fmt.Errorf("node already initialized (see: cog node status)")
			}
		}
	}

	var ws initWizardState

	if nonInteractive {
		// Use flags + defaults for non-interactive mode
		if tier == "" {
			tier = "primary"
		}
		ws = initWizardState{
			nodeName:      hostname(),
			workspaceID:   firstNonEmpty(workspaceID, filepath.Base(root)),
			tier:          tier,
			syncProfile:   tierToSyncProfile(tier),
			inferenceMode: "claude-code",
			channels:      []string{},
			enableVault:   tier == "primary",
			enableTTS:     false,
			bindAddr:      "0.0.0.0",
			kernelPort:    5100,
			gatewayPort:   18789,
			mdnsEnabled:   true,
		}
	} else {
		ws, err = runInitWizard(root, workspaceID, tier)
		if err != nil {
			return err
		}
	}

	// Validate tier
	switch ws.tier {
	case "primary", "standard", "micro":
		// OK
	default:
		return fmt.Errorf("invalid tier %q (must be primary, standard, or micro)", ws.tier)
	}

	// Generate node identity
	fmt.Println()
	fmt.Println("Generating node identity...")
	nodeID, err := GenerateNodeID(root)
	if err != nil {
		return fmt.Errorf("generate node identity: %w", err)
	}

	// Build config from wizard state
	cfg := wizardToConfig(nodeID, &ws)

	// Detect container runtime
	docker := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := docker.Ping(ctx); err != nil {
		fmt.Printf("  Container runtime: not detected (%v)\n", err)
		fmt.Println("  Services will run natively (no containerization)")
	} else {
		fmt.Printf("  Container runtime: %s\n", docker.SocketPath())
	}

	// Save config
	if err := SaveNodeConfig(root, cfg); err != nil {
		return fmt.Errorf("save node config: %w", err)
	}

	// Create .envspec from wizard selections
	envspecPath := filepath.Join(root, ".envspec")
	if err := writeWizardEnvspec(envspecPath, &ws); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create .envspec: %v\n", err)
	} else {
		fmt.Printf("  Created: .envspec (secrets template)\n")
	}

	// Print summary
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║         Node Initialized Successfully        ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  Node ID:     %s\n", nodeID)
	fmt.Printf("  Name:        %s\n", ws.nodeName)
	fmt.Printf("  Workspace:   %s\n", ws.workspaceID)
	fmt.Printf("  Tier:        %s\n", ws.tier)
	fmt.Printf("  Inference:   %s\n", ws.inferenceMode)
	fmt.Printf("  Sync:        %s\n", ws.syncProfile)
	if len(ws.channels) > 0 {
		fmt.Printf("  Channels:    %s\n", strings.Join(ws.channels, ", "))
	}
	if ws.enableVault {
		fmt.Printf("  Vaultwarden: enabled\n")
	}
	if ws.enableTTS {
		fmt.Printf("  TTS:         enabled\n")
	}
	fmt.Printf("  Config:      %s\n", nodeConfigPath(root))
	fmt.Printf("  Keys:        %s\n", nodeKeyDir(root))
	fmt.Println()

	// Print next steps based on selections
	fmt.Println("Next steps:")
	step := 1
	if ws.inferenceMode == "claude-code" {
		if ws.claudeAuthPath == "" {
			fmt.Printf("  %d. Run: claude login (authenticate Claude Code)\n", step)
			step++
		} else {
			fmt.Printf("  %d. Claude auth: using %s\n", step, ws.claudeAuthPath)
			step++
		}
	}
	if len(ws.channels) > 0 {
		fmt.Printf("  %d. Edit .envspec to add your %s token(s)\n", step, strings.Join(ws.channels, "/"))
		step++
	}
	if ws.enableVault {
		fmt.Printf("  %d. Start Vaultwarden: cog node start (web UI at http://localhost:8222)\n", step)
		step++
	}
	fmt.Printf("  %d. Run: cog node start\n", step)

	return nil
}

// ─── Interactive Wizard ──────────────────────────────────────────────────────────

func runInitWizard(root, presetWorkspace, presetTier string) (initWizardState, error) {
	scanner := bufio.NewScanner(os.Stdin)
	ws := initWizardState{}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║          CogOS Node Initialization           ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// ── Step 1: Node name ──────────────────────────────────────────────
	defaultName := hostname()
	fmt.Printf("Node name [%s]: ", defaultName)
	ws.nodeName = promptLine(scanner, defaultName)

	// ── Step 2: Workspace ID ───────────────────────────────────────────
	defaultWS := firstNonEmpty(presetWorkspace, filepath.Base(root))
	fmt.Printf("Workspace ID [%s]: ", defaultWS)
	ws.workspaceID = promptLine(scanner, defaultWS)

	// ── Step 3: Node Tier ──────────────────────────────────────────────
	fmt.Println()
	fmt.Println("Node tier:")
	fmt.Println("  1) primary   — Full stack: kernel + gateway + vaultwarden (always-on machine)")
	fmt.Println("  2) standard  — Kernel + gateway (secondary node, syncs from primary)")
	fmt.Println("  3) micro     — Kernel only (minimal: Pi, edge device, VPS)")
	fmt.Println()

	defaultTierChoice := "1"
	if presetTier != "" {
		switch presetTier {
		case "primary":
			defaultTierChoice = "1"
		case "standard":
			defaultTierChoice = "2"
		case "micro":
			defaultTierChoice = "3"
		}
	}
	fmt.Printf("Select tier [%s]: ", defaultTierChoice)
	tierChoice := promptLine(scanner, defaultTierChoice)
	switch tierChoice {
	case "1", "primary":
		ws.tier = "primary"
	case "2", "standard":
		ws.tier = "standard"
	case "3", "micro":
		ws.tier = "micro"
	default:
		ws.tier = "primary"
	}

	// ── Step 4: Sync Profile ───────────────────────────────────────────
	fmt.Println()
	fmt.Println("Sync profile (what this node keeps in sync):")
	fmt.Println("  1) full    — Everything (default for primary)")
	fmt.Println("  2) build   — Source + memory + config (good for dev machines)")
	fmt.Println("  3) mobile  — Binaries + memory + config (saves space)")
	fmt.Println("  4) edge    — Memory + config only (minimal)")
	fmt.Println()

	defaultSync := tierToSyncChoice(ws.tier)
	fmt.Printf("Select profile [%s]: ", defaultSync)
	syncChoice := promptLine(scanner, defaultSync)
	switch syncChoice {
	case "1", "full":
		ws.syncProfile = "full"
	case "2", "build":
		ws.syncProfile = "build"
	case "3", "mobile":
		ws.syncProfile = "mobile"
	case "4", "edge":
		ws.syncProfile = "edge"
	default:
		ws.syncProfile = tierToSyncProfile(ws.tier)
	}

	// ── Step 5: Inference Provider ─────────────────────────────────────
	fmt.Println()
	fmt.Println("Inference provider:")
	fmt.Println("  1) Claude Code  — Uses Claude Max subscription (OAuth login, no API key)")
	fmt.Println("  2) API Key      — Anthropic API key (stored in secrets)")
	fmt.Println("  3) OpenRouter   — OpenRouter API (multi-model)")
	fmt.Println()
	fmt.Printf("Select provider [1]: ")
	inferChoice := promptLine(scanner, "1")
	switch inferChoice {
	case "1", "claude-code", "claude":
		ws.inferenceMode = "claude-code"
		// Check for existing Claude auth
		home, _ := os.UserHomeDir()
		claudeDir := filepath.Join(home, ".claude")
		if _, err := os.Stat(claudeDir); err == nil {
			fmt.Printf("  Found existing Claude auth at %s\n", claudeDir)
			fmt.Printf("  Mount into container? (Y/n): ")
			if promptConfirmDefault(scanner, true) {
				ws.claudeAuthPath = claudeDir
			}
		}
	case "2", "api-key", "api":
		ws.inferenceMode = "api-key"
	case "3", "openrouter":
		ws.inferenceMode = "openrouter"
	default:
		ws.inferenceMode = "claude-code"
	}

	// ── Step 6: Messaging Channels ─────────────────────────────────────
	if ws.tier != "micro" {
		fmt.Println()
		fmt.Println("Messaging channels (space-separated, or 'none'):")
		fmt.Println("  Available: discord telegram signal whatsapp slack irc")
		fmt.Println()
		fmt.Printf("Channels [discord]: ")
		channelInput := promptLine(scanner, "discord")
		if channelInput != "none" && channelInput != "" {
			ws.channels = strings.Fields(channelInput)
		}
	}

	// ── Step 7: Sidecars ───────────────────────────────────────────────
	fmt.Println()
	fmt.Println("Optional sidecars:")

	// Vaultwarden
	vaultDefault := ws.tier == "primary"
	if vaultDefault {
		fmt.Printf("  Enable Vaultwarden (self-hosted secrets manager)? (Y/n): ")
	} else {
		fmt.Printf("  Enable Vaultwarden (self-hosted secrets manager)? (y/N): ")
	}
	ws.enableVault = promptConfirmDefault(scanner, vaultDefault)

	// TTS
	fmt.Printf("  Enable Kokoro TTS (text-to-speech)? (y/N): ")
	ws.enableTTS = promptConfirmDefault(scanner, false)

	// ── Step 8: Network ────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("Network configuration:")

	fmt.Printf("  Bind address [0.0.0.0]: ")
	ws.bindAddr = promptLine(scanner, "0.0.0.0")

	fmt.Printf("  Kernel port [5100]: ")
	ws.kernelPort = promptInt(scanner, 5100)

	if ws.tier != "micro" {
		fmt.Printf("  Gateway port [18789]: ")
		ws.gatewayPort = promptInt(scanner, 18789)
	}

	fmt.Printf("  Enable mDNS peer discovery? (Y/n): ")
	ws.mdnsEnabled = promptConfirmDefault(scanner, true)

	// ── Summary + Confirm ──────────────────────────────────────────────
	fmt.Println()
	fmt.Println("─── Configuration Summary ─────────────────────")
	fmt.Printf("  Name:        %s\n", ws.nodeName)
	fmt.Printf("  Workspace:   %s\n", ws.workspaceID)
	fmt.Printf("  Tier:        %s\n", ws.tier)
	fmt.Printf("  Sync:        %s\n", ws.syncProfile)
	fmt.Printf("  Inference:   %s\n", ws.inferenceMode)
	if len(ws.channels) > 0 {
		fmt.Printf("  Channels:    %s\n", strings.Join(ws.channels, ", "))
	}
	fmt.Printf("  Vaultwarden: %v\n", ws.enableVault)
	fmt.Printf("  TTS:         %v\n", ws.enableTTS)
	fmt.Printf("  Network:     %s (kernel:%d", ws.bindAddr, ws.kernelPort)
	if ws.tier != "micro" {
		fmt.Printf(", gateway:%d", ws.gatewayPort)
	}
	fmt.Printf(", mDNS:%v)\n", ws.mdnsEnabled)
	fmt.Println("────────────────────────────────────────────────")
	fmt.Println()
	fmt.Printf("Initialize node with these settings? (Y/n): ")
	if !promptConfirmDefault(scanner, true) {
		return ws, fmt.Errorf("initialization cancelled")
	}

	return ws, nil
}

// ─── Wizard → Config ─────────────────────────────────────────────────────────────

// wizardToConfig converts wizard selections into a NodeConfig.
func wizardToConfig(nodeID string, ws *initWizardState) *NodeConfig {
	cfg := &NodeConfig{
		NodeID:      nodeID,
		WorkspaceID: ws.workspaceID,
		Hostname:    ws.nodeName,
		Platform:    fmt.Sprintf("%s/%s", goRuntime.GOOS, goRuntime.GOARCH),
		Tier:        ws.tier,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Services: ServiceConfig{
			Kernel: ContainerSpec{
				Image:  "cogos/kernel:latest",
				Ports:  map[string]string{strconv.Itoa(ws.kernelPort): "5100"},
				Always: true,
			},
		},
		Secrets: SecretsConfig{
			Provider:   "envspec",
			SchemaFile: ".envspec",
		},
		Network: NetworkConfig{
			KernelPort:  ws.kernelPort,
			GatewayPort: ws.gatewayPort,
			MDNSEnabled: ws.mdnsEnabled,
			BindAddr:    ws.bindAddr,
		},
	}

	// Gateway (not on micro)
	if ws.tier != "micro" {
		cfg.Services.Gateway = ContainerSpec{
			Image:     "cogos/gateway:latest",
			Ports:     map[string]string{strconv.Itoa(ws.gatewayPort): "18789"},
			DependsOn: []string{"kernel"},
			Always:    true,
		}
	}

	// Sync profile
	profiles := SyncProfilePresets()
	if profile, ok := profiles[ws.syncProfile]; ok {
		cfg.Sync = profile
	} else {
		cfg.Sync = profiles["full"]
	}

	// Claude auth volume mount
	if ws.claudeAuthPath != "" {
		cfg.Services.Kernel.Volumes = append(cfg.Services.Kernel.Volumes,
			ws.claudeAuthPath+":/home/cog/.claude:ro")
		if ws.tier != "micro" {
			cfg.Services.Gateway.Volumes = append(cfg.Services.Gateway.Volumes,
				ws.claudeAuthPath+":/home/cog/.claude:ro")
		}
	}

	// Vaultwarden sidecar
	if ws.enableVault {
		cfg.Sidecars.Vaultwarden = &ContainerSpec{
			Image:  "vaultwarden/server:latest",
			Ports:  map[string]string{"8222": "80"},
			Always: true,
		}
		cfg.Secrets.VaultURL = "http://localhost:8222"
	}

	// TTS sidecar
	if ws.enableTTS {
		cfg.Sidecars.TTS = &ContainerSpec{
			Image:  "cogos/kokoro-tts:latest",
			Ports:  map[string]string{"8880": "8880"},
			Always: true,
		}
	}

	return cfg
}

// ─── Envspec Generation ──────────────────────────────────────────────────────────

// writeWizardEnvspec creates a .envspec file based on wizard selections.
func writeWizardEnvspec(path string, ws *initWizardState) error {
	var buf bytes.Buffer
	buf.WriteString("# CogOS Node Secrets — @env-spec format (Varlock-compatible)\n")
	buf.WriteString("# Secrets are resolved at runtime; this file contains NO plaintext secrets.\n")
	buf.WriteString("#\n")
	buf.WriteString("# Providers:\n")
	buf.WriteString("#   bitwarden-cli(id=\"uuid\")   — bw CLI (personal vault)\n")
	buf.WriteString("#   bitwarden(id=\"uuid\")       — Vaultwarden Secrets Manager API\n")
	buf.WriteString("#   file(path=\"/path\")          — Docker secrets / tmpfs file\n")
	buf.WriteString("#   env(name=\"VAR\")             — Existing environment variable\n")
	buf.WriteString("#   literal                      — Plaintext value (non-secret config only)\n")
	buf.WriteString("\n")

	// Inference section
	switch ws.inferenceMode {
	case "claude-code":
		buf.WriteString("# ─── Inference (Claude Code / Claude Max) ────────────────────────────────────\n")
		buf.WriteString("# Claude Max uses OAuth — no API key needed.\n")
		buf.WriteString("# Claude Code authenticates via: claude login\n")
		if ws.claudeAuthPath != "" {
			buf.WriteString(fmt.Sprintf("# Auth mounted from: %s\n", ws.claudeAuthPath))
		}
		buf.WriteString("\n")
	case "api-key":
		buf.WriteString("# ─── Inference (Anthropic API Key) ───────────────────────────────────────────\n")
		buf.WriteString("\n")
		buf.WriteString("# @env-spec bitwarden-cli(id=\"REPLACE_WITH_UUID\")\n")
		buf.WriteString("ANTHROPIC_API_KEY=\n")
		buf.WriteString("\n")
	case "openrouter":
		buf.WriteString("# ─── Inference (OpenRouter) ──────────────────────────────────────────────────\n")
		buf.WriteString("\n")
		buf.WriteString("# @env-spec bitwarden-cli(id=\"REPLACE_WITH_UUID\")\n")
		buf.WriteString("OPENROUTER_API_KEY=\n")
		buf.WriteString("\n")
	}

	// Messaging channels
	if len(ws.channels) > 0 {
		buf.WriteString("# ─── Messaging ───────────────────────────────────────────────────────────────\n")
		buf.WriteString("\n")
		for _, ch := range ws.channels {
			envVar := channelToEnvVar(ch)
			buf.WriteString(fmt.Sprintf("# @env-spec bitwarden-cli(id=\"REPLACE_WITH_%s_UUID\")\n", strings.ToUpper(ch)))
			buf.WriteString(fmt.Sprintf("%s=\n", envVar))
			buf.WriteString("\n")
		}
	}

	// Node config (non-secret)
	buf.WriteString("# ─── Node Config (non-secret) ────────────────────────────────────────────────\n")
	buf.WriteString("\n")
	buf.WriteString("# @env-spec literal\n")
	buf.WriteString(fmt.Sprintf("COG_NODE_TIER=%s\n", ws.tier))
	buf.WriteString("\n")
	buf.WriteString("# @env-spec literal\n")
	buf.WriteString(fmt.Sprintf("COG_NODE_NAME=%s\n", ws.nodeName))
	buf.WriteString("\n")

	// Vaultwarden
	if ws.enableVault {
		buf.WriteString("# ─── Vaultwarden ─────────────────────────────────────────────────────────────\n")
		buf.WriteString("\n")
		buf.WriteString("# @env-spec literal\n")
		buf.WriteString("VAULTWARDEN_URL=http://localhost:8222\n")
		buf.WriteString("\n")
	}

	return os.WriteFile(path, buf.Bytes(), 0644)
}

// channelToEnvVar maps a channel name to its environment variable.
func channelToEnvVar(channel string) string {
	switch channel {
	case "discord":
		return "DISCORD_TOKEN"
	case "telegram":
		return "TELEGRAM_TOKEN"
	case "signal":
		return "SIGNAL_CLI_PATH"
	case "whatsapp":
		return "WHATSAPP_TOKEN"
	case "slack":
		return "SLACK_TOKEN"
	case "irc":
		return "IRC_PASSWORD"
	default:
		return strings.ToUpper(channel) + "_TOKEN"
	}
}

// ─── Prompt Helpers ──────────────────────────────────────────────────────────────

// promptLine reads a line from the scanner, returning defaultVal if empty.
func promptLine(scanner *bufio.Scanner, defaultVal string) string {
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			return line
		}
	}
	return defaultVal
}

// promptConfirm reads y/N from stdin (default: no).
func promptConfirm() bool {
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		line := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return line == "y" || line == "yes"
	}
	return false
}

// promptConfirmDefault reads y/n with a configurable default.
func promptConfirmDefault(scanner *bufio.Scanner, defaultYes bool) bool {
	if scanner.Scan() {
		line := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if line == "" {
			return defaultYes
		}
		return line == "y" || line == "yes"
	}
	return defaultYes
}

// promptInt reads an integer, returning defaultVal if empty or invalid.
func promptInt(scanner *bufio.Scanner, defaultVal int) int {
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			if v, err := strconv.Atoi(line); err == nil {
				return v
			}
		}
	}
	return defaultVal
}

// hostname returns the system hostname.
func hostname() string {
	h, _ := os.Hostname()
	return h
}

// runtimeOS returns the OS name using the runtime package.
func runtimeOS() string {
	return goRuntime.GOOS
}

// runtimeArch returns the architecture using the runtime package.
func runtimeArch() string {
	return goRuntime.GOARCH
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// tierToSyncProfile returns the default sync profile for a tier.
func tierToSyncProfile(tier string) string {
	switch tier {
	case "primary":
		return "full"
	case "standard":
		return "build"
	case "micro":
		return "edge"
	default:
		return "full"
	}
}

// tierToSyncChoice returns the default sync choice number for a tier.
func tierToSyncChoice(tier string) string {
	switch tier {
	case "primary":
		return "1"
	case "standard":
		return "2"
	case "micro":
		return "4"
	default:
		return "1"
	}
}

// ─── start ──────────────────────────────────────────────────────────────────────

func cmdNodeStart(args []string) error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found: %w", err)
	}

	cfg, err := LoadNodeConfig(root)
	if err != nil {
		return fmt.Errorf("node not initialized (run: cog node init)\n%w", err)
	}

	fmt.Printf("Starting CogOS node %s (tier: %s)...\n", cfg.NodeID, cfg.Tier)

	// Step 1: Resolve secrets via envspec
	envspecPath := filepath.Join(root, cfg.Secrets.SchemaFile)
	var resolvedEnv *envspec.Env
	if _, err := os.Stat(envspecPath); err == nil {
		fmt.Println("  Resolving secrets...")
		resolvedEnv, err = resolveNodeSecrets(root, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: secret resolution failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "  Continuing without resolved secrets\n")
		} else {
			fmt.Printf("  Resolved %d secret(s)\n", len(resolvedEnv.Vars))
		}
	}

	// Step 2: Check container runtime
	docker := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := docker.Ping(ctx); err != nil {
		return fmt.Errorf("container runtime not available: %w\nInstall Docker, Podman, or Colima first", err)
	}
	fmt.Printf("  Container runtime: %s\n", docker.SocketPath())

	// Step 3: Generate docker-compose.yml
	composePath := filepath.Join(root, ".cog", "run", "docker-compose.yml")
	if err := generateCompose(root, cfg, resolvedEnv); err != nil {
		return fmt.Errorf("generate compose: %w", err)
	}
	fmt.Printf("  Generated: %s\n", composePath)

	// Step 4: Start services via docker compose
	fmt.Println("  Starting services...")
	if err := dockerComposeUp(ctx, composePath); err != nil {
		return fmt.Errorf("start services: %w", err)
	}

	// Write PID/state file
	stateFile := filepath.Join(root, ".cog", "run", "node.state.json")
	state := map[string]interface{}{
		"node_id":    cfg.NodeID,
		"tier":       cfg.Tier,
		"started_at": time.Now().UTC().Format(time.RFC3339),
		"compose":    composePath,
		"status":     "running",
	}
	stateData, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(stateFile, stateData, 0644)

	fmt.Println()
	fmt.Printf("Node %s is running!\n", cfg.NodeID)
	fmt.Printf("  Kernel:  http://localhost:%d\n", cfg.Network.KernelPort)
	if cfg.Services.Gateway.Always {
		fmt.Printf("  Gateway: http://localhost:%d\n", cfg.Network.GatewayPort)
	}
	if cfg.Sidecars.Vaultwarden != nil {
		fmt.Printf("  Vault:   http://localhost:8222\n")
	}

	return nil
}

// resolveNodeSecrets loads and resolves the .envspec file using the appropriate
// resolver chain for this node's tier.
func resolveNodeSecrets(root string, cfg *NodeConfig) (*envspec.Env, error) {
	envspecPath := filepath.Join(root, cfg.Secrets.SchemaFile)
	schema, err := envspec.ParseFile(envspecPath)
	if err != nil {
		return nil, fmt.Errorf("parse .envspec: %w", err)
	}

	// Build resolver chain based on tier
	var resolvers []envspec.Resolver

	// Tier 1: Local vault file (offline-first)
	vaultPath := filepath.Join(root, ".cog", "secrets", "vault.json")
	if data, err := os.ReadFile(vaultPath); err == nil {
		if vfr, err := envspec.NewVaultFileResolver(data); err == nil {
			resolvers = append(resolvers, vfr)
		}
	}

	// Tier 2: Vaultwarden API (if configured)
	if cfg.Secrets.VaultURL != "" {
		token := os.Getenv("BW_MACHINE_TOKEN")
		if token != "" {
			resolvers = append(resolvers, envspec.NewBitwardenResolver(cfg.Secrets.VaultURL, token))
		}
	}

	// Tier 3: Bitwarden CLI fallback
	resolvers = append(resolvers, envspec.NewBitwardenCLIResolver())

	// Tier 4: Environment variable passthrough
	resolvers = append(resolvers, envspec.NewEnvResolver())

	// Tier 5: File resolver (Docker secrets)
	resolvers = append(resolvers, envspec.NewFileResolver())

	// Tier 6: OS keychain cache (last resort)
	resolvers = append(resolvers, envspec.NewKeychainResolver("cogos-secrets"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return envspec.Resolve(ctx, schema, resolvers...)
}

// generateCompose creates a docker-compose.yml for this node's services.
func generateCompose(root string, cfg *NodeConfig, resolvedEnv *envspec.Env) error {
	runDir := filepath.Join(root, ".cog", "run")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return err
	}

	compose := map[string]interface{}{
		// version key omitted — deprecated in modern compose
	}

	services := make(map[string]interface{})

	// Environment variables for containers
	envVars := make(map[string]string)
	envVars["COG_NODE_ID"] = cfg.NodeID
	envVars["COG_NODE_TIER"] = cfg.Tier
	envVars["COG_WORKSPACE"] = cfg.WorkspaceID
	if resolvedEnv != nil {
		for k, v := range resolvedEnv.Vars {
			envVars[k] = v
		}
	}

	// Convert env map to list
	envList := make([]string, 0, len(envVars))
	for k, v := range envVars {
		envList = append(envList, k+"="+v)
	}

	// Kernel service
	if cfg.Services.Kernel.Always {
		kernelService := map[string]interface{}{
			"image":       cfg.Services.Kernel.Image,
			"container_name": "cogos-kernel",
			"ports":       portsToList(cfg.Services.Kernel.Ports),
			"volumes": []string{
				root + "/.cog:/workspace/.cog",
				root + "/CLAUDE.md:/workspace/CLAUDE.md:ro",
				root + "/SOUL.md:/workspace/SOUL.md:ro",
			},
			"environment":  envList,
			"restart":      "unless-stopped",
			"labels": map[string]string{
				"com.cogos.managed": "true",
				"com.cogos.service": "kernel",
				"com.cogos.node":    cfg.NodeID,
			},
		}
		services["kernel"] = kernelService
	}

	// Gateway service
	if cfg.Services.Gateway.Always {
		gatewayService := map[string]interface{}{
			"image":       cfg.Services.Gateway.Image,
			"container_name": "cogos-gateway",
			"ports":       portsToList(cfg.Services.Gateway.Ports),
			"volumes": []string{
				root + "/.cog:/workspace/.cog",
			},
			"environment":  envList,
			"depends_on":   []string{"kernel"},
			"restart":      "unless-stopped",
			"labels": map[string]string{
				"com.cogos.managed": "true",
				"com.cogos.service": "gateway",
				"com.cogos.node":    cfg.NodeID,
			},
		}
		services["gateway"] = gatewayService
	}

	// Vaultwarden sidecar (primary nodes only)
	if cfg.Sidecars.Vaultwarden != nil {
		vwService := map[string]interface{}{
			"image":       cfg.Sidecars.Vaultwarden.Image,
			"container_name": "cogos-vaultwarden",
			"ports":       portsToList(cfg.Sidecars.Vaultwarden.Ports),
			"volumes": []string{
				"vw-data:/data",
			},
			"restart": "unless-stopped",
			"labels": map[string]string{
				"com.cogos.managed": "true",
				"com.cogos.service": "vaultwarden",
				"com.cogos.node":    cfg.NodeID,
			},
		}
		services["vaultwarden"] = vwService
	}

	compose["services"] = services

	// Named volumes
	volumes := map[string]interface{}{
		"cogos-data": map[string]interface{}{},
	}
	if cfg.Sidecars.Vaultwarden != nil {
		volumes["vw-data"] = map[string]interface{}{}
	}
	compose["volumes"] = volumes

	// Write compose file as YAML
	data, err := yaml.Marshal(compose)
	if err != nil {
		return fmt.Errorf("marshal compose: %w", err)
	}

	composePath := filepath.Join(runDir, "docker-compose.yml")
	return os.WriteFile(composePath, data, 0644)
}

// portsToList converts {"host": "container"} to ["host:container"].
func portsToList(ports map[string]string) []string {
	var list []string
	for host, container := range ports {
		list = append(list, host+":"+container)
	}
	return list
}

// dockerComposeUp runs docker compose up -d.
func dockerComposeUp(ctx context.Context, composePath string) error {
	// Try docker compose (v2) first, fall back to docker-compose (v1)
	args := []string{"compose", "-f", composePath, "up", "-d"}

	cmd := exec.CommandContext(ctx,"docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		// Fallback to docker-compose v1
		args = []string{"-f", composePath, "up", "-d"}
		cmd = exec.CommandContext(ctx,"docker-compose", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

// dockerComposeDown runs docker compose down.
func dockerComposeDown(ctx context.Context, composePath string) error {
	args := []string{"compose", "-f", composePath, "down"}
	cmd := exec.CommandContext(ctx,"docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		args = []string{"-f", composePath, "down"}
		cmd = exec.CommandContext(ctx,"docker-compose", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

// ─── stop ───────────────────────────────────────────────────────────────────────

func cmdNodeStop(args []string) error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found: %w", err)
	}

	cfg, err := LoadNodeConfig(root)
	if err != nil {
		return fmt.Errorf("node not initialized: %w", err)
	}

	composePath := filepath.Join(root, ".cog", "run", "docker-compose.yml")
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		return fmt.Errorf("no compose file found — node may not be running")
	}

	fmt.Printf("Stopping CogOS node %s...\n", cfg.NodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := dockerComposeDown(ctx, composePath); err != nil {
		return fmt.Errorf("stop services: %w", err)
	}

	// Update state file
	stateFile := filepath.Join(root, ".cog", "run", "node.state.json")
	state := map[string]interface{}{
		"node_id":    cfg.NodeID,
		"tier":       cfg.Tier,
		"stopped_at": time.Now().UTC().Format(time.RFC3339),
		"status":     "stopped",
	}
	stateData, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(stateFile, stateData, 0644)

	fmt.Println("Node stopped.")
	return nil
}

// ─── status ─────────────────────────────────────────────────────────────────────

func cmdNodeStatus(args []string) error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("no workspace found: %w", err)
	}

	cfg, err := LoadNodeConfig(root)
	if err != nil {
		return fmt.Errorf("node not initialized (run: cog node init)")
	}

	fmt.Println("CogOS Node Status")
	fmt.Println("==================")
	fmt.Printf("  Node ID:     %s\n", cfg.NodeID)
	fmt.Printf("  Workspace:   %s\n", cfg.WorkspaceID)
	fmt.Printf("  Hostname:    %s\n", cfg.Hostname)
	fmt.Printf("  Platform:    %s\n", cfg.Platform)
	fmt.Printf("  Tier:        %s\n", cfg.Tier)
	fmt.Printf("  Sync:        %s\n", cfg.Sync.Profile)
	fmt.Printf("  Discovery:   %s\n", cfg.Sync.Discovery)

	// Check runtime state
	stateFile := filepath.Join(root, ".cog", "run", "node.state.json")
	if data, err := os.ReadFile(stateFile); err == nil {
		var state map[string]interface{}
		if json.Unmarshal(data, &state) == nil {
			if status, ok := state["status"].(string); ok {
				fmt.Printf("  Status:      %s\n", status)
			}
			if started, ok := state["started_at"].(string); ok && state["status"] == "running" {
				if t, err := time.Parse(time.RFC3339, started); err == nil {
					fmt.Printf("  Uptime:      %s\n", time.Since(t).Round(time.Second))
				}
			}
		}
	} else {
		fmt.Printf("  Status:      not started\n")
	}

	// Check container runtime
	docker := NewDockerClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := docker.Ping(ctx); err != nil {
		fmt.Printf("  Runtime:     not available\n")
	} else {
		fmt.Printf("  Runtime:     %s\n", docker.SocketPath())

		// Check managed containers
		containers, err := listManagedContainers(ctx, docker, cfg.NodeID)
		if err == nil && len(containers) > 0 {
			fmt.Println()
			fmt.Println("  Containers:")
			for _, c := range containers {
				fmt.Printf("    %-20s %s (%s)\n", c.Service, c.Status, c.ID[:12])
			}
		}
	}

	// Check envspec
	envspecPath := filepath.Join(root, cfg.Secrets.SchemaFile)
	if _, err := os.Stat(envspecPath); err == nil {
		fmt.Printf("  Secrets:     %s (configured)\n", cfg.Secrets.SchemaFile)
	} else {
		fmt.Printf("  Secrets:     not configured\n")
	}

	// Show network
	fmt.Println()
	fmt.Println("  Network:")
	fmt.Printf("    Kernel:    %s:%d\n", cfg.Network.BindAddr, cfg.Network.KernelPort)
	if cfg.Services.Gateway.Always {
		fmt.Printf("    Gateway:   %s:%d\n", cfg.Network.BindAddr, cfg.Network.GatewayPort)
	}
	if cfg.Sidecars.Vaultwarden != nil {
		fmt.Printf("    Vault:     %s:8222\n", cfg.Network.BindAddr)
	}
	fmt.Printf("    mDNS:      %v\n", cfg.Network.MDNSEnabled)

	return nil
}

// managedContainer represents a CogOS-managed container's status.
type managedContainer struct {
	ID      string
	Service string
	Status  string
}

// listManagedContainers lists all CogOS-managed containers via Docker API.
func listManagedContainers(ctx context.Context, docker *DockerClient, nodeID string) ([]managedContainer, error) {
	// Use Docker API to list containers with our labels
	resp, err := docker.doRequest(ctx, "GET",
		fmt.Sprintf("/containers/json?all=true&filters={\"label\":[\"com.cogos.managed=true\",\"com.cogos.node=%s\"]}", nodeID),
		nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw []struct {
		ID     string            `json:"Id"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	var result []managedContainer
	for _, c := range raw {
		result = append(result, managedContainer{
			ID:      c.ID,
			Service: c.Labels["com.cogos.service"],
			Status:  c.State,
		})
	}
	return result, nil
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdNodeHelp() error {
	fmt.Println("Usage: cog node <command>")
	fmt.Println()
	fmt.Println("Node Management (ADR-063):")
	fmt.Println("  init      Initialize this machine as a CogOS node")
	fmt.Println("  start     Start node services (kernel, gateway, sidecars)")
	fmt.Println("  stop      Stop node services")
	fmt.Println("  status    Show node status, containers, and sync state")
	fmt.Println()
	fmt.Println("Node Identity:")
	fmt.Println("  info      Show node identity, capabilities, and cluster status")
	fmt.Println("  shells    List registered shells with status and capabilities")
	fmt.Println()
	fmt.Println("Init Options:")
	fmt.Println("  --tier, -t <tier>          Node tier: primary, standard, micro")
	fmt.Println("  --workspace, -w <id>       Workspace ID (default: directory name)")
	fmt.Println("  --non-interactive, --ni    Skip interactive prompts (use defaults)")
	fmt.Println("  --yes, -y                  Non-interactive + auto-confirm")
	fmt.Println()
	fmt.Println("Interactive init walks you through:")
	fmt.Println("  - Node tier (primary/standard/micro)")
	fmt.Println("  - Sync profile (full/build/mobile/edge)")
	fmt.Println("  - Inference provider (Claude Code/API key/OpenRouter)")
	fmt.Println("  - Messaging channels (Discord, Telegram, etc.)")
	fmt.Println("  - Optional sidecars (Vaultwarden, TTS)")
	fmt.Println("  - Network settings (ports, mDNS)")
	fmt.Println()
	fmt.Println("Tiers:")
	fmt.Println("  primary    Full stack: kernel + gateway + vaultwarden (always-on machine)")
	fmt.Println("  standard   Kernel + gateway (secondary node, syncs from primary)")
	fmt.Println("  micro      Kernel only (minimal: Pi, edge device, VPS)")
	fmt.Println()
	fmt.Println("Node config:    .cog/config/node/node.json")
	fmt.Println("Secrets schema: .envspec")
	return nil
}
