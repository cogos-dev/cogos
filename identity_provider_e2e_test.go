// identity_provider_e2e_test.go
// End-to-end tests for IdentityProvider closing the deferred integration
// coverage from PR #284 (workspace_root field).
//
// Pattern matches reconcile_e2e_test.go:
//   1. TempDir workspace root
//   2. Write CRD YAML(s) under <root>/.cog/config/identities/
//   3. Instantiate provider directly (not via global registry)
//   4. LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → Health
//   5. Assert state at each step
//
// Fully hermetic: no kernel boot, no HTTP surface, no global state mutation.
//
// What these tests do NOT cover and why:
//   - cog_get_agent_state MCP tool assertions — that surface requires booting
//     the full kernel and a live MCP connection; deferred to the testkernel
//     harness planned in a separate ADR (Gap B per the blind-batch-review).
//   - Real ConstellationDB wiring — stubConstellationDB is Wave 6c scope.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── fixture helpers ─────────────────────────────────────────────────────────

// writeE2EIdentityCRD writes a minimal valid identity YAML with a single
// catch-all expression. Used by the basic workspace-root tests.
func writeE2EIdentityCRD(t *testing.T, root, sub, workspaceRoot string) {
	t.Helper()
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir identities dir: %v", err)
	}
	wrLine := ""
	if workspaceRoot != "" {
		wrLine = fmt.Sprintf("      workspace_root: %q\n", workspaceRoot)
	}
	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: cogos-dev
  sub: %s
  type: agent
  expressions:
    - aud: "*"
      display_name: %q
%s`, sub, sub, sub, wrLine)
	if err := os.WriteFile(filepath.Join(dir, sub+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write identity CRD: %v", err)
	}
}

// writeE2EIdentityCRDMultiExpression writes an identity with multiple
// expressions, each optionally carrying different workspace_root values.
// auds and roots must have equal lengths.
func writeE2EIdentityCRDMultiExpression(t *testing.T, root, sub string, auds, roots []string) {
	t.Helper()
	if len(auds) != len(roots) {
		t.Fatalf("writeE2EIdentityCRDMultiExpression: len(auds)=%d != len(roots)=%d", len(auds), len(roots))
	}
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir identities dir: %v", err)
	}

	var exprs strings.Builder
	for i, aud := range auds {
		fmt.Fprintf(&exprs, "    - aud: %q\n      display_name: %q\n", aud, sub+"-"+aud)
		if roots[i] != "" {
			fmt.Fprintf(&exprs, "      workspace_root: %q\n", roots[i])
		}
	}

	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: cogos-dev
  sub: %s
  type: agent
  expressions:
%s`, sub, sub, exprs.String())
	if err := os.WriteFile(filepath.Join(dir, sub+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write identity CRD: %v", err)
	}
}

// newE2EIdentityProvider builds a provider with a fresh memDB and no-op emit.
func newE2EIdentityProvider(t *testing.T) (*IdentityProvider, *memDB) {
	t.Helper()
	db := newMemDB()
	prov := NewIdentityProvider(db, nil, nil)
	return prov, db
}

// ─── E2E tests ────────────────────────────────────────────────────────────────

