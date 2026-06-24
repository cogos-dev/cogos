// reconcile_daemon.go — daemon-resident continuous reconcile loop driver.
//
// ReconcileDaemon closes ADR-092 §2 step 4: "Reconcile loop start — periodic /
// on-demand reconciliation begins." It periodically iterates all registered
// Reconcilables and drives the full per-provider cycle:
//
//	LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → WriteState
//
// Each provider's cycle is error-isolated: a panic or error in one provider
// does not block or terminate the cycles of other providers (ADR-092 §3).
//
// Per-provider error isolation is non-negotiable per ADR-092 §3 at-least-once
// semantics: one bad Reconcilable must not block others.
//
// ADR-091 Layer: Kernel (requires a running process; consumes Substrate
// primitives pkg/reconcile.Reconcilable + process-local registry).
// ADR-095: this file implements the contract specified therein.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ReconcileDaemonState represents the lifecycle state of the daemon.
type ReconcileDaemonState string

const (
	// ReconcileDaemonStarting is the initial state before the goroutine starts.
	ReconcileDaemonStarting ReconcileDaemonState = "Starting"
	// ReconcileDaemonLive means the goroutine is running; last tick had no errors.
	ReconcileDaemonLive ReconcileDaemonState = "Live"
	// ReconcileDaemonStalled means running but last tick had provider errors.
	// The daemon continues ticking.
	ReconcileDaemonStalled ReconcileDaemonState = "Stalled"
	// ReconcileDaemonShutdown means context was cancelled; goroutine has exited.
	ReconcileDaemonShutdown ReconcileDaemonState = "Shutdown"
)

// ReconcileDaemonConfig holds configuration for the ReconcileDaemon.
type ReconcileDaemonConfig struct {
	// WorkspaceRoot is the workspace root path, passed to LoadConfig and
	// reconcile.LoadState / reconcile.WriteState.
	WorkspaceRoot string

	// PollInterval is how often all registered providers are iterated.
	// Default: 30 seconds.
	PollInterval time.Duration

	// MaxConcurrent is the maximum number of providers reconciled concurrently
	// within a single tick. Default: 1 (serial) per ADR-095 §5 rationale:
	// serializes all providers to avoid ADR-092 §1 ledger chain-break risk until
	// the per-session mutex is added to pkg/cogblock.AppendEvent.
	MaxConcurrent int

	// ShutdownGracePeriod is how long the daemon waits for in-flight cycles to
	// complete after context cancel before returning. Default: 5 seconds.
	ShutdownGracePeriod time.Duration

	// Providers, when non-nil, is an explicit list of Reconcilables the daemon
	// iterates instead of the global registry. This is the ADR-101 Phase 2
	// registry-isolation mechanism: integration tests inject real providers here
	// without touching the global registry (which may contain daemon-binary stub
	// providers). When nil, the daemon uses reconcile.ListProviders() as before.
	// Production code leaves this nil; only testkernel sets it.
	Providers []reconcile.Reconcilable

	// Convergence tunes the per-provider anomaly thresholds (cost-over-budget
	// and persistent-degraded). Zero values fall back to sensible defaults.
	Convergence ConvergenceConfig
}

func (c *ReconcileDaemonConfig) withDefaults() ReconcileDaemonConfig {
	cfg := *c
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.ShutdownGracePeriod <= 0 {
		cfg.ShutdownGracePeriod = 5 * time.Second
	}
	return cfg
}

// ReconcileDaemon is the daemon-resident goroutine that drives the full
// reconcile cycle for all registered Reconcilables on each periodic tick.
//
// Start the daemon with Start(ctx). Stop it by cancelling the context.
// Use Trigger to queue an early (out-of-band) reconcile for a specific provider.
type ReconcileDaemon struct {
	cfg ReconcileDaemonConfig

	mu    sync.Mutex
	state ReconcileDaemonState

	// triggerCh is a non-blocking channel for early trigger requests.
	// Keys are provider type strings. The map is drained at the start of
	// each tick and after each regular tick.
	triggerMu sync.Mutex
	triggered map[string]struct{}
	triggerCh chan struct{} // notifies the loop that triggers are queued

	// health self-reports per-provider reconcile anomalies (slow cycles,
	// persistent degraded health) as WARNs and a queryable snapshot.
	health *convergenceTracker
}

