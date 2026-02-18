// reconcile_meta_test.go
// Tests for the meta-reconciler.

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// metaTestProvider implements Reconcilable for meta-reconciler testing.
type metaTestProvider struct {
	name       string
	configErr  error
	fetchErr   error
	planResult *ReconcilePlan
	applyErr   error
}

func (m *metaTestProvider) Type() string { return m.name }

func (m *metaTestProvider) LoadConfig(root string) (any, error) {
	if m.configErr != nil {
		return nil, m.configErr
	}
	return map[string]string{"root": root}, nil
}

func (m *metaTestProvider) FetchLive(_ context.Context, _ any) (any, error) {
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	return map[string]string{"live": "true"}, nil
}

func (m *metaTestProvider) ComputePlan(_ any, _ any, _ *ReconcileState) (*ReconcilePlan, error) {
	if m.planResult != nil {
		return m.planResult, nil
	}
	return &ReconcilePlan{
		ResourceType: m.name,
		Summary:      ReconcileSummary{},
	}, nil
}

func (m *metaTestProvider) ApplyPlan(_ context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	if m.applyErr != nil {
		return nil, m.applyErr
	}
	results := make([]ReconcileResult, len(plan.Actions))
	for i, a := range plan.Actions {
		results[i] = ReconcileResult{
			Phase:  "apply",
			Action: string(a.Action),
			Name:   a.Name,
			Status: ApplySucceeded,
		}
	}
	return results, nil
}

func (m *metaTestProvider) BuildState(_ any, _ any, existing *ReconcileState) (*ReconcileState, error) {
	if existing != nil {
		return existing, nil
	}
	return NewReconcileState(m.name), nil
}

func (m *metaTestProvider) Health() ResourceStatus {
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// --- Dependency resolution tests ---

func TestResolveOrderNoDeps(t *testing.T) {
	resources := []MetaResource{
		{Name: "b", Wave: 1},
		{Name: "a", Wave: 0},
		{Name: "c", Wave: 0},
	}

	levels, err := resolveOrder(resources)
	if err != nil {
		t.Fatalf("resolveOrder error: %v", err)
	}

	if len(levels) != 1 {
		t.Fatalf("expected 1 level, got %d", len(levels))
	}

	// Should be sorted by wave, then name
	if levels[0][0].Name != "a" {
		t.Errorf("first = %s, want a (wave 0)", levels[0][0].Name)
	}
	if levels[0][1].Name != "c" {
		t.Errorf("second = %s, want c (wave 0)", levels[0][1].Name)
	}
	if levels[0][2].Name != "b" {
		t.Errorf("third = %s, want b (wave 1)", levels[0][2].Name)
	}
}

func TestResolveOrderWithDeps(t *testing.T) {
	resources := []MetaResource{
		{Name: "discord", DependsOn: []string{"agents"}},
		{Name: "agents"},
	}

	levels, err := resolveOrder(resources)
	if err != nil {
		t.Fatalf("resolveOrder error: %v", err)
	}

	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}

	if levels[0][0].Name != "agents" {
		t.Errorf("level 0 = %s, want agents", levels[0][0].Name)
	}
	if levels[1][0].Name != "discord" {
		t.Errorf("level 1 = %s, want discord", levels[1][0].Name)
	}
}

func TestResolveOrderCycleDetection(t *testing.T) {
	resources := []MetaResource{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}

	_, err := resolveOrder(resources)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want cycle message", err)
	}
}

func TestResolveOrderUnknownDep(t *testing.T) {
	resources := []MetaResource{
		{Name: "a", DependsOn: []string{"nonexistent"}},
	}

	_, err := resolveOrder(resources)
	if err == nil {
		t.Fatal("expected error for unknown dependency")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %v, want mention of nonexistent", err)
	}
}

func TestResolveOrderMultiLevel(t *testing.T) {
	resources := []MetaResource{
		{Name: "c", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "a"},
	}

	levels, err := resolveOrder(resources)
	if err != nil {
		t.Fatalf("resolveOrder error: %v", err)
	}

	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	if levels[0][0].Name != "a" {
		t.Errorf("level 0 = %s, want a", levels[0][0].Name)
	}
	if levels[1][0].Name != "b" {
		t.Errorf("level 1 = %s, want b", levels[1][0].Name)
	}
	if levels[2][0].Name != "c" {
		t.Errorf("level 2 = %s, want c", levels[2][0].Name)
	}
}

// --- Meta-reconciler execution tests ---

