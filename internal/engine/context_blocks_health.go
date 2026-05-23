// context_blocks_health.go — proprioception block for the foveated context.
//
// buildHealthBlock iterates pkg/reconcile's provider registry, calls Health()
// on each registered Reconcilable with a per-provider timeout, and renders a
// compact "Substrate Health" block. This is the kernel's body-state surfacing
// into Claude Code's foveated context — the return path that closes the
// sensorium → cognitive substrate loop (proprioception, not perception).
//
// Design notes:
//   - The block is cheap by design: Health() is a synchronous, near-zero-cost
//     call by Reconcilable contract. Providers that take longer than the
//     per-provider timeout (200ms) are marked Unknown so one slow provider
//     can't stall the foveated handler.
//   - Panic recovery is per-provider so a single broken Reconcilable can't
//     suppress the whole block.
//   - When everything is green, the rendering compresses to a single
//     summary line; the full table only appears when at least one provider
//     is non-healthy or non-synced. Proprioception should be cheap when the
//     body is fine and expressive when it isn't.
package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// healthProbeTimeout is the per-provider Health() timeout. Health() is
// supposed to return cached/computed status without blocking; anything
// slower indicates a defect we want to surface as Unknown rather than
// silently stalling the foveated handler.
const healthProbeTimeout = 200 * time.Millisecond

// healthSample is the result of probing a single Reconcilable.
type healthSample struct {
	Name   string
	Status reconcile.ResourceStatus
	Probed time.Duration
	Error  string // non-empty if probe failed (panic, timeout)
}

// isGreen reports whether a sample is in a non-attention-requiring state.
//
// Sync=Unknown is treated as not-disqualifying: most daemon-side providers
// report Unknown by design (they're stubs without a comparable declared
// state), and Health is the load-bearing axis for "does the operator need
// to look at this?" A provider with Sync=Unknown but Health=Healthy is
// quiet — same as Sync=Synced + Healthy. A provider with Sync=OutOfSync
// is loud regardless of Health, and is caught separately by HasOutOfSync
// in the escalation predicate.
func (h *healthSample) isGreen() bool {
	if h.Error != "" {
		return false
	}
	syncOK := h.Status.Sync == reconcile.SyncStatusSynced ||
		h.Status.Sync == reconcile.SyncStatusUnknown ||
		h.Status.Sync == ""
	return syncOK &&
		h.Status.Health == reconcile.HealthHealthy &&
		(h.Status.Operation == reconcile.OperationIdle ||
			h.Status.Operation == "")
}

// buildHealthBlock returns a context block summarizing the live status of
// every registered Reconcilable. Returns nil when no providers are
// registered (e.g. during a stripped-down test boot).
func buildHealthBlock(ctx context.Context) *ContextBlock {
	names := reconcile.ListProviders()
	if len(names) == 0 {
		return nil
	}

	samples := probeAllProviders(ctx, names)

	greenCount := 0
	for i := range samples {
		if samples[i].isGreen() {
			greenCount++
		}
	}

	content := renderHealthBlock(samples, greenCount)
	block := NewBlock(BlockHealth, content)
	return &block
}

// probeAllProviders probes every named provider sequentially. Each probeOne
// call runs Health() in its own goroutine (so a misbehaving provider can't
// block the foveated handler past healthProbeTimeout), but the outer loop
// is sequential — worst-case wall-clock latency is N × healthProbeTimeout.
// The kernel's registry mutex is only held briefly to fetch the provider
// reference. If provider count grows large enough that N × timeout becomes
// noticeable, this loop can be fanned out with a WaitGroup; today's N keeps
// it simple.
func probeAllProviders(ctx context.Context, names []string) []healthSample {
	sort.Strings(names)
	samples := make([]healthSample, len(names))
	for i, name := range names {
		samples[i] = probeOne(ctx, name)
	}
	return samples
}

// probeOne calls Health() on a single named provider. Recovers from panics
// and respects healthProbeTimeout. Health() is supposed to be synchronous
// and cheap; we run it in a goroutine so a misbehaving provider can't block
// the foveated handler past the timeout.
func probeOne(ctx context.Context, name string) healthSample {
	provider, err := reconcile.GetProvider(name)
	if err != nil {
		return healthSample{Name: name, Error: err.Error()}
	}

	type result struct {
		status reconcile.ResourceStatus
		err    string
	}
	ch := make(chan result, 1)
	start := time.Now()

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				ch <- result{
					status: reconcile.ResourceStatus{
						Sync:      reconcile.SyncStatusUnknown,
						Health:    reconcile.HealthMissing,
						Operation: reconcile.OperationIdle,
						Message:   fmt.Sprintf("panic: %v", rec),
					},
					err: fmt.Sprintf("panic: %v\n%s", rec, debug.Stack()),
				}
			}
		}()
		ch <- result{status: provider.Health()}
	}()

	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()

	select {
	case r := <-ch:
		return healthSample{Name: name, Status: r.status, Probed: time.Since(start), Error: r.err}
	case <-probeCtx.Done():
		return healthSample{
			Name: name,
			Status: reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusUnknown,
				Health:    reconcile.HealthMissing,
				Operation: reconcile.OperationIdle,
				Message:   fmt.Sprintf("Health() exceeded %s", healthProbeTimeout),
			},
			Probed: time.Since(start),
			Error:  "timeout",
		}
	}
}

// renderHealthBlock formats samples for inclusion in the foveated context.
// When every sample is green, output collapses to a single line. Otherwise
// a markdown table is emitted with non-green samples first.
func renderHealthBlock(samples []healthSample, greenCount int) string {
	var sb strings.Builder
	sb.WriteString("## Substrate Health\n\n")

	total := len(samples)
	if greenCount == total {
		fmt.Fprintf(&sb, "%d providers — all Synced/Healthy/Idle.\n", total)
		return sb.String()
	}

	// One-line summary first, then the table.
	fmt.Fprintf(&sb, "%d providers — %d healthy, %d need attention.\n\n", total, greenCount, total-greenCount)

	// Order: non-green first (sorted by name), then green (sorted by name).
	nonGreen := make([]healthSample, 0, total-greenCount)
	green := make([]healthSample, 0, greenCount)
	for _, s := range samples {
		if s.isGreen() {
			green = append(green, s)
		} else {
			nonGreen = append(nonGreen, s)
		}
	}
	ordered := append(nonGreen, green...)

	sb.WriteString("| Provider | Sync | Health | Op | Note |\n")
	sb.WriteString("|----------|------|--------|----|------|\n")
	for _, s := range ordered {
		note := s.Status.Message
		if s.Error != "" && note == "" {
			note = s.Error
		}
		note = compactNote(note)
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n",
			s.Name,
			fallback(string(s.Status.Sync), "Unknown"),
			fallback(string(s.Status.Health), "Missing"),
			fallback(string(s.Status.Operation), "Idle"),
			note,
		)
	}
	return sb.String()
}

// compactNote trims a status message to a single line bounded length so the
// table stays readable when injected into a prompt.
func compactNote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse newlines and tabs to single spaces.
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '|':
			// pipes would break the markdown table — replace with a slash.
			return '/'
		default:
			return r
		}
	}, s)
	// Squeeze multiple spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	const maxNote = 100
	if len(s) > maxNote {
		// Break at a word boundary if possible.
		if idx := strings.LastIndex(s[:maxNote], " "); idx > maxNote/2 {
			return s[:idx] + "…"
		}
		return s[:maxNote] + "…"
	}
	return s
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
