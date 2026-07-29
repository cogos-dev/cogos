// site_build_log_test.go — regression test for issue #494's "unrelated
// observation" that siteBuild logged the FULL combined stdout+stderr of
// `bash build.sh` via the stdlib log package (no level, always writes to
// os.Stderr) on every successful build. Because ComputePlan's drift
// detection runs this build on every reconcile cycle for every deployed
// site regardless of drift, a subprocess that emits anything noisy to its
// own stderr (the issue's example: macOS's
// "MallocStackLogging: can't turn off malloc stack logging because it was
// not enabled.") got captured verbatim and re-logged forever, contributing
// to ~/.cog/var/logs/serve.log's observed 488 MB.
//
// The fix moves the success-path log to slog.Debug (suppressed by default,
// see internal/engine/log_capture.go's upgradeLoggerWithFileSink) while
// leaving the error path (which still includes the full output in the
// returned error) unchanged.
package site

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const noisyMarker = "MallocStackLogging: can't turn off malloc stack logging because it was not enabled."

func writeBuildScript(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "build.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write build.sh: %v", err)
	}
}

// TestSiteBuild_SuccessOutputIsDebugNotWarn asserts a successful build's
// combined output is logged at Debug (invisible at the daemon's default
// Info level) rather than unconditionally via the stdlib log package.
func TestSiteBuild_SuccessOutputIsDebugNotWarn(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho \""+noisyMarker+"\" >&2\nexit 0\n")

	var buf bytes.Buffer
	prev := slog.Default()
	// Default-level handler (Info) — matches the daemon's default
	// (log_capture.go's upgradeLoggerWithFileSink uses slog.LevelInfo unless
	// COG_LOG_DEBUG is set).
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	if err := siteBuild(context.Background(), dir, "test-site"); err != nil {
		t.Fatalf("siteBuild: %v", err)
	}

	if strings.Contains(buf.String(), noisyMarker) {
		t.Fatalf("successful build's noisy subprocess output leaked into the default (Info-level) log stream — this is the exact #494 log-noise regression:\n%s", buf.String())
	}

	// Confirm the output is still captured, just at Debug — raising the
	// handler's level to Debug should surface it. Nothing about siteBuild's
	// success path should be silently dropped, only demoted.
	buf.Reset()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := siteBuild(context.Background(), dir, "test-site"); err != nil {
		t.Fatalf("siteBuild (debug level): %v", err)
	}
	if !strings.Contains(buf.String(), noisyMarker) {
		t.Fatalf("build output missing entirely at Debug level — success-path logging should be demoted, not deleted:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("expected the build-succeeded log line to be at DEBUG level, got:\n%s", buf.String())
	}
}

// TestSiteBuild_FailureStillReturnsFullOutput asserts the error path is
// unchanged: a failing build.sh still returns an error whose text includes
// the full combined output, regardless of the success-path logging change.
func TestSiteBuild_FailureStillReturnsFullOutput(t *testing.T) {
	dir := t.TempDir()
	writeBuildScript(t, dir, "#!/bin/bash\necho 'boom: something broke' >&2\nexit 1\n")

	err := siteBuild(context.Background(), dir, "test-site")
	if err == nil {
		t.Fatal("expected siteBuild to return an error for a failing build.sh")
	}
	if !strings.Contains(err.Error(), "boom: something broke") {
		t.Fatalf("error does not include the build's output, got: %v", err)
	}
}