// NewReconcileDaemon creates a ReconcileDaemon with the given config.
// Call Start(ctx) to begin the loop.
func NewReconcileDaemon(cfg ReconcileDaemonConfig) *ReconcileDaemon {
	return &ReconcileDaemon{
		cfg:       cfg.withDefaults(),
		state:     ReconcileDaemonStarting,
		triggered: make(map[string]struct{}),
		triggerCh: make(chan struct{}, 1),
		health:    newConvergenceTracker(cfg.Convergence),
	}
}

// ProviderConvergence returns the current per-provider reconcile anomaly
// snapshot (cost-over-budget / persistent-degraded). It is the queryable surface
// for operators and agents, and a test oracle: assert on observed runtime
// behaviour after running the daemon, not just on code paths.
func (d *ReconcileDaemon) ProviderConvergence() []ProviderConvergence {
	return d.health.Snapshot()
}

// observeConvergence feeds one completed cycle's cost and a fresh Health() read
// (for the degraded axis) to the anomaly tracker.
func (d *ReconcileDaemon) observeConvergence(providerType string, provider reconcile.Reconcilable, cycleMs, fetchMs int64) {
	degraded := provider.Health().Health == reconcile.HealthDegraded
	d.health.Observe(providerType, cycleMs, fetchMs, degraded)
}

// State returns the current lifecycle state of the daemon.
func (d *ReconcileDaemon) State() ReconcileDaemonState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// ─── Isolated-registry helpers ───────────────────────────────────────────────
//
// When cfg.Providers is nil these helpers delegate to the global registry,
// preserving all pre-Phase-2 behaviour exactly. When cfg.Providers is set
// (testkernel isolation path) they operate solely on that list.

// hasProvider reports whether providerType is available in the active provider
// source (injected list or global registry).
func (d *ReconcileDaemon) hasProvider(pt string) bool {
	if d.cfg.Providers != nil {
		for _, p := range d.cfg.Providers {
			if p.Type() == pt {
				return true
			}
		}
		return false
	}
	return reconcile.HasProvider(pt)
}

// listProviderTypes returns the ordered type strings for all active providers.
func (d *ReconcileDaemon) listProviderTypes() []string {
	if d.cfg.Providers != nil {
		names := make([]string, len(d.cfg.Providers))
		for i, p := range d.cfg.Providers {
			names[i] = p.Type()
		}
		return names
	}
	return reconcile.ListProviders()
}

// getProvider returns the Reconcilable for the given type from the active
// provider source (injected list or global registry).
func (d *ReconcileDaemon) getProvider(pt string) (reconcile.Reconcilable, error) {
	if d.cfg.Providers != nil {
		for _, p := range d.cfg.Providers {
			if p.Type() == pt {
				return p, nil
			}
		}
		return nil, fmt.Errorf("provider %q not in injected list", pt)
	}
	return reconcile.GetProvider(pt)
}

// Trigger queues an immediate (out-of-band) reconcile for the named provider
// type. Non-blocking: if the provider is already queued, this is a no-op.
// If the provider is not registered (or not in the injected list when one is
// set), the trigger is silently dropped.
//
// This is the integration point for watch-based early-trigger mechanisms
// (e.g., ProjectionWatcher). See ADR-095 §4.
func (d *ReconcileDaemon) Trigger(providerType string) {
	if !d.hasProvider(providerType) {
		return
	}
	d.triggerMu.Lock()
	d.triggered[providerType] = struct{}{}
	d.triggerMu.Unlock()

	// Non-blocking notify: if the channel already has a notification queued,
	// the loop will drain triggers when it wakes. No need to send again.
	select {
	case d.triggerCh <- struct{}{}:
	default:
	}
}

