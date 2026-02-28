// cmd_cluster.go — CLI commands for BEP cluster management.
// Commands: init, device-id, status, peers, trust, untrust.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// cmdCluster dispatches cluster subcommands.
func cmdCluster(args []string) error {
	if len(args) == 0 {
		return cmdClusterHelp()
	}

	switch args[0] {
	case "init":
		return cmdClusterInit()
	case "activate":
		return cmdClusterActivate()
	case "device-id":
		return cmdClusterDeviceID()
	case "status":
		return cmdClusterStatus()
	case "peers":
		return cmdClusterPeers()
	case "trust":
		return cmdClusterTrust(args[1:])
	case "untrust":
		return cmdClusterUntrust(args[1:])
	case "help", "-h", "--help":
		return cmdClusterHelp()
	default:
		return fmt.Errorf("unknown cluster command: %s", args[0])
	}
}

// ─── init ───────────────────────────────────────────────────────────────────────

func cmdClusterInit() error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	certDir := CertDir()

	// Generate TLS certificate.
	if err := GenerateBEPCert(certDir); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			fmt.Printf("Certificate already exists at %s\n", certDir)
		} else {
			return fmt.Errorf("generate cert: %w", err)
		}
	} else {
		fmt.Printf("Generated BEP certificate in %s\n", certDir)
	}

	// Load cert and derive DeviceID.
	cert, err := LoadBEPCert(certDir)
	if err != nil {
		return fmt.Errorf("load cert: %w", err)
	}
	deviceID, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		return fmt.Errorf("derive device ID: %w", err)
	}

	// Update cluster.yaml.
	cfgPath := filepath.Join(root, ".cog", "config", "cluster.yaml")
	cfg := &ClusterConfig{}

	data, err := os.ReadFile(cfgPath)
	if err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	cfg.Enabled = true
	cfg.DeviceID = FormatDeviceID(deviceID)
	if nodeIdent, err := LoadNodeIdentity(); err == nil {
		cfg.NodeName = nodeIdent.Node.ID
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 22000
	}
	if cfg.CertDir == "" {
		cfg.CertDir = "~/.cog/etc"
	}
	if cfg.Discovery == "" {
		cfg.Discovery = "static"
	}
	if len(cfg.SyncDirs) == 0 {
		cfg.SyncDirs = []string{".cog/bin/agents/definitions/"}
	}

	if err := saveClusterConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Printf("Cluster initialized.\n")
	fmt.Printf("  DeviceID: %s\n", FormatDeviceID(deviceID))
	fmt.Printf("  Listen:   :%d\n", cfg.ListenPort)
	fmt.Printf("  Config:   %s\n", cfgPath)
	return nil
}

// ─── activate ───────────────────────────────────────────────────────────────────

func cmdClusterActivate() error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	certDir := CertDir()

	// Verify cert exists.
	if _, err := LoadBEPCert(certDir); err != nil {
		return fmt.Errorf("no certificate found — run 'cog cluster init' first")
	}

	// Load cert and derive DeviceID.
	cert, err := LoadBEPCert(certDir)
	if err != nil {
		return fmt.Errorf("load cert: %w", err)
	}
	deviceID, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		return fmt.Errorf("derive device ID: %w", err)
	}

	// Load or create cluster config.
	cfgPath := filepath.Join(root, ".cog", "config", "cluster.yaml")
	cfg := &ClusterConfig{}
	data, err := os.ReadFile(cfgPath)
	if err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	// Read node identity for NodeName.
	if nodeIdent, err := LoadNodeIdentity(); err == nil {
		cfg.NodeName = nodeIdent.Node.ID
	}

	cfg.Enabled = true
	cfg.DeviceID = FormatDeviceID(deviceID)
	if cfg.ListenPort == 0 {
		cfg.ListenPort = 22000
	}
	if cfg.CertDir == "" {
		cfg.CertDir = "~/.cog/etc"
	}
	if cfg.Discovery == "" {
		cfg.Discovery = "static"
	}

	if err := saveClusterConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Println("Cluster activated.")
	fmt.Printf("  Node:     %s\n", cfg.NodeName)
	fmt.Printf("  DeviceID: %s\n", cfg.DeviceID)
	fmt.Printf("  Listen:   :%d\n", cfg.ListenPort)
	fmt.Printf("  Peers:    %d configured\n", len(cfg.Peers))
	return nil
}

// ─── device-id ──────────────────────────────────────────────────────────────────

func cmdClusterDeviceID() error {
	certDir := CertDir()
	cert, err := LoadBEPCert(certDir)
	if err != nil {
		return fmt.Errorf("load cert (run 'cog cluster init' first): %w", err)
	}
	deviceID, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		return fmt.Errorf("derive device ID: %w", err)
	}
	fmt.Println(FormatDeviceID(deviceID))
	return nil
}

// ─── status ─────────────────────────────────────────────────────────────────────

func cmdClusterStatus() error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	provider := NewBEPProvider(root)
	cfg, err := provider.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if !cfg.Enabled {
		fmt.Println("Cluster: disabled")
		fmt.Println("Run 'cog cluster init' to enable.")
		return nil
	}

	fmt.Println("Cluster: enabled")
	fmt.Printf("  DeviceID:  %s\n", cfg.DeviceID)
	fmt.Printf("  Listen:    :%d\n", cfg.ListenPort)
	fmt.Printf("  Discovery: %s\n", cfg.Discovery)
	fmt.Printf("  Peers:     %d configured\n", len(cfg.Peers))

	if len(cfg.SyncDirs) > 0 {
		fmt.Printf("  Sync dirs: %s\n", strings.Join(cfg.SyncDirs, ", "))
	}

	return nil
}

