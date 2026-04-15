// reconcile_generic.go
// Generic CLI command implementations that work with any Reconcilable provider.
// These form the extensibility backbone: new providers get plan/apply/status/refresh
// for free by implementing Reconcilable and registering in the provider registry.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/cogos-dev/cogos/pkg/reconcile"
)

// resolveGenericToken resolves an auth token from flag or environment.
func resolveGenericToken(resourceType, flagToken string) string {
	return reconcile.ResolveToken(resourceType, flagToken)
}

// parseGenericFlags parses common flags from args: --token, --json.
// Returns token, jsonOutput, and remaining positional args.
func parseGenericFlags(args []string) (token string, jsonOutput bool, positional []string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		case "--json":
			jsonOutput = true
		default:
			positional = append(positional, args[i])
		}
	}
	return
}

// --- Generic command implementations ---

// cmdGenericPlan runs a plan for any registered provider.
func cmdGenericPlan(resourceType string, args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	provider, err := GetProvider(resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	flagToken, jsonOutput, _ := parseGenericFlags(args)
	configureProvider(provider, resourceType, flagToken)

	// Load config
	config, err := provider.LoadConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	// Fetch live state
	ctx := context.Background()
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching live state: %v\n", err)
		return 1
	}

	// Load existing state
	state, err := LoadReconcileState(root, resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load state: %v\n", err)
	}

	// Compute plan
	plan, err := provider.ComputePlan(config, live, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error computing plan: %v\n", err)
		return 1
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(plan)
	} else {
		printGenericPlan(plan)
	}
	return 0
}

// cmdGenericApply runs plan + apply for any registered provider.
func cmdGenericApply(resourceType string, args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	provider, err := GetProvider(resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	flagToken, _, _ := parseGenericFlags(args)
	configureProvider(provider, resourceType, flagToken)

	// Load config
	config, err := provider.LoadConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	// Fetch live state
	ctx := context.Background()
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching live state: %v\n", err)
		return 1
	}

	// Load existing state
	state, err := LoadReconcileState(root, resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load state: %v\n", err)
	}

	// Compute plan
	plan, err := provider.ComputePlan(config, live, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error computing plan: %v\n", err)
		return 1
	}

	if !plan.Summary.HasChanges() {
		fmt.Println("No changes needed — already in sync.")
		return 0
	}

	// Show plan
	printGenericPlan(plan)
	fmt.Println()

	// Apply
	EmitApplyStart(resourceType, len(plan.Actions))
	applyStart := time.Now()
	results, err := provider.ApplyPlan(ctx, plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error applying plan: %v\n", err)
		return 1
	}
	applyDuration := time.Since(applyStart).Milliseconds()

	// Print results
	succeeded, failed := 0, 0
	for _, r := range results {
		status := "OK"
		if r.Status == ApplyFailed {
			status = "FAIL"
			failed++
		} else {
			succeeded++
		}
		fmt.Printf("  %s %-8s %s", status, r.Action, r.Name)
		if r.CreatedID != "" {
			fmt.Printf(" (id: %s)", r.CreatedID)
		}
		if r.Error != "" {
			fmt.Printf(" — %s", r.Error)
		}
		fmt.Println()
	}

	fmt.Printf("\nApply complete: %d succeeded, %d failed (%dms)\n", succeeded, failed, applyDuration)

	// Emit events
	applyResults := make([]ApplyResult, len(results))
	for i, r := range results {
		applyResults[i] = ApplyResult{
			Phase:  r.Phase,
			Action: r.Action,
			Name:   r.Name,
			Status: string(r.Status),
			Error:  r.Error,
		}
	}
	EmitApplyComplete(resourceType, applyResults, applyDuration)

	// Update state
	newState, err := provider.BuildState(config, live, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not build state: %v\n", err)
	} else {
		if err := WriteReconcileState(root, resourceType, newState); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not write state: %v\n", err)
		}
	}

	if failed > 0 {
		return 1
	}
	return 0
}

