// Package component provides the Reconcilable provider for workspace
// components.
//
// Part of ADR-060 Phase 1: report-only drift detection.
//
// The provider detects drift between the declared component registry
// (.cog/conf/components.cog.md) and live on-disk state (indexed blobs).
// Phase 1 is report-only — ApplyPlan prints what it would do but does not
// execute destructive actions.
//
// Extracted from apps/cogos root in Wave 1a of ADR-085 (subpackage
// decomposition). The component registry/indexer types and helpers still
// live in the main package; this package reaches them through the DI
// function variables declared below. The main package wires them in an
// init() hook (see apps/cogos/component_wiring.go).
package component

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/myrgic/cogos/internal/workspace"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// --- Dependency-injection seams ----------------------------------------------
//
// These variables are set by the main package in an init() hook so the
// provider can reach the component registry / indexer machinery that still
// lives at the apps/cogos root. If any are nil at call time the provider
// returns an explanatory error rather than panicking.

// RegistryDecl describes a single declared component. Mirrors the fields
// component_provider.go reads from the main-package ComponentDecl type.
type RegistryDecl struct {
	Kind     string
	Required bool
}

// ReconcilerConfig mirrors the reconciler sub-config the provider reads.
type ReconcilerConfig struct {
	PruneUnregistered bool
}

// Registry is the structural view of the component registry the provider
// needs. The concrete main-package ComponentRegistry is converted into this
// shape by LoadRegistry.
type Registry struct {
	Reconciler ReconcilerConfig
	Components map[string]RegistryDecl
}

// Blob is the structural view of a single indexed component blob.
type Blob struct {
	Kind        string
	TreeHash    string
	CommitHash  string
	BlobHash    string
	Dirty       bool
	Language    string
	BuildSystem string
	IndexedAt   string
}

var (
	// LoadRegistry loads and returns the declared component registry.
	LoadRegistry func(root string) (*Registry, error)
	// IndexComponentPaths runs the indexer and returns the list of known
	// component paths (keys of the Merkle index).
	IndexComponentPaths func(root string) ([]string, error)
	// LoadBlob loads a single indexed blob for the given component path.
	LoadBlob func(root, path string) (*Blob, error)
	// EncodePath produces the stable reconcile address fragment for a
	// component path.
	EncodePath func(path string) string
	// NowISO returns the current timestamp in ISO-8601 form.
	NowISO func() string
)

// daemonWorkspaceRoot is injected by SetWorkspaceRoot at daemon boot so
// Health() does not fall back to workspace.ResolveWorkspace() (whose DI seams
// are not wired in cmd/cogos).
var (
	daemonWorkspaceRootMu sync.RWMutex
	daemonWorkspaceRoot   string
)

// SetWorkspaceRoot injects the resolved workspace path into this package so
// that Health() can resolve the component config path without depending on
// workspace.ResolveWorkspace(). Called by engine.SetProvidersWorkspace
// immediately after LoadConfig resolves cfg.WorkspaceRoot.
func SetWorkspaceRoot(root string) {
	daemonWorkspaceRootMu.Lock()
	defer daemonWorkspaceRootMu.Unlock()
	daemonWorkspaceRoot = root
}

// ComponentProvider implements reconcile.Reconcilable for workspace
// component management.
type ComponentProvider struct {
	root string
}

func init() {
	reconcile.RegisterProvider("component", &ComponentProvider{})
}

func (c *ComponentProvider) Type() string { return "component" }

// LoadConfig loads the component registry from .cog/conf/components.cog.md.
func (c *ComponentProvider) LoadConfig(root string) (any, error) {
	c.root = root
	if LoadRegistry == nil {
		return nil, fmt.Errorf("component provider: LoadRegistry dependency not wired")
	}
	return LoadRegistry(root)
}

