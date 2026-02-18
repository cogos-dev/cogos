// reconcile_watch.go
// Continuous reconciliation loop for CogOS resources.
// Implements the Flux/ArgoCD pattern: plan → apply → sleep → repeat.
//
// Usage:
//   cog watch <resource> [--interval 5m] [--auto-apply] [--max-cycles N] [--token TOKEN]

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// WatchConfig controls the continuous reconciliation loop.
type WatchConfig struct {
	ResourceType string
	Interval     time.Duration
	AutoApply    bool // if true, apply after plan; if false, plan-only (drift detection)
	MaxCycles    int  // 0 = unlimited
	Root         string
	Token        string // provider-specific auth (e.g., Discord bot token)
}

// cmdWatch handles `cog watch <resource> [flags]`
func cmdWatch(args []string) int {
	if len(args) == 0 {
		fmt.Println("Usage: cog watch <resource> [--interval 5m] [--auto-apply] [--max-cycles N]")
		fmt.Println("\nResources:", ListProviders())
		return 1
	}

	resourceType := args[0]

	// Parse flags
	cfg := WatchConfig{
		ResourceType: resourceType,
		Interval:     5 * time.Minute, // default
		AutoApply:    false,           // default: plan-only (safe)
		MaxCycles:    0,               // unlimited
	}

	// Parse remaining args for flags
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "Invalid interval: %v\n", err)
					return 1
				}
				cfg.Interval = d
				i++
			}
		case "--auto-apply":
			cfg.AutoApply = true
		case "--max-cycles":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &cfg.MaxCycles)
				i++
			}
		case "--token":
			if i+1 < len(args) {
				cfg.Token = args[i+1]
				i++
			}
		}
	}

	// Resolve workspace
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	cfg.Root = root

	if err := RunWatch(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Watch error: %v\n", err)
		return 1
	}
	return 0
}

// RunWatch executes the continuous reconciliation loop.
func RunWatch(cfg WatchConfig) error {
	// Get provider
	provider, err := GetProvider(cfg.ResourceType)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "[watch] Starting continuous reconciliation for %s (interval: %s, auto-apply: %v)\n",
		cfg.ResourceType, cfg.Interval, cfg.AutoApply)

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "\n[watch] Received shutdown signal, stopping...\n")
		cancel()
	}()

	cycle := 0
	for {
		cycle++
		if cfg.MaxCycles > 0 && cycle > cfg.MaxCycles {
			fmt.Fprintf(os.Stderr, "[watch] Reached max cycles (%d), stopping\n", cfg.MaxCycles)
			return nil
		}

		fmt.Fprintf(os.Stderr, "\n[watch] === Cycle %d (%s) ===\n", cycle, time.Now().UTC().Format(time.RFC3339))

		err := runWatchCycle(ctx, provider, cfg)
		if err != nil {
			EmitReconcileError(cfg.ResourceType, err)
			fmt.Fprintf(os.Stderr, "[watch] Cycle %d error: %v\n", cycle, err)
			// Don't exit on error — continue watching
		}

		// Wait for interval or shutdown
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "[watch] Shutdown complete\n")
			return nil
		case <-time.After(cfg.Interval):
			// continue to next cycle
		}
	}
}

// runWatchCycle executes a single plan (and optionally apply) cycle.
func runWatchCycle(ctx context.Context, provider Reconcilable, cfg WatchConfig) error {
	startTime := time.Now()

	// 1. Load config
	EmitPlanStart(cfg.ResourceType)
	config, err := provider.LoadConfig(cfg.Root)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// 2. Fetch live state
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		return fmt.Errorf("fetching live: %w", err)
	}

	// 3. Load existing state
	state, err := LoadReconcileState(cfg.Root, cfg.ResourceType)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// 4. Compute plan
	plan, err := provider.ComputePlan(config, live, state)
	if err != nil {
		return fmt.Errorf("computing plan: %w", err)
	}

	planDuration := time.Since(startTime).Milliseconds()
	EmitPlanComplete(cfg.ResourceType, PlanSummary{
		Creates: plan.Summary.Creates,
		Updates: plan.Summary.Updates,
		Deletes: plan.Summary.Deletes,
		Skipped: plan.Summary.Skipped,
	}, planDuration)

	// 5. Report
	if plan.Summary.HasChanges() {
		EmitDriftDetected(cfg.ResourceType, plan.Summary.Creates+plan.Summary.Updates+plan.Summary.Deletes)
		fmt.Fprintf(os.Stderr, "[watch] Drift detected: +%d ~%d -%d\n",
			plan.Summary.Creates, plan.Summary.Updates, plan.Summary.Deletes)

		// 6. Auto-apply if enabled
		if cfg.AutoApply {
			applyStart := time.Now()
			EmitApplyStart(cfg.ResourceType, len(plan.Actions))

			results, err := provider.ApplyPlan(ctx, plan)
			if err != nil {
				return fmt.Errorf("applying plan: %w", err)
			}

			applyDuration := time.Since(applyStart).Milliseconds()

			// Convert ReconcileResult to ApplyResult for event emission
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
			EmitApplyComplete(cfg.ResourceType, applyResults, applyDuration)

			// Update state after apply
			newState, err := provider.BuildState(config, live, state)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[watch] Warning: failed to build state: %v\n", err)
			} else {
				if err := WriteReconcileState(cfg.Root, cfg.ResourceType, newState); err != nil {
					fmt.Fprintf(os.Stderr, "[watch] Warning: failed to write state: %v\n", err)
				}
			}

			// Count results
			succeeded, failed := 0, 0
			for _, r := range results {
				if r.Status == ApplySucceeded {
					succeeded++
				} else if r.Status == ApplyFailed {
					failed++
				}
			}
			fmt.Fprintf(os.Stderr, "[watch] Applied: %d succeeded, %d failed\n", succeeded, failed)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[watch] In sync\n")
	}

	return nil
}
