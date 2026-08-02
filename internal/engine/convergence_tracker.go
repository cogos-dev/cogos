// convergence_tracker.go — per-provider reconcile anomaly self-reporting.
//
// The reconcile daemon runs every provider's cycle on each tick. This tracker
// watches the resulting stream and self-reports providers that are misbehaving,
// so a problem surfaces as a kernel WARN (and a queryable snapshot) instead of
// requiring an operator to notice elevated CPU and hand-trace the logs.
//
// Three signals, covering the failure classes seen in practice:
//
//   - Cost over budget: a provider whose reconcile cycle exceeds CycleBudget for
//     OverBudgetCycles consecutive ticks. Catches a provider "eating CPU" — e.g.
//     a FetchLive that reloads its whole index from disk every cycle.
//   - Persistent degraded: a provider reporting Degraded health for
//     DegradedCycles consecutive ticks. Catches a provider stuck unhealthy — e.g.
//     a misreported Health() that pins the autonomic engine on a standing
//     degraded=1.
//   - Quarantined: the daemon has stopped actuating a provider after
//     ReconcileDaemonConfig.QuarantineAfter consecutive failed cycles. Without
//     this axis a quarantined provider would go quiet AND uncounted — the
//     failure mode where suppressing noise also suppresses the signal.
//
// "Has changes for N cycles" is deliberately NOT a signal: an actively-used
// provider (e.g. conversations indexing a live session) legitimately changes
// every cycle, so it would false-positive constantly.
//
// # Episode identity (why the three messages differ)
//
// A standing anomaly must stay visible — a condition that is still broken must
// keep saying so. But "still broken" and "newly broken" are different events,
// and before this change both were emitted under the identical message
// "reconcile: provider anomaly". Any consumer counting occurrences of that
// string therefore counted one continuous condition once per ReWarnEvery
// window forever: a single never-clearing provider drove the operator's
// anomaly counter from 142 to 236 in under 24h, at which point the counter
// stopped distinguishing "one thing is broken" from "the substrate is on
// fire". An alarm that is always firing has stopped being an alarm.
//
// So the lifecycle is now three distinct messages, each carrying an explicit
// "lifecycle" attribute and a monotonic per-provider "episode" number:
//
//	raised  → "reconcile: provider anomaly"        WARN  (counts as one anomaly)
//	persist → "reconcile: anomaly still open"      WARN  (heartbeat, counts as zero)
//	cleared → "reconcile: provider anomaly cleared" INFO  (closes the episode)
//
// The persist message deliberately does NOT contain the substring "provider
// anomaly". That is load-bearing, not cosmetic: the operator's out-of-repo
// vitals probe counts raises by grepping for exactly that substring (and
// excluding "provider anomaly cleared"), so choosing this wording makes an
// already-deployed, unmodified hook count episodes correctly the moment the
// kernel restarts — no coordinated deploy. TestConvergenceTracker_
// PersistMessageIsNotCountedAsARaise pins that contract in-tree; do not
// reword these constants without reading it.
//
// Snapshot() exposes the current per-provider status and is the authoritative
// structural surface (episode number, open duration, quarantine state), so a
// consumer never has to grep log text at all. It is both the operator/
// agent-queryable surface — served at GET /v1/reconcile/convergence — and a
// test oracle: an integration test can run the daemon for N ticks and assert
// the runtime expression, testing observed behaviour rather than code paths.
package engine

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Anomaly lifecycle log messages. See the package-level note above: the
// substring relationship between these three is a cross-surface contract with
// the operator's vitals probe, pinned by test.
const (
	msgAnomalyRaised  = "reconcile: provider anomaly"
	msgAnomalyPersist = "reconcile: anomaly still open"
	msgAnomalyCleared = "reconcile: provider anomaly cleared"
)

// ConvergenceConfig tunes the anomaly thresholds. Zero values fall back to
// withDefaults().
type ConvergenceConfig struct {
	// CycleBudget: a cycle slower than this counts as over budget.
	CycleBudget time.Duration
	// OverBudgetCycles: consecutive over-budget ticks before flagging.
	OverBudgetCycles int
	// DegradedCycles: consecutive Degraded-health ticks before flagging.
	DegradedCycles int
	// ReWarnEvery: while still flagged, re-emit the persist heartbeat every N
	// ticks so a standing anomaly stays visible without spamming every tick.
	ReWarnEvery int
	// ClearCycles: consecutive healthy ticks required to CLOSE an episode.
	// Defaults to DegradedCycles so raising and clearing are symmetric.
	//
	// Without this hysteresis, raising took 3 consecutive bad cycles but
	// clearing took a single good one, so a provider straddling a threshold
	// (observed: `conversations` at ~500ms against a 500ms CycleBudget)
	// churned ~28 raise/clear episode pairs per day. The net counter blinks
	// 0<->1 while the episode count climbs — unreadable in a different way
	// from a counter that only climbs.
	ClearCycles int
}