// FetchLive runs the component indexer and loads individual blobs for
// comparison. Returns map[string]*Blob keyed by component path.
func (c *ComponentProvider) FetchLive(ctx context.Context, config any) (any, error) {
	root := c.root
	if root == "" {
		return nil, fmt.Errorf("component provider: root not set (call LoadConfig first)")
	}
	if IndexComponentPaths == nil || LoadBlob == nil {
		return nil, fmt.Errorf("component provider: indexer dependencies not wired")
	}

	paths, err := IndexComponentPaths(root)
	if err != nil {
		return nil, fmt.Errorf("component index: %w", err)
	}

	blobs := make(map[string]*Blob, len(paths))
	for _, path := range paths {
		blob, err := LoadBlob(root, path)
		if err != nil {
			// Non-fatal: log and skip this component.
			fmt.Fprintf(os.Stderr, "warning: could not load blob for %s: %v\n", path, err)
			continue
		}
		blobs[path] = blob
	}

	return blobs, nil
}

// ComputePlan compares declared config (registry) against live state (blobs)
// and produces a reconciliation plan.
func (c *ComponentProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	reg := config.(*Registry)
	blobs := live.(map[string]*Blob)

	plan := &reconcile.Plan{
		ResourceType: "component",
		GeneratedAt:  nowISO(),
		ConfigPath:   ".cog/conf/components.cog.md",
	}

	// Track which blobs are accounted for by the registry.
	seen := make(map[string]bool)

	// Sort declared paths for deterministic output.
	declaredPaths := make([]string, 0, len(reg.Components))
	for path := range reg.Components {
		declaredPaths = append(declaredPaths, path)
	}
	sort.Strings(declaredPaths)

	for _, path := range declaredPaths {
		decl := reg.Components[path]
		seen[path] = true

		blob, exists := blobs[path]
		if !exists {
			// Declared but not on disk — no blob was produced.
			action := reconcile.Action{
				Action:       reconcile.ActionCreate,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":   "declared component not found on disk",
					"kind":     decl.Kind,
					"required": decl.Required,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Creates++

			if decl.Required {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("required component %q is missing", path))
			}
			continue
		}

		// Blob exists — check for drift.
		drifted := false
		reasons := []string{}

		// Check kind mismatch.
		if decl.Kind != "" && blob.Kind != decl.Kind {
			drifted = true
			reasons = append(reasons, fmt.Sprintf("kind: declared=%s live=%s", decl.Kind, blob.Kind))
		}

		// Check tree hash against previous state (if we have one).
		if state != nil {
			stateIdx := reconcile.ResourceIndex(state)
			addr := "component." + encodePath(path)
			if prev, ok := stateIdx[addr]; ok {
				if prev.ExternalID != "" && blob.TreeHash != prev.ExternalID {
					drifted = true
					reasons = append(reasons, fmt.Sprintf("tree_hash changed: %s -> %s",
						truncHash(prev.ExternalID), truncHash(blob.TreeHash)))
				}
			}
		}

		// Check dirty status.
		if blob.Dirty {
			drifted = true
			reasons = append(reasons, "working tree is dirty")
		}

		if drifted {
			action := reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":    fmt.Sprintf("drift detected: %s", joinReasons(reasons)),
					"tree_hash": blob.TreeHash,
					"dirty":     blob.Dirty,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Updates++
		} else {
			action := reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason":    "in sync",
					"tree_hash": blob.TreeHash,
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Skipped++
		}
	}

	// Check for auto-discovered components not in the registry.
	var undeclaredPaths []string
	for path := range blobs {
		if !seen[path] {
			undeclaredPaths = append(undeclaredPaths, path)
		}
	}
	sort.Strings(undeclaredPaths)

	for _, path := range undeclaredPaths {
		// Add as warnings — prune_unregistered defaults to false.
		if reg.Reconciler.PruneUnregistered {
			action := reconcile.Action{
				Action:       reconcile.ActionDelete,
				ResourceType: "component",
				Name:         path,
				Details: map[string]any{
					"reason": "not in registry and prune_unregistered is true",
				},
			}
			plan.Actions = append(plan.Actions, action)
			plan.Summary.Deletes++
		} else {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("component %q found on disk but not declared in registry", path))
		}
	}

	return plan, nil
}