// TestIdentityProvider_E2E_WorkspaceRoot closes #284's deferral.
// Runs the full LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState
// → Health cycle for an identity with workspace_root set on its catch-all
// expression. Asserts IdentityProjection.WorkspaceRoot matches at both the DB
// layer (from GetProjection) and the BuildState resource attributes layer.
func TestIdentityProvider_E2E_WorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	const sub = "cog"
	const wantRoot = "cog://workspaces/cog"

	writeE2EIdentityCRD(t, root, sub, wantRoot)

	prov, db := newE2EIdentityProvider(t)

	// 1. LoadConfig
	cfg, err := prov.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	idCfg, ok := cfg.(*identityConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *identityConfig", cfg)
	}
	if len(idCfg.CRDs) != 1 {
		t.Fatalf("LoadConfig: CRDs len = %d, want 1", len(idCfg.CRDs))
	}
	if idCfg.CRDs[0].Spec.Subject != sub {
		t.Errorf("CRDs[0].Spec.Subject = %q, want %q", idCfg.CRDs[0].Spec.Subject, sub)
	}

	// 2. FetchLive (empty DB — no projections yet)
	live, err := prov.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	liveState := live.(*identityLive)
	if len(liveState.Projections) != 0 {
		t.Errorf("FetchLive: expected 0 projections before apply, got %d", len(liveState.Projections))
	}

	// 3. ComputePlan (should show 1 create)
	plan, err := prov.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Fatalf("ComputePlan: Creates = %d, want 1", plan.Summary.Creates)
	}

	// 4. ApplyPlan
	results, err := prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ApplyPlan: results len = %d, want 1", len(results))
	}
	if results[0].Status != ApplySucceeded {
		t.Fatalf("ApplyPlan: Status = %q, want ApplySucceeded (error: %s)", results[0].Status, results[0].Error)
	}

	// Assert: DB projection carries WorkspaceRoot
	proj, err := db.GetProjection(context.Background(), sub)
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("GetProjection: nil projection after apply")
	}
	if proj.WorkspaceRoot != wantRoot {
		t.Errorf("IdentityProjection.WorkspaceRoot = %q, want %q", proj.WorkspaceRoot, wantRoot)
	}

	// Assert: projection cogdoc on disk contains workspace_root value
	cogdocPath := filepath.Join(root, ".cog", "id", sub+".cog.md")
	cogdocData, err := os.ReadFile(cogdocPath)
	if err != nil {
		t.Fatalf("read projection cogdoc: %v", err)
	}
	if !strings.Contains(string(cogdocData), wantRoot) {
		t.Errorf("projection cogdoc missing workspace_root %q\ncontent:\n%s", wantRoot, string(cogdocData))
	}

	// 5. BuildState — verify projection appears in state resources
	live2, err := prov.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (post-apply): %v", err)
	}
	state, err := prov.BuildState(cfg, live2, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if len(state.Resources) != 1 {
		t.Fatalf("BuildState: Resources len = %d, want 1", len(state.Resources))
	}
	res := state.Resources[0]
	if res.Name != sub {
		t.Errorf("BuildState: resource Name = %q, want %q", res.Name, sub)
	}

	// 6. Health — should be Synced + Healthy after a converged cycle
	live3, err := prov.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive (health): %v", err)
	}
	_, err = prov.ComputePlan(cfg, live3, nil)
	if err != nil {
		t.Fatalf("ComputePlan (health): %v", err)
	}
	health := prov.Health()
	if health.Sync != SyncStatusSynced {
		t.Errorf("Health.Sync = %q, want Synced", health.Sync)
	}
	if health.Health != HealthHealthy {
		t.Errorf("Health.Health = %q, want Healthy", health.Health)
	}
}

// TestIdentityProvider_E2E_WorkspaceRootOptional verifies that an identity
// with no workspace_root set reconciles cleanly and produces an empty
// WorkspaceRoot on the projection.
func TestIdentityProvider_E2E_WorkspaceRootOptional(t *testing.T) {
	root := t.TempDir()
	const sub = "sandy"

	// No workspace_root on this identity
	writeE2EIdentityCRD(t, root, sub, "")

	prov, db := newE2EIdentityProvider(t)

	cfg, err := prov.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	live, err := prov.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	plan, err := prov.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 1 {
		t.Fatalf("ComputePlan: Creates = %d, want 1", plan.Summary.Creates)
	}

	results, err := prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("ApplyPlan: %+v, want 1 succeeded", results)
	}

	// No validation error, projection exists, WorkspaceRoot is empty
	proj, err := db.GetProjection(context.Background(), sub)
	if err != nil {
		t.Fatalf("GetProjection: %v", err)
	}
	if proj == nil {
		t.Fatal("GetProjection: nil after apply")
	}
	if proj.WorkspaceRoot != "" {
		t.Errorf("WorkspaceRoot = %q, want empty string for identity without workspace_root", proj.WorkspaceRoot)
	}

	// Health: Synced + Healthy (no schema error for optional absent field)
	live2, _ := prov.FetchLive(context.Background(), cfg)
	_, _ = prov.ComputePlan(cfg, live2, nil)
	h := prov.Health()
	if h.Sync != SyncStatusSynced {
		t.Errorf("Health.Sync = %q, want Synced", h.Sync)
	}
	if h.Health != HealthHealthy {
		t.Errorf("Health.Health = %q, want Healthy (absent workspace_root is not a schema error)", h.Health)
	}
}