// Start begins the reconcile loop in a background goroutine. The goroutine
// exits when ctx is cancelled. Start is safe to call only once.
func (d *ReconcileDaemon) Start(ctx context.Context) {
	d.mu.Lock()
	d.state = ReconcileDaemonLive
	d.mu.Unlock()

	slog.Info("reconcile-daemon: starting",
		"poll_interval", d.cfg.PollInterval,
		"max_concurrent", d.cfg.MaxConcurrent,
		"workspace", d.cfg.WorkspaceRoot,
	)

	go d.run(ctx)
}

// run is the main goroutine body. It runs until ctx is cancelled.
func (d *ReconcileDaemon) run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	defer func() {
		d.mu.Lock()
		d.state = ReconcileDaemonShutdown
		d.mu.Unlock()
		slog.Info("reconcile-daemon: stopped")
	}()

	for {
		select {
		case <-ctx.Done():
			// Drain any queued triggers with the grace period.
			d.drainTriggersOnShutdown(ctx)
			return

		case <-d.triggerCh:
			// Early trigger from a watcher or external caller.
			d.runTriggered(ctx)

		case <-ticker.C:
			// Periodic tick: reconcile all registered providers.
			d.runTick(ctx)
		}
	}
}

// runTick iterates all providers and runs their reconcile cycle.
// When cfg.Providers is set, iterates the injected list; otherwise uses the
// global registry. Also drains any pending triggers before iterating (so a
// trigger that fired between ticks is absorbed into this tick rather than
// generating a double run).
func (d *ReconcileDaemon) runTick(ctx context.Context) {
	// Absorb any outstanding triggers into this tick (they'll be covered by
	// the full provider scan).
	d.triggerMu.Lock()
	d.triggered = make(map[string]struct{})
	d.triggerMu.Unlock()

	providers := d.listProviderTypes()
	if len(providers) == 0 {
		return
	}

	slog.Debug("reconcile-daemon: tick", "provider_count", len(providers))

	errCount := d.runProviders(ctx, providers)

	d.mu.Lock()
	if errCount > 0 {
		d.state = ReconcileDaemonStalled
	} else {
		d.state = ReconcileDaemonLive
	}
	d.mu.Unlock()
}

// runTriggered drains the pending trigger set and runs cycles for only the
// queued providers. Used for early (out-of-band) reconcile requests.
func (d *ReconcileDaemon) runTriggered(ctx context.Context) {
	d.triggerMu.Lock()
	queued := d.triggered
	d.triggered = make(map[string]struct{})
	d.triggerMu.Unlock()

	if len(queued) == 0 {
		return
	}

	types := make([]string, 0, len(queued))
	for t := range queued {
		types = append(types, t)
	}
	slog.Debug("reconcile-daemon: triggered", "providers", types)
	d.runProviders(ctx, types)
}

// drainTriggersOnShutdown runs any pending triggers within the shutdown grace period.
func (d *ReconcileDaemon) drainTriggersOnShutdown(ctx context.Context) {
	d.triggerMu.Lock()
	queued := d.triggered
	d.triggered = make(map[string]struct{})
	d.triggerMu.Unlock()

	if len(queued) == 0 {
		return
	}

	graceCtx, cancel := context.WithTimeout(context.Background(), d.cfg.ShutdownGracePeriod)
	defer cancel()

	types := make([]string, 0, len(queued))
	for t := range queued {
		types = append(types, t)
	}
	slog.Info("reconcile-daemon: draining triggers on shutdown", "providers", types)
	d.runProviders(graceCtx, types)
}

