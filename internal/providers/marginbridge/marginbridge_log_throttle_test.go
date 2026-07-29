// marginbridge_log_throttle_test.go — regression test for issue #494's
// log-noise fix, fourth pass (cog-review, PR #496): FetchLive's per-dir
// (listDir) and per-file (fetchContentSHA) failure warnings used the
// unthrottled stdlib log package. Because FetchLive always returns
// (snap, nil) overall — one broken watch target must not fail FetchLive for
// every other target — a persistently unreachable dir or file repeated the
// identical warning on every ~30s reconcile tick forever, invisible to
// reconcile_daemon.go's phase-level throttle around the FetchLive call
// (which only fires on a non-nil top-level error).
//
// Both call sites now go through (*Provider).logThrottled, the same
// Warn-once-then-Debug shape as internal/providers/site's
// (*siteProvider).logThrottled and internal/engine/reconcile_daemon.go's
// warnPhaseFailureThrottled.
package marginbridge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestFetchLive_ListDirFailureThrottled asserts a persistently unreachable
// watch dir logs its listDir failure at Warn once across repeated FetchLive
// calls, then at Debug for identical repeats.
func TestFetchLive_ListDirFailureThrottled(t *testing.T) {
	gh := newMockGH()
	p := newTestProvider(gh)
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_dirs: [signals/inbox]\n")
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*Config)
	// signals/inbox is never registered with the mock, so every listDir
	// call fails identically with the mock's default 404-shaped error.

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
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
}

// TestFetchLive_FetchContentSHAFailureThrottled is the WatchFiles
// counterpart of TestFetchLive_ListDirFailureThrottled.
func TestFetchLive_FetchContentSHAFailureThrottled(t *testing.T) {
	gh := newMockGH()
	p := newTestProvider(gh)
	root := t.TempDir()
	writeConfig(t, root, "repo: my/repo\nwatch_files: [comments/ledger.json]\n")
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg := cfgAny.(*Config)
	// comments/ledger.json is never registered with the mock, so every
	// fetchContentSHA call fails identically.

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
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