// TestIdentityProvider_E2E_MultiExpressionWorkspaceRoot verifies the
// audience-scoped resolution rule: the provider picks the catch-all expression
// (*) for the projection's primary WorkspaceRoot, falling back to the first
// expression when no catch-all is present.
//
// Case A: catch-all (*) present alongside a named audience — primary should
//         pick the catch-all's workspace_root.
// Case B: no catch-all, only named audience expressions — primary picks the
//         first expression's workspace_root.
func TestIdentityProvider_E2E_MultiExpressionWorkspaceRoot(t *testing.T) {
	// Case A: catch-all wins over named audience
	t.Run("CatchAllWins", func(t *testing.T) {
		root := t.TempDir()
		const sub = "cog"
		const catchAllRoot = "cog://workspaces/cog"
		const namedRoot = "cog://workspaces/cog-channel"

		// Expression order: named audience first, catch-all second.
		// The provider's ExpressionFor("*") logic picks the catch-all regardless
		// of position in the slice.
		writeE2EIdentityCRDMultiExpression(t, root, sub,
			[]string{"channel:mod3-main", "*"},
			[]string{namedRoot, catchAllRoot},
		)

		prov, db := newE2EIdentityProvider(t)

		cfg, _ := prov.LoadConfig(root)
		live, _ := prov.FetchLive(context.Background(), cfg)
		plan, err := prov.ComputePlan(cfg, live, nil)
		if err != nil {
			t.Fatalf("ComputePlan: %v", err)
		}
		if plan.Summary.Creates != 1 {
			t.Fatalf("ComputePlan: Creates = %d, want 1", plan.Summary.Creates)
		}

		results, err := prov.ApplyPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("ApplyPlan: %v", err)
		}
		if len(results) != 1 || results[0].Status != ApplySucceeded {
			t.Fatalf("ApplyPlan: %+v, want 1 succeeded", results)
		}

		proj, err := db.GetProjection(context.Background(), sub)
		if err != nil {
			t.Fatalf("GetProjection: %v", err)
		}
		if proj == nil {
			t.Fatal("GetProjection: nil after apply")
		}
		// catch-all expression's workspace_root should be the primary
		if proj.WorkspaceRoot != catchAllRoot {
			t.Errorf("WorkspaceRoot = %q, want catch-all %q (not named-audience %q)",
				proj.WorkspaceRoot, catchAllRoot, namedRoot)
		}
	})

	// Case B: no catch-all — first expression's workspace_root is used
	t.Run("FirstExpressionFallback", func(t *testing.T) {
		root := t.TempDir()
		const sub = "eclipse"
		const firstRoot = "cog://workspaces/eclipse"
		const secondRoot = "cog://workspaces/eclipse-alt"

		// No catch-all expression; two named audiences
		writeE2EIdentityCRDMultiExpression(t, root, sub,
			[]string{"workspace:myrgic", "channel:mod3-main"},
			[]string{firstRoot, secondRoot},
		)

		prov, db := newE2EIdentityProvider(t)

		cfg, _ := prov.LoadConfig(root)
		live, _ := prov.FetchLive(context.Background(), cfg)
		plan, _ := prov.ComputePlan(cfg, live, nil)

		results, err := prov.ApplyPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("ApplyPlan: %v", err)
		}
		if len(results) != 1 || results[0].Status != ApplySucceeded {
			t.Fatalf("ApplyPlan: %+v, want 1 succeeded", results)
		}

		proj, err := db.GetProjection(context.Background(), sub)
		if err != nil {
			t.Fatalf("GetProjection: %v", err)
		}
		if proj == nil {
			t.Fatal("GetProjection: nil after apply")
		}
		// No catch-all → first expression's workspace_root
		if proj.WorkspaceRoot != firstRoot {
			t.Errorf("WorkspaceRoot = %q, want first-expression fallback %q",
				proj.WorkspaceRoot, firstRoot)
		}
	})
}
