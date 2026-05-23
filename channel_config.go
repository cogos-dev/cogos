// channel_config.go — Thin re-export shim. Canonical schema lives in
// pkg/substrate/channel per ADR-100 Step 3.
//
// Type aliases below let existing kernel call sites compile unchanged.
// New code should prefer the pkg/substrate/channel import path.

package main

import (
	"github.com/myrgic/cogos/pkg/substrate/channel"
)

// ─── Type aliases ───────────────────────────────────────────────────────────────

// ChannelBridgeConfig holds the mapping of channel names to their Discord targets.
// Canonical home: pkg/substrate/channel.BridgeConfig.
type ChannelBridgeConfig = channel.BridgeConfig

// ChannelBridgeEntry describes a single named channel for the bridge.
// Canonical home: pkg/substrate/channel.BridgeEntry.
type ChannelBridgeEntry = channel.BridgeEntry

// ─── Function wrapper ───────────────────────────────────────────────────────────

// LoadChannelBridgeConfig reads .cog/config/channels.yaml from the workspace root.
// Delegates to pkg/substrate/channel.LoadBridgeConfig.
func LoadChannelBridgeConfig(root string) (*ChannelBridgeConfig, error) {
	return channel.LoadBridgeConfig(root)
}
