// site_provider.go — Reconciliation provider for static-site deployments.
//
// Implements Reconcilable to manage myrgic.* site lifecycle through the standard
// plan/apply/state reconciliation loop. Compares declared SiteCRDs against
// live deployment state and produces create/update/skip/delete actions.
//
// Config layout: <root>/apps/<name>/site.yaml
// Build output:  <root>/apps/<name>/dist/

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// SiteProvider implements Reconcilable for static-site deployment management.
type SiteProvider struct {
	mu         sync.Mutex
	root       string
	strategies map[string]DeployStrategy

	// buildHashFn builds an app and returns the content hash of its dist tree.
	// Injectable so tests can drive ComputePlan's drift logic without running a
	// real build; when nil, ComputePlan falls back to (*SiteProvider).buildAndHash.
	buildHashFn func(ctx context.Context, appDir, name string) (string, error)
}

func defaultStrategies() map[string]DeployStrategy {
	return map[string]DeployStrategy{}
}

func init() {
	RegisterProvider("site", &SiteProvider{strategies: defaultStrategies()})
}

// Type returns the resource type identifier.
func (s *SiteProvider) Type() string { return "site" }

// RegisterStrategy adds or replaces a DeployStrategy implementation.
// Called by strategy init() functions (e.g. GHPagesStrategy) to self-register.
func (s *SiteProvider) RegisterStrategy(name string, strat DeployStrategy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategies[name] = strat
}

// lookupStrategy retrieves a strategy by name, returning ErrUnsupportedStrategy if absent.
func (s *SiteProvider) lookupStrategy(name string) (DeployStrategy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	strat, ok := s.strategies[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedStrategy, name)
	}
	return strat, nil
}

// Health returns the three-axis status of the site subsystem.
func (s *SiteProvider) Health() ResourceStatus {
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	if root == "" {
		var err error
		root, _, err = ResolveWorkspace()
		if err != nil {
			return ResourceStatus{
				Sync:      SyncStatusUnknown,
				Health:    HealthMissing,
				Operation: OperationIdle,
				Message:   "workspace not found",
			}
		}
	}
	_, err := siteLoadCRDs(root)
	if err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthDegraded,
			Operation: OperationIdle,
			Message:   fmt.Sprintf("config load error: %v", err),
		}
	}
	return NewResourceStatus(SyncStatusUnknown, HealthProgressing)
}

// ─── LoadConfig ─────────────────────────────────────────────────────────────────

// LoadConfig loads all site CRD definitions from <root>/apps/*/site.yaml.
func (s *SiteProvider) LoadConfig(root string) (any, error) {
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
	crds, err := siteLoadCRDs(root)
	if err != nil {
		return nil, err
	}
	if crds == nil {
		crds = []SiteCRD{}
	}
	return crds, nil
}

// ─── FetchLive ──────────────────────────────────────────────────────────────────

// FetchLive retrieves deployed state for each site from the strategy's target.
// Per-app errors are logged and the partial map is returned; no fail-fast.
func (s *SiteProvider) FetchLive(ctx context.Context, config any) (any, error) {
	crds, ok := config.([]SiteCRD)
	if !ok {
		return nil, fmt.Errorf("site provider: FetchLive: unexpected config type %T", config)
	}
	live := make(map[string]LiveSiteState, len(crds))
	for _, crd := range crds {
		strat, err := s.lookupStrategy(crd.Spec.Deploy.Strategy)
		if err != nil {
			log.Printf("[site] warning: FetchLive %s: %v", crd.Metadata.Name, err)
			live[crd.Metadata.Name] = LiveSiteState{}
			continue
		}
		state, err := strat.FetchLive(ctx, crd)
		if err != nil {
			log.Printf("[site] warning: FetchLive %s: %v", crd.Metadata.Name, err)
			live[crd.Metadata.Name] = LiveSiteState{}
			continue
		}
		live[crd.Metadata.Name] = state
	}
	return live, nil
}

// ─── ComputePlan ────────────────────────────────────────────────────────────────

