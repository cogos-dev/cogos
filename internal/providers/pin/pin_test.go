package pin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/providers/pin"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── Test helpers ────────────────────────────────────────────────────────────

// setupWorkspace creates a temporary workspace directory with a .cog/pins/
// sub-directory and returns the workspace root.
func setupWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "pins"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return root
}

// writePinYAML writes a raw YAML string to <root>/.cog/pins/<name>.yaml.
func writePinYAML(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, ".cog", "pins", name+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writePinYAML %s: %v", name, err)
	}
}

// stubResolver satisfies GitHeadResolver for tests.
type stubResolver struct {
	// refs maps target → (ref, digest, err).
	refs map[string]struct {
		ref    string
		digest string
		err    error
	}
}

func (r *stubResolver) ResolveHead(_ context.Context, target, _ string) (string, string, error) {
	if r.refs == nil {
		return "", "", nil
	}
	entry, ok := r.refs[target]
	if !ok {
		return "", "", nil // unreachable but no error signal
	}
	return entry.ref, entry.digest, entry.err
}

// newStub constructs a stubResolver from a simple map[target]ref.
func newStub(refs map[string]string) *stubResolver {
	s := &stubResolver{refs: make(map[string]struct {
		ref    string
		digest string
		err    error
	}, len(refs))}
	for target, ref := range refs {
		s.refs[target] = struct {
			ref    string
			digest string
			err    error
		}{ref: ref}
	}
	return s
}

// newStubWithErr builds a resolver where some targets return errors.
func newStubWithErr(target string, err error) *stubResolver {
	s := &stubResolver{refs: make(map[string]struct {
		ref    string
		digest string
		err    error
	})}
	s.refs[target] = struct {
		ref    string
		digest string
		err    error
	}{err: err}
	return s
}

// ─── Tests: LoadConfig ───────────────────────────────────────────────────────

func TestLoadConfig_NoPinsDir(t *testing.T) {
	root := t.TempDir() // no .cog/pins/ dir
	p := pin.New(nil)
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig with no pins dir: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestLoadConfig_EmptyPinsDir(t *testing.T) {
	root := setupWorkspace(t)
	p := pin.New(nil)
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig empty: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
}

func TestLoadConfig_SingleRecord(t *testing.T) {
	root := setupWorkspace(t)
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: v0.4.1
branch: main
sync: read-only
updated: 2026-05-01T00:00:00Z
`)
	p := pin.New(nil)
	_, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig single: %v", err)
	}
}

func TestLoadConfig_TwoSourcesIndependent(t *testing.T) {
	// Two source workspaces pin the same target at different refs.
	// This test proves: pins are source-side, not target-side.
	root1 := setupWorkspace(t)
	root2 := setupWorkspace(t)

	const target = "myrgic/cogos"

	writePinYAML(t, root1, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: v0.4.0
sync: read-only
`)
	writePinYAML(t, root2, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: v0.5.0
sync: read-only
`)

	p1 := pin.New(newStub(map[string]string{target: "v0.4.0"}))
	p2 := pin.New(newStub(map[string]string{target: "v0.5.0"}))

	cfg1, err := p1.LoadConfig(root1)
	if err != nil {
		t.Fatalf("root1 LoadConfig: %v", err)
	}
	cfg2, err := p2.LoadConfig(root2)
	if err != nil {
		t.Fatalf("root2 LoadConfig: %v", err)
	}

	live1, err := p1.FetchLive(context.Background(), cfg1)
	if err != nil {
		t.Fatalf("root1 FetchLive: %v", err)
	}
	live2, err := p2.FetchLive(context.Background(), cfg2)
	if err != nil {
		t.Fatalf("root2 FetchLive: %v", err)
	}

	plan1, err := p1.ComputePlan(cfg1, live1, nil)
	if err != nil {
		t.Fatalf("root1 ComputePlan: %v", err)
	}
	plan2, err := p2.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("root2 ComputePlan: %v", err)
	}

	// Both plans should contain skip actions (pinned == live for each).
	for _, action := range plan1.Actions {
		if action.Name == target && action.Action != reconcile.ActionSkip {
			t.Errorf("root1: expected skip for %s, got %s", target, action.Action)
		}
	}
	for _, action := range plan2.Actions {
		if action.Name == target && action.Action != reconcile.ActionSkip {
			t.Errorf("root2: expected skip for %s, got %s", target, action.Action)
		}
	}

	// Verify the two providers are independent — health is Healthy for both.
	h1 := p1.Health()
	h2 := p2.Health()
	if h1.Health != reconcile.HealthHealthy {
		t.Errorf("root1 health: want Healthy, got %s: %s", h1.Health, h1.Message)
	}
	if h2.Health != reconcile.HealthHealthy {
		t.Errorf("root2 health: want Healthy, got %s: %s", h2.Health, h2.Message)
	}
}

// ─── Tests: Reconcile — pinned ref matches live ───────────────────────────────

func TestReconcile_InSync_Green(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	const ref = "abc1234567890"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
sync: read-only
`)

	p := pin.New(newStub(map[string]string{target: ref}))
	runAndCheck(t, p, root, func(h reconcile.ResourceStatus) {
		if h.Health != reconcile.HealthHealthy {
			t.Errorf("in-sync: want Healthy, got %s: %s", h.Health, h.Message)
		}
		if h.Sync != reconcile.SyncStatusSynced {
			t.Errorf("in-sync: want Synced, got %s", h.Sync)
		}
	})
}

