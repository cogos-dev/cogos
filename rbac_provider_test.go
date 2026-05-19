// rbac_provider_test.go
// Tests for RBACProvider: LoadConfig, ComputePlan, ApplyPlan+FetchLive, Health.

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// newTestRBACProvider builds a provider with a no-op emit for testing.
func newTestRBACProvider() *RBACProvider {
	return NewRBACProvider(nil) // nil emit → no-op
}

// writeFixtureBinding writes a binding YAML to a temp directory.
func writeFixtureBinding(t *testing.T, root, kind, name, content string) {
	t.Helper()
	dir := filepath.Join(root, ".cog", "config", "rbac", "bindings", kind)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ─── LoadConfig ───────────────────────────────────────────────────────────────

func TestRBACProvider_LoadConfig_DirectoryScan(t *testing.T) {
	root := t.TempDir()

	writeFixtureBinding(t, root, "rolebinding", "cog-orchestrator.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)
	writeFixtureBinding(t, root, "accountbinding", "cog-on-darkstar.yaml", `
apiVersion: cog.os/v1alpha1
kind: AccountBinding
metadata:
  name: cog-on-darkstar
spec:
  subject: cog
  node: darkstar
  account: slowbro
`)
	writeFixtureBinding(t, root, "nodebinding", "cog-can-embody.yaml", `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: cog-can-embody
spec:
  subject: cog
  node: darkstar
  relation: can-embody
`)
	writeFixtureBinding(t, root, "workspacebinding", "cog-workspace-owner.yaml", `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: cog-workspace-owner
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: owner
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rbacCfg, ok := cfg.(*rbacConfig)
	if !ok {
		t.Fatalf("expected *rbacConfig, got %T", cfg)
	}

	if len(rbacCfg.Bindings.RoleBindings) != 1 {
		t.Errorf("RoleBindings: got %d, want 1", len(rbacCfg.Bindings.RoleBindings))
	}
	if len(rbacCfg.Bindings.AccountBindings) != 1 {
		t.Errorf("AccountBindings: got %d, want 1", len(rbacCfg.Bindings.AccountBindings))
	}
	if len(rbacCfg.Bindings.NodeBindings) != 1 {
		t.Errorf("NodeBindings: got %d, want 1", len(rbacCfg.Bindings.NodeBindings))
	}
	if len(rbacCfg.Bindings.WorkspaceBindings) != 1 {
		t.Errorf("WorkspaceBindings: got %d, want 1", len(rbacCfg.Bindings.WorkspaceBindings))
	}
}

func TestRBACProvider_LoadConfig_EmptyWorkspace(t *testing.T) {
	root := t.TempDir()
	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig on empty workspace: %v", err)
	}
	rbacCfg := cfg.(*rbacConfig)
	if len(rbacCfg.Bindings.RoleBindings) != 0 {
		t.Errorf("expected 0 RoleBindings, got %d", len(rbacCfg.Bindings.RoleBindings))
	}
}

// ─── ComputePlan ─────────────────────────────────────────────────────────────

func TestRBACProvider_ComputePlan_EmptyLive(t *testing.T) {
	// With nothing in live state, all spec bindings become creates.
	root := t.TempDir()

	writeFixtureBinding(t, root, "rolebinding", "cog-orchestrator.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	live := newRBACLive() // empty live state
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	if plan.Summary.Creates != 1 {
		t.Errorf("Creates: got %d, want 1", plan.Summary.Creates)
	}
	if plan.Summary.Skipped != 0 {
		t.Errorf("Skipped: got %d, want 0", plan.Summary.Skipped)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("Actions: got %d, want 1", len(plan.Actions))
	}
	if plan.Actions[0].Action != ActionCreate {
		t.Errorf("Action: got %q, want %q", plan.Actions[0].Action, ActionCreate)
	}
	if plan.Actions[0].ResourceType != "rolebinding" {
		t.Errorf("ResourceType: got %q, want %q", plan.Actions[0].ResourceType, "rolebinding")
	}
}

func TestRBACProvider_ComputePlan_AlreadySynced(t *testing.T) {
	// When the live state already has a binding matching the spec, it becomes a skip.
	root := t.TempDir()

	writeFixtureBinding(t, root, "rolebinding", "cog-orchestrator.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Pre-populate live state with the same binding.
	live := newRBACLive()
	live.RoleBindings["rolebinding/cog-orchestrator"] = &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: "cog-orchestrator"},
		Spec:       RoleBindingSpec{Subject: "cog", RoleRef: "orchestrator"},
	}

	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 0 {
		t.Errorf("Creates: got %d, want 0", plan.Summary.Creates)
	}
	if plan.Summary.Skipped != 1 {
		t.Errorf("Skipped: got %d, want 1", plan.Summary.Skipped)
	}
}

func TestRBACProvider_ComputePlan_MultipleKinds(t *testing.T) {
	// All four structural binding kinds generate creates against empty live.
	root := t.TempDir()

	writeFixtureBinding(t, root, "rolebinding", "rb.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: rb
spec:
  subject: cog
  role_ref: orchestrator
`)
	writeFixtureBinding(t, root, "accountbinding", "ab.yaml", `
apiVersion: cog.os/v1alpha1
kind: AccountBinding
metadata:
  name: ab
spec:
  subject: cog
  node: darkstar
  account: slowbro
`)
	writeFixtureBinding(t, root, "nodebinding", "nb.yaml", `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: nb
spec:
  subject: cog
  node: darkstar
  relation: can-embody
`)
	writeFixtureBinding(t, root, "workspacebinding", "wb.yaml", `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: wb
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: owner
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan, err := p.ComputePlan(cfg, newRBACLive(), nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 4 {
		t.Errorf("Creates: got %d, want 4", plan.Summary.Creates)
	}
}

// ─── ApplyPlan + FetchLive convergence ───────────────────────────────────────

func TestRBACProvider_ApplyThenFetchLive_Convergence(t *testing.T) {
	// Apply a create action and verify FetchLive returns the new binding.
	// This is the scope-packet test plan item 5:
	// "ApplyPlan followed by FetchLive; assert live state matches spec"
	root := t.TempDir()

	writeFixtureBinding(t, root, "rolebinding", "cog-orchestrator.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	live0, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (before apply): %v", err)
	}

	plan, err := p.ComputePlan(cfg, live0, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Fatalf("expected 1 create before apply, got %d", plan.Summary.Creates)
	}

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if results[0].Status != ApplySucceeded {
		t.Errorf("result status: got %q, want %q (err: %s)", results[0].Status, ApplySucceeded, results[0].Error)
	}

	// Verify disk write.
	diskPath := filepath.Join(root, ".cog", "config", "rbac", "bindings", "rolebinding", "cog-orchestrator.yaml")
	if _, err := os.Stat(diskPath); err != nil {
		t.Errorf("binding not written to disk: %v", err)
	}

	// FetchLive should now reflect the applied binding.
	live1, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (after apply): %v", err)
	}
	liveState := live1.(*rbacLive)
	if _, ok := liveState.RoleBindings["rolebinding/cog-orchestrator"]; !ok {
		t.Errorf("expected rolebinding/cog-orchestrator in live state after apply")
	}

	// Second ComputePlan should show 0 creates (converged).
	plan2, err := p.ComputePlan(cfg, live1, nil)
	if err != nil {
		t.Fatalf("ComputePlan (post-apply): %v", err)
	}
	if plan2.Summary.Creates != 0 {
		t.Errorf("Creates after convergence: got %d, want 0", plan2.Summary.Creates)
	}
	if plan2.Summary.Skipped != 1 {
		t.Errorf("Skipped after convergence: got %d, want 1", plan2.Summary.Skipped)
	}
}

// ─── AttachHarness + DetachHarness ───────────────────────────────────────────

func TestRBACProvider_AttachDetachHarness(t *testing.T) {
	p := newTestRBACProvider()

	binding := &HarnessBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "HarnessBinding",
		Metadata:   RBACMeta{Name: "sess-001-agent"},
		Spec: HarnessBindingSpec{
			SessionID:   "sess-001",
			Subject:     "cog",
			Type:        "agent",
			HarnessType: "claude-code",
		},
	}

	p.AttachHarness(binding)

	live, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	liveState := live.(*rbacLive)
	if _, ok := liveState.HarnessBindings["sess-001/agent"]; !ok {
		t.Errorf("harness binding not found after attach")
	}

	p.DetachHarness("sess-001", "agent")

	live2, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive after detach: %v", err)
	}
	liveState2 := live2.(*rbacLive)
	if _, ok := liveState2.HarnessBindings["sess-001/agent"]; ok {
		t.Errorf("harness binding still present after detach")
	}
}

// ─── Health ───────────────────────────────────────────────────────────────────

func TestRBACProvider_Health_InitiallyHealthy(t *testing.T) {
	// Before any reconcile cycle, Health should return Healthy + Synced
	// (no plan data yet, no schema errors).
	p := newTestRBACProvider()
	status := p.Health()
	if status.Health != HealthHealthy {
		t.Errorf("Health: got %q, want %q", status.Health, HealthHealthy)
	}
	if status.Sync != SyncStatusSynced {
		t.Errorf("Sync: got %q, want %q (before any plan, summary zero = synced)", status.Sync, SyncStatusSynced)
	}
	if status.Operation != OperationIdle {
		t.Errorf("Operation: got %q, want %q", status.Operation, OperationIdle)
	}
}

func TestRBACProvider_Health_OutOfSync(t *testing.T) {
	// After a plan with creates, Health should return OutOfSync.
	root := t.TempDir()
	writeFixtureBinding(t, root, "rolebinding", "rb.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: rb
spec:
  subject: cog
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	_, err = p.ComputePlan(cfg, newRBACLive(), nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	status := p.Health()
	if status.Sync != SyncStatusOutOfSync {
		t.Errorf("Sync: got %q, want OutOfSync after plan with creates", status.Sync)
	}
	if status.Health != HealthHealthy {
		t.Errorf("Health: got %q, want Healthy (no schema errors)", status.Health)
	}
}

func TestRBACProvider_Health_SyncedAfterApply(t *testing.T) {
	// After a full apply+fetch cycle, next ComputePlan shows 0 changes → Synced.
	root := t.TempDir()
	writeFixtureBinding(t, root, "rolebinding", "rb.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: rb
spec:
  subject: cog
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, _ := p.LoadConfig(root)
	plan, _ := p.ComputePlan(cfg, newRBACLive(), nil)
	_, _ = p.ApplyPlan(context.Background(), plan)
	live, _ := p.FetchLive(context.Background(), cfg)
	_, _ = p.ComputePlan(cfg, live, nil)

	status := p.Health()
	if status.Sync != SyncStatusSynced {
		t.Errorf("Sync: got %q, want Synced after converge", status.Sync)
	}
}

// ─── BuildState ──────────────────────────────────────────────────────────────

func TestRBACProvider_BuildState(t *testing.T) {
	p := newTestRBACProvider()

	live := newRBACLive()
	live.RoleBindings["rolebinding/rb"] = &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: "rb"},
		Spec:       RoleBindingSpec{Subject: "cog", RoleRef: "orchestrator"},
	}
	live.HarnessBindings["sess-001/agent"] = &HarnessBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "HarnessBinding",
		Metadata:   RBACMeta{Name: "sess-001-agent"},
		Spec:       HarnessBindingSpec{SessionID: "sess-001", Subject: "cog", Type: "agent", HarnessType: "claude-code"},
	}

	state, err := p.BuildState(nil, live, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != "rbac-bindings" {
		t.Errorf("ResourceType: got %q", state.ResourceType)
	}
	if len(state.Resources) != 2 {
		t.Errorf("Resources: got %d, want 2 (1 rolebinding + 1 harnessbinding)", len(state.Resources))
	}
	if state.Metadata["role_binding_count"] != 1 {
		t.Errorf("role_binding_count: got %v", state.Metadata["role_binding_count"])
	}
	if state.Metadata["harness_binding_count"] != 1 {
		t.Errorf("harness_binding_count: got %v", state.Metadata["harness_binding_count"])
	}
}

func TestRBACProvider_BuildState_SerialIncrement(t *testing.T) {
	p := newTestRBACProvider()
	live := newRBACLive()

	state1, err := p.BuildState(nil, live, nil)
	if err != nil {
		t.Fatalf("BuildState (first): %v", err)
	}
	if state1.Serial != 1 {
		t.Errorf("first Serial: got %d, want 1", state1.Serial)
	}

	state2, err := p.BuildState(nil, live, state1)
	if err != nil {
		t.Fatalf("BuildState (second): %v", err)
	}
	if state2.Serial != 2 {
		t.Errorf("second Serial: got %d, want 2", state2.Serial)
	}
	if state2.Lineage != state1.Lineage {
		t.Errorf("Lineage should be preserved across increments")
	}
}

// ─── schemaErrors wiring + Health.Degraded ───────────────────────────────────

// TestRBACProvider_LoadConfig_SchemaErrorsStored verifies that LoadConfig
// populates p.schemaErrors when a binding file has a missing required field,
// and that the valid bindings in the same directory are still loaded.
func TestRBACProvider_LoadConfig_SchemaErrorsStored(t *testing.T) {
	root := t.TempDir()

	// One valid binding.
	writeFixtureBinding(t, root, "rolebinding", "valid.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)
	// One invalid binding: subject missing.
	writeFixtureBinding(t, root, "rolebinding", "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: missing-subject
spec:
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig returned fatal error: %v", err)
	}

	// Valid bindings still present in returned config.
	rbacCfg, ok := cfg.(*rbacConfig)
	if !ok {
		t.Fatalf("expected *rbacConfig, got %T", cfg)
	}
	if len(rbacCfg.Bindings.RoleBindings) != 1 {
		t.Errorf("RoleBindings: got %d, want 1 (only valid binding)", len(rbacCfg.Bindings.RoleBindings))
	}

	// schemaErrors populated.
	p.mu.Lock()
	gotErrs := len(p.schemaErrors)
	p.mu.Unlock()
	if gotErrs != 1 {
		t.Errorf("schemaErrors: got %d, want 1", gotErrs)
	}
}

// TestRBACProvider_Health_DegradedOnSchemaError verifies that Health() reports
// Degraded when LoadConfig stored at least one schema error. This closes the
// dead-code path for p.schemaErrors that was identified in the PR #285 review.
func TestRBACProvider_Health_DegradedOnSchemaError(t *testing.T) {
	root := t.TempDir()

	// Write a binding with a missing required field.
	writeFixtureBinding(t, root, "rolebinding", "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: missing-subject
spec:
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	_, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig returned fatal error: %v", err)
	}

	h := p.Health()
	if h.Health != HealthDegraded {
		t.Errorf("Health.Health = %q, want Degraded after schema error", h.Health)
	}
	if h.Message == "" {
		t.Error("Health.Message should be non-empty when Degraded")
	}
}

// TestRBACProvider_Health_HealthyAfterCleanReload verifies that a subsequent
// LoadConfig with valid files clears the schema errors and restores Healthy.
// This confirms per-call accumulation semantics: each LoadConfig replaces
// schemaErrors rather than appending cumulatively.
func TestRBACProvider_Health_HealthyAfterCleanReload(t *testing.T) {
	root := t.TempDir()

	// First load: bad binding → Degraded.
	writeFixtureBinding(t, root, "rolebinding", "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: missing-subject
spec:
  role_ref: orchestrator
`)

	p := newTestRBACProvider()
	if _, err := p.LoadConfig(root); err != nil {
		t.Fatalf("first LoadConfig: %v", err)
	}
	if p.Health().Health != HealthDegraded {
		t.Fatal("expected Degraded after first load with bad binding")
	}

	// Fix the bad binding in place.
	writeFixtureBinding(t, root, "rolebinding", "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: now-valid
spec:
  subject: cog
  role_ref: orchestrator
`)

	// Second load: all bindings valid → Healthy.
	if _, err := p.LoadConfig(root); err != nil {
		t.Fatalf("second LoadConfig: %v", err)
	}
	h := p.Health()
	if h.Health != HealthHealthy {
		t.Errorf("Health.Health = %q, want Healthy after clean reload", h.Health)
	}
}
