// reconcile_meta.go
// Meta-reconciler kernel integration.
// Types, dependency resolution, and orchestration logic delegate to pkg/reconcile.
// YAML loading remains here because it uses kernel internals (workspace layout,
// registered init() providers). The `cog reconcile` CLI has been removed.

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
	"gopkg.in/yaml.v3"
)

// --- Type aliases for backward compatibility ---

type MetaResource = reconcile.MetaResource
type MetaConfig = reconcile.MetaConfig
type MetaResult = reconcile.MetaResult
type MetaReconcileOpts = reconcile.MetaOpts

// --- Re-exported functions ---

var (
	resolveOrder          = reconcile.ResolveOrder
	RunMetaReconcile      = reconcile.RunMeta
	autoDiscoverResources = reconcile.AutoDiscoverResources
	configureProvider     = reconcile.ConfigureProvider
)

// --- Config loading (kernel-specific: reads from workspace) ---

// loadMetaConfig loads resources.yaml from the workspace.
func loadMetaConfig(root string) (*MetaConfig, error) {
	// Try YAML first
	yamlPath := filepath.Join(root, ".cog", "config", "resources.yaml")
	if data, err := os.ReadFile(yamlPath); err == nil {
		var cfg MetaConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", yamlPath, err)
		}
		return &cfg, nil
	}

	// Auto-discover: if no config exists, build from registered providers
	return autoDiscoverResources(), nil
}

