// rates.go — GET /v1/kernel/rates / "cogos rates" (First Instruments Module C, M3).
//
// KernelRates computes the complete pairwise-ratio table (all C(6,2)=15
// ratios) over the kernel's six clock constants. This is a DETERMINISTIC-
// ECHO surface (IMPL-SPEC §5.1.1, PREREG §5.1.1): every value here is
// computed without the kernel executing a single reconcile/heartbeat/
// consolidation cycle — the ratios are decided entirely by grid/config
// design, not observed behavior. Module C is retained because a truthful
// rates surface is genuine kernel value (fills a real gap: rate-ratios were
// never first-class before this) and feeds the negative-control battery —
// but these ratios are NOT the invariant search. The invariant search is
// Module E's OBSERVED behavioral cadences (M11/M12/M13). Do not conflate:
// this file surfaces the dial settings, Module E measures the behavior.
//
// The six clock constants, as they actually exist on current main (verified
// against origin/main; no config knob exists for three of them):
//
//   - ConsolidationInterval (C) — live *Config field, seconds. Default 3600.
//   - HeartbeatInterval     (H) — live *Config field, seconds. Default 60.
//   - PollInterval          (P) — ReconcileDaemonConfig field, seconds
//     (time.Duration on the daemon; not a *Config field). Default 30s
//     (ReconcileDaemonConfig.withDefaults). Passed in by the caller since
//     the daemon, not *Config, owns it.
//   - ActiveWindow          (A) — NOT a config field. Hardcoded
//     defaultActiveWithinSeconds=600 (internal/engine/sessions.go). There is
//     no WithActiveWindow knob (verified) — this is exactly why M13r is
//     LAW-CHARACTERIZATION-ONLY in v1 (IMPL-SPEC §1(c), PREREG §9).
//   - HandoffTTL            (T) — NOT a config field; a per-request
//     TTLSeconds with hardcoded default 3600 when the caller omits it
//     (internal/engine/serve_sessions_mgmt.go). Reported here as the
//     default, clearly tagged as such.
//   - IdleRecheck           (I) — NOT a config field. Hardcoded
//     defaultIdleRecheckIn=1h (internal/engine/autonomic_ticker.go), on the
//     LocalHarnessController's autonomic ticker (a different subsystem from
//     the ReconcileDaemon/Process cadence this package otherwise measures).
//
// Three of the six constants have no config knob today; KernelRates reports
// their hardcoded default values so the 15-ratio table is complete and
// honest, and tags every entry as a config-independent echo regardless.
package engine

import (
	"time"
)

// clockConstant names one of the six clock constants entering the ratio
// table, paired with its current value in seconds.
type clockConstant struct {
	name          string
	seconds       float64
	hasConfigKnob bool // false = no config field exists; value is a hardcoded default
}

// RateRatio is one pairwise ratio entry in the 15-ratio table.
type RateRatio struct {
	Numerator   string  `json:"numerator"`
	Denominator string  `json:"denominator"`
	Ratio       float64 `json:"ratio"`

	// EchoClass tags this ratio as barred from target-matching (IMPL-SPEC
	// §5.1.1 / PREREG §5.1.1): it is computed without the kernel executing a
	// cycle, decided entirely by grid/config design. ALWAYS
	// "deterministic_config_read" for every entry in this table.
	EchoClass string `json:"echo_class"`

	// ClusterID groups related literal ratios so downstream accounting can
	// mechanically re-verify them. "sixty" for ratios that literally equal
	// 60 under kernel defaults (Consolidation/Heartbeat = 3600/60,
	// IdleRecheck/Heartbeat = 3600/60, HandoffTTL/Heartbeat = 3600/60);
	// "six" for ratios that literally equal 6 under kernel defaults
	// (Consolidation/ActiveWindow = 3600/600, HandoffTTL/ActiveWindow =
	// 3600/600, IdleRecheck/ActiveWindow = 3600/600); "" otherwise.
	ClusterID string `json:"cluster_id,omitempty"`

	// SourceCitation records where each operand's value comes from, for a
	// future re-run to re-verify the literal without re-deriving it.
	SourceCitation string `json:"source_citation"`
}

// RateReport is the full deterministic-echo rate-ratio surface (Module C, M3).
type RateReport struct {
	// Constants lists the six clock constants and their current values, so
	// the ratio table below is self-documenting.
	Constants []clockConstantReport `json:"constants"`

	// Ratios is the complete C(6,2)=15-ratio table (IMPL-SPEC C1: "all
	// pairwise ratios of the six clock constants, NOT a 6-of-15 subset").
	Ratios []RateRatio `json:"ratios"`

	Timestamp string `json:"timestamp"`
}

// clockConstantReport is the public (JSON) shape of a clockConstant.
type clockConstantReport struct {
	Name          string  `json:"name"`
	Seconds       float64 `json:"seconds"`
	HasConfigKnob bool    `json:"has_config_knob"`
}

