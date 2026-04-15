// reconcile_meta.go
// Meta-reconciler CLI commands and kernel integration.
// Types, dependency resolution, and orchestration logic delegate to pkg/reconcile.
// CLI commands and YAML loading remain here because they use kernel internals
// (ResolveWorkspace, registered init() providers).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cogos-dev/cogos/pkg/reconcile"
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

// --- CLI commands (kernel-specific: use ResolveWorkspace) ---

// cmdReconcile handles `cog reconcile [init] [--resource X] [--dry-run] [--json]`
func cmdReconcile(args []string) int {
	// Subcommand: reconcile init — generate resources.yaml from providers
	if len(args) > 0 && args[0] == "init" {
		return cmdReconcileInit()
	}

	opts := MetaReconcileOpts{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--resource":
			if i+1 < len(args) {
				opts.ResourceFilter = args[i+1]
				i++
			}
		case "--dry-run":
			opts.DryRun = true
		case "--json":
			opts.JSONOutput = true
		case "--token":
			if i+1 < len(args) {
				opts.Token = args[i+1]
				i++
			}
		}
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cfg, err := loadMetaConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading resources config: %v\n", err)
		return 1
	}

	results, err := RunMetaReconcile(root, cfg, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if opts.JSONOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	} else {
		printMetaResults(results)
	}

	// Check for failures
	for _, r := range results {
		if r.Status == "failed" {
			return 1
		}
	}
	return 0
}

// printMetaResults displays reconciliation results in human-readable format.
func printMetaResults(results []MetaResult) {
	if len(results) == 0 {
		fmt.Println("No resources to reconcile.")
		return
	}

	fmt.Printf("Reconciliation results (%d resources):\n\n", len(results))
	for _, r := range results {
		symbol := "?"
		switch r.Status {
		case "synced":
			symbol = "="
		case "drifted":
			symbol = "~"
		case "applied":
			symbol = "+"
		case "failed":
			symbol = "!"
		case "skipped":
			symbol = "-"
		case "suspended":
			symbol = "z"
		}

		line := fmt.Sprintf("  %s %-12s %s", symbol, r.Resource, r.Status)
		if r.Plan != nil && r.Plan.Summary.HasChanges() {
			line += fmt.Sprintf(" (+%d ~%d -%d)",
				r.Plan.Summary.Creates, r.Plan.Summary.Updates, r.Plan.Summary.Deletes)
		}
		if r.Error != "" {
			line += fmt.Sprintf(" — %s", r.Error)
		}
		if r.Duration > 0 {
			line += fmt.Sprintf(" [%dms]", r.Duration)
		}
		fmt.Println(line)
	}
}

// cmdReconcileInit generates resources.yaml from auto-discovered providers.
func cmdReconcileInit() int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	outPath := filepath.Join(root, ".cog", "config", "resources.yaml")

	// Check if already exists
	if _, err := os.Stat(outPath); err == nil {
		fmt.Printf("resources.yaml already exists at %s\n", outPath)
		fmt.Println("Delete it first if you want to regenerate.")
		return 0
	}

	cfg := autoDiscoverResources()

	// Assign waves based on known dependency patterns
	for i := range cfg.Resources {
		switch cfg.Resources[i].Name {
		case "agent":
			cfg.Resources[i].Wave = 0
			cfg.Resources[i].Interval = "5m"
		case "openclaw-agents", "openclaw-cron":
			cfg.Resources[i].Wave = 1
			cfg.Resources[i].Interval = "5m"
			cfg.Resources[i].AutoApply = true
			cfg.Resources[i].DependsOn = []string{"agent"}
		case "openclaw-gateway":
			cfg.Resources[i].Wave = 2
			cfg.Resources[i].Interval = "10m"
			cfg.Resources[i].DependsOn = []string{"openclaw-agents"}
		default:
			cfg.Resources[i].Wave = 3
			cfg.Resources[i].Interval = "30m"
		}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	header := "# CogOS Resource Declarations\n# Generated by: cog reconcile init\n# Edit to customize intervals, waves, and auto_apply settings.\n\n"
	if err := os.WriteFile(outPath, []byte(header+string(data)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing resources.yaml: %v\n", err)
		return 1
	}

	fmt.Printf("Generated resources.yaml with %d providers:\n", len(cfg.Resources))
	for _, r := range cfg.Resources {
		fmt.Printf("  wave %d: %s (interval=%s, auto_apply=%v)\n", r.Wave, r.Name, r.Interval, r.AutoApply)
	}
	fmt.Printf("\nWritten to: %s\n", outPath)
	return 0
}
