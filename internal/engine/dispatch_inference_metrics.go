// dispatch_inference_metrics.go — RFC-040 S0 completion: InferenceP50Ms and
// InferenceQueue.
//
// host_vitals.go originally shipped RFC-040 S0 with these two gauges
// deliberately omitted: no rolling latency series existed anywhere, and the
// one candidate structure (inflightRequests in inference_inflight.go) is an
// opt-in retry-dedup map for a handful of call sites, not a count of
// in-flight inference. Per the no-stubs directive, both were left out rather
// than backed by a misleading reading.
//
// This file closes that gap by tapping the one chokepoint that already
// carries exactly the traffic these gauges are meant to describe:
// CompleteCancelSafeIfSupported (provider.go). Its own doc comment scopes it
// precisely: "Internal non-interactive call sites (autonomic consult,
// dispatch, tool-loop re-calls)" — concretely, every call LocalHarnessController
// makes on the dispatch fan-out path (agent_dispatch.go/local_agent_harness.go
// dispatchSlot -> completeWithToolLoop -> tool_loop.go re-calls) and the
// autonomic assess-cycle consult (local_agent_harness.go assessCycle). That is
// the same population inference_inflight.go's abandoned-inference counter
// (#432) already instruments at these exact call sites — this is a second tap
// on the same funnel, not a new subsystem.
//
// Honesty about what these gauges measure:
//   - InferenceQueue is NOT a literal FIFO queue depth — no queue exists on
//     this path (fan-out is unbounded goroutines, no worker pool or
//     semaphore). It is the number of these internal completions currently
//     between call and return, which is exactly the contention signal the
//     RFC-040 gauge exists to surface: issue #427 documents that the local LM
//     Studio provider serializes concurrent inference server-side, so a
//     nonzero reading here is real queueing pressure on that single-capacity
//     resource, and is the sensor the 2026-07-29 scheduling census's
//     manned-valve design (Claim{Resource: "inference:<node>-lms", Capacity:
//     1}) and load-balancing v0 gate on.
//   - InferenceP50Ms is the rolling median of the last dispatchDurationRingCap
//     completed calls' wall-clock duration, in milliseconds. A small
//     fixed-capacity ring buffer behind one mutex — bounded and lock-cheap,
//     per RFC-040 S0's explicit allowance for exactly this shape rather than
//     a new tracking subsystem.
//
// Per RFC-040 N5, both are pure observation: neither is read by AllGreen(),
// neither participates in dispatch timeout/retry/routing decisions today —
// they are a read-only tap on an existing call path.
package engine

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// dispatchInflight counts internal (CompleteCancelSafeIfSupported-routed)
// inference calls currently in progress, process-wide. See the package doc
// above for exactly which call sites this covers and what "queue" means
// here.
var dispatchInflight atomic.Int64

// dispatchDurationRingCap bounds the rolling-p50 sample window. 256 recent
// calls is enough to smooth over a burst of fan-out slots without growing
// unboundedly; the buffer is fixed-size so steady-state memory is constant.
const dispatchDurationRingCap = 256

var (
	dispatchDurationMu   sync.Mutex
	dispatchDurationRing [dispatchDurationRingCap]float64
	dispatchDurationLen  int // number of valid entries, saturates at cap
	dispatchDurationNext int // next write index (ring cursor)
)

// beginDispatchInferenceSample marks one internal inference call as started.
// Pair with endDispatchInferenceSample via defer at the call site.
func beginDispatchInferenceSample() {
	dispatchInflight.Add(1)
}

// endDispatchInferenceSample marks one internal inference call as finished
// and folds its duration into the rolling p50 sample ring. Safe to call from
// any goroutine; the critical section is a fixed-size array write, no
// allocation once the ring has filled once.
func endDispatchInferenceSample(dur time.Duration) {
	dispatchInflight.Add(-1)

	ms := float64(dur) / float64(time.Millisecond)

	dispatchDurationMu.Lock()
	dispatchDurationRing[dispatchDurationNext] = ms
	dispatchDurationNext = (dispatchDurationNext + 1) % dispatchDurationRingCap
	if dispatchDurationLen < dispatchDurationRingCap {
		dispatchDurationLen++
	}
	dispatchDurationMu.Unlock()
}

// dispatchQueueDepth returns the current count of in-flight internal
// inference calls. Always well-defined (never "unknown") — a pure atomic
// read, no I/O, nothing to fail — unlike dispatchP50Ms, which can have no
// data yet.
func dispatchQueueDepth() int {
	return int(dispatchInflight.Load())
}

// dispatchP50Ms returns the median call duration, in milliseconds, over the
// current rolling sample window, and whether any samples exist yet. A fresh
// process with no internal inference calls yet has no samples — ok=false —
// so callers can omit the gauge (RFC-040's soft-degrade contract: an
// unmeasured gauge is a missing metric, never a fabricated zero).
func dispatchP50Ms() (float64, bool) {
	dispatchDurationMu.Lock()
	n := dispatchDurationLen
	if n == 0 {
		dispatchDurationMu.Unlock()
		return 0, false
	}
	samples := make([]float64, n)
	copy(samples, dispatchDurationRing[:n])
	dispatchDurationMu.Unlock()

	sort.Float64s(samples)
	return samples[n/2], true
}

// resetDispatchInferenceMetricsForTest clears both the in-flight counter and
// the duration ring. Test-only: mirrors
// resetAbandonedInferenceCounterForTest's rationale (inference_inflight.go)
// — these are process-global by design, so tests that assert exact values
// must reset the shared state first to avoid cross-test pollution.
func resetDispatchInferenceMetricsForTest() {
	dispatchInflight.Store(0)
	dispatchDurationMu.Lock()
	dispatchDurationLen = 0
	dispatchDurationNext = 0
	dispatchDurationMu.Unlock()
}
