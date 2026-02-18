package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// BusChatConfig holds configuration for bus-native chat event emission.
// Loaded from .cog/config/bus_chat.yaml (or falls back to defaults).
type BusChatConfig struct {
	TAAProfile string          `yaml:"taa_profile"`
	MaxHistory int             `yaml:"max_history"`
	Features   BusChatFeatures `yaml:"features"`
}

// BusChatFeatures holds feature toggles for the bus chat pipeline.
type BusChatFeatures struct {
	TAAEnabled     bool `yaml:"taa_enabled"`
	ContextFromBus bool `yaml:"context_from_bus"`
	CogFieldEmit   bool `yaml:"cogfield_emit"`
}

// DefaultBusChatConfig returns a BusChatConfig with sensible defaults.
func DefaultBusChatConfig() *BusChatConfig {
	return &BusChatConfig{
		TAAProfile: "default",
		MaxHistory: 50,
		Features: BusChatFeatures{
			TAAEnabled:     true,
			ContextFromBus: true,
			CogFieldEmit:   true,
		},
	}
}

// LoadBusChatConfig loads bus chat config from .cog/config/bus_chat.yaml.
// Returns defaults if the file does not exist.
func LoadBusChatConfig(workspaceRoot string) *BusChatConfig {
	configPath := filepath.Join(workspaceRoot, ".cog", "config", "bus_chat.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return DefaultBusChatConfig()
	}

	cfg := DefaultBusChatConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to parse bus_chat.yaml: %v (using defaults)\n", err)
		return DefaultBusChatConfig()
	}

	return cfg
}