// cmdGenericStatus shows health + state summary for any registered provider.
func cmdGenericStatus(resourceType string, args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	provider, err := GetProvider(resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	flagToken, _, _ := parseGenericFlags(args)
	configureProvider(provider, resourceType, flagToken)

	// Health check
	health := provider.Health()
	fmt.Printf("Resource: %s\n", resourceType)
	fmt.Printf("Health:   %s\n", health.Health)
	fmt.Printf("Sync:     %s\n", health.Sync)
	fmt.Printf("Phase:    %s\n", health.Operation)
	if health.Message != "" {
		fmt.Printf("Message:  %s\n", health.Message)
	}

	// State summary
	state, err := LoadReconcileState(root, resourceType)
	if err != nil || state == nil {
		fmt.Println("\nState: not found")
	} else {
		managed, unmanaged := 0, 0
		for _, r := range state.Resources {
			if r.Mode == ModeManaged {
				managed++
			} else {
				unmanaged++
			}
		}
		fmt.Printf("\nState: serial %d, lineage %s\n", state.Serial, state.Lineage)
		fmt.Printf("Resources: %d managed, %d unmanaged (%d total)\n", managed, unmanaged, len(state.Resources))
		fmt.Printf("Last updated: %s\n", state.GeneratedAt)
	}

	return 0
}

// cmdGenericRefresh updates state from live for any registered provider.
func cmdGenericRefresh(resourceType string, args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	provider, err := GetProvider(resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	flagToken, _, _ := parseGenericFlags(args)
	configureProvider(provider, resourceType, flagToken)

	// Load config
	config, err := provider.LoadConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	// Fetch live state
	ctx := context.Background()
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching live state: %v\n", err)
		return 1
	}

	// Load existing state
	existing, _ := LoadReconcileState(root, resourceType)

	// Build updated state
	newState, err := provider.BuildState(config, live, existing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building state: %v\n", err)
		return 1
	}

	// Write state
	if err := WriteReconcileState(root, resourceType, newState); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing state: %v\n", err)
		return 1
	}

	fmt.Printf("Refreshed %s state: %d resources (serial %d)\n",
		resourceType, len(newState.Resources), newState.Serial)
	return 0
}

// cmdGenericSnapshot crawls live and writes state for any registered provider.
// If the provider implements ConfigExporter, also exports a config file.
func cmdGenericSnapshot(resourceType string, args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	provider, err := GetProvider(resourceType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	flagToken, _, _ := parseGenericFlags(args)
	configureProvider(provider, resourceType, flagToken)

	// If provider supports config export, do it first
	if exporter, ok := provider.(ConfigExporter); ok {
		fmt.Printf("Exporting %s config...\n", resourceType)
		if err := exporter.ExportConfig(root); err != nil {
			fmt.Fprintf(os.Stderr, "Error exporting config: %v\n", err)
			return 1
		}
		fmt.Printf("Config exported to .cog/config/%s/config.yaml\n", resourceType)
	}

	// Then refresh state
	fmt.Printf("Refreshing %s state...\n", resourceType)
	return cmdGenericRefresh(resourceType, args)
}

// --- Output helpers ---

func printGenericPlan(plan *ReconcilePlan) {
	if !plan.Summary.HasChanges() {
		fmt.Printf("[%s] No changes — config matches live state.\n", plan.ResourceType)
		return
	}

	fmt.Printf("[%s] Plan: %d to create, %d to update, %d to delete, %d skipped\n",
		plan.ResourceType,
		plan.Summary.Creates, plan.Summary.Updates,
		plan.Summary.Deletes, plan.Summary.Skipped)

	for _, w := range plan.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}

	for _, a := range plan.Actions {
		symbol := "?"
		switch a.Action {
		case ActionCreate:
			symbol = "+"
		case ActionUpdate:
			symbol = "~"
		case ActionDelete:
			symbol = "-"
		case ActionSkip:
			symbol = "○"
		}
		fmt.Printf("  %s %s %s\n", symbol, a.ResourceType, a.Name)
	}
}
