// meta.go
// Meta-reconciler types and dependency resolution for multi-provider orchestration.
// Reads resource declarations to determine which providers to reconcile,
// in what order, with what settings.

package reconcile

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

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
	Plan     *Plan  `json:"plan,omitempty"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
}

// MetaOpts controls meta-reconciler behavior.
type MetaOpts struct {
	DryRun         bool   // plan only, no apply
	ResourceFilter string // if set, only reconcile this resource
	Token          string // auth token (passed to providers)
	JSONOutput     bool
}

// ResolveOrder returns resources in dependency-resolved order.
// Uses Kahn's algorithm with wave-based ordering within levels.
func ResolveOrder(resources []MetaResource) ([][]MetaResource, error) {
	// Build index and adjacency
	byName := make(map[string]*MetaResource, len(resources))
	inDegree := make(map[string]int, len(resources))
	dependents := make(map[string][]string) // name -> names that depend on it

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

// AutoDiscoverResources creates a MetaConfig from all registered providers.
// Each gets default settings (manual interval, no prune, no auto-apply).
func AutoDiscoverResources() *MetaConfig {
	providerNames := ListProviders()
	resources := make([]MetaResource, len(providerNames))
	for i, name := range providerNames {
		resources[i] = MetaResource{
			Name:     name,
			Interval: "manual",
		}
	}
	return &MetaConfig{Resources: resources}
}

// RunMeta orchestrates reconciliation across all declared resources.
func RunMeta(root string, cfg *MetaConfig, opts MetaOpts) ([]MetaResult, error) {
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
	levels, err := ResolveOrder(active)
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

// ConfigureProvider sets up a provider with auth token if needed.
func ConfigureProvider(provider Reconcilable, resourceType, flagToken string) {
	if t, ok := provider.(Tokenable); ok {
		token := ResolveToken(resourceType, flagToken)
		if token != "" {
			t.SetToken(token)
		}
	}
}

// ResolveToken resolves an auth token from flag or environment.
// Checks flag first, then {RESOURCE_TYPE}_TOKEN env vars.
func ResolveToken(resourceType, flagToken string) string {
	if flagToken != "" {
		return flagToken
	}

	// Check common env var patterns
	upper := strings.ToUpper(resourceType)
	envNames := []string{
		upper + "_BOT_TOKEN",
		upper + "_TOKEN",
		upper + "_API_TOKEN",
	}
	for _, name := range envNames {
		if v, ok := lookupEnv(name); ok && v != "" {
			return v
		}
	}
	return ""
}

// lookupEnv wraps os.LookupEnv for testability.
var lookupEnv = func(key string) (string, bool) {
	v := strings.TrimSpace(getEnv(key))
	return v, v != ""
}

var getEnv = os.Getenv

// reconcileResource runs plan (and optionally apply) for a single resource.
func reconcileResource(root string, res MetaResource, opts MetaOpts) MetaResult {
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
	ConfigureProvider(provider, res.Name, opts.Token)

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
	state, _ := LoadState(root, res.Name)

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
			WriteState(root, res.Name, newState)
		}

		result.Status = "applied"
	}

	result.Duration = time.Since(start).Milliseconds()
	return result
}