// ─── Tests: Reconcile — pinned ref behind live ────────────────────────────────

func TestReconcile_Drift_Yellow(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc000000000
sync: read-only
`)

	p := pin.New(newStub(map[string]string{target: "def111111111"}))
	runAndCheck(t, p, root, func(h reconcile.ResourceStatus) {
		if h.Health != reconcile.HealthDegraded {
			t.Errorf("drift: want Degraded, got %s: %s", h.Health, h.Message)
		}
		if h.Sync != reconcile.SyncStatusOutOfSync {
			t.Errorf("drift: want OutOfSync, got %s", h.Sync)
		}
	})
}

// ─── Tests: Reconcile — target unreachable ────────────────────────────────────

func TestReconcile_TargetUnreachable_Red(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc000000000
sync: read-only
`)

	p := pin.New(newStubWithErr(target, os.ErrNotExist))
	runAndCheck(t, p, root, func(h reconcile.ResourceStatus) {
		if h.Health != reconcile.HealthDegraded {
			t.Errorf("unreachable: want Degraded, got %s: %s", h.Health, h.Message)
		}
		if h.Sync != reconcile.SyncStatusOutOfSync {
			t.Errorf("unreachable: want OutOfSync, got %s", h.Sync)
		}
	})
}

// ─── Tests: Reconcile — digest mismatch ──────────────────────────────────────

func TestReconcile_DigestMismatch_Red(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
  digest: sha256:aaaa
sync: read-only
`)

	// Stub returns same ref but different digest.
	s := &stubResolver{refs: map[string]struct {
		ref    string
		digest string
		err    error
	}{
		target: {ref: "abc1234567890", digest: "sha256:bbbb"},
	}}

	p := pin.New(s)
	runAndCheck(t, p, root, func(h reconcile.ResourceStatus) {
		if h.Health != reconcile.HealthDegraded {
			t.Errorf("digest mismatch: want Degraded, got %s: %s", h.Health, h.Message)
		}
		if h.Sync != reconcile.SyncStatusOutOfSync {
			t.Errorf("digest mismatch: want OutOfSync, got %s", h.Sync)
		}
	})
}

// ─── Tests: ApplyPlan — read-only rejects write ───────────────────────────────

func TestApplyPlan_ReadOnly_Rejects(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: old000000000
sync: read-only
`)

	p := pin.New(newStub(map[string]string{target: "new111111111"}))
	cfg, _ := p.LoadConfig(root)
	live, _ := p.FetchLive(context.Background(), cfg)
	plan, _ := p.ComputePlan(cfg, live, nil)

	results, err := p.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	for _, r := range results {
		if r.Status == reconcile.ApplySucceeded {
			t.Errorf("read-only: ApplyPlan should not succeed for %s", r.Name)
		}
	}
}

