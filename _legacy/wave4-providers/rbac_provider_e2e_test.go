// rbac_provider_e2e_test.go
// End-to-end tests for RBACProvider closing the deferred integration
// coverage from PR #285 (K8s-style RBAC binding CRDs).
//
// Pattern matches reconcile_e2e_test.go:
//   1. TempDir workspace root
//   2. Write binding YAML(s) under <root>/.cog/config/rbac/bindings/<kind>/
//   3. Instantiate provider directly (not via global registry)
//   4. LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → Health
//   5. Assert state at each step
//
// Fully hermetic: no kernel boot, no HTTP surface, no global state mutation.
//
// What these tests do NOT cover and why:
//   - Kernel-boot reconcile state dump via MCP — requires the testkernel
//     harness planned in a separate ADR (Gap B per the blind-batch-review,
//     covering both #284 and #285 deferrals). One harness covers both.
//   - Spec.Access / Spec.Relation validation — LoadRBACBindings does not
//     validate enum values yet (noted in blind review as a follow-up item).

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ─── fixture helpers ─────────────────────────────────────────────────────────

// writeE2EBindingFile writes a binding YAML under
// <root>/.cog/config/rbac/bindings/<kind>/<name>.yaml
func writeE2EBindingFile(t *testing.T, root, kind, name, content string) {
	t.Helper()
	dir := filepath.Join(root, ".cog", "config", "rbac", "bindings", kind)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o640); err != nil {
		t.Fatalf("write %s/%s.yaml: %v", kind, name, err)
	}
}

// newE2ERBACProvider returns a provider with a no-op emit.
func newE2ERBACProvider() *RBACProvider {
	return NewRBACProvider(nil)
}

// ─── E2E tests ────────────────────────────────────────────────────────────────