func TestRunMetaReconcileAllSynced(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("test-a", &metaTestProvider{name: "test-a"})
	RegisterProvider("test-b", &metaTestProvider{name: "test-b"})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "test-a"},
			{Name: "test-b"},
		},
	}

	tmpDir := t.TempDir()
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{})
	if err != nil {
		t.Fatalf("RunMetaReconcile error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Status != "synced" {
			t.Errorf("resource %s status = %s, want synced", r.Resource, r.Status)
		}
	}
}

func TestRunMetaReconcileSuspended(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("active", &metaTestProvider{name: "active"})
	RegisterProvider("paused", &metaTestProvider{name: "paused"})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "active"},
			{Name: "paused", Suspended: true},
		},
	}

	tmpDir := t.TempDir()
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Find suspended result
	for _, r := range results {
		if r.Resource == "paused" && r.Status != "suspended" {
			t.Errorf("paused status = %s, want suspended", r.Status)
		}
	}
}

func TestRunMetaReconcileResourceFilter(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("target", &metaTestProvider{name: "target"})
	RegisterProvider("other", &metaTestProvider{name: "other"})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "target"},
			{Name: "other"},
		},
	}

	tmpDir := t.TempDir()
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{
		ResourceFilter: "target",
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result (filtered), got %d", len(results))
	}
	if results[0].Resource != "target" {
		t.Errorf("filtered resource = %s, want target", results[0].Resource)
	}
}

func TestRunMetaReconcileDepFailSkips(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("base", &metaTestProvider{
		name:     "base",
		fetchErr: fmt.Errorf("API down"),
	})
	RegisterProvider("dependent", &metaTestProvider{name: "dependent"})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "base"},
			{Name: "dependent", DependsOn: []string{"base"}},
		},
	}

	tmpDir := t.TempDir()
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Base should fail
	if results[0].Resource != "base" || results[0].Status != "failed" {
		t.Errorf("base: resource=%s status=%s, want base/failed", results[0].Resource, results[0].Status)
	}

	// Dependent should be skipped
	if results[1].Resource != "dependent" || results[1].Status != "skipped" {
		t.Errorf("dependent: resource=%s status=%s, want dependent/skipped", results[1].Resource, results[1].Status)
	}
}

func TestRunMetaReconcileDrift(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("drifted", &metaTestProvider{
		name: "drifted",
		planResult: &ReconcilePlan{
			ResourceType: "drifted",
			Actions: []ReconcileAction{
				{Action: ActionCreate, ResourceType: "test", Name: "new-thing"},
			},
			Summary: ReconcileSummary{Creates: 1},
		},
	})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "drifted"},
		},
	}

	tmpDir := t.TempDir()

	// Dry run: should report drifted
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{DryRun: true})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if results[0].Status != "drifted" {
		t.Errorf("dry-run status = %s, want drifted", results[0].Status)
	}
	if results[0].Plan == nil {
		t.Error("expected plan to be present")
	}
}

func TestRunMetaReconcileAutoApply(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("auto", &metaTestProvider{
		name: "auto",
		planResult: &ReconcilePlan{
			ResourceType: "auto",
			Actions: []ReconcileAction{
				{Action: ActionCreate, ResourceType: "test", Name: "new-thing"},
			},
			Summary: ReconcileSummary{Creates: 1},
		},
	})

	cfg := &MetaConfig{
		Resources: []MetaResource{
			{Name: "auto", AutoApply: true},
		},
	}

	tmpDir := t.TempDir()
	results, err := RunMetaReconcile(tmpDir, cfg, MetaReconcileOpts{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if results[0].Status != "applied" {
		t.Errorf("auto-apply status = %s, want applied", results[0].Status)
	}
}

func TestAutoDiscoverResources(t *testing.T) {
	resetProviders()
	defer resetProviders()

	RegisterProvider("alpha", &metaTestProvider{name: "alpha"})
	RegisterProvider("beta", &metaTestProvider{name: "beta"})

	cfg := autoDiscoverResources()
	if len(cfg.Resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(cfg.Resources))
	}

	// Should be sorted (ListProviders returns sorted)
	if cfg.Resources[0].Name != "alpha" {
		t.Errorf("first = %s, want alpha", cfg.Resources[0].Name)
	}
	if cfg.Resources[1].Name != "beta" {
		t.Errorf("second = %s, want beta", cfg.Resources[1].Name)
	}

	// All should have manual interval
	for _, r := range cfg.Resources {
		if r.Interval != "manual" {
			t.Errorf("%s interval = %s, want manual", r.Name, r.Interval)
		}
	}
}