// ─── Tests: Health before LoadConfig ─────────────────────────────────────────

func TestHealth_BeforeLoadConfig_Missing(t *testing.T) {
	p := pin.New(nil)
	h := p.Health()
	if h.Health != reconcile.HealthMissing {
		t.Errorf("pre-config health: want Missing, got %s: %s", h.Health, h.Message)
	}
}

// ─── Tests: WritePinRecord / RemovePinRecord ──────────────────────────────────

func TestWriteAndRemovePinRecord(t *testing.T) {
	root := t.TempDir() // no pins dir yet
	rec := &pin.PinRecord{
		Target: "myrgic/cogos",
		Pin:    pin.PinRef{Ref: "v0.5.0"},
		Branch: "main",
		Sync:   pin.SyncReadOnly,
		Updated: time.Now().UTC(),
	}

	// Write creates the dir and file.
	if err := pin.WritePinRecord(root, rec); err != nil {
		t.Fatalf("WritePinRecord: %v", err)
	}

	// Verify the file exists and is parseable.
	p := pin.New(nil)
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig after write: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config after write")
	}

	// Remove it.
	if err := pin.RemovePinRecord(root, "myrgic/cogos"); err != nil {
		t.Fatalf("RemovePinRecord: %v", err)
	}

	// Verify gone.
	pinPath := filepath.Join(root, ".cog", "pins", "myrgic_cogos.yaml")
	if _, err := os.Stat(pinPath); !os.IsNotExist(err) {
		t.Errorf("expected pin file to be removed, got: %v", err)
	}
}

// ─── Tests: BuildState ───────────────────────────────────────────────────────

