// config.go — CogOS v3 configuration loading
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// hasUsableCogConfig reports whether dir looks like a real workspace root for
// v3 rather than a nested helper directory that happens to contain .cog/.
func hasUsableCogConfig(dir string) bool {
	configDir := filepath.Join(dir, ".cog", "config")
	info, err := os.Stat(configDir)
	return err == nil && info.IsDir()
}

// Config holds all runtime configuration for the v3 kernel.
type Config struct {
	// WorkspaceRoot is the absolute path to the cog-workspace root.
	WorkspaceRoot string

	// CogDir is WorkspaceRoot/.cog
	CogDir string

	// Port the HTTP API listens on. Default: 5200 (v2 is 5100).
	Port int

	// ConsolidationInterval is how often the consolidation loop fires (seconds).
	ConsolidationInterval int

	// HeartbeatInterval is the dormant-state heartbeat cadence (seconds).
	HeartbeatInterval int

	// SalienceDaysWindow is the git history window for salience scoring.
	SalienceDaysWindow int

	// OutputReserve is tokens reserved for model generation (subtracted from budget).
	OutputReserve int

	// TRMWeightsPath is the path to the TRM binary weights file.
	// If empty, TRM is disabled and keyword+salience scoring is used.
	TRMWeightsPath string

	// TRMEmbeddingsPath is the path to the TRM embedding index binary.
	TRMEmbeddingsPath string

	// TRMChunksPath is the path to the TRM chunk metadata JSON.
	TRMChunksPath string

	// OllamaEmbedEndpoint is the Ollama /api/embeddings endpoint URL.
	// Default: http://localhost:11434
	OllamaEmbedEndpoint string

	// OllamaEmbedModel is the embedding model name for Ollama.
	// Default: nomic-embed-text
	OllamaEmbedModel string
}

// kernelConfigSection holds settings that can appear at the top level or inside v3:.
type kernelConfigSection struct {
	Port                  int    `yaml:"port"`
	ConsolidationInterval int    `yaml:"consolidation_interval"`
	HeartbeatInterval     int    `yaml:"heartbeat_interval"`
	SalienceDaysWindow    int    `yaml:"salience_days_window"`
	OutputReserve         int    `yaml:"output_reserve"`
	TRMWeightsPath        string `yaml:"trm_weights_path"`
	TRMEmbeddingsPath     string `yaml:"trm_embeddings_path"`
	TRMChunksPath         string `yaml:"trm_chunks_path"`
	OllamaEmbedEndpoint   string `yaml:"ollama_embed_endpoint"`
	OllamaEmbedModel      string `yaml:"ollama_embed_model"`
}

// kernelConfig is the on-disk YAML shape of .cog/config/kernel.yaml.
// Top-level fields apply to all kernels; the v3: section overrides them
// for the v3 kernel specifically (allowing shared kernel.yaml across v2/v3).
type kernelConfig struct {
	kernelConfigSection `yaml:",inline"`
	V3                  kernelConfigSection `yaml:"v3"`
}

// LoadConfig builds a Config from flags + environment + .cog/config/kernel.yaml.
// Precedence: flag > env > file > default.
func LoadConfig(workspaceRoot string, port int) (*Config, error) {
	if workspaceRoot == "" {
		// Auto-detect: walk up from cwd until we find a .cog directory.
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
		found, err := findWorkspaceRoot(wd)
		if err != nil {
			return nil, err
		}
		workspaceRoot = found
	}

	cfg := &Config{
		WorkspaceRoot:         workspaceRoot,
		CogDir:                filepath.Join(workspaceRoot, ".cog"),
		Port:                  5200,
		ConsolidationInterval: 900,
		HeartbeatInterval:     60,
		SalienceDaysWindow:    90,
		OutputReserve:         4096,
	}

	// Load from file if present.
	kf := filepath.Join(cfg.CogDir, "config", "kernel.yaml")
	if data, err := os.ReadFile(kf); err == nil {
		var kc kernelConfig
		if err := yaml.Unmarshal(data, &kc); err == nil {
			// Apply top-level shared settings first, then v3: section overrides.
			applyKernelSection(cfg, kc.kernelConfigSection)
			applyKernelSection(cfg, kc.V3)
		}
	}

	// Flag override.
	if port != 0 {
		cfg.Port = port
	}

	return cfg, nil
}

// applyKernelSection applies non-zero values from a config section to cfg.
func applyKernelSection(cfg *Config, s kernelConfigSection) {
	if s.Port != 0 {
		cfg.Port = s.Port
	}
	if s.ConsolidationInterval != 0 {
		cfg.ConsolidationInterval = s.ConsolidationInterval
	}
	if s.HeartbeatInterval != 0 {
		cfg.HeartbeatInterval = s.HeartbeatInterval
	}
	if s.SalienceDaysWindow != 0 {
		cfg.SalienceDaysWindow = s.SalienceDaysWindow
	}
	if s.OutputReserve != 0 {
		cfg.OutputReserve = s.OutputReserve
	}
	if s.TRMWeightsPath != "" {
		cfg.TRMWeightsPath = s.TRMWeightsPath
	}
	if s.TRMEmbeddingsPath != "" {
		cfg.TRMEmbeddingsPath = s.TRMEmbeddingsPath
	}
	if s.TRMChunksPath != "" {
		cfg.TRMChunksPath = s.TRMChunksPath
	}
	if s.OllamaEmbedEndpoint != "" {
		cfg.OllamaEmbedEndpoint = s.OllamaEmbedEndpoint
	}
	if s.OllamaEmbedModel != "" {
		cfg.OllamaEmbedModel = s.OllamaEmbedModel
	}
}

// findWorkspaceRoot walks up from dir until it finds a directory containing a
// usable .cog/config/ directory.
func findWorkspaceRoot(dir string) (string, error) {
	for {
		if hasUsableCogConfig(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no usable .cog/config directory found from %s upward", dir)
		}
		dir = parent
	}
}
