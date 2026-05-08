// autonomic_ticker.go — deterministic control-loop iteration for the kernel.
//
// The autonomic ticker is the "homeostasis is default; consciousness is the
// interrupt" implementation. Each tick:
//
//  1. Probes all registered Reconcilables via Health() — synchronous,
//     near-zero cost by Reconcilable contract.
//  2. Aggregates into a KernelHealthSnapshot.
//  3. Emits the snapshot to the bus_kernel_proprio channel.
//  4. Evaluates the escalation predicate:
//     - Any provider degraded / out-of-sync / operation-in-progress → escalate.
//     - Explicit trigger queued (TriggerAgent) → escalate.
//     - Idle re-checkin window elapsed → escalate.
//     - Otherwise → tick ends; no LLM call.
//
// Only the escalation path calls assessCycle / executeCycleTask.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/myrgic/cogos/pkg/reconcile"
)

// --- Constants ---------------------------------------------------------------

// autonomicBusChannel is the bus channel where the kernel emits
// KernelHealthSnapshot events every tick. Mirrors the bus_tournament naming
// convention from internal/eval.
const autonomicBusChannel = "bus_kernel_proprio"

// autonomicEventFrom is the sender identity used in bus events emitted by the
// autonomic ticker.
const autonomicEventFrom = "kernel-autonomic"

// autonomicEventType is the event type written to the bus.
const autonomicEventType = "kernel.health.snapshot.v1"

// defaultIdleRecheckIn is how long the ticker waits before forcing an LLM
// escalation even when all providers are green. This ensures the maintenance
// agent checks in periodically rather than being silent indefinitely.
const defaultIdleRecheckIn = 1 * time.Hour

// --- Types -------------------------------------------------------------------

// KernelHealthSnapshot is the aggregate view produced each tick.
type KernelHealthSnapshot struct {
	Timestamp time.Time                         `json:"timestamp"`
	Providers map[string]reconcile.ResourceStatus `json:"providers"`
	Counts    HealthCounts                      `json:"counts"`
}

// HealthCounts is the four-bucket summary.
type HealthCounts struct {
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Missing   int `json:"missing"`
	Suspended int `json:"suspended"`
}

// AllGreen reports whether every provider is Synced/Healthy/(Idle or empty).
// A snapshot with zero providers is considered green — the ticker should only
// escalate when something is observably wrong, not when the registry is empty.
func (s KernelHealthSnapshot) AllGreen() bool {
	return s.Counts.Degraded == 0 && s.Counts.Missing == 0 && s.Counts.Suspended == 0
}

// HasOperationInProgress reports whether any provider is currently running an
// apply operation (Syncing / Waiting). Operations in progress don't
// necessarily warrant LLM attention — the system is already converging. We
// escalate only on health degradation. This is exposed for test introspection.
func (s KernelHealthSnapshot) HasOperationInProgress() bool {
	for _, st := range s.Providers {
		if st.Operation == reconcile.OperationSyncing ||
			st.Operation == reconcile.OperationWaiting {
			return true
		}
	}
	return false
}

// HasOutOfSync reports whether any provider's Sync axis is not Synced.
func (s KernelHealthSnapshot) HasOutOfSync() bool {
	for _, st := range s.Providers {
		if st.Sync != reconcile.SyncStatusSynced && st.Sync != "" {
			return true
		}
	}
	return false
}

// --- AutonomicConfig ---------------------------------------------------------

// AutonomicConfig holds tunables for the autonomic ticker. Zero values are
// filled with defaults at runtime. Embedding in Config is deferred for v1;
// the controller reads from this struct directly.
type AutonomicConfig struct {
	// IdleRecheckIn is the maximum time the ticker will go without escalating
	// to an LLM cycle, even when all providers are green. Default 1h.
	IdleRecheckIn time.Duration
}

func (a AutonomicConfig) idleRecheckIn() time.Duration {
	if a.IdleRecheckIn > 0 {
		return a.IdleRecheckIn
	}
	return defaultIdleRecheckIn
}