func TestBuildState_PopulatesResources(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: v0.4.1
sync: read-only
`)

	p := pin.New(newStub(map[string]string{target: "v0.4.1"}))
	cfg, _ := p.LoadConfig(root)
	live, _ := p.FetchLive(context.Background(), cfg)

	state, err := p.BuildState(cfg, live, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if len(state.Resources) == 0 {
		t.Fatal("BuildState: expected at least one resource")
	}
	found := false
	for _, r := range state.Resources {
		if r.ExternalID == target {
			found = true
			if r.Type != "pin" {
				t.Errorf("resource type: want pin, got %s", r.Type)
			}
		}
	}
	if !found {
		t.Errorf("BuildState: resource %q not found in state", target)
	}
}

// ─── Tests: Codex Bug 2 — Digest fail-closed (declared digest, empty live) ───

// TestComputePlan_DeclaredDigest_EmptyLive_Red verifies that when a pin record
// declares a digest but the resolver returns an empty live digest, ComputePlan
// produces a RED (Degraded/OutOfSync) health state rather than silently passing.
//
// This is the fail-closed behaviour mandated by the ADR-067 amendment:
// "if a pin record declares digest: sha256:X and the resolver can't compute a
// live digest, the result must be RED, not GREEN."
func TestComputePlan_DeclaredDigest_EmptyLive_Red(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
  digest: sha256:deadbeef
sync: read-only
`)

	// Resolver returns a ref but NO digest (empty string) — simulates the
	// localGitHeadResolver's current behaviour where digest computation is
	// deferred to v1.
	s := &stubResolver{refs: map[string]struct {
		ref    string
		digest string
		err    error
	}{
		target: {ref: "abc1234567890", digest: ""},
	}}

	p := pin.New(s)
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	h := p.Health()
	if h.Health != reconcile.HealthDegraded {
		t.Errorf("declared digest + empty live: want Degraded, got %s: %s", h.Health, h.Message)
	}
	if h.Sync != reconcile.SyncStatusOutOfSync {
		t.Errorf("declared digest + empty live: want OutOfSync, got %s", h.Sync)
	}

	// Verify the plan action reason is "digest_unverifiable".
	found := false
	for _, a := range plan.Actions {
		if a.Name == target {
			reason, _ := a.Details["reason"].(string)
			if reason != "digest_unverifiable" {
				t.Errorf("plan action reason: want digest_unverifiable, got %q", reason)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("no plan action found for target %q", target)
	}
}

// ─── Tests: Codex Bug 3 — verify plan reflects digest/unreachable state ──────

// TestComputePlan_VerifyDigestMismatch_PlanHasReason verifies that when a pin
// record declares a digest that mismatches the live digest, the plan action
// carries reason="digest_mismatch" in its Details map.
//
// This is the ground truth for cmdPinVerify: the CLI now reads from plan.Actions
// directly (Bug 3 fix) instead of re-resolving refs. Correct plan action details
// are the necessary precondition for correct verify output.
func TestComputePlan_VerifyDigestMismatch_PlanHasReason(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
  digest: sha256:aaaa
sync: read-only
`)

	s := &stubResolver{refs: map[string]struct {
		ref    string
		digest string
		err    error
	}{
		target: {ref: "abc1234567890", digest: "sha256:bbbb"},
	}}

	p := pin.New(s)
	cfg, _ := p.LoadConfig(root)
	live, _ := p.FetchLive(context.Background(), cfg)
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	found := false
	for _, a := range plan.Actions {
		if a.Name == target {
			reason, _ := a.Details["reason"].(string)
			if reason != "digest_mismatch" {
				t.Errorf("plan action reason: want digest_mismatch, got %q", reason)
			}
			pinnedDigest, _ := a.Details["pinned_digest"].(string)
			liveDigest, _ := a.Details["live_digest"].(string)
			if pinnedDigest == "" {
				t.Error("plan action missing pinned_digest detail")
			}
			if liveDigest == "" {
				t.Error("plan action missing live_digest detail")
			}
			found = true
		}
	}
	if !found {
		t.Errorf("no plan action found for target %q", target)
	}

	// Health must also reflect the mismatch.
	h := p.Health()
	if h.Health != reconcile.HealthDegraded {
		t.Errorf("digest mismatch: want Degraded, got %s: %s", h.Health, h.Message)
	}
}

// TestComputePlan_VerifyUnreachable_PlanHasReason verifies that when a target
// is unreachable, the plan action carries reason="target_unreachable" so that
// cmdPinVerify surfaces it correctly.
func TestComputePlan_VerifyUnreachable_PlanHasReason(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
sync: read-only
`)

	p := pin.New(newStubWithErr(target, os.ErrNotExist))
	cfg, _ := p.LoadConfig(root)
	live, _ := p.FetchLive(context.Background(), cfg)
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}

	found := false
	for _, a := range plan.Actions {
		if a.Name == target {
			reason, _ := a.Details["reason"].(string)
			if reason != "target_unreachable" {
				t.Errorf("plan action reason: want target_unreachable, got %q", reason)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("no plan action found for target %q", target)
	}
}

// ─── helper ──────────────────────────────────────────────────────────────────

// runAndCheck runs a full reconcile cycle (LoadConfig→FetchLive→ComputePlan)
// and then calls check with the resulting Health status.
func runAndCheck(t *testing.T, p *pin.Provider, root string, check func(reconcile.ResourceStatus)) {
	t.Helper()
	cfg, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	live, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	_, err = p.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	check(p.Health())
}

// ─── Tests: Bug C — direct cmdPinVerify via pin.RunVerify ────────────────────
//
// These tests invoke pin.RunVerify (the extracted helper that cmdPinVerify
// wraps) directly, so reverting the CLI shim to its old broken form would
// cause the tests here — not just the plan-reason tests — to fail.

// TestRunVerify_DigestUnverifiable calls RunVerify against a workspace where
// the pin declares a digest but the stub resolver returns no digest (empty
// string). Expects [DRIFT] output containing "digest unverifiable" and a
// non-nil error.
func TestRunVerify_DigestUnverifiable(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
  digest: sha256:aabbcc
sync: read-only
`)

	// Stub returns a ref but no digest — simulates a resolver that cannot
	// compute content digests (the current localGitHeadResolver behaviour).
	s := &stubResolver{refs: map[string]struct {
		ref    string
		digest string
		err    error
	}{
		target: {ref: "abc1234567890", digest: ""},
	}}

	var buf bytes.Buffer
	results, err := pin.RunVerify(context.Background(), root, "", s, &buf)
	if err == nil {
		t.Fatal("RunVerify: expected error for digest_unverifiable, got nil")
	}
	if !strings.Contains(err.Error(), "drifted or unreachable") {
		t.Errorf("RunVerify error: want 'drifted or unreachable', got %q", err.Error())
	}

	out := buf.String()
	if !strings.Contains(out, "[DRIFT]") {
		t.Errorf("RunVerify output missing [DRIFT] marker; got:\n%s", out)
	}
	if !strings.Contains(out, "digest unverifiable") {
		t.Errorf("RunVerify output missing 'digest unverifiable'; got:\n%s", out)
	}

	if len(results) != 1 || results[0].OK {
		t.Errorf("RunVerify results: want 1 NOT-OK result, got %+v", results)
	}
}

// TestRunVerify_DigestMismatch calls RunVerify against a workspace where the
// pin declares a digest that does not match what the resolver returns.
// Expects [DRIFT] output containing "digest mismatch".
func TestRunVerify_DigestMismatch(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
  digest: sha256:aaaa
sync: read-only
`)

	s := &stubResolver{refs: map[string]struct {
		ref    string
		digest string
		err    error
	}{
		target: {ref: "abc1234567890", digest: "sha256:bbbb"},
	}}

	var buf bytes.Buffer
	results, err := pin.RunVerify(context.Background(), root, "", s, &buf)
	if err == nil {
		t.Fatal("RunVerify: expected error for digest_mismatch, got nil")
	}

	out := buf.String()
	if !strings.Contains(out, "[DRIFT]") {
		t.Errorf("RunVerify output missing [DRIFT] marker; got:\n%s", out)
	}
	if !strings.Contains(out, "digest mismatch") {
		t.Errorf("RunVerify output missing 'digest mismatch'; got:\n%s", out)
	}

	if len(results) != 1 || results[0].OK {
		t.Errorf("RunVerify results: want 1 NOT-OK result, got %+v", results)
	}
}

