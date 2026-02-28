// channel_config.go — Maps channel names to Discord channel IDs and agent targets.
//
// Config file: .cog/config/channels.yaml
// Env var override: CHANNEL_{UPPER_NAME}_DISCORD_ID (e.g. CHANNEL_COLONY_CHAT_DISCORD_ID)

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// ChannelBridgeConfig holds the mapping of channel names to their Discord targets.
type ChannelBridgeConfig struct {
	Channels map[string]ChannelBridgeEntry `yaml:"channels"`
}

// ChannelBridgeEntry describes a single named channel for the bridge.
type ChannelBridgeEntry struct {
	DiscordID string `yaml:"discordId"` // Discord channel ID
	AgentID   string `yaml:"agentId"`   // which agent responds (e.g. "whirl")
}

// ─── Loader ─────────────────────────────────────────────────────────────────────

// LoadChannelBridgeConfig reads .cog/config/channels.yaml from the workspace root.
// Env var overrides are applied per-channel: CHANNEL_{UPPER}_DISCORD_ID.
func LoadChannelBridgeConfig(root string) (*ChannelBridgeConfig, error) {
	cfgPath := filepath.Join(root, ".cog", "config", "channels.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read channel config: %w", err)
	}

	var cfg ChannelBridgeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse channel config: %w", err)
	}

	if cfg.Channels == nil {
		cfg.Channels = make(map[string]ChannelBridgeEntry)
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

// Lookup returns the ChannelBridgeEntry for the given name, or an error if not found.
func (c *ChannelBridgeConfig) Lookup(name string) (ChannelBridgeEntry, error) {
	entry, ok := c.Channels[name]
	if !ok {
		return ChannelBridgeEntry{}, fmt.Errorf("unknown channel %q — run 'cog channel list' to see available channels", name)
	}
	if entry.DiscordID == "" {
		return ChannelBridgeEntry{}, fmt.Errorf("channel %q has no discordId configured", name)
	}
	return entry, nil
}
