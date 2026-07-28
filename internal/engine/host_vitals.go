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

	// InferenceP50Ms and InferenceQueue from the RFC-040 S0 design sketch
	// are intentionally NOT included in this struct. Neither is cheaply
	// readable from an existing in-process structure today: no rolling
	// latency history is retained anywhere (DispatchResult.DurationSec in
	// agent_dispatch.go is per-call and ephemeral, not accumulated into a
	// queryable series), and the one inflight-tracking structure that does
	// exist (inflightRequests in inference_inflight.go) is an opt-in dedup
	// map for a handful of retry-sensitive call sites, not a count of all
	// in-flight inference — treating its size as "the inference queue"
	// would be a misleading reading dressed up as a measurement, not a
	// cheap accurate one. Per the no-stubs directive, both are omitted
	// rather than stubbed; see the PR body for the same rationale.
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

	return hv
}
