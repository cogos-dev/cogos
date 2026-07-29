// site_log_throttle_test.go — regression tests for issue #494's log-noise
// fix, third pass (cog-review, PR #496): three more call sites in this file
// logged a per-resource warning/notice via the unthrottled stdlib log
// package (bypassing both slog's level filtering and
// reconcile_daemon.go's phase-level throttle, since FetchLive/loadCRDs
// swallow these into per-resource state and applyAction's delete path never
// returns ApplyFailed) — each would repeat the identical line on every
// ~30s reconcile tick forever for a persistently broken/no-op resource.
//
// All three now go through (*siteProvider).logThrottled, the same
// Warn-once-then-Debug shape reconcile_daemon.go's warnPhaseFailureThrottled
// uses.
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

func withCapturedLog(t *testing.T, level slog.Level, fn func(buf *bytes.Buffer)) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	defer slog.SetDefault(prev)
	fn(&buf)
}

// TestFetchLive_UnregisteredStrategyThrottled asserts that a CRD naming an
// unregistered deploy strategy logs its lookup failure at Warn once, then at
// Debug for identical repeats — not at Warn on every FetchLive call.
func TestFetchLive_UnregisteredStrategyThrottled(t *testing.T) {
	sp := newSiteProvider() // no strategies registered

	crds := []siteCRD{
		{Metadata: siteMetadata{Name: "app-a"}, Spec: siteSpec{Deploy: deploySpec{Strategy: "no-such-strategy"}}},
	}

	withCapturedLog(t, slog.LevelDebug, func(buf *bytes.Buffer) {
		if _, err := sp.FetchLive(context.Background(), crds); err != nil {
			t.Fatalf("FetchLive: %v", err)
		}
		if _, err := sp.FetchLive(context.Background(), crds); err != nil {
			t.Fatalf("FetchLive (2nd call): %v", err)
		}

		lines := nonEmptyLines(buf.String())
		if len(lines) != 2 {
			t.Fatalf("expected 2 log lines for 2 FetchLive calls, got %d:\n%s", len(lines), buf.String())
		}
		if !strings.Contains(lines[0], "level=WARN") {
			t.Errorf("first occurrence should log at WARN, got: %s", lines[0])
		}
		if !strings.Contains(lines[1], "level=DEBUG") {
			t.Errorf("second (identical) occurrence should log at DEBUG, got: %s", lines[1])
		}
	})
}

// TestLoadCRDs_ValidationWarningThrottled asserts a persistently invalid
// site.yaml logs its validation error at Warn once across repeated
// LoadConfig calls, then at Debug.
func TestLoadCRDs_ValidationWarningThrottled(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "broken-app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Missing spec.domain -- fails siteCRD.Validate().
	yaml := "apiVersion: v1\nkind: Site\nmetadata:\n  name: broken-app\nspec:\n  deploy:\n    strategy: gh-pages\n"
	if err := os.WriteFile(filepath.Join(appDir, "site.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write site.yaml: %v", err)
	}

	sp := newSiteProvider()

	withCapturedLog(t, slog.LevelDebug, func(buf *bytes.Buffer) {
		if _, err := sp.loadCRDs(root); err != nil {
			t.Fatalf("loadCRDs: %v", err)
		}
		if _, err := sp.loadCRDs(root); err != nil {
			t.Fatalf("loadCRDs (2nd call): %v", err)
		}

		lines := nonEmptyLines(buf.String())
		if len(lines) != 2 {
			t.Fatalf("expected 2 log lines for 2 loadCRDs calls, got %d:\n%s", len(lines), buf.String())
		}
		if !strings.Contains(lines[0], "level=WARN") {
			t.Errorf("first occurrence should log at WARN, got: %s", lines[0])
		}
		if !strings.Contains(lines[1], "level=DEBUG") {
			t.Errorf("second (identical) occurrence should log at DEBUG, got: %s", lines[1])
		}
	})
}

// TestApplyAction_DeleteNoOpThrottled asserts the delete no-op notice (v0.0.1
// requires manual teardown, so ComputePlan keeps re-proposing the delete)
// logs once at Info, then at Debug for repeats — even though this path
// always reports ApplySucceeded and so is never covered by
// warnActionFailureThrottled (which only triggers on ApplyFailed).
func TestApplyAction_DeleteNoOpThrottled(t *testing.T) {
	sp := newSiteProvider()
	action := reconcile.Action{Action: reconcile.ActionDelete, Name: "orphaned-app"}

	withCapturedLog(t, slog.LevelDebug, func(buf *bytes.Buffer) {
		res1 := sp.applyAction(context.Background(), action)
		res2 := sp.applyAction(context.Background(), action)
		if res1.Status != reconcile.ApplySucceeded || res2.Status != reconcile.ApplySucceeded {
			t.Fatalf("expected both delete no-ops to report ApplySucceeded, got %v / %v", res1.Status, res2.Status)
		}

		lines := nonEmptyLines(buf.String())
		if len(lines) != 2 {
			t.Fatalf("expected 2 log lines for 2 delete no-ops, got %d:\n%s", len(lines), buf.String())
		}
		if !strings.Contains(lines[0], "level=INFO") {
			t.Errorf("first occurrence should log at INFO, got: %s", lines[0])
		}
		if !strings.Contains(lines[1], "level=DEBUG") {
			t.Errorf("second (identical) occurrence should log at DEBUG, got: %s", lines[1])
		}
	})
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