// TestRunVerify_TargetUnreachable calls RunVerify against a workspace where
// the stub resolver returns an error (target not found). Expects [DRIFT]
// output containing "unreachable".
func TestRunVerify_TargetUnreachable(t *testing.T) {
	root := setupWorkspace(t)
	const target = "myrgic/cogos"
	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
sync: read-only
`)

	s := newStubWithErr(target, os.ErrNotExist)

	var buf bytes.Buffer
	results, err := pin.RunVerify(context.Background(), root, "", s, &buf)
	if err == nil {
		t.Fatal("RunVerify: expected error for target_unreachable, got nil")
	}

	out := buf.String()
	if !strings.Contains(out, "[DRIFT]") {
		t.Errorf("RunVerify output missing [DRIFT] marker; got:\n%s", out)
	}
	if !strings.Contains(out, "unreachable") {
		t.Errorf("RunVerify output missing 'unreachable'; got:\n%s", out)
	}

	if len(results) != 1 || results[0].OK {
		t.Errorf("RunVerify results: want 1 NOT-OK result, got %+v", results)
	}
}

// ─── WorkspaceLocator integration tests (issue #175) ─────────────────────────

// stubLocator is a test implementation of pin.WorkspaceLocator.
type stubLocator struct {
	// paths maps workspace name → filesystem path.
	// An absent or empty entry means ErrWorkspaceNotFound.
	paths map[string]string
	// calls records the names that were looked up (in order).
	calls []string
}

func (l *stubLocator) LocateWorkspace(_ context.Context, name string) (string, error) {
	l.calls = append(l.calls, name)
	p, ok := l.paths[name]
	if !ok || p == "" {
		return "", pin.ErrWorkspaceNotFound
	}
	return p, nil
}

// TestSetWorkspaceLocator_ConsultedBeforeFallback verifies that after
// SetWorkspaceLocator is called, the locator is queried for every target
// workspace before the sibling-directory scan fires.
//
// The locator returns ErrWorkspaceNotFound so the fallback scan fires; the
// test only asserts the locator was consulted, not that resolution succeeded.
func TestSetWorkspaceLocator_ConsultedBeforeFallback(t *testing.T) {
	t.Parallel()
	root := setupWorkspace(t)
	const target = "myrgic/cogos"

	writePinYAML(t, root, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890
sync: read-only
`)

	locator := &stubLocator{paths: map[string]string{}} // always not-found
	p := pin.New(nil)
	p.SetWorkspaceLocator(locator)

	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	_, _ = p.FetchLive(context.Background(), cfgAny) // error expected (no real git repo)

	found := false
	for _, name := range locator.calls {
		if name == target {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WorkspaceLocator not consulted for target %q; calls = %v", target, locator.calls)
	}
}

