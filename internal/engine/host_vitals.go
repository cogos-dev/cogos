// host_vitals.go — RFC-040 S0: host gauges sampled into the existing
// autonomic tick.
//
// This is deliberately NOT a new loop or daemon: sampleHostVitals is called
// once per invocation of buildKernelHealthSnapshotWith (autonomic_ticker.go),
// which is already the tick's aggregation point. Per RFC-040 N5, everything
// here is an OBSERVATION — no gauge value feeds AllGreen() or any
// escalation decision.
//
// Every sampler is best-effort and platform-gated (see host_vitals_linux.go,
// host_vitals_darwin.go, host_vitals_other.go). A gauge that can't be read —
// whether because the platform has no cheap non-cgo implementation or
// because the read itself failed — is omitted from HostVitals (nil pointer,
// so encoding/json's omitempty drops the key) rather than reported as a
// fabricated zero. sampleHostVitals never panics and never returns an error;
// callers get whatever subset of gauges was actually obtainable.
package engine

import (
	"os"
	"sort"
	"time"
)

// HostVitals is the RFC-040 S0 host-gauge block. Every field is a pointer so
// a gauge that fails to sample is simply absent from the JSON via
// omitempty — never a zero-value placeholder standing in for "unknown".
type HostVitals struct {
	// DiskFreeBytes is free space on the workspace volume (the kernel
	// process's working directory, per this codebase's existing
	// os.Getwd()-as-workspace-root convention; see config.go, node_manifest.go).
	DiskFreeBytes *uint64 `json:"disk_free_bytes,omitempty"`
	// MemFreeBytes is free host memory. Omitted on platforms with no cheap
	// non-cgo reading (darwin — see host_vitals_darwin.go).
	MemFreeBytes *uint64 `json:"mem_free_bytes,omitempty"`
	// Load1 is the 1-minute load average. Omitted on platforms with no cheap
	// non-cgo reading (darwin — see host_vitals_darwin.go).
	Load1 *float64 `json:"load1,omitempty"`
	// UptimeSeconds is kernel PROCESS uptime (time since this process
	// started), not machine/OS boot time — consistent with the existing
	// UptimeSec convention in agent_controller.go / local_agent_harness.go's
	// AgentStatus. Always populated: pure in-memory time arithmetic, no I/O,
	// nothing to fail.
	UptimeSeconds *uint64 `json:"uptime_seconds,omitempty"`

	// InferenceP50Ms is the rolling median duration, in milliseconds, of
	// recent CompleteCancelSafeIfSupported-routed completions — dispatch
	// fan-out, tool-loop re-calls, the autonomic assess-cycle consult, AND
	// ordinary non-streaming external chat/Anthropic-compat HTTP traffic.
	// Streaming completions are NOT included (they bypass this call path
	// entirely). See dispatch_inference_metrics.go's package doc for the
	// full, corrected call-site scope. Omitted (nil) until at least one such
	// call has completed since process start — never a fabricated zero.
	InferenceP50Ms *float64 `json:"inference_p50_ms,omitempty"`

	// InferenceQueue is the current count of in-flight calls in the same
	// population as InferenceP50Ms (see above). Always populated: a pure
	// atomic read, no I/O, nothing to fail. Not a literal FIFO queue, and
	// not dispatch-exclusive — see dispatch_inference_metrics.go's package
	// doc for what "queue" means here, what it does and doesn't cover, and
	// why it's still an honest (if partial) proxy for contention on the
	// single-capacity local inference resource.
	InferenceQueue *int `json:"inference_queue,omitempty"`

	// LocalQueueDepth is the total number of callers currently WAITING
	// (not yet granted a slot) across every kernel-owned local-backend
	// backendQueue (#556, provider_queue.go — registered in backendQueues
	// as of router construction). Unlike InferenceQueue this IS a literal
	// FIFO queue depth, and it covers streaming traffic (InferenceQueue
	// explicitly does not — see dispatch_inference_metrics.go). Nil only
	// when backendQueues is empty, i.e. no local OpenAI-compat backend is
	// configured at all; once at least one is, 0 is a real, meaningful
	// reading (queue idle), never a fabricated placeholder.
	LocalQueueDepth *int `json:"local_queue_depth,omitempty"`

	// LocalQueueWaitP50Ms is the median WAITING time (milliseconds, as of
	// this sample) across every caller currently queued on any local
	// backend's backendQueue, pooled across backends. Nil whenever no
	// caller is currently waiting anywhere (a genuine "no data" state, same
	// convention as InferenceP50Ms) — including when backendQueues is
	// non-empty but every queue is idle. This is a snapshot statistic (how
	// long has each CURRENTLY waiting caller waited SO FAR), not a
	// completed-wait history — there is no ring buffer of past waits the
	// way InferenceP50Ms has one of past completion durations.
	LocalQueueWaitP50Ms *float64 `json:"local_queue_wait_p50_ms,omitempty"`
}

// kernelProcessStart is captured at package init and used to compute
// UptimeSeconds without threading process-start state through the ticker's
// call chain (which has no such parameter today, and every existing caller
// of buildKernelHealthSnapshot/-Peek would need updating to add one).
var kernelProcessStart = time.Now()

// sampleHostVitals samples every RFC-040 S0 gauge for one tick. Best-effort
// throughout: any individual failure degrades soft (field left nil) rather
// than failing the whole sample, panicking, or blocking the tick.
func sampleHostVitals() HostVitals {
	var hv HostVitals

	if wd, err := os.Getwd(); err == nil {
		if free, err := diskFreeBytes(wd); err == nil {
			hv.DiskFreeBytes = &free
		}
	}

	if free, err := memFreeBytes(); err == nil {
		hv.MemFreeBytes = &free
	}

	if l1, err := loadAvg1(); err == nil {
		hv.Load1 = &l1
	}

	up := uint64(time.Since(kernelProcessStart).Seconds())
	hv.UptimeSeconds = &up

	if p50, ok := dispatchP50Ms(); ok {
		hv.InferenceP50Ms = &p50
	}
	queue := dispatchQueueDepth()
	hv.InferenceQueue = &queue

	if depth, waitP50, hasRegistry, hasWaiters := sampleLocalQueueVitals(); hasRegistry {
		hv.LocalQueueDepth = &depth
		if hasWaiters {
			hv.LocalQueueWaitP50Ms = &waitP50
		}
	}

	return hv
}

// sampleLocalQueueVitals pools every registered backend's backendQueue
// (provider_queue.go) into the two #556 gauges above. hasRegistry reports
// whether backendQueues has at least one entry at all (distinct from
// hasWaiters, which reports whether any caller is currently queued anywhere
// — depth can be a real 0 while hasWaiters is false).
func sampleLocalQueueVitals() (depth int, waitP50Ms float64, hasRegistry, hasWaiters bool) {
	var waits []int64
	backendQueues.Range(func(_, v any) bool {
		q, ok := v.(*backendQueue)
		if !ok {
			return true
		}
		hasRegistry = true
		snap := q.Snapshot()
		depth += snap.Waiting
		for _, c := range snap.Callers {
			waits = append(waits, c.WaitingMs)
		}
		return true
	})
	if len(waits) == 0 {
		return depth, 0, hasRegistry, false
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	return depth, float64(waits[len(waits)/2]), hasRegistry, true
}