func (c ConvergenceConfig) withDefaults() ConvergenceConfig {
	if c.CycleBudget <= 0 {
		c.CycleBudget = 500 * time.Millisecond
	}
	if c.OverBudgetCycles <= 0 {
		c.OverBudgetCycles = 3
	}
	if c.DegradedCycles <= 0 {
		c.DegradedCycles = 3
	}
	if c.ReWarnEvery <= 0 {
		c.ReWarnEvery = 20
	}
	if c.ClearCycles <= 0 {
		c.ClearCycles = c.DegradedCycles
	}
	return c
}

// ProviderConvergence is a queryable snapshot of one provider's recent reconcile
// behaviour.
type ProviderConvergence struct {
	Provider         string   `json:"provider"`
	LastCycleMs      int64    `json:"last_cycle_ms"`
	LastFetchMs      int64    `json:"last_fetch_ms"`
	OverBudgetCycles int      `json:"over_budget_cycles"`
	DegradedCycles   int      `json:"degraded_cycles"`
	Flagged          bool     `json:"flagged"`
	Reasons          []string `json:"reasons,omitempty"`

	// Episode is the monotonic per-provider anomaly episode counter. A
	// consumer wanting "how many distinct anomalies has this provider had"
	// reads this rather than counting log lines.
	Episode int `json:"episode,omitempty"`
	// OpenSeconds is how long the current episode has been open; 0 when not
	// flagged.
	OpenSeconds int64 `json:"open_seconds,omitempty"`

	// Quarantined reports that the daemon has stopped ACTUATING this provider
	// (it is still observed — see ReconcileDaemon.runOneCycle). Populated by
	// ReconcileDaemon.ProviderConvergence, not by the tracker itself.
	Quarantined bool `json:"quarantined,omitempty"`
	// QuarantinedSince is RFC3339 when quarantine began; empty when not
	// quarantined.
	QuarantinedSince string `json:"quarantined_since,omitempty"`
	// ConsecutiveFailures is the provider's current failed-cycle streak as
	// tracked by the daemon's backoff.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// Recovery, when non-empty, tells an operator how to resume a quarantined
	// provider.
	Recovery string `json:"recovery,omitempty"`
}

// convObservation is one completed reconcile cycle's outcome, as fed to the
// tracker. A struct rather than positional args because the two booleans are
// otherwise indistinguishable at the call site.
type convObservation struct {
	CycleMs     int64
	FetchMs     int64
	Degraded    bool
	Quarantined bool
}

type provConvState struct {
	lastCycleMs     int64
	lastFetchMs     int64
	overBudget      int
	degraded        int
	quarantined     bool
	flagged         bool
	cyclesSinceWarn int
	healthyStreak   int

	// episode is bumped on each RAISE. openSince timestamps that raise.
	episode   int
	openSince time.Time
	// reasonsAtRaise is the reason set the current episode was opened with,
	// compared each cycle so a CHANGE of shape re-raises (see Observe).
	reasonsAtRaise []string
}

func (s *provConvState) reasons(cfg ConvergenceConfig) []string {
	var r []string
	if s.overBudget >= cfg.OverBudgetCycles {
		r = append(r, "over_budget")
	}
	if s.degraded >= cfg.DegradedCycles {
		r = append(r, "degraded")
	}
	if s.quarantined {
		r = append(r, "quarantined")
	}
	return r
}

