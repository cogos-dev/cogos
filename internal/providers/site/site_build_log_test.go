// site_build_log_test.go — regression tests for issue #494's "unrelated
// observation" that siteBuild logged the FULL combined stdout+stderr of
// `bash build.sh` via the stdlib log package (no level, always writes to
// os.Stderr) on every successful build. Because ComputePlan's drift
// detection (buildAndHash) runs this build on every reconcile cycle for
// every deployed site regardless of drift, a subprocess that emits anything
// noisy to its own stderr (the issue's example: macOS's
// "MallocStackLogging: can't turn off malloc stack logging because it was
// not enabled.") got captured verbatim and re-logged forever, contributing
// to ~/.cog/var/logs/serve.log's observed 488 MB.
//
// cog-review's first pass on the original fix (PR #496) correctly flagged
// that siteBuild is called from TWO places with very different noise
// profiles — buildAndHash (every ~30s reconcile cycle, purely for drift
// hashing) and applyAction (only on an actual deploy, a rare, operator-
// relevant event) — and that demoting siteBuild's own success log to Debug
// uniformly would also hide a genuine deploy's build output by default,
// which was never the noise this issue was about. The fix: siteBuild itself
// logs nothing and just returns the output; each caller logs it at the
// level appropriate to its own frequency (buildAndHash: Debug; applyAction:
// Info).
package site

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

const noisyMarker = "MallocStackLogging: can't turn off malloc stack logging because it was not enabled."

func writeBuildScript(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "build.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write build.sh: %v", err)
	}
}

// TestSiteBuild_DoesNotLogItself asserts the low-level siteBuild helper logs
// nothing on either path — logging is entirely the callers' responsibility
// (see buildAndHash/applyAction tests below), so siteBuild can't reintroduce
// noise no matter which caller demotes or keeps its own log level.
func TestSiteBuild_DoesNotLogItself(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho \""+noisyMarker+"\" >&2\nexit 0\n")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	out, err := siteBuild(context.Background(), dir, "test-site")
	if err != nil {
		t.Fatalf("siteBuild: %v", err)
	}
	if !strings.Contains(string(out), noisyMarker) {
		t.Fatalf("siteBuild's returned output is missing the build's stderr content, got: %q", out)
	}
	if buf.Len() != 0 {
		t.Fatalf("siteBuild logged something itself (even at Debug level, nothing should be captured by the handler): %s", buf.String())
	}
}

// TestBuildAndHash_DriftCheckBuildLogsAtDebug asserts the periodic
// drift-detection path (buildAndHash, called every reconcile cycle) logs a
// successful build's output at Debug — invisible at the daemon's default
// Info level (see internal/engine/log_capture.go's upgradeLoggerWithFileSink) —
// so the noisy per-cycle case from the issue is fixed.
func TestBuildAndHash_DriftCheckBuildLogsAtDebug(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho \""+noisyMarker+"\" >&2\nmkdir -p dist\ntouch dist/index.html\nexit 0\n")

	sp := newSiteProvider()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	if _, err := sp.buildAndHash(context.Background(), dir, "test-site"); err != nil {
		t.Fatalf("buildAndHash: %v", err)
	}
	if strings.Contains(buf.String(), noisyMarker) {
		t.Fatalf("drift-check build's noisy output leaked into the default (Info-level) log stream — this is the exact #494 log-noise regression:\n%s", buf.String())
	}

	buf.Reset()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if _, err := sp.buildAndHash(context.Background(), dir, "test-site"); err != nil {
		t.Fatalf("buildAndHash (debug level): %v", err)
	}
	if !strings.Contains(buf.String(), noisyMarker) {
		t.Fatalf("build output missing entirely at Debug level — should be demoted, not deleted:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("expected the drift-check build-succeeded log line to be at DEBUG level, got:\n%s", buf.String())
	}
}

// TestApplyAction_DeployBuildLogsAtInfo asserts the real-deploy path
// (applyAction, only invoked when a create/update action is actually being
// applied) keeps its build-succeeded log visible at Info by default — a
// genuine deploy is a rare, operator-relevant event, not the per-cycle noise
// buildAndHash produces, and must not be silently hidden behind
// COG_LOG_DEBUG=1.
func TestApplyAction_DeployBuildLogsAtInfo(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho \""+noisyMarker+"\" >&2\nexit 0\n")

	sp := newSiteProvider()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	action := reconcile.Action{
		Action: reconcile.ActionUpdate,
		Name:   "test-site",
		Details: map[string]any{
			"app_dir": dir,
			// No "strategy" set — lookupStrategy will fail after the build
			// step, which is fine: this test only cares that the build
			// succeeded and its output was logged before that later failure,
			// not that the whole deploy completes.
		},
	}

	_ = sp.applyAction(context.Background(), action)

	if !strings.Contains(buf.String(), noisyMarker) {
		t.Fatalf("a real deploy's build output must remain visible at the default Info level, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=INFO") {
		t.Fatalf("expected the deploy build-succeeded log line to be at INFO level, got:\n%s", buf.String())
	}
}

// TestSiteBuild_FailureStillReturnsFullOutput asserts the error path is
// unchanged by the logging refactor: a failing build.sh still returns an
// error whose text includes the full combined output.
func TestSiteBuild_FailureStillReturnsFullOutput(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho 'boom: something broke' >&2\nexit 1\n")

	_, err := siteBuild(context.Background(), dir, "test-site")
	if err == nil {
		t.Fatal("expected siteBuild to return an error for a failing build.sh")
	}
	if !strings.Contains(err.Error(), "boom: something broke") {
		t.Fatalf("error does not include the build's output, got: %v", err)
	}
}