// --- Snapshot builder --------------------------------------------------------

// buildKernelHealthSnapshot probes all registered Reconcilables and returns an
// aggregate snapshot. Uses the same probeAllProviders machinery as
// buildHealthBlock (context_blocks_health.go) so the two surfaces stay
// consistent.
func buildKernelHealthSnapshot(ctx context.Context) KernelHealthSnapshot {
	snap := KernelHealthSnapshot{
		Timestamp: time.Now().UTC(),
		Providers: make(map[string]reconcile.ResourceStatus),
	}

	names := reconcile.ListProviders()
	if len(names) == 0 {
		return snap
	}

	samples := probeAllProviders(ctx, names)
	for _, s := range samples {
		snap.Providers[s.Name] = s.Status
		switch {
		case s.Status.Health == reconcile.HealthDegraded:
			snap.Counts.Degraded++
		case s.Status.Health == reconcile.HealthMissing:
			snap.Counts.Missing++
		case s.Status.Health == reconcile.HealthSuspended:
			snap.Counts.Suspended++
		default:
			// Healthy or Progressing — count as healthy for escalation purposes.
			snap.Counts.Healthy++
		}
	}
	return snap
}

// --- Escalation predicate ----------------------------------------------------

// escalationReason describes why a tick escalated to an LLM call.
type escalationReason string

const (
	escalateDegradedHealth   escalationReason = "degraded_health"
	escalateOutOfSync        escalationReason = "out_of_sync"
	escalateExplicitTrigger  escalationReason = "explicit_trigger"
	escalateIdleRecheckIn    escalationReason = "idle_recheckin"
)

// shouldEscalate returns a non-empty reason if the tick should route to the
// LLM assess/execute path, or "" if the tick should end after deterministic
// work. triggerPending is true when an external TriggerAgent call has been
// queued; lastLLMCycle is the wall-clock time the last LLM cycle ran.
func shouldEscalate(snap KernelHealthSnapshot, triggerPending bool, lastLLMCycle time.Time, cfg AutonomicConfig) escalationReason {
	// Health degradation is the highest-priority signal.
	if !snap.AllGreen() {
		return escalateDegradedHealth
	}
	// OutOfSync on any provider (without health degradation) still warrants
	// attention — the declared state and live state have diverged.
	if snap.HasOutOfSync() {
		return escalateOutOfSync
	}
	// Explicit trigger from TriggerAgent — bypass idle window.
	if triggerPending {
		return escalateExplicitTrigger
	}
	// Idle re-checkin: force at least one LLM cycle per hour so the agent
	// doesn't become completely silent on healthy workspaces.
	window := cfg.idleRecheckIn()
	if lastLLMCycle.IsZero() || time.Since(lastLLMCycle) >= window {
		return escalateIdleRecheckIn
	}
	return ""
}

// --- Bus emission ------------------------------------------------------------

// emitHealthSnapshot writes the snapshot to the bus_kernel_proprio channel.
// If the bus manager is nil (common in tests or stripped-down boots) the call
// is a no-op. Errors are logged but never returned — failing to emit
// telemetry should not affect the control loop.
func emitHealthSnapshot(ctx context.Context, mgr *BusSessionManager, snap KernelHealthSnapshot) {
	if mgr == nil {
		return
	}

	payload, err := snapshotToPayload(snap)
	if err != nil {
		slog.Warn("autonomic: failed to marshal health snapshot for bus emit", "err", err)
		return
	}

	if err := mgr.EnsureBus(autonomicBusChannel); err != nil {
		slog.Warn("autonomic: failed to ensure bus channel", "channel", autonomicBusChannel, "err", err)
		return
	}

	if _, err := mgr.AppendEvent(autonomicBusChannel, autonomicEventType, autonomicEventFrom, payload); err != nil {
		slog.Warn("autonomic: failed to emit health snapshot to bus", "channel", autonomicBusChannel, "err", err)
	}
}