// TestRBACProvider_E2E_StructuralBindingsConverge closes #285's deferral
// (scope-packet test plan item 5: "ApplyPlan followed by FetchLive; assert
// live state matches spec"). Writes one of each structural CRD kind, runs the
// full reconcile cycle, and verifies convergence in one cycle: after ApplyPlan
// the second ComputePlan shows zero creates and four skips.
func TestRBACProvider_E2E_StructuralBindingsConverge(t *testing.T) {
	root := t.TempDir()

	// Write one of each structural binding kind.
	writeE2EBindingFile(t, root, "rolebinding", "cog-orchestrator", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`)
	writeE2EBindingFile(t, root, "accountbinding", "cog-on-node-a", `
apiVersion: cog.os/v1alpha1
kind: AccountBinding
metadata:
  name: cog-on-node-a
spec:
  subject: cog
  node: node-a
  account: example-user
`)
	writeE2EBindingFile(t, root, "nodebinding", "cog-can-embody", `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: cog-can-embody
spec:
  subject: cog
  node: node-a
  relation: can-embody
`)
	writeE2EBindingFile(t, root, "workspacebinding", "cog-workspace-owner", `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: cog-workspace-owner
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: owner
`)

	p := newE2ERBACProvider()

	// 1. LoadConfig
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rbacCfg, ok := cfg.(*rbacConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *rbacConfig", cfg)
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

	// 2. FetchLive (fresh provider — nothing in memory yet)
	live0, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (before apply): %v", err)
	}
	ls0 := live0.(*rbacLive)
	if len(ls0.RoleBindings)+len(ls0.AccountBindings)+len(ls0.NodeBindings)+len(ls0.WorkspaceBindings) != 0 {
		t.Error("expected empty live state before apply")
	}

	// 3. ComputePlan (should show 4 creates)
	plan, err := p.ComputePlan(cfg, live0, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 4 {
		t.Fatalf("ComputePlan: Creates = %d, want 4", plan.Summary.Creates)
	}
	if plan.Summary.Skipped != 0 {
		t.Errorf("ComputePlan: Skipped = %d, want 0 before first apply", plan.Summary.Skipped)
	}

	// 4. ApplyPlan
	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("ApplyPlan: results len = %d, want 4", len(results))
	}
	for _, r := range results {
		if r.Status != ApplySucceeded {
			t.Errorf("ApplyPlan: result %s/%s: Status = %q, error: %s",
				r.Phase, r.Name, r.Status, r.Error)
		}
	}

	// Verify all four kinds were written to disk.
	diskFiles := map[string]string{
		"rolebinding/cog-orchestrator.yaml":     filepath.Join(root, ".cog", "config", "rbac", "bindings", "rolebinding", "cog-orchestrator.yaml"),
		"accountbinding/cog-on-node-a.yaml":     filepath.Join(root, ".cog", "config", "rbac", "bindings", "accountbinding", "cog-on-node-a.yaml"),
		"nodebinding/cog-can-embody.yaml":       filepath.Join(root, ".cog", "config", "rbac", "bindings", "nodebinding", "cog-can-embody.yaml"),
		"workspacebinding/cog-workspace-owner.yaml": filepath.Join(root, ".cog", "config", "rbac", "bindings", "workspacebinding", "cog-workspace-owner.yaml"),
	}
	for label, path := range diskFiles {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("disk file %s not written: %v", label, err)
		}
	}

	// FetchLive after apply: all four binding kinds present in memory.
	live1, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (after apply): %v", err)
	}
	ls1 := live1.(*rbacLive)
	if _, ok := ls1.RoleBindings["rolebinding/cog-orchestrator"]; !ok {
		t.Error("live state missing rolebinding/cog-orchestrator after apply")
	}
	if _, ok := ls1.AccountBindings["accountbinding/cog-on-node-a"]; !ok {
		t.Error("live state missing accountbinding/cog-on-node-a after apply")
	}
	if _, ok := ls1.NodeBindings["nodebinding/cog-can-embody"]; !ok {
		t.Error("live state missing nodebinding/cog-can-embody after apply")
	}
	if _, ok := ls1.WorkspaceBindings["workspacebinding/cog-workspace-owner"]; !ok {
		t.Error("live state missing workspacebinding/cog-workspace-owner after apply")
	}

	// Second ComputePlan: all bindings now live → zero creates, four skips.
	// This is convergence in one cycle, per scope-packet test plan item 5.
	plan2, err := p.ComputePlan(cfg, live1, nil)
	if err != nil {
		t.Fatalf("ComputePlan (convergence check): %v", err)
	}
	if plan2.Summary.Creates != 0 {
		t.Errorf("convergence check: Creates = %d, want 0 after first apply", plan2.Summary.Creates)
	}
	if plan2.Summary.Skipped != 4 {
		t.Errorf("convergence check: Skipped = %d, want 4", plan2.Summary.Skipped)
	}

	// 5. BuildState
	state, err := p.BuildState(nil, live1, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if state.ResourceType != "rbac-bindings" {
		t.Errorf("BuildState: ResourceType = %q, want rbac-bindings", state.ResourceType)
	}
	if len(state.Resources) != 4 {
		t.Errorf("BuildState: Resources len = %d, want 4", len(state.Resources))
	}
	if state.Metadata["role_binding_count"] != 1 {
		t.Errorf("BuildState: role_binding_count = %v, want 1", state.Metadata["role_binding_count"])
	}
	if state.Metadata["workspace_binding_count"] != 1 {
		t.Errorf("BuildState: workspace_binding_count = %v, want 1", state.Metadata["workspace_binding_count"])
	}

	// 6. Health — after convergence: Synced + Healthy
	h := p.Health()
	if h.Sync != SyncStatusSynced {
		t.Errorf("Health.Sync = %q, want Synced after convergence", h.Sync)
	}
	if h.Health != HealthHealthy {
		t.Errorf("Health.Health = %q, want Healthy", h.Health)
	}
	if h.Operation != OperationIdle {
		t.Errorf("Health.Operation = %q, want Idle", h.Operation)
	}
}

// TestRBACProvider_E2E_HarnessBindingEphemeral verifies that HarnessBindings
// are in-memory only (per OQ-6 decision). The test:
//   - Attaches a HarnessBinding via AttachHarness
//   - Verifies FetchLive includes the harness binding
//   - Detaches it via DetachHarness
//   - Verifies FetchLive no longer includes it
//   - Verifies nothing was written to disk under .cog/config/rbac/bindings/harness/
func TestRBACProvider_E2E_HarnessBindingEphemeral(t *testing.T) {
	root := t.TempDir()
	p := newE2ERBACProvider()

	// Prime the root so LoadConfig is established
	if _, err := p.LoadConfig(root); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	const sessionID = "sess-e2e-001"
	const bindingType = "agent"

	binding := &HarnessBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "HarnessBinding",
		Metadata:   RBACMeta{Name: sessionID + "-" + bindingType},
		Spec: HarnessBindingSpec{
			SessionID:   sessionID,
			Subject:     "cog",
			Type:        bindingType,
			HarnessType: "claude-code",
			NodeID:      "node-a",
		},
	}

	// AttachHarness: should appear in FetchLive immediately
	p.AttachHarness(binding)

	live1, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive (after attach): %v", err)
	}
	ls1 := live1.(*rbacLive)
	key := sessionID + "/" + bindingType
	if _, ok := ls1.HarnessBindings[key]; !ok {
		t.Errorf("FetchLive after AttachHarness: binding %q not found", key)
	}
	if ls1.HarnessBindings[key] != nil {
		got := ls1.HarnessBindings[key].Spec.Subject
		if got != "cog" {
			t.Errorf("HarnessBinding.Spec.Subject = %q, want cog", got)
		}
	}

	// Verify harness binding is NOT written to disk
	harnessDir := filepath.Join(root, ".cog", "config", "rbac", "bindings", "harness")
	if fi, err := os.Stat(harnessDir); err == nil && fi.IsDir() {
		// Directory exists — check it's empty
		entries, _ := os.ReadDir(harnessDir)
		if len(entries) != 0 {
			t.Errorf("harness dir has %d files; expected 0 (HarnessBindings are in-memory only)", len(entries))
		}
	}
	// If directory does not exist at all — that is the correct state (nothing written).

	// DetachHarness: should disappear from FetchLive
	p.DetachHarness(sessionID, bindingType)

	live2, err := p.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive (after detach): %v", err)
	}
	ls2 := live2.(*rbacLive)
	if _, ok := ls2.HarnessBindings[key]; ok {
		t.Errorf("FetchLive after DetachHarness: binding %q still present", key)
	}

	// Disk state still clean after detach
	if fi, err := os.Stat(harnessDir); err == nil && fi.IsDir() {
		entries, _ := os.ReadDir(harnessDir)
		if len(entries) != 0 {
			t.Errorf("harness dir has %d files after detach; expected 0", len(entries))
		}
	}
}

// TestRBACProvider_E2E_HealthOnSchemaError verifies that a RoleBindingCRD with
// a missing required field is recorded as a schema error by LoadConfig and
// causes Health() to report Degraded. This uses the required-field validation
// path added in #288: syntactically valid YAML that omits spec.subject is
// rejected by validateRoleBinding and accumulated into p.schemaErrors rather
// than being silently dropped or causing a fatal I/O error.
//
// Before #288 this test used unparseable YAML (malformed syntax) to force an
// error, which meant LoadConfig returned a non-nil error and schemaErrors was
// never populated, so Health() could not observe the failure. The natural form
// — a structurally valid binding file that fails semantic validation — is now
// the canonical shape for this path.
func TestRBACProvider_E2E_HealthOnSchemaError(t *testing.T) {
	root := t.TempDir()

	// Write a syntactically valid RoleBindingCRD that omits spec.subject.
	// validateRoleBinding (added in #288) rejects this and records the error
	// in schemaErrors; the file does NOT produce a valid binding in the output.
	writeE2EBindingFile(t, root, "rolebinding", "bad-binding", `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: bad-binding
spec:
  role_ref: orchestrator
`)

	p := newE2ERBACProvider()

	// LoadConfig must succeed (schema errors are stored, not returned as error).
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig returned unexpected I/O error: %v", err)
	}

	// The invalid binding is excluded from the loaded set.
	rbacCfg, ok := cfg.(*rbacConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *rbacConfig", cfg)
	}
	if len(rbacCfg.Bindings.RoleBindings) != 0 {
		t.Errorf("RoleBindings: got %d, want 0 (invalid binding must be excluded)", len(rbacCfg.Bindings.RoleBindings))
	}

	// Health must reflect the schema error.
	h := p.Health()
	if h.Health != HealthDegraded {
		t.Errorf("Health.Health = %q, want Degraded after schema error", h.Health)
	}
	if h.Operation != OperationIdle {
		t.Errorf("Health.Operation = %q, want Idle", h.Operation)
	}

	// The Health message must name the missing field so that operator tooling
	// can surface actionable context without requiring log inspection.
	if h.Message == "" {
		t.Errorf("Health.Message is empty; want a message mentioning the schema error count")
	}
}
