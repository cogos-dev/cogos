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
// This file closes that gap by tapping the one chokepoint every non-streaming
// completion in this kernel already funnels through: CompleteCancelSafeIfSupported
// (provider.go). Corrected scope (a fix-review finding on the first version
// of this PR named the original claim inaccurate — see git history on this
// file for the wrong version): that function is called by dispatch fan-out
// (agent_dispatch.go/local_agent_harness.go dispatchSlot -> completeWithToolLoop
// -> tool_loop.go re-calls), the autonomic assess-cycle consult
// (local_agent_harness.go assessCycle), AND the external, interactive
// non-streaming chat/Anthropic-compat HTTP handlers (serve.go completeChat,
// serve_anthropic.go completeAnthropicMessages). It is NOT scoped to
// "internal-only" traffic, and it explicitly EXCLUDES every streaming
// completion (streamChat, streamAnthropicMessages, and any other caller of
// provider.Stream directly) — including interactive chat UIs, which are the
// common case there. Per issue #427, the local LM Studio backend serializes
// concurrent inference server-side regardless of transport, so streaming
// traffic contends for the same resource these gauges are meant to observe
// but is invisible to them; conversely, ordinary non-streaming chat traffic
// with no dispatch or autonomic involvement at all does move these gauges.
// Read InferenceQueue/InferenceP50Ms as "contention across all non-streaming
// completions this process is inflight on," not "dispatch-only contention."
//
// Honesty about what these gauges measure:
//
//   - InferenceQueue is NOT a literal FIFO queue depth — no queue existed on
//     this path when this file was written (fan-out is unbounded
//     goroutines, no worker pool or semaphore). It is the number of
//     non-streaming completions currently between call and return, which is
//     a real (if partial — see the streaming exclusion above) contention
//     signal: issue #427 documents that the local LM Studio provider
//     serializes concurrent inference server-side, so a nonzero reading
//     here is real queueing pressure on that single-capacity resource. It
//     is one input the 2026-07-29 scheduling census's manned-valve design
//     (Claim{Resource: "inference:<node>-lms", Capacity: 1}) and
//     load-balancing v0 name as a needed gate — not a complete signal on
//     its own, since it misses streaming traffic on the same resource.
//
//     UPDATE (#556): a real FIFO queue now DOES exist — backendQueue in
//     provider_queue.go, one per local OpenAI-compat backend, gating
//     Complete/CompleteCancelSafe/Stream via the queuedProvider decorator
//     wired in router.go's makeProvider. Its depth/wait are the SEPARATE
//     HostVitals.LocalQueueDepth / LocalQueueWaitP50Ms gauges
//     (host_vitals.go), not this one. The two overlap in population
//     (both, in part, observe local-backend contention) but measure
//     different things and must not be conflated:
//
//   - InferenceQueue: non-streaming in-flight COUNT, across ALL
//     providers (local and cloud) and ALL call sites that route
//     through CompleteCancelSafeIfSupported (dispatch, tool-loop,
//     autonomic consult, non-streaming external chat). Streaming is
//     invisible to it regardless of provider.
//
//   - LocalQueueDepth/LocalQueueWaitP50Ms: actual FIFO WAIT-LIST depth
//     and per-waiter wait time, scoped to the local OpenAI-compat
//     backend family ONLY (the providers wrapped in queuedProvider),
//     but covering streaming traffic too — the population this file's
//     InferenceQueue explicitly excludes.
//     Read InferenceQueue as it always has been: a partial, non-streaming,
//     all-providers in-flight count. Read the #556 gauges as the honest
//     literal-queue answer for the local-backend population specifically.
//
//   - InferenceP50Ms is the rolling median of the last dispatchDurationRingCap
//     completed calls' wall-clock duration, in milliseconds, over the same
//     non-streaming population described above. A small fixed-capacity ring
//     buffer behind one mutex — bounded and lock-cheap, per RFC-040 S0's
//     explicit allowance for exactly this shape rather than a new tracking
//     subsystem.
//
// Double-counting note: CompleteCancelSafeIfSupported is instrumented once,
// at its top level. countingProvider (local_agent_harness.go), which wraps
// the provider on the dispatch path to count turns, implements
// CancelSafeCompleter itself — so without care, a top-level instrumented
// call into countingProvider.CompleteCancelSafe that then called the
// instrumented function again on the inner provider would count one logical
// dispatch completion twice. countingProvider.CompleteCancelSafe therefore
// delegates to completeCancelSafeIfSupportedRaw (provider.go), the
// uninstrumented core, not back through CompleteCancelSafeIfSupported.
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
