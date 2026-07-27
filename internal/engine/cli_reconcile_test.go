// cli_reconcile_test.go — tests for "cogos reconcile <type>" branches flagged
// by cog-review as having no automated coverage: the --snapshot flag-dispatch
// branch (ConfigExporter type assertion, the snapshot-specific state-lock
// acquisition, and the success output format; PR #473), and the
// RegisterProviders/SetProvidersWorkspace hook invocation that makes
// hook-only-registered providers reachable at all (PR #475).
//
// runReconcileCmd calls os.Exit on every error branch and prints directly to
// os.Stdout/os.Stderr, so it cannot be exercised in-process without killing
// the test binary. These tests use the standard Go "helper process" pattern
// (cf. os/exec_test.go's TestHelperProcess): each test re-execs this test
// binary as a subprocess restricted to TestHelperProcessReconcileSnapshot,
// selects a scenario via env var, and asserts on the child's exit code and
// stdout/stderr.
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// mockPlainReconciler implements reconcile.Reconcilable only — deliberately
// no ConfigExporter — to exercise the "does not support snapshot" branch.
type mockPlainReconciler struct{ name string }

func (m *mockPlainReconciler) Type() string                   { return m.name }
func (m *mockPlainReconciler) LoadConfig(string) (any, error) { return nil, nil }
func (m *mockPlainReconciler) FetchLive(context.Context, any) (any, error) {
	return nil, nil
}
func (m *mockPlainReconciler) ComputePlan(any, any, *reconcile.State) (*reconcile.Plan, error) {
	return nil, nil
}
func (m *mockPlainReconciler) ApplyPlan(context.Context, *reconcile.Plan) ([]reconcile.Result, error) {
	return nil, nil
}
func (m *mockPlainReconciler) BuildState(any, any, *reconcile.State) (*reconcile.State, error) {
	return nil, nil
}
func (m *mockPlainReconciler) Health() reconcile.ResourceStatus {
	return reconcile.NewResourceStatus(reconcile.SyncStatusUnknown, reconcile.HealthMissing)
}

// mockExportReconciler additionally implements reconcile.ConfigExporter, to
// exercise the state-lock-acquisition and success paths.
type mockExportReconciler struct {
	mockPlainReconciler
}

func (m *mockExportReconciler) ExportConfig(root string) error { return nil }

// mockHookOnlyReconciler simulates a provider that is registered exclusively
// inside the RegisterProviders hook — never via a package init() — which is
// exactly the class of provider runReconcileCmd left unreachable before the
// fix for PR #475 (ProjectionCompiler being the real-world instance, per
// internal/providers/all/all.go's RegisterProviders closure). Unlike
// mockPlainReconciler, ComputePlan returns a non-nil empty Plan so the
// --dry-run path can render and exit cleanly once the provider is found.
type mockHookOnlyReconciler struct{ mockPlainReconciler }

func (m *mockHookOnlyReconciler) ComputePlan(any, any, *reconcile.State) (*reconcile.Plan, error) {
	return &reconcile.Plan{ResourceType: m.name}, nil
}

const (
	reconcileSnapshotHelperEnv = "CLI_RECONCILE_SNAPSHOT_HELPER"
	reconcileSnapshotCaseEnv   = "CLI_RECONCILE_SNAPSHOT_CASE"
	reconcileSnapshotRootEnv   = "CLI_RECONCILE_SNAPSHOT_ROOT"
)

// TestHelperProcessReconcileSnapshot is not a real test — it only acts when
// invoked as a subprocess by the Test* functions below (gated on
// CLI_RECONCILE_SNAPSHOT_HELPER so a normal `go test` run treats it as an
// instant no-op pass, same idiom as os/exec_test.go's TestHelperProcess).
func TestHelperProcessReconcileSnapshot(t *testing.T) {
	if os.Getenv(reconcileSnapshotHelperEnv) != "1" {
		return
	}
	root := os.Getenv(reconcileSnapshotRootEnv)
	switch os.Getenv(reconcileSnapshotCaseEnv) {
	case "noexporter":
		reconcile.RegisterProvider("mocknoexport", &mockPlainReconciler{name: "mocknoexport"})
		runReconcileCmd([]string{"mocknoexport", "--snapshot"}, root)
	case "lockbusy":
		reconcile.RegisterProvider("mockexport", &mockExportReconciler{mockPlainReconciler{name: "mockexport"}})
		runReconcileCmd([]string{"mockexport", "--snapshot"}, root)
	case "success":
		reconcile.RegisterProvider("mockexport", &mockExportReconciler{mockPlainReconciler{name: "mockexport"}})
		runReconcileCmd([]string{"mockexport", "--snapshot"}, root)
		os.Exit(0)
	case "hookonly":
		// Deliberately do NOT call reconcile.RegisterProvider directly here —
		// the whole point is that this provider is reachable ONLY through the
		// RegisterProviders hook, mirroring how cmd/cogos/providers_wire.go's
		// init() wires ProjectionCompiler into engine.RegisterProviders rather
		// than registering it via a package init().
		RegisterProviders = func() {
			reconcile.RegisterProvider("mockhookonly", &mockHookOnlyReconciler{mockPlainReconciler{name: "mockhookonly"}})
		}
		runReconcileCmd([]string{"mockhookonly", "--dry-run", "--json"}, root)
		os.Exit(0)
	}
	// runReconcileCmd exits the process directly on every branch above except
	// "success" (which exits explicitly right after); reaching here means
	// the case switch didn't match any known scenario.
	os.Exit(99)
}