// KernelRates reads the LIVE configured values (not hardcoded defaults, for
// the two constants that have a config knob) and computes the complete
// 15-ratio table (IMPL-SPEC C1). pollInterval is supplied by the caller
// because PollInterval lives on ReconcileDaemonConfig, not *Config — the
// daemon, not the static config, owns its resolved value (0 or negative is
// treated as the daemon's own default, 30s, mirroring
// ReconcileDaemonConfig.withDefaults).
//
// Side-effect-free: pure arithmetic over already-loaded config; no
// mutation, no ticker interaction, no reconcile cycle triggered.
func KernelRates(cfg *Config, pollInterval time.Duration) RateReport {
	consolidation := 3600.0
	heartbeat := 60.0
	if cfg != nil {
		if cfg.ConsolidationInterval > 0 {
			consolidation = float64(cfg.ConsolidationInterval)
		}
		if cfg.HeartbeatInterval > 0 {
			heartbeat = float64(cfg.HeartbeatInterval)
		}
	}

	poll := 30.0
	if pollInterval > 0 {
		poll = pollInterval.Seconds()
	}

	// ActiveWindow, HandoffTTL, IdleRecheck have no config knob on current
	// main (verified) — reported as their hardcoded defaults.
	const activeWindowDefaultSeconds = 600.0 // sessions.go defaultActiveWithinSeconds
	const handoffTTLDefaultSeconds = 3600.0  // serve_sessions_mgmt.go req.TTLSeconds default
	const idleRecheckDefaultSeconds = 3600.0 // autonomic_ticker.go defaultIdleRecheckIn (1h)

	constants := []clockConstant{
		{"ConsolidationInterval", consolidation, true},
		{"HeartbeatInterval", heartbeat, true},
		{"PollInterval", poll, true},
		{"ActiveWindow", activeWindowDefaultSeconds, false},
		{"HandoffTTL", handoffTTLDefaultSeconds, false},
		{"IdleRecheck", idleRecheckDefaultSeconds, false},
	}

	constReports := make([]clockConstantReport, len(constants))
	for i, c := range constants {
		constReports[i] = clockConstantReport{
			Name:          c.name,
			Seconds:       c.seconds,
			HasConfigKnob: c.hasConfigKnob,
		}
	}

	ratios := make([]RateRatio, 0, 15) // C(6,2) = 15
	for i := 0; i < len(constants); i++ {
		for j := i + 1; j < len(constants); j++ {
			a, b := constants[i], constants[j]
			var ratio float64
			if b.seconds != 0 {
				ratio = a.seconds / b.seconds
			}
			ratios = append(ratios, RateRatio{
				Numerator:      a.name,
				Denominator:    b.name,
				Ratio:          ratio,
				EchoClass:      "deterministic_config_read",
				ClusterID:      rateClusterID(a.name, b.name, ratio),
				SourceCitation: rateSourceCitation(a) + " / " + rateSourceCitation(b),
			})
		}
	}

	return RateReport{
		Constants: constReports,
		Ratios:    ratios,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// rateClusterID tags the two literal clusters called out in IMPL-SPEC C2:
// the three "60" ratios and the three "6" ratios (computed under kernel
// defaults; a caller who has overridden ConsolidationInterval/
// HeartbeatInterval will see the literal value drift off 60/6, which is
// exactly the point of tagging by NAME PAIR rather than by observed value —
// the cluster membership is a config-design fact, not a runtime coincidence).
func rateClusterID(numerator, denominator string, _ float64) string {
	// Pairs are unordered for clustering purposes — KernelRates generates
	// each pair exactly once in a fixed (i<j) traversal order, so match
	// membership regardless of which operand this traversal put first.
	sixty := [][2]string{
		{"ConsolidationInterval", "HeartbeatInterval"},
		{"IdleRecheck", "HeartbeatInterval"},
		{"HandoffTTL", "HeartbeatInterval"},
	}
	six := [][2]string{
		{"ConsolidationInterval", "ActiveWindow"},
		{"HandoffTTL", "ActiveWindow"},
		{"IdleRecheck", "ActiveWindow"},
	}
	matches := func(pairs [][2]string) bool {
		for _, p := range pairs {
			if (p[0] == numerator && p[1] == denominator) || (p[0] == denominator && p[1] == numerator) {
				return true
			}
		}
		return false
	}
	switch {
	case matches(sixty):
		return "sixty"
	case matches(six):
		return "six"
	default:
		return ""
	}
}

// rateSourceCitation returns a short human-readable pointer to where a
// clock constant's current value came from, for the C2 "future re-run can
// re-verify the literal" requirement.
func rateSourceCitation(c clockConstant) string {
	switch c.name {
	case "ConsolidationInterval":
		return "Config.ConsolidationInterval"
	case "HeartbeatInterval":
		return "Config.HeartbeatInterval"
	case "PollInterval":
		return "ReconcileDaemonConfig.PollInterval"
	case "ActiveWindow":
		return "sessions.go:defaultActiveWithinSeconds (no config knob)"
	case "HandoffTTL":
		return "serve_sessions_mgmt.go:TTLSeconds default (no config knob)"
	case "IdleRecheck":
		return "autonomic_ticker.go:defaultIdleRecheckIn (no config knob)"
	default:
		return "unknown"
	}
}