// sameReasons compares two reason sets. Both are produced by reasons() in a
// fixed order, so a positional compare suffices.
func sameReasons(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// convergenceTracker accumulates per-provider reconcile-cycle outcomes and emits
// the raise/persist/clear episode lifecycle described in the package comment.
// Thread-safe.
type convergenceTracker struct {
	cfg ConvergenceConfig
	mu  sync.Mutex
	st  map[string]*provConvState
}

func newConvergenceTracker(cfg ConvergenceConfig) *convergenceTracker {
	return &convergenceTracker{
		cfg: cfg.withDefaults(),
		st:  make(map[string]*provConvState),
	}
}

// Observe records the outcome of one provider reconcile cycle and advances the
// anomaly episode state machine.
func (t *convergenceTracker) Observe(provider string, obs convObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.st[provider]
	if s == nil {
		s = &provConvState{}
		t.st[provider] = s
	}
	s.lastCycleMs = obs.CycleMs
	s.lastFetchMs = obs.FetchMs
	s.quarantined = obs.Quarantined

	overBudgetNow := obs.CycleMs > t.cfg.CycleBudget.Milliseconds()
	if overBudgetNow {
		s.overBudget++
	} else {
		s.overBudget = 0
	}
	if obs.Degraded {
		s.degraded++
	} else {
		s.degraded = 0
	}

	// healthyNow is stricter than "no reasons": a cycle that is over budget or
	// degraded but has not yet re-crossed its consecutive-count threshold is
	// NOT evidence of recovery. Counting such a cycle toward the clear streak
	// would defeat the hysteresis for exactly the provider it exists to
	// protect — a threshold straddler alternating over/under never
	// re-accumulates enough consecutive bad cycles to re-raise, but would
	// still drift up to ClearCycles and close the episode, resuming the
	// raise/clear churn.
	healthyNow := !overBudgetNow && !obs.Degraded && !obs.Quarantined

	reasons := s.reasons(t.cfg)
	if len(reasons) > 0 {
		s.healthyStreak = 0
		switch {
		case !s.flagged:
			t.raise(provider, s, reasons, obs)
		case !sameReasons(s.reasonsAtRaise, reasons):
			// The anomaly changed shape (e.g. a degraded provider also crossed
			// its cost budget, or entered quarantine). Close the old episode
			// and open a new one: net-open stays correct for a counter doing
			// raised-minus-cleared, and the reason set a consumer harvests
			// from the raise line stays accurate instead of freezing at
			// whatever the condition looked like when it first appeared.
			t.clear(provider, s)
			t.raise(provider, s, reasons, obs)
		case s.cyclesSinceWarn >= t.cfg.ReWarnEvery:
			// PERSIST: same episode, still open. Still WARN — suppression here
			// is deduplication of a report, never silencing of a condition —
			// but under a message no counter mistakes for a new occurrence.
			s.cyclesSinceWarn = 0
			slog.Warn(msgAnomalyPersist,
				"lifecycle", "persist",
				"provider", provider,
				"episode", s.episode,
				"reasons", reasons,
				"open_for", time.Since(s.openSince).Round(time.Second).String(),
				"degraded_cycles", s.degraded,
				"over_budget_cycles", s.overBudget,
			)
		default:
			s.cyclesSinceWarn++
		}
		return
	}

	// Below every threshold this cycle. Require ClearCycles consecutive
	// genuinely-healthy observations before closing, so a provider oscillating
	// around a threshold does not emit an episode pair every other tick.
	if !healthyNow {
		s.healthyStreak = 0
		return
	}
	s.healthyStreak++
	if s.flagged && s.healthyStreak >= t.cfg.ClearCycles {
		t.clear(provider, s)
	}
}

// raise opens a new anomaly episode. Caller holds t.mu.
//
// This is the ONLY line that means "one more anomaly happened" and the only
// one a counter may tally.
func (t *convergenceTracker) raise(provider string, s *provConvState, reasons []string, obs convObservation) {
	s.episode++
	s.openSince = time.Now()
	s.cyclesSinceWarn = 0
	s.flagged = true
	s.reasonsAtRaise = reasons
	slog.Warn(msgAnomalyRaised,
		"lifecycle", "raised",
		"provider", provider,
		"episode", s.episode,
		"reasons", reasons,
		"cycle_ms", obs.CycleMs,
		"fetch_ms", obs.FetchMs,
		"over_budget_cycles", s.overBudget,
		"degraded_cycles", s.degraded,
		"cycle_budget_ms", t.cfg.CycleBudget.Milliseconds(),
	)
}

// clear closes the current anomaly episode. Caller holds t.mu.
func (t *convergenceTracker) clear(provider string, s *provConvState) {
	slog.Info(msgAnomalyCleared,
		"lifecycle", "cleared",
		"provider", provider,
		"episode", s.episode,
		"open_for", time.Since(s.openSince).Round(time.Second).String(),
	)
	s.flagged = false
	s.cyclesSinceWarn = 0
	s.reasonsAtRaise = nil
}

// Snapshot returns the current per-provider convergence status, sorted by
// provider name for stable output. Safe to call concurrently.
func (t *convergenceTracker) Snapshot() []ProviderConvergence {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]ProviderConvergence, 0, len(t.st))
	for name, s := range t.st {
		var openSeconds int64
		if s.flagged && !s.openSince.IsZero() {
			openSeconds = int64(time.Since(s.openSince).Seconds())
		}
		out = append(out, ProviderConvergence{
			Provider:         name,
			LastCycleMs:      s.lastCycleMs,
			LastFetchMs:      s.lastFetchMs,
			OverBudgetCycles: s.overBudget,
			DegradedCycles:   s.degraded,
			Flagged:          s.flagged,
			Reasons:          s.reasons(t.cfg),
			Episode:          s.episode,
			OpenSeconds:      openSeconds,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}