// TestSetWorkspaceLocator_RegistryPathUsed verifies the end-to-end happy path:
// locator returns a real git repo root → FetchLive resolves HEAD without error.
func TestSetWorkspaceLocator_RegistryPathUsed(t *testing.T) {
	t.Parallel()
	source := setupWorkspace(t)
	const target = "myorg/myrepo"
	const pinnedRef = "abc1234567890abcdef1234567890abcdef123456"

	writePinYAML(t, source, "myorg_myrepo", `
target: myorg/myrepo
pin:
  ref: `+pinnedRef+`
sync: read-only
`)

	// Create a minimal git repo to serve as the target workspace.
	targetDir := t.TempDir()
	for _, args := range [][]string{
		{"git", "-C", targetDir, "init"},
		{"git", "-C", targetDir, "config", "user.email", "test@example.com"},
		{"git", "-C", targetDir, "config", "user.name", "Test"},
		{"git", "-C", targetDir, "commit", "--allow-empty", "-m", "initial"},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Skipf("git setup failed (%v): %s — skipping registry-path test", err, out)
		}
	}

	locator := &stubLocator{paths: map[string]string{target: targetDir}}
	p := pin.New(nil)
	p.SetWorkspaceLocator(locator)

	cfgAny, err := p.LoadConfig(source)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	liveAny, err := p.FetchLive(context.Background(), cfgAny)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	// Verify the locator was consulted.
	found := false
	for _, name := range locator.calls {
		if name == target {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WorkspaceLocator not consulted for target %q; calls = %v", target, locator.calls)
	}

	// Verify FetchLive succeeded by running ComputePlan — if liveAny is valid,
	// ComputePlan won't panic.
	planAny, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if planAny == nil {
		t.Fatal("ComputePlan returned nil plan")
	}
}

// ─── Locator error-propagation tests (PR #180 fixup) ─────────────────────────

// errLocator is a stub WorkspaceLocator that always returns a hard error that
// is NOT ErrWorkspaceNotFound (simulates I/O failure / corrupt registry file).
type errLocator struct {
	err error
}

func (l *errLocator) LocateWorkspace(_ context.Context, _ string) (string, error) {
	return "", l.err
}