// ComputePlan diffs declared SiteCRDs against live deployment state.
// Drift detection: ResolvedFromTarget=false → create; otherwise build the app
// and hash its content, comparing against the hash recorded at the live target
// (.artifact-sha). Empty live hash or a mismatch → update; equal → skip.
func (s *SiteProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	crds, ok := config.([]SiteCRD)
	if !ok {
		return nil, fmt.Errorf("site provider: ComputePlan: unexpected config type %T", config)
	}
	liveMap, ok := live.(map[string]LiveSiteState)
	if !ok {
		return nil, fmt.Errorf("site provider: ComputePlan: unexpected live type %T", live)
	}

	s.mu.Lock()
	root := s.root
	s.mu.Unlock()

	buildHash := s.buildHashFn
	if buildHash == nil {
		buildHash = s.buildAndHash
	}

	plan := &ReconcilePlan{
		ResourceType: "site",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   root + "/apps",
	}

	// Track which CRD names appear in config
	declared := make(map[string]bool, len(crds))
	for _, crd := range crds {
		name := crd.Metadata.Name
		declared[name] = true
		appDir := root + "/apps/" + name

		liveState := liveMap[name]
		details := map[string]any{
			"domain":   crd.Spec.Domain,
			"strategy": crd.Spec.Deploy.Strategy,
			"target":   crd.Spec.Deploy.Target,
			"app_dir":  appDir,
		}

		// Warn if strategy is not registered (FetchLive will have logged it too)
		if _, err := s.lookupStrategy(crd.Spec.Deploy.Strategy); err != nil {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s: strategy %q not registered — deploy will fail", name, crd.Spec.Deploy.Strategy))
		}

		if !liveState.ResolvedFromTarget {
			// Not yet deployed — create
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "site",
				Name:         name,
				Details:      details,
			})
			plan.Summary.Creates++
			continue
		}

		// Drift detection: build the artifact and hash its content, then compare
		// against the hash recorded at the live target (.artifact-sha). A build
		// or hash failure is surfaced as an update (safer to attempt a deploy
		// than to silently skip a possibly-stale target).
		//
		// NOTE: this builds the app to hash it, duplicating the build ApplyPlan
		// performs when the update is applied. Acceptable for now — plan and
		// apply run the same build.sh, so the extra build is cheap and keeps the
		// diff honest (an empty stub check could never detect content changes).
		localSHA, buildErr := buildHash(context.Background(), appDir, name)
		if buildErr != nil {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s: drift build failed, assuming update: %v", name, buildErr))
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "site",
				Name:         name,
				Details:      details,
			})
			plan.Summary.Updates++
			continue
		}
		details["artifact_sha"] = localSHA

		if liveState.ArtifactSHA == "" || liveState.ArtifactSHA != localSHA {
			// No recorded content hash at the target, or the content changed.
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "site",
				Name:         name,
				Details:      details,
			})
			plan.Summary.Updates++
			continue
		}

		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionSkip,
			ResourceType: "site",
			Name:         name,
			Details:      details,
		})
		plan.Summary.Skipped++
	}

	// Orphans: managed in state but no longer in config
	if state != nil {
		for _, res := range state.Resources {
			if !declared[res.Name] {
				plan.Actions = append(plan.Actions, ReconcileAction{
					Action:       ActionDelete,
					ResourceType: "site",
					Name:         res.Name,
					Details: map[string]any{
						"reason": "no matching site CRD",
					},
				})
				plan.Summary.Deletes++
			}
		}
	}

	return plan, nil
}

// ─── ApplyPlan ──────────────────────────────────────────────────────────────────

// ApplyPlan builds and deploys each site action.
func (s *SiteProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}
		result := s.applyAction(ctx, action)
		results = append(results, result)
	}
	return results, nil
}

func (s *SiteProvider) applyAction(ctx context.Context, action ReconcileAction) ReconcileResult {
	name := action.Name
	base := ReconcileResult{
		Phase:  "site",
		Action: string(action.Action),
		Name:   name,
	}

	if action.Action == ActionDelete {
		// v0.0.1: delete is a no-op — manual teardown required for deployed sites
		log.Printf("[site] delete %s: no-op in v0.0.1 (manual teardown required)", name)
		base.Status = ApplySucceeded
		return base
	}

	appDir, _ := action.Details["app_dir"].(string)
	if appDir == "" {
		base.Status = ApplyFailed
		base.Error = "no app_dir in action details"
		return base
	}

	// Step 1: build the artifact
	if err := siteBuild(ctx, appDir, name); err != nil {
		base.Status = ApplyFailed
		base.Error = fmt.Sprintf("build: %v", err)
		return base
	}

	// Step 2: deploy via strategy
	stratName, _ := action.Details["strategy"].(string)
	strat, err := s.lookupStrategy(stratName)
	if err != nil {
		base.Status = ApplyFailed
		base.Error = fmt.Sprintf("strategy lookup: %v", err)
		return base
	}

	s.mu.Lock()
	root := s.root
	s.mu.Unlock()

	crds, err := siteLoadCRDs(root)
	if err != nil {
		base.Status = ApplyFailed
		base.Error = fmt.Sprintf("reload config: %v", err)
		return base
	}
	crd, found := siteCRDByName(crds, name)
	if !found {
		base.Status = ApplyFailed
		base.Error = fmt.Sprintf("CRD not found after reload: %s", name)
		return base
	}

	distDir := appDir + "/dist"
	if err := strat.Deploy(ctx, crd, distDir); err != nil {
		base.Status = ApplyFailed
		base.Error = fmt.Sprintf("deploy: %v", err)
		return base
	}

	base.Status = ApplySucceeded
	base.CreatedID = "site:" + name
	return base
}

// ─── BuildState ─────────────────────────────────────────────────────────────────

// BuildState constructs reconcile state from live site data.
func (s *SiteProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveMap, ok := live.(map[string]LiveSiteState)
	if !ok {
		return nil, fmt.Errorf("site provider: BuildState: unexpected live type %T", live)
	}

	state := &ReconcileState{
		Version:      1,
		ResourceType: "site",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "site-" + time.Now().UTC().Format("20060102T150405Z")
	}

	for name, liveState := range liveMap {
		resource := ReconcileResource{
			Address:       "site." + name,
			Type:          "site",
			Mode:          ModeManaged,
			ExternalID:    liveState.ArtifactSHA,
			Name:          name,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
			Attributes: map[string]any{
				"artifact_sha":         liveState.ArtifactSHA,
				"cname_content":        liveState.CNAMEContent,
				"resolved_from_target": liveState.ResolvedFromTarget,
			},
		}
		state.Resources = append(state.Resources, resource)
	}

	return state, nil
}