// ApplyPlan is Phase 1: report-only. Prints what would be done but does not
// execute any destructive actions.
func (c *ComponentProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result
	for _, action := range plan.Actions {
		if action.Action == reconcile.ActionSkip {
			continue
		}
		reason, _ := action.Details["reason"].(string)
		fmt.Printf("  [dry-run] %s %s: %s\n", action.Action, action.Name, reason)
		results = append(results, reconcile.Result{
			Phase:  "component",
			Action: string(action.Action),
			Name:   action.Name,
			Status: reconcile.ApplySkipped,
			Error:  "phase 1: report-only mode",
		})
	}
	return results, nil
}

// BuildState constructs reconcile state from live blobs.
func (c *ComponentProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	blobs := live.(map[string]*Blob)

	state := &reconcile.State{
		Version:      1,
		ResourceType: "component",
		GeneratedAt:  nowISO(),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "component-" + nowISO()
	}

	for path, blob := range blobs {
		resource := reconcile.Resource{
			Address:       "component." + encodePath(path),
			Type:          blob.Kind,
			Mode:          reconcile.ModeManaged,
			ExternalID:    blob.TreeHash,
			Name:          path,
			LastRefreshed: blob.IndexedAt,
			Attributes: map[string]any{
				"commit_hash":  blob.CommitHash,
				"language":     blob.Language,
				"build_system": blob.BuildSystem,
				"dirty":        blob.Dirty,
				"blob_hash":    blob.BlobHash,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	// Sort by address for deterministic output.
	sort.Slice(state.Resources, func(i, j int) bool {
		return state.Resources[i].Address < state.Resources[j].Address
	})

	return state, nil
}

// Health returns the current three-axis status of the component subsystem.
func (c *ComponentProvider) Health() reconcile.ResourceStatus {
	root := c.root
	if root == "" {
		// Prefer the daemon-injected root (set at boot by SetWorkspaceRoot) so
		// the daemon binary does not need workspace.ResolveWorkspace's DI seams.
		daemonWorkspaceRootMu.RLock()
		root = daemonWorkspaceRoot
		daemonWorkspaceRootMu.RUnlock()
	}
	if root == "" {
		var err error
		root, _, err = workspace.ResolveWorkspace()
		if err != nil {
			return reconcile.ResourceStatus{
				Sync: reconcile.SyncStatusUnknown, Health: reconcile.HealthMissing, Operation: reconcile.OperationIdle,
				Message: "workspace not found",
			}
		}
	}

	if LoadRegistry == nil {
		return reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusUnknown, Health: reconcile.HealthDegraded, Operation: reconcile.OperationIdle,
			Message: "component registry loader not wired",
		}
	}

	reg, err := LoadRegistry(root)
	if err != nil {
		return reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusUnknown, Health: reconcile.HealthDegraded, Operation: reconcile.OperationIdle,
			Message: "component registry not found",
		}
	}

	// Check required components exist on disk.
	missing := 0
	for path, decl := range reg.Components {
		if decl.Required {
			absPath := filepath.Join(root, path)
			if _, err := os.Stat(absPath); err != nil {
				missing++
			}
		}
	}

	if missing > 0 {
		return reconcile.ResourceStatus{
			Sync: reconcile.SyncStatusOutOfSync, Health: reconcile.HealthDegraded, Operation: reconcile.OperationIdle,
			Message: fmt.Sprintf("%d required components missing", missing),
		}
	}

	return reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
}

// --- helpers ----------------------------------------------------------------

// nowISO returns the current timestamp, delegating to the main-package
// NowISO if it is wired and falling back to a sentinel otherwise.
func nowISO() string {
	if NowISO != nil {
		return NowISO()
	}
	return ""
}

// encodePath delegates to the main-package EncodePath if wired; otherwise
// returns the path unchanged (safe fallback for the trivial cases).
func encodePath(path string) string {
	if EncodePath != nil {
		return EncodePath(path)
	}
	return path
}

// truncHash shortens a hash for display, keeping the first 12 characters.
func truncHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// joinReasons joins multiple drift reasons into a semicolon-separated string.
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "unknown"
	}
	result := reasons[0]
	for _, r := range reasons[1:] {
		result += "; " + r
	}
	return result
}
