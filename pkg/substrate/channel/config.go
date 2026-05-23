// Package channel holds the substrate-canonical schema and loader for the
// channel bridge configuration (mapping channel names to Discord channel IDs
// and agent targets).
//
// This file holds pure schema + a YAML loader. No daemon, no routing, no
// runtime state — per ADR-100 Step 3's diagnostic rule (RFC-034 §3.3):
// "if the behavior can be tested without a running process, it belongs in
// the substrate library."
//
// Routing logic (Discord inlet, agent dispatch) stays in the kernel
// (root package main).
package channel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// BridgeConfig holds the mapping of channel names to their Discord targets.
//
// Config file: .cog/config/channels.yaml
// Env var override: CHANNEL_{UPPER_NAME}_DISCORD_ID
// (e.g. CHANNEL_COLONY_CHAT_DISCORD_ID)
type BridgeConfig struct {
	Channels map[string]BridgeEntry `yaml:"channels"`
}

// BridgeEntry describes a single named channel for the bridge.
type BridgeEntry struct {
	DiscordID string `yaml:"discordId"` // Discord channel ID
	AgentID   string `yaml:"agentId"`   // which agent responds (e.g. "whirl")
}

// ─── Loader ─────────────────────────────────────────────────────────────────────

// LoadBridgeConfig reads .cog/config/channels.yaml from the workspace root.
// Env var overrides are applied per-channel: CHANNEL_{UPPER}_DISCORD_ID.
func LoadBridgeConfig(root string) (*BridgeConfig, error) {
	cfgPath := filepath.Join(root, ".cog", "config", "channels.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read channel config: %w", err)
	}

	var cfg BridgeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse channel config: %w", err)
	}

	if cfg.Channels == nil {
		cfg.Channels = make(map[string]BridgeEntry)
	}

	// Apply env var overrides
	for name, entry := range cfg.Channels {
		envKey := "CHANNEL_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_DISCORD_ID"
		if override := os.Getenv(envKey); override != "" {
			entry.DiscordID = override
			cfg.Channels[name] = entry
		}
	}

	return &cfg, nil
}

// Lookup returns the BridgeEntry for the given name, or an error if not found.
func (c *BridgeConfig) Lookup(name string) (BridgeEntry, error) {
	entry, ok := c.Channels[name]
	if !ok {
		return BridgeEntry{}, fmt.Errorf("unknown channel %q — run 'cog channel list' to see available channels", name)
	}
	if entry.DiscordID == "" {
		return BridgeEntry{}, fmt.Errorf("channel %q has no discordId configured", name)
	}
	return entry, nil
}
