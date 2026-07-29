// reconcile_phase_throttle_test.go — regression tests for issue #494's
// "unrelated observation" that a chronically-failing provider (e.g. discord
// with no bot token configured) logs the identical
// `reconcile-daemon: FetchLive failed provider=discord err="discord: bot
// token not set"` line at Warn on every single reconcile tick forever,
// contributing to ~/.cog/var/logs/serve.log growing unbounded with no new
// information in each repeat.
//
// warnPhaseFailureThrottled (reconcile_daemon.go) fixes this generically for
// every cycle-aborting phase (LoadConfig, FetchLive, acquire state lock,
// ComputePlan, ApplyPlan): the first failure for a given (providerType,
// phase) — or any failure whose error text differs from the last one seen —
// still logs at Warn; an exact repeat logs at Debug (suppressed at the
// daemon's default log level, see log_capture.go's upgradeLoggerWithFileSink).
package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// syncBuffer wraps bytes.Buffer with a mutex so it can safely serve as an
// slog.Handler sink written from a daemon's background goroutine while the
// test goroutine concurrently reads it. A plain bytes.Buffer here would
// (correctly) trip -race: the daemon's run() goroutine logs a final
// "reconcile-daemon: stopped" line from inside its shutdown defer AFTER
// flipping State() to Shutdown, so polling State() alone does not establish
// a happens-before edge against that trailing write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestWarnPhaseFailureThrottled_LogLevels drives warnPhaseFailureThrottled
// directly (white-box, same package) and asserts the level-selection logic:
// first occurrence and any text change log at Warn; an exact repeat logs at
// Debug; clearPhaseFailureThrottle resets the streak; different
// (providerType, phase) keys are independent of each other.
func TestWarnPhaseFailureThrottled_LogLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	d := &ReconcileDaemon{lastPhaseErr: make(map[string]string)}

	err1 := errors.New("simulated fetch failure")
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err1)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines for 3 calls, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("first occurrence should log at WARN, got: %s", lines[0])
	}
	for i, line := range lines[1:] {
		if !strings.Contains(line, "level=DEBUG") {
			t.Errorf("repeat occurrence #%d (identical error text) should log at DEBUG (throttled), got: %s", i+2, line)
		}
		if strings.Contains(line, "level=WARN") {
			t.Errorf("repeat occurrence #%d must NOT log at WARN — this is exactly the spam issue #494 reports", i+2)
		}
	}

	// A DIFFERENT error text for the same (providerType, phase) key is new
	// information and must re-WARN.
	buf.Reset()
	err2 := errors.New("a different, new failure")
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err2)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a changed error message must re-WARN, got: %s", buf.String())
	}

	// clearPhaseFailureThrottle (called after a phase succeeds) resets the
	// streak: a later failure with the SAME text as err2 must WARN again,
	// not be treated as a continuation of the old streak.
	d.clearPhaseFailureThrottle("throttle-test", "FetchLive")
	buf.Reset()
	d.warnPhaseFailureThrottled("throttle-test", "FetchLive", err2)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("after clearPhaseFailureThrottle, a repeat of the previously-seen text must WARN again (fresh recurrence), got: %s", buf.String())
	}

	// A different providerType is an independent key.
	buf.Reset()
	d.warnPhaseFailureThrottled("other-provider", "FetchLive", err1)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a different providerType must WARN independently of throttle-test's streak, got: %s", buf.String())
	}

	// A different phase for the SAME providerType is also independent.
	buf.Reset()
	d.warnPhaseFailureThrottled("throttle-test", "ComputePlan", err1)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a different phase for the same providerType must WARN independently, got: %s", buf.String())
	}
}

// TestReconcileDaemon_ChronicFetchFailureIsThrottled is the end-to-end
// regression test: a provider whose FetchLive fails identically on every
// tick (mirroring discord with no bot token) must produce exactly one Warn
// "FetchLive failed" log line across many real daemon ticks, not one per
// tick.
func TestReconcileDaemon_ChronicFetchFailureIsThrottled(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bad := &errorReconcilable{typeName: "test-chronic-fetch-failure"}
	reconcile.UpsertProvider(bad.Type(), bad)

	daemon := newTestDaemon(t.TempDir(), 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	daemon.Start(ctx)
	<-ctx.Done()
	cancel()

	// Wait for the daemon's goroutine to fully exit (State() only transitions
	// to Shutdown once the run loop has returned — see the doc comment on
	// ReconcileDaemonShutdown) before reading its tick counters below.
	deadline := time.After(2 * time.Second)
	for daemon.State() != ReconcileDaemonShutdown {
		select {
		case <-deadline:
			t.Fatal("daemon did not reach Shutdown state in time")
		case <-time.After(5 * time.Millisecond):
		}
	}

	ticks := bad.callCount.Load()
	if ticks < 3 {
		t.Fatalf("test setup: expected several ticks (LoadConfig calls), got %d — increase the test window", ticks)
	}

	warnCount := 0
	debugCount := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "reconcile-daemon: FetchLive failed") {
			continue
		}
		switch {
		case strings.Contains(line, "level=WARN"):
			warnCount++
		case strings.Contains(line, "level=DEBUG"):
			debugCount++
		}
	}

	if warnCount != 1 {
		t.Errorf("got %d WARN-level 'FetchLive failed' lines across %d ticks, want exactly 1 (first occurrence only) — issue #494's log-noise regression", warnCount, ticks)
	}
	if debugCount < int(ticks)-1 {
		t.Errorf("got %d DEBUG-level 'FetchLive failed' lines, want at least %d (every tick after the first, throttled)", debugCount, ticks-1)
	}
}