// snapshotFingerprint returns a deterministic hash of the per-provider health
// shape (name, Health, Sync) sorted by name. Two snapshots with the same
// fingerprint represent the same provider population in the same buckets —
// the caller can treat them as the same event for the purpose of suppressing
// repeat LLM escalation. Operation and Message are intentionally excluded:
// Operation transitions are short-lived and Message often carries timestamps
// or counters that would defeat the dedupe.
//
// Returns "" when the snapshot has no providers (an empty registry has no
// degradation to dedupe).
func snapshotFingerprint(snap KernelHealthSnapshot) string {
	if len(snap.Providers) == 0 {
		return ""
	}
	names := make([]string, 0, len(snap.Providers))
	for n := range snap.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		st := snap.Providers[n]
		h.Write([]byte(n))
		h.Write([]byte{'|'})
		h.Write([]byte(st.Health))
		h.Write([]byte{'|'})
		h.Write([]byte(st.Sync))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- Self-healing reconcile loop ---------------------------------------------

// healDegradedProviders iterates all registered Reconcilables and, for any
// whose Health() is not Healthy, runs the full plan/apply cycle:
//
//  1. FetchLive — probe actual state.
//  2. ComputePlan — diff declared vs live.
//  3. ApplyPlan — execute any corrective actions (e.g. start a crashed process).
//
// This is the deterministic self-healing path. It runs on every tick before the
// LLM escalation predicate so that transient crashes (mlx_lm.server exited) are
// corrected autonomically without waking the LLM. Errors are logged but never
// propagate — a failed apply leaves Health() degraded, which will trigger the
// LLM escalation predicate on the same tick if needed.
func healDegradedProviders(ctx context.Context) {
	names := reconcile.ListProviders()
	for _, name := range names {
		p, err := reconcile.GetProvider(name)
		if err != nil {
			continue
		}
		h := p.Health()
		// Only attempt self-heal when the provider is non-healthy (Degraded,
		// Missing, OutOfSync). Suspended and Progressing providers are either
		// intentionally paused or already converging — skip them.
		needsHeal := h.Health == reconcile.HealthDegraded ||
			h.Health == reconcile.HealthMissing ||
			h.Sync == reconcile.SyncStatusOutOfSync

		if !needsHeal {
			continue
		}

		slog.Info("autonomic: self-heal: starting reconcile cycle",
			"provider", name,
			"health", string(h.Health),
			"sync", string(h.Sync),
		)

		// Load config (no-op for most providers that parse config at construction).
		cfg, err := p.LoadConfig("")
		if err != nil {
			slog.Warn("autonomic: self-heal: LoadConfig failed", "provider", name, "err", err)
			continue
		}

		// Fetch live state.
		live, err := p.FetchLive(ctx, cfg)
		if err != nil {
			slog.Warn("autonomic: self-heal: FetchLive failed", "provider", name, "err", err)
			continue
		}

		// Compute plan.
		plan, err := p.ComputePlan(cfg, live, nil)
		if err != nil {
			slog.Warn("autonomic: self-heal: ComputePlan failed", "provider", name, "err", err)
			continue
		}
		if plan == nil || !plan.Summary.HasChanges() {
			slog.Debug("autonomic: self-heal: no changes needed", "provider", name)
			continue
		}

		// Apply plan.
		results, err := p.ApplyPlan(ctx, plan)
		if err != nil {
			slog.Warn("autonomic: self-heal: ApplyPlan failed",
				"provider", name,
				"err", err,
				"results", len(results),
			)
			continue
		}
		slog.Info("autonomic: self-heal: apply complete",
			"provider", name,
			"actions", len(plan.Actions),
			"results", len(results),
		)
	}
}

// snapshotToPayload serialises KernelHealthSnapshot into the map[string]any
// shape expected by BusSessionManager.AppendEvent.
func snapshotToPayload(snap KernelHealthSnapshot) (map[string]any, error) {
	// Round-trip through JSON to get a map[string]any cleanly.
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal to payload: %w", err)
	}
	return payload, nil
}