// runProviders runs the reconcile cycle for each named provider type.
// Providers are run with MaxConcurrent parallelism (default 1 = serial).
// Per-provider error isolation: panics and errors are recovered and logged;
// other providers in the same batch continue unaffected.
// Returns the number of providers that errored.
func (d *ReconcileDaemon) runProviders(ctx context.Context, providerTypes []string) int {
	if d.cfg.MaxConcurrent <= 1 {
		// Serial path: simple loop, no goroutines needed.
		errCount := 0
		for _, pt := range providerTypes {
			if err := d.runOneCycle(ctx, pt); err != nil {
				errCount++
			}
			// Bail early if context is done.
			if ctx.Err() != nil {
				break
			}
		}
		return errCount
	}

	// Concurrent path: semaphore-bounded goroutines.
	sem := make(chan struct{}, d.cfg.MaxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCount := 0

	for _, pt := range providerTypes {
		if ctx.Err() != nil {
			break
		}
		pt := pt
		sem <- struct{}{} // acquire
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release
			if err := d.runOneCycle(ctx, pt); err != nil {
				mu.Lock()
				errCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errCount
}

// runOneCycle runs the full reconcile cycle for a single provider type.
// The cycle is:
//
//	LoadConfig → FetchLive → ComputePlan → ApplyPlan → BuildState → WriteState
//
// Panics are recovered and returned as errors to preserve error isolation.
// Conforms to ADR-092 §4 Reconcilable contract order.
func (d *ReconcileDaemon) runOneCycle(ctx context.Context, providerType string) (retErr error) {
	// Recover from panics so one misbehaving provider can't take down the loop.
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic in provider %s: %v", providerType, r)
			slog.Error("reconcile-daemon: provider panicked",
				"provider", providerType,
				"panic", r,
			)
		}
	}()

	tracer := otel.Tracer("cogos.reconcile-daemon")
	spanCtx, span := tracer.Start(ctx, "reconcile.daemon.cycle")
	span.SetAttributes(attribute.String("provider.type", providerType))
	defer span.End()

	start := time.Now()

	provider, err := d.getProvider(providerType)
	if err != nil {
		slog.Warn("reconcile-daemon: provider not found", "provider", providerType, "err", err)
		return err
	}

	// Per-phase timing. Each reconcile step is timed so a slow phase — e.g. an
	// O(corpus) FetchLive index reload, or a large WriteState — is visible in
	// the cycle-complete log and on the span, instead of only the opaque cycle
	// total. This makes "which phase is hot?" answerable from telemetry alone,
	// without attaching a profiler.
	var loadMs, fetchMs, stateMs, planMs, applyMs, writeMs int64

	// Step 1: LoadConfig — read-only disk operation.
	loadStart := time.Now()
	config, err := provider.LoadConfig(d.cfg.WorkspaceRoot)
	loadMs = time.Since(loadStart).Milliseconds()
	if err != nil {
		slog.Warn("reconcile-daemon: LoadConfig failed",
			"provider", providerType, "err", err)
		return fmt.Errorf("LoadConfig %s: %w", providerType, err)
	}

	// Step 2: FetchLive — read-only observation of world state.
	fetchStart := time.Now()
	live, err := provider.FetchLive(spanCtx, config)
	fetchMs = time.Since(fetchStart).Milliseconds()
	if err != nil {
		slog.Warn("reconcile-daemon: FetchLive failed",
			"provider", providerType, "err", err)
		return fmt.Errorf("FetchLive %s: %w", providerType, err)
	}

	// Step 3: Load persisted state.
	stateStart := time.Now()
	state, stateErr := reconcile.LoadState(d.cfg.WorkspaceRoot, providerType)
	if stateErr != nil {
		// LoadState maps a missing file to (nil, nil); a non-nil error here is
		// corruption or a permission/read fault. Surface it instead of silently
		// resetting lineage serials, mirroring the WriteState warning below.
		// Continue with the nil state — providers already handle a nil state.
		slog.Warn("reconcile-daemon: LoadState failed; continuing with empty state",
			"provider", providerType, "err", stateErr)
	}
	stateMs = time.Since(stateStart).Milliseconds()

	// Step 4: ComputePlan — pure function, deterministic.
	planStart := time.Now()
	plan, err := provider.ComputePlan(config, live, state)
	planMs = time.Since(planStart).Milliseconds()
	if err != nil {
		slog.Warn("reconcile-daemon: ComputePlan failed",
			"provider", providerType, "err", err)
		return fmt.Errorf("ComputePlan %s: %w", providerType, err)
	}

	// phaseAttrs records the per-phase timings on the cycle span. Called at both
	// exits (early "in sync" return and full cycle) so traces always carry the
	// breakdown.
	phaseAttrs := func() []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.Int64("phase.load_config_ms", loadMs),
			attribute.Int64("phase.fetch_live_ms", fetchMs),
			attribute.Int64("phase.load_state_ms", stateMs),
			attribute.Int64("phase.compute_plan_ms", planMs),
			attribute.Int64("phase.apply_plan_ms", applyMs),
			attribute.Int64("phase.write_state_ms", writeMs),
		}
	}

	span.SetAttributes(
		attribute.Int("plan.creates", plan.Summary.Creates),
		attribute.Int("plan.updates", plan.Summary.Updates),
		attribute.Int("plan.deletes", plan.Summary.Deletes),
		attribute.Int("plan.skipped", plan.Summary.Skipped),
	)

	if !plan.Summary.HasChanges() {
		// No drift — log at debug level and exit early; no write needed.
		dur := time.Since(start)
		span.SetAttributes(attribute.Int64("cycle.duration_ms", dur.Milliseconds()))
		span.SetAttributes(phaseAttrs()...)
		slog.Debug("reconcile-daemon: provider in sync",
			"provider", providerType,
			"skipped", plan.Summary.Skipped,
			"fetch_ms", fetchMs,
			"plan_ms", planMs,
			"duration_ms", dur.Milliseconds(),
		)
		d.observeConvergence(providerType, provider, dur.Milliseconds(), fetchMs)
		return nil
	}

	// Step 5: ApplyPlan — idempotent per ADR-092 §3.
	applyStart := time.Now()
	results, err := provider.ApplyPlan(spanCtx, plan)
	applyMs = time.Since(applyStart).Milliseconds()
	if err != nil {
		slog.Warn("reconcile-daemon: ApplyPlan failed",
			"provider", providerType, "err", err)
		return fmt.Errorf("ApplyPlan %s: %w", providerType, err)
	}

	// Count apply failures.
	applyFailed := 0
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed {
			applyFailed++
			slog.Warn("reconcile-daemon: action failed",
				"provider", providerType,
				"action", r.Action,
				"name", r.Name,
				"err", r.Error,
			)
		}
	}

	// Steps 6-7: BuildState (pure) + WriteState (atomic tmp+rename), timed
	// together as the persist phase.
	writeStart := time.Now()
	newState, buildErr := provider.BuildState(config, live, state)
	if buildErr == nil && newState != nil {
		// Step 7: WriteState — atomic tmp+rename.
		if writeErr := reconcile.WriteState(d.cfg.WorkspaceRoot, providerType, newState); writeErr != nil {
			slog.Warn("reconcile-daemon: WriteState failed",
				"provider", providerType, "err", writeErr)
		}
	} else if buildErr != nil {
		slog.Warn("reconcile-daemon: BuildState failed",
			"provider", providerType, "err", buildErr)
	}
	writeMs = time.Since(writeStart).Milliseconds()

	dur := time.Since(start)
	span.SetAttributes(attribute.Int64("cycle.duration_ms", dur.Milliseconds()))
	span.SetAttributes(phaseAttrs()...)

	logLevel := slog.LevelInfo
	if applyFailed > 0 {
		logLevel = slog.LevelWarn
	}
	slog.Log(ctx, logLevel, "reconcile-daemon: cycle complete",
		"provider", providerType,
		"creates", plan.Summary.Creates,
		"updates", plan.Summary.Updates,
		"deletes", plan.Summary.Deletes,
		"skipped", plan.Summary.Skipped,
		"apply_failed", applyFailed,
		"load_ms", loadMs,
		"fetch_ms", fetchMs,
		"state_ms", stateMs,
		"plan_ms", planMs,
		"apply_ms", applyMs,
		"write_ms", writeMs,
		"duration_ms", dur.Milliseconds(),
	)

	d.observeConvergence(providerType, provider, dur.Milliseconds(), fetchMs)

	if applyFailed > 0 {
		return fmt.Errorf("provider %s: %d action(s) failed during apply", providerType, applyFailed)
	}
	return nil
}
