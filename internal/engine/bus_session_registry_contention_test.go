// bus_session_registry_contention_test.go — regression coverage for #461
// (bus registry cross-process lock coordination) and its #505 fallout (OS
// thread growth under registry-lock contention).
//
// #505 diagnosed the mechanism: handler_span.go calls RegisterBus +
// AppendEvent on every single HTTP/MCP handler invocation, and each of those
// independently called filelock.Acquire — a real flock(2) syscall, retried
// on a 50ms poll — from its own goroutine. Under request-storm concurrency,
// hundreds of goroutines were simultaneously inside that retry loop, and Go's
// runtime pins one OS thread per goroutine actively in a syscall (and never
// gives created threads back), so the process's OS thread count only
// ratcheted upward (32 baseline -> 459 in under an hour).
//
// These tests exercise the fix (registryFileMu single-flighting in-process
// access, plus the registeredActive cache short-circuiting repeat
// RegisterBus calls) from both angles:
//
//   - TestBusRegistry_ConcurrentInProcessStormNoGoroutineLeak: a storm of
//     concurrent goroutines against ONE BusSessionManager, mirroring
//     handler_span.go's per-request pattern.
//   - TestBusRegistry_CrossProcessWriterConverges: the storm PLUS a real
//     second OS process (the standard Go "helper process" re-exec idiom,
//     same pattern as cli_reconcile_test.go's TestHelperProcessReconcileSnapshot)
//     writing to the same workspace root, to prove cross-process correctness
//     survives the in-process single-flighting.
//
// Both assert on runtime.NumGoroutine() before/after (no leak) and on final
// on-disk registry state (no lost entries, seq converges once contention
// clears).
package engine

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitForGoroutineBaseline polls runtime.NumGoroutine() until it settles at
// or below baseline+slack, or the timeout elapses. Background goroutines
// belonging to the Go runtime/test harness (GC, timers) can take a moment to
// quiesce after a burst of concurrent work, so a bare before/after equality
// check is flaky; polling with a generous timeout is not.
func waitForGoroutineBaseline(t *testing.T, baseline, slack int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= baseline+slack {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine count did not settle: baseline=%d slack=%d final=%d (leak suspected)", baseline, slack, last)
}

// TestBusRegistry_ConcurrentInProcessStormNoGoroutineLeak simulates
// handler_span.go's access pattern under load: many concurrent goroutines
// each repeatedly call RegisterBus (same busID, mirroring "register on every
// request") and AppendEvent on a SINGLE BusSessionManager. Before the fix,
// every one of these calls independently raced for the cross-process
// filelock; this test's goroutine/worker counts are chosen to be large
// enough that the pre-fix version of this code visibly queues real syscalls
// concurrently.
func TestBusRegistry_ConcurrentInProcessStormNoGoroutineLeak(t *testing.T) {
	root := t.TempDir()
	m := NewBusSessionManager(root)
	const busID = "bus_traces"
	const workers = 150
	const eventsPerWorker = 20
	const wantTotal = workers * eventsPerWorker

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < eventsPerWorker; j++ {
				// Every call: mirrors withSpan's emitSpan, which calls
				// RegisterBus unconditionally before every AppendEvent.
				if err := m.RegisterBus(busID, fmt.Sprintf("sess-%d", worker), "test"); err != nil {
					errs <- fmt.Errorf("worker %d: RegisterBus: %w", worker, err)
					return
				}
				if _, err := m.AppendEvent(busID, "kernel.handler.span.v1", "kernel", map[string]interface{}{
					"worker": worker, "seq": j,
				}); err != nil {
					errs <- fmt.Errorf("worker %d: AppendEvent: %w", worker, err)
					return
				}
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("storm did not complete within 30s — suspect a deadlock or unbounded blocking wait introduced by the fix")
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// No lost events: events.jsonl must contain every append.
	events, err := m.ReadEvents(busID)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != wantTotal {
		t.Fatalf("expected %d events, got %d (lost writes under contention)", wantTotal, len(events))
	}

	// No lost registry entry: bus_traces must still be the only, correctly
	// registered entry.
	registry := m.LoadRegistry()
	if len(registry) != 1 {
		t.Fatalf("expected exactly 1 registry entry, got %d", len(registry))
	}
	if registry[0].BusID != busID || registry[0].State != "active" {
		t.Fatalf("unexpected registry entry: %+v", registry[0])
	}

	// Best-effort seq metadata is allowed to lag under contention (that is
	// the documented contract), but it must SELF-HEAL: one more append, run
	// after the storm with no concurrent contention, must succeed and bring
	// LastEventSeq to the true total.
	if _, err := m.AppendEvent(busID, "kernel.handler.span.v1", "kernel", map[string]interface{}{"settle": true}); err != nil {
		t.Fatalf("settle AppendEvent: %v", err)
	}
	registry = m.LoadRegistry()
	if got := registry[0].LastEventSeq; got != wantTotal+1 {
		t.Fatalf("registry seq did not converge after contention cleared: want %d, got %d", wantTotal+1, got)
	}

	// The core #505 regression check: no goroutine leak. Every one of the
	// (workers * eventsPerWorker) RegisterBus/AppendEvent calls above either
	// completed its cross-process lock attempt or skipped it — none should
	// leave a goroutine parked forever.
	waitForGoroutineBaseline(t, baseline, 5, 5*time.Second)

	if skips := m.RegistrySeqSkipCount(); skips > 0 {
		t.Logf("registry seq updates skipped under in-process contention: %d (expected — best-effort hot path)", skips)
	}
}

// ── Cross-process regression: real second OS process ────────────────────────

const (
	busRegistryHelperEnv   = "BUS_REGISTRY_CONTENTION_HELPER"
	busRegistryHelperRoot  = "BUS_REGISTRY_CONTENTION_ROOT"
	busRegistryHelperBusID = "BUS_REGISTRY_CONTENTION_BUSID"
	busRegistryHelperCount = "BUS_REGISTRY_CONTENTION_COUNT"
)

// TestHelperProcessBusRegistryWriter is not a real test — it only acts when
// invoked as a subprocess by TestBusRegistry_CrossProcessWriterConverges
// (gated on busRegistryHelperEnv so a normal `go test` run treats it as an
// instant no-op pass). Same idiom as cli_reconcile_test.go's
// TestHelperProcessReconcileSnapshot / os/exec_test.go's TestHelperProcess.
//
// It constructs its OWN independent BusSessionManager against the shared
// workspace root — exactly what a `cogos mcp serve` subprocess does
// alongside a running `cogos serve` daemon (see cli_mcp.go) — and registers
// + appends to a distinct bus concurrently with the parent process's storm.
func TestHelperProcessBusRegistryWriter(t *testing.T) {
	if os.Getenv(busRegistryHelperEnv) != "1" {
		return
	}
	root := os.Getenv(busRegistryHelperRoot)
	busID := os.Getenv(busRegistryHelperBusID)
	count, err := strconv.Atoi(os.Getenv(busRegistryHelperCount))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad count: %v\n", err)
		os.Exit(2)
	}

	m := NewBusSessionManager(root)
	if err := m.RegisterBus(busID, "cli-session", "cli"); err != nil {
		fmt.Fprintf(os.Stderr, "RegisterBus: %v\n", err)
		os.Exit(1)
	}
	for i := 0; i < count; i++ {
		if _, err := m.AppendEvent(busID, "cli.event", "cli", map[string]interface{}{"i": i}); err != nil {
			fmt.Fprintf(os.Stderr, "AppendEvent %d: %v\n", i, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// TestBusRegistry_CrossProcessWriterConverges runs a real second OS process
// (a `cogos mcp serve`-shaped writer, per cli_mcp.go) concurrently with an
// in-process storm against the SAME workspace root — the exact dual-writer
// shape #461 was filed for. It asserts both writers' registrations survive
// (the original F3 last-writer-wins-drops failure mode), the daemon-side
// seq metadata converges once contention clears, and the parent process
// leaks no goroutines despite the child forcing genuine cross-process
// filelock contention (registryFileMu's TryLock skip path is exercised for
// real here, not just simulated in-process).
func TestBusRegistry_CrossProcessWriterConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real subprocess; skipped in -short")
	}
	root := t.TempDir()
	const daemonBusID = "bus_traces"
	const cliBusID = "cli-bus"
	const daemonWorkers = 40
	const daemonEventsPerWorker = 15
	const daemonTotal = daemonWorkers * daemonEventsPerWorker
	const cliCount = 300

	m := NewBusSessionManager(root)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	// Start the real cross-process writer first so it's contending for the
	// registry lock for the full duration of the in-process storm below.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessBusRegistryWriter$")
	cmd.Env = append(os.Environ(),
		busRegistryHelperEnv+"=1",
		busRegistryHelperRoot+"="+root,
		busRegistryHelperBusID+"="+cliBusID,
		busRegistryHelperCount+"="+strconv.Itoa(cliCount),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper subprocess: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, daemonWorkers)
	for i := 0; i < daemonWorkers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < daemonEventsPerWorker; j++ {
				if err := m.RegisterBus(daemonBusID, fmt.Sprintf("sess-%d", worker), "test"); err != nil {
					errs <- fmt.Errorf("worker %d: RegisterBus: %w", worker, err)
					return
				}
				if _, err := m.AppendEvent(daemonBusID, "kernel.handler.span.v1", "kernel", map[string]interface{}{
					"worker": worker, "seq": j,
				}); err != nil {
					errs <- fmt.Errorf("worker %d: AppendEvent: %w", worker, err)
					return
				}
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("in-process storm did not complete within 30s")
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper subprocess failed: %v (stderr=%q)", err, stderr.String())
	}

	// No dropped entries: BOTH writers' buses must survive concurrent
	// cross-process registration — the exact failure mode #461 was filed
	// for (root-CLI-shaped writer + daemon writer, last-writer-wins on
	// registry.json without a shared lock).
	registry := m.LoadRegistry()
	found := map[string]bool{}
	for _, e := range registry {
		found[e.BusID] = true
	}
	if !found[daemonBusID] || !found[cliBusID] {
		t.Fatalf("lost a registry entry under cross-process contention: got %+v, want both %q and %q", registry, daemonBusID, cliBusID)
	}

	// The CLI-shaped writer ran uncontended by other CLI writers (only the
	// daemon's storm competed with it), and its own AppendEvent/loop is
	// strictly sequential, so its on-disk events must be complete.
	cliEvents, err := m.ReadEvents(cliBusID)
	if err != nil {
		t.Fatalf("ReadEvents(%s): %v", cliBusID, err)
	}
	if len(cliEvents) != cliCount {
		t.Fatalf("cross-process writer lost events: want %d, got %d", cliCount, len(cliEvents))
	}

	// Settle: one more uncontended daemon-side append must converge the
	// best-effort seq metadata to the true total, proving no PERMANENT loss
	// even though individual updates were skipped during the storm.
	if _, err := m.AppendEvent(daemonBusID, "kernel.handler.span.v1", "kernel", map[string]interface{}{"settle": true}); err != nil {
		t.Fatalf("settle AppendEvent: %v", err)
	}
	registry = m.LoadRegistry()
	for _, e := range registry {
		if e.BusID == daemonBusID && e.LastEventSeq != daemonTotal+1 {
			t.Fatalf("daemon bus seq did not converge: want %d, got %d", daemonTotal+1, e.LastEventSeq)
		}
	}

	// No goroutine leak in the parent process despite real cross-process
	// filelock contention against the child's writes.
	waitForGoroutineBaseline(t, baseline, 5, 5*time.Second)
}
