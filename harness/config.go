// config.go implements node-level inference configuration.
//
// CogOS supports a three-tier config resolution for inference settings:
//
//	~/.cog/etc/inference.yaml           node (shared across all workspaces)
//	$workspace/.cog/conf/inference.yaml workspace (overrides node)
//	Environment variables               OPENAI_API_KEY, OPENROUTER_API_KEY, etc.
//	Compiled defaults                   DefaultProviders() in providers.go
//
// LoadInferenceConfig merges node + workspace layers. Environment variables and
// compiled defaults are handled separately by DefaultProviders().
package harness

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// InferenceConfig represents the inference configuration file.
// Resolution order: node (~/.cog/etc/) → workspace (.cog/conf/) → env → defaults.
type InferenceConfig struct {
	DefaultProvider string                       `yaml:"default_provider,omitempty"`
	Providers       map[string]*ProviderConfig   `yaml:"providers,omitempty"`
}

// LoadInferenceConfig loads inference configuration with three-tier resolution:
//  1. ~/.cog/etc/inference.yaml        (node — shared across workspaces)
//  2. $workspaceRoot/.cog/conf/inference.yaml  (workspace — overrides)
//  3. Environment variables             (OPENAI_API_KEY, OPENROUTER_API_KEY, etc.)
//  4. Compiled defaults                 (claude, localhost:11434)
func LoadInferenceConfig(workspaceRoot string) *InferenceConfig {
	cfg := &InferenceConfig{}

	// Layer 1: Node-level config
	if home, err := os.UserHomeDir(); err == nil {
		nodePath := filepath.Join(home, ".cog", "etc", "inference.yaml")
		if data, err := os.ReadFile(nodePath); err == nil {
			yaml.Unmarshal(data, cfg)
		}
	}

	// Layer 2: Workspace-level config (overrides node)
	if workspaceRoot != "" {
		wsPath := filepath.Join(workspaceRoot, ".cog", "conf", "inference.yaml")
		if data, err := os.ReadFile(wsPath); err == nil {
			var wsCfg InferenceConfig
			if err := yaml.Unmarshal(data, &wsCfg); err == nil {
				// Merge workspace over node
				if wsCfg.DefaultProvider != "" {
					cfg.DefaultProvider = wsCfg.DefaultProvider
				}
				if wsCfg.Providers != nil {
					if cfg.Providers == nil {
						cfg.Providers = make(map[string]*ProviderConfig)
					}
					for k, v := range wsCfg.Providers {
						cfg.Providers[k] = v
					}
				}
			}
		}
	}

	return cfg
}