// TestLocatorNonNotFoundError_Propagated is the regression test for the
// silent-swallow bug found by Codex review of PR #180.
//
// Before the fix, any error from WorkspaceLocator.LocateWorkspace (other than
// ErrWorkspaceNotFound) was silently swallowed and the sibling-directory
// fallback always ran. The comment at locateTarget:247-250 said non-not-found
// errors should return immediately — but the code didn't match.
//
// After the fix, a hard registry error (e.g. I/O failure) propagates out of
// FetchLive so the caller knows the registry is broken, not just empty.
func TestLocatorNonNotFoundError_Propagated(t *testing.T) {
	t.Parallel()
	source := setupWorkspace(t)
	const target = "myrgic/cogos"

	writePinYAML(t, source, "myrgic_cogos", `
target: myrgic/cogos
pin:
  ref: abc1234567890abcdef1234567890abcdef123456
sync: read-only
`)

	registryErr := errors.New("simulated registry I/O failure")
	p := pin.New(nil)
	p.SetWorkspaceLocator(&errLocator{err: registryErr})

	cfgAny, err := p.LoadConfig(source)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	liveAny, fetchErr := p.FetchLive(context.Background(), cfgAny)
	if fetchErr != nil {
		t.Fatalf("FetchLive unexpected top-level error: %v", fetchErr)
	}

	// ComputePlan stores lr.Error.Error() in the plan action details["error"].
	// This is the observable surface for what error message was captured.
	plan, planErr := p.ComputePlan(cfgAny, liveAny, nil)
	if planErr != nil {
		t.Fatalf("ComputePlan unexpected error: %v", planErr)
	}

	// After the fix: the registry error propagates through locateTarget →
	// ResolveHead (wrapped as "pin: locating target …: simulated registry I/O
	// failure") → LiveRef.Error → plan action details["error"].
	// The original registryErr.Error() string must appear in the action details.
	//
	// Without the fix: locateTarget silently falls through to sibling scan,
	// which also fails but produces a generic ErrWorkspaceNotFound message —
	// "pin: workspace not found …" — that does NOT contain the original registry
	// error text. The test would fail on pre-fix code because the error string
	// "simulated registry I/O failure" is absent from the action details.
	if len(plan.Actions) == 0 {
		t.Fatal("ComputePlan returned no actions; expected at least one for the unreachable target")
	}
	actionErr, _ := plan.Actions[0].Details["error"].(string)
	if !strings.Contains(actionErr, registryErr.Error()) {
		t.Errorf("plan action error %q does not contain registry error %q; "+
			"registry I/O error was silently swallowed and replaced by fallback error",
			actionErr, registryErr.Error())
	}
}

// TestLocatorErrWorkspaceNotFound_FallsBack verifies the complementary case:
// ErrWorkspaceNotFound from the locator still falls through to the sibling scan
// (not treated as a hard failure). This ensures the fallback logic is intact.
func TestLocatorErrWorkspaceNotFound_FallsBack(t *testing.T) {
	t.Parallel()
	source := setupWorkspace(t)
	const target = "some-workspace"

	writePinYAML(t, source, "some_workspace", `
target: some-workspace
pin:
  ref: abc1234567890abcdef1234567890abcdef123456
sync: read-only
`)

	// Locator returns ErrWorkspaceNotFound — should fall through, not hard-fail.
	notFoundLocator := &stubLocator{paths: map[string]string{}} // empty → not found
	p := pin.New(nil)
	p.SetWorkspaceLocator(notFoundLocator)

	cfgAny, err := p.LoadConfig(source)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// FetchLive should not propagate an error from ErrWorkspaceNotFound alone;
	// the sibling scan fires (and finds nothing), then localGitHeadResolver
	// wraps the absence as ErrWorkspaceNotFound in the LiveRef.
	_, err = p.FetchLive(context.Background(), cfgAny)
	if err != nil {
		// Acceptable — may wrap ErrWorkspaceNotFound from git resolution.
		// What is NOT acceptable is a hard error.
		if !errors.Is(err, pin.ErrWorkspaceNotFound) && !strings.Contains(err.Error(), "not found") {
			t.Errorf("FetchLive returned unexpected error (want workspace-not-found, got): %v", err)
		}
	}
	// Locator must have been consulted.
	if len(notFoundLocator.calls) == 0 {
		t.Error("locator was not consulted at all")
	}
}

// TestErrWorkspaceNotFound_SentinelExported verifies the exported sentinel error
// is the right type for errors.Is checks in callers.
func TestErrWorkspaceNotFound_SentinelExported(t *testing.T) {
	t.Parallel()
	if pin.ErrWorkspaceNotFound == nil {
		t.Fatal("ErrWorkspaceNotFound is nil")
	}
	// Ensure wrapping works.
	wrapped := fmt.Errorf("wrapped: %w", pin.ErrWorkspaceNotFound)
	if !errors.Is(wrapped, pin.ErrWorkspaceNotFound) {
		t.Errorf("errors.Is(wrapped, ErrWorkspaceNotFound) = false; wrapping broken")
	}
}
