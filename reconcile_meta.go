// reconcile_meta.go
// Meta-reconciler: orchestrates multi-provider reconciliation.
// Reads resources.hcl (or resources.yaml) to determine which providers
// to reconcile, in what order, with what settings.
//
// This is the Flux Kustomization equivalent for CogOS.
//
// Usage:
//   cog reconcile              # all resources in dependency order
//   cog reconcile --resource X # only resource X
//   cog reconcile --dry-run    # plan only, no apply

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Meta-reconciliation types ──────────────────────────────────────────────

// MetaResource declares a single resource provider to reconcile.
type MetaResource struct {
	Name      string   `yaml:"name" json:"name"`
	Source    string   `yaml:"source" json:"source"`
	Interval  string   `yaml:"interval" json:"interval"`
	Prune     bool     `yaml:"prune" json:"prune"`
	AutoApply bool     `yaml:"auto_apply" json:"auto_apply"`
	DependsOn []string `yaml:"depends_on" json:"depends_on"`
	Wave      int      `yaml:"wave" json:"wave"`
	Suspended bool     `yaml:"suspended" json:"suspended"`
}

// MetaConfig holds the full resources declaration.
type MetaConfig struct {
	Resources []MetaResource `yaml:"resources" json:"resources"`
}

// MetaResult tracks the outcome of reconciling a single resource.
type MetaResult struct {
	Resource string `json:"resource"`
	Status   string `json:"status"` // "synced", "drifted", "applied", "failed", "skipped", "suspended"
	Plan     *ReconcilePlan   `json:"plan,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
}

// ─── Config loading ─────────────────────────────────────────────────────────

// loadMetaConfig loads resources.yaml (or resources.hcl in future) from the workspace.
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

// autoDiscoverResources creates a MetaConfig from all registered providers.
// Each gets default settings (manual interval, no prune, no auto-apply).
func autoDiscoverResources() *MetaConfig {
	providers := ListProviders()
	resources := make([]MetaResource, len(providers))
	for i, name := range providers {
		resources[i] = MetaResource{
			Name:     name,
			Interval: "manual",
		}
	}
	return &MetaConfig{Resources: resources}
}

// ─── Dependency resolution (topological sort) ───────────────────────────────

// resolveOrder returns resources in dependency-resolved order.
// Uses Kahn's algorithm with wave-based ordering within levels.
func resolveOrder(resources []MetaResource) ([][]MetaResource, error) {
	// Build index and adjacency
	byName := make(map[string]*MetaResource, len(resources))
	inDegree := make(map[string]int, len(resources))
	dependents := make(map[string][]string) // name → names that depend on it

	for i := range resources {
		r := &resources[i]
		byName[r.Name] = r
		inDegree[r.Name] = 0
	}

	for _, r := range resources {
		for _, dep := range r.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("resource %q depends on unknown resource %q", r.Name, dep)
			}
			inDegree[r.Name]++
			dependents[dep] = append(dependents[dep], r.Name)
		}
	}

	// Kahn's algorithm: process by levels
	var levels [][]MetaResource
	visited := 0

	for visited < len(resources) {
		// Find all resources with in-degree 0
		var ready []MetaResource
		for _, r := range resources {
			if inDegree[r.Name] == 0 {
				ready = append(ready, r)
			}
		}

		if len(ready) == 0 {
			// Cycle detected
			var remaining []string
			for name, deg := range inDegree {
				if deg > 0 {
					remaining = append(remaining, name)
				}
			}
			sort.Strings(remaining)
			return nil, fmt.Errorf("dependency cycle detected among: %s", strings.Join(remaining, ", "))
		}

		// Sort by wave within this level
		sort.Slice(ready, func(i, j int) bool {
			if ready[i].Wave != ready[j].Wave {
				return ready[i].Wave < ready[j].Wave
			}
			return ready[i].Name < ready[j].Name
		})

		levels = append(levels, ready)

		// Remove processed nodes
		for _, r := range ready {
			inDegree[r.Name] = -1 // mark as processed
			visited++
			for _, dep := range dependents[r.Name] {
				inDegree[dep]--
			}
		}
	}

	return levels, nil
}

// ─── Meta-reconciler execution ──────────────────────────────────────────────

// RunMetaReconcile orchestrates reconciliation across all declared resources.
func RunMetaReconcile(root string, cfg *MetaConfig, opts MetaReconcileOpts) ([]MetaResult, error) {
	// Filter suspended resources
	var active []MetaResource
	var results []MetaResult

	for _, r := range cfg.Resources {
		if r.Suspended {
			results = append(results, MetaResult{
				Resource: r.Name,
				Status:   "suspended",
			})
			continue
		}
		if opts.ResourceFilter != "" && r.Name != opts.ResourceFilter {
			continue
		}
		active = append(active, r)
	}

	if len(active) == 0 {
		return results, nil
	}

	// Resolve dependency order
	levels, err := resolveOrder(active)
	if err != nil {
		return nil, err
	}

	// Execute level by level
	failed := make(map[string]bool)

	for _, level := range levels {
		for _, res := range level {
			// Check if any dependency failed
			skip := false
			for _, dep := range res.DependsOn {
				if failed[dep] {
					results = append(results, MetaResult{
						Resource: res.Name,
						Status:   "skipped",
						Error:    fmt.Sprintf("dependency %q failed", dep),
					})
					skip = true
					break
				}
			}
			if skip {
				continue
			}

			result := reconcileResource(root, res, opts)
			if result.Status == "failed" {
				failed[res.Name] = true
			}
			results = append(results, result)
		}
	}

	return results, nil
}

// MetaReconcileOpts controls meta-reconciler behavior.
type MetaReconcileOpts struct {
	DryRun         bool   // plan only, no apply
	ResourceFilter string // if set, only reconcile this resource
	Token          string // auth token (passed to providers)
	JSONOutput     bool
}

// reconcileResource runs plan (and optionally apply) for a single resource.
func reconcileResource(root string, res MetaResource, opts MetaReconcileOpts) MetaResult {
	start := time.Now()
	result := MetaResult{Resource: res.Name}

	// Get provider
	provider, err := GetProvider(res.Name)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("provider not found: %v", err)
		result.Duration = time.Since(start).Milliseconds()
		return result
	}

	// Configure auth
	configureProvider(provider, res.Name, opts.Token)

	// Load config
	config, err := provider.LoadConfig(root)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("config load: %v", err)
		result.Duration = time.Since(start).Milliseconds()
		return result
	}

	// Fetch live
	ctx := context.Background()
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("fetch live: %v", err)
		result.Duration = time.Since(start).Milliseconds()
		return result
	}

	// Load state
	state, _ := LoadReconcileState(root, res.Name)

	// Compute plan
	plan, err := provider.ComputePlan(config, live, state)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("plan: %v", err)
		result.Duration = time.Since(start).Milliseconds()
		return result
	}
	result.Plan = plan

	if !plan.Summary.HasChanges() {
		result.Status = "synced"
		result.Duration = time.Since(start).Milliseconds()
		return result
	}

	result.Status = "drifted"

	// Apply if enabled (and not dry-run)
	shouldApply := (res.AutoApply || !opts.DryRun) && !opts.DryRun
	if shouldApply {
		_, err := provider.ApplyPlan(ctx, plan)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("apply: %v", err)
			result.Duration = time.Since(start).Milliseconds()
			return result
		}

		// Update state
		newState, err := provider.BuildState(config, live, state)
		if err == nil {
			WriteReconcileState(root, res.Name, newState)
		}

		result.Status = "applied"
	}

	result.Duration = time.Since(start).Milliseconds()
	return result
}

// ─── CLI command ────────────────────────────────────────────────────────────

// cmdReconcile handles `cog reconcile [--resource X] [--dry-run] [--json]`
func cmdReconcile(args []string) int {
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
