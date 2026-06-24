// convergence_tracker.go — per-provider reconcile anomaly self-reporting.
//
// The reconcile daemon runs every provider's cycle on each tick. This tracker
// watches the resulting stream and self-reports providers that are misbehaving,
// so a problem surfaces as a kernel WARN (and a queryable snapshot) instead of
// requiring an operator to notice elevated CPU and hand-trace the logs.
//
// Two clean signals, covering the two failure classes seen in practice:
//
//   - Cost over budget: a provider whose reconcile cycle exceeds CycleBudget for
//     OverBudgetCycles consecutive ticks. Catches a provider "eating CPU" — e.g.
//     a FetchLive that reloads its whole index from disk every cycle.
//   - Persistent degraded: a provider reporting Degraded health for
//     DegradedCycles consecutive ticks. Catches a provider stuck unhealthy — e.g.
//     a misreported Health() that pins the autonomic engine on a standing
//     degraded=1.
//
// "Has changes for N cycles" is deliberately NOT a signal: an actively-used
// provider (e.g. conversations indexing a live session) legitimately changes
// every cycle, so it would false-positive constantly.
//
// Snapshot() exposes the current per-provider status. It is both the operator/
// agent-queryable surface and a test oracle: an integration test can run the
// daemon for N ticks and assert the runtime expression — no provider flagged, or
// a deliberately-broken provider flagged — testing observed behaviour, not just
// code paths.
package engine

import (
	"log/slog"
	"sort"
	"sync"
	"time"
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
	// ReWarnEvery: while still flagged, re-emit the WARN every N ticks so a
	// standing anomaly stays visible without spamming every tick.
	ReWarnEvery int
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
}

type provConvState struct {
	lastCycleMs     int64
	lastFetchMs     int64
	overBudget      int
	degraded        int
	flagged         bool
	cyclesSinceWarn int
}

func (s *provConvState) reasons(cfg ConvergenceConfig) []string {
	var r []string
	if s.overBudget >= cfg.OverBudgetCycles {
		r = append(r, "over_budget")
	}
	if s.degraded >= cfg.DegradedCycles {
		r = append(r, "degraded")
	}
	return r
}

// convergenceTracker accumulates per-provider reconcile-cycle outcomes and emits
// a WARN when a provider crosses (or remains past) an anomaly threshold.
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

// Observe records the outcome of one provider reconcile cycle. cycleMs is the
// full cycle duration; fetchMs is the FetchLive phase (the usual dominant cost);
// degraded is whether the provider reported Degraded health this cycle.
func (t *convergenceTracker) Observe(provider string, cycleMs, fetchMs int64, degraded bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.st[provider]
	if s == nil {
		s = &provConvState{}
		t.st[provider] = s
	}
	s.lastCycleMs = cycleMs
	s.lastFetchMs = fetchMs

	if cycleMs > t.cfg.CycleBudget.Milliseconds() {
		s.overBudget++
	} else {
		s.overBudget = 0
	}
	if degraded {
		s.degraded++
	} else {
		s.degraded = 0
	}

	reasons := s.reasons(t.cfg)
	if len(reasons) > 0 {
		// Anomalous: warn on the transition into anomaly, then periodically.
		if !s.flagged || s.cyclesSinceWarn >= t.cfg.ReWarnEvery {
			slog.Warn("reconcile: provider anomaly",
				"provider", provider,
				"reasons", reasons,
				"cycle_ms", cycleMs,
				"fetch_ms", fetchMs,
				"over_budget_cycles", s.overBudget,
				"degraded_cycles", s.degraded,
				"cycle_budget_ms", t.cfg.CycleBudget.Milliseconds(),
			)
			s.cyclesSinceWarn = 0
		} else {
			s.cyclesSinceWarn++
		}
		s.flagged = true
		return
	}

	// Healthy this cycle: clear and announce recovery once.
	if s.flagged {
		slog.Info("reconcile: provider anomaly cleared", "provider", provider)
	}
	s.flagged = false
	s.cyclesSinceWarn = 0
}

// Snapshot returns the current per-provider convergence status, sorted by
// provider name for stable output. Safe to call concurrently.
func (t *convergenceTracker) Snapshot() []ProviderConvergence {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]ProviderConvergence, 0, len(t.st))
	for name, s := range t.st {
		out = append(out, ProviderConvergence{
			Provider:         name,
			LastCycleMs:      s.lastCycleMs,
			LastFetchMs:      s.lastFetchMs,
			OverBudgetCycles: s.overBudget,
			DegradedCycles:   s.degraded,
			Flagged:          s.flagged,
			Reasons:          s.reasons(t.cfg),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}