// runReconcileSnapshotHelper re-execs this test binary restricted to
// TestHelperProcessReconcileSnapshot for the given scenario and workspace
// root, and returns the child's exit code, stdout, and stderr.
func runReconcileSnapshotHelper(t *testing.T, scenario, root string) (exitCode int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessReconcileSnapshot$")
	cmd.Env = append(os.Environ(),
		reconcileSnapshotHelperEnv+"=1",
		reconcileSnapshotCaseEnv+"="+scenario,
		reconcileSnapshotRootEnv+"="+root,
	)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()
	if err == nil {
		return 0, stdout, stderr
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), stdout, stderr
	}
	t.Fatalf("running helper process: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	return -1, stdout, stderr
}

// TestReconcileSnapshotNoConfigExporter covers the ConfigExporter
// type-assertion failure path: a provider that doesn't implement
// reconcile.ConfigExporter must fail --snapshot with exit 1 and a
// descriptive stderr message, not a panic or silent success.
func TestReconcileSnapshotNoConfigExporter(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := runReconcileSnapshotHelper(t, "noexporter", root)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, `does not support snapshot`) {
		t.Errorf("stderr = %q, want it to mention unsupported snapshot", stderr)
	}
}

// TestReconcileSnapshotLockBusy covers the state-lock-acquisition failure
// path inside the --snapshot branch: AcquireStateLock's MkdirAll fails
// deterministically (no polling/timeout needed) when the resource-type
// directory it needs already exists as a plain file.
func TestReconcileSnapshotLockBusy(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("setup: mkdir %s: %v", configDir, err)
	}
	collision := filepath.Join(configDir, "mockexport")
	if err := os.WriteFile(collision, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("setup: write %s: %v", collision, err)
	}

	code, _, stderr := runReconcileSnapshotHelper(t, "lockbusy", root)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "acquire state lock for mockexport") {
		t.Errorf("stderr = %q, want it to mention state lock acquisition failure", stderr)
	}
}

// TestReconcileHookOnlyProviderReachable is the regression test flagged by
// cog-review on PR #475: runReconcileCmd must call the RegisterProviders /
// SetProvidersWorkspace hooks before looking up the provider, exactly as
// runServe does (cli.go:271-274, 308-311). Before that fix, any provider
// registered only inside the RegisterProviders closure — never via a package
// init() — was permanently unreachable from "cogos reconcile <type>" and
// GetProvider always returned "unknown resource type", regardless of what was
// wired into the hook. ProjectionCompiler was the real-world instance of this
// (internal/providers/all/all.go:171-174), reported as an "unknown resource
// type" failure for `cogos reconcile projection-compiler`.
//
// This test reproduces that class with a mock: a provider registered
// exclusively from inside RegisterProviders, never called directly. On the
// pre-fix code (RegisterProviders/SetProvidersWorkspace not invoked in
// runReconcileCmd) this fails with exit 1 and "unknown resource type" in
// stderr; on the fixed code it succeeds with exit 0.
func TestReconcileHookOnlyProviderReachable(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := runReconcileSnapshotHelper(t, "hookonly", root)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if strings.Contains(stderr, "unknown resource type") {
		t.Errorf("stderr = %q, provider registered only via the RegisterProviders hook was not found — the hook was not invoked", stderr)
	}
}

// TestReconcileSnapshotSuccess covers the success output format: exit 0 and
// the exact "<config-dir>\nsnapshot written\n" line the doc comment promises.
func TestReconcileSnapshotSuccess(t *testing.T) {
	root := t.TempDir()
	code, stdout, stderr := runReconcileSnapshotHelper(t, "success", root)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	wantPath := filepath.Join(root, ".cog", "config", "mockexport")
	wantOut := wantPath + "\nsnapshot written\n"
	if stdout != wantOut {
		t.Errorf("stdout = %q, want %q", stdout, wantOut)
	}
}