// ─── peers ──────────────────────────────────────────────────────────────────────

func cmdClusterPeers() error {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	provider := NewBEPProvider(root)
	cfg, err := provider.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if len(cfg.Peers) == 0 {
		fmt.Println("No peers configured.")
		fmt.Println("Add one with: cog cluster trust <device-id> --address <host:port> --name <name>")
		return nil
	}

	for _, p := range cfg.Peers {
		trusted := "untrusted"
		if p.Trusted {
			trusted = "trusted"
		}
		idShort := p.DeviceID
		if len(idShort) > 7 {
			idShort = idShort[:7] + "..."
		}
		fmt.Printf("  %s  %s  %s  [%s]\n", idShort, p.Name, p.Address, trusted)
	}
	return nil
}

// ─── trust ──────────────────────────────────────────────────────────────────────

func cmdClusterTrust(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog cluster trust <device-id> --address <host:port> --name <name>")
	}

	deviceID := args[0]
	address := ""
	name := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--address":
			if i+1 < len(args) {
				address = args[i+1]
				i++
			}
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		}
	}

	// Validate device ID.
	if _, err := ParseDeviceID(deviceID); err != nil {
		return fmt.Errorf("invalid device ID: %w", err)
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	cfgPath := filepath.Join(root, ".cog", "config", "cluster.yaml")
	cfg := &ClusterConfig{}
	data, err := os.ReadFile(cfgPath)
	if err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	// Check for duplicate.
	for i, p := range cfg.Peers {
		if p.DeviceID == deviceID {
			cfg.Peers[i].Trusted = true
			if address != "" {
				cfg.Peers[i].Address = address
			}
			if name != "" {
				cfg.Peers[i].Name = name
			}
			return saveClusterConfig(cfgPath, cfg)
		}
	}

	// Add new peer.
	cfg.Peers = append(cfg.Peers, ClusterPeer{
		DeviceID: deviceID,
		Address:  address,
		Name:     name,
		Trusted:  true,
	})

	if err := saveClusterConfig(cfgPath, cfg); err != nil {
		return err
	}

	idShort := deviceID
	if len(idShort) > 7 {
		idShort = idShort[:7]
	}
	fmt.Printf("Trusted peer %s", idShort)
	if name != "" {
		fmt.Printf(" (%s)", name)
	}
	fmt.Println()
	return nil
}

// ─── untrust ────────────────────────────────────────────────────────────────────

func cmdClusterUntrust(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog cluster untrust <device-id>")
	}

	deviceID := args[0]

	root, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	cfgPath := filepath.Join(root, ".cog", "config", "cluster.yaml")
	cfg := &ClusterConfig{}
	data, err := os.ReadFile(cfgPath)
	if err == nil {
		_ = yaml.Unmarshal(data, cfg)
	}

	found := false
	filtered := cfg.Peers[:0]
	for _, p := range cfg.Peers {
		if p.DeviceID == deviceID || strings.HasPrefix(p.DeviceID, deviceID) {
			found = true
			continue
		}
		filtered = append(filtered, p)
	}
	cfg.Peers = filtered

	if !found {
		return fmt.Errorf("peer %s not found", deviceID)
	}

	if err := saveClusterConfig(cfgPath, cfg); err != nil {
		return err
	}

	idShort := deviceID
	if len(idShort) > 7 {
		idShort = idShort[:7]
	}
	fmt.Printf("Removed peer %s\n", idShort)
	return nil
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdClusterHelp() error {
	fmt.Println("Usage: cog cluster <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  init        Generate TLS cert, derive DeviceID, enable cluster")
	fmt.Println("  activate    Enable cluster with node identity")
	fmt.Println("  device-id   Print this node's DeviceID")
	fmt.Println("  status      Show cluster state and sync status")
	fmt.Println("  peers       List configured peers with connection status")
	fmt.Println("  trust       Add/update a trusted peer")
	fmt.Println("  untrust     Remove a peer from trusted list")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  cog cluster init")
	fmt.Println("  cog cluster trust MFZWI3D-... --address 10.0.0.100:22000 --name server")
	return nil
}

// ─── Config types for cluster.yaml ──────────────────────────────────────────────

// ClusterConfig is the full cluster.yaml structure with BEP transport fields.
type ClusterConfig struct {
	Enabled    bool          `yaml:"enabled"`
	DeviceID   string        `yaml:"deviceId,omitempty"`
	NodeName   string        `yaml:"nodeName,omitempty"`
	ListenPort int           `yaml:"listenPort,omitempty"`
	CertDir    string        `yaml:"certDir,omitempty"`
	Discovery  string        `yaml:"discovery,omitempty"`
	Peers      []ClusterPeer `yaml:"peers"`
	SyncDirs   []string      `yaml:"syncDirs,omitempty"`
}

// ClusterPeer is a peer entry in cluster.yaml.
type ClusterPeer struct {
	DeviceID string `yaml:"deviceId"`
	Address  string `yaml:"address"`
	Name     string `yaml:"name"`
	Trusted  bool   `yaml:"trusted"`
}

// saveClusterConfig writes the cluster config to disk with header comment.
func saveClusterConfig(path string, cfg *ClusterConfig) error {
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	header := "# CogOS Cluster Configuration\n# Enable cross-node agent distribution via BEP\n# Reference: cog://mem/semantic/architecture/bep-agent-sync-spec\n"
	return os.WriteFile(path, []byte(header+string(out)), 0644)
}
