package engine

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// TestSampleHostVitals_UptimeAlwaysPresent verifies UptimeSeconds — pure
// in-memory arithmetic with no I/O — is always populated regardless of
// platform.
func TestSampleHostVitals_UptimeAlwaysPresent(t *testing.T) {
	hv := sampleHostVitals()
	if hv.UptimeSeconds == nil {
		t.Fatal("expected UptimeSeconds to always be sampled")
	}
}

// TestSampleHostVitals_InferenceQueueAlwaysPresent is InferenceQueue's
// analogue of the uptime test above: a pure atomic read, no I/O, nothing to
// fail, so it must always be populated regardless of whether any internal
// inference has run yet.
func TestSampleHostVitals_InferenceQueueAlwaysPresent(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	hv := sampleHostVitals()
	if hv.InferenceQueue == nil {
		t.Fatal("expected InferenceQueue to always be sampled")
	}
	if *hv.InferenceQueue != 0 {
		t.Fatalf("want InferenceQueue 0 with no in-flight calls, got %d", *hv.InferenceQueue)
	}
}

// TestSampleHostVitals_InferenceP50MsOmittedUntilASampleExists is RFC-040's
// soft-degrade contract applied to the rolling-p50 gauge specifically: unlike
// InferenceQueue, it has a genuine "no data yet" state (a fresh process, or
// this test's reset), and must be nil — not a fabricated zero — until at
// least one internal inference call has completed.
func TestSampleHostVitals_InferenceP50MsOmittedUntilASampleExists(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	hv := sampleHostVitals()
	if hv.InferenceP50Ms != nil {
		t.Fatalf("expected InferenceP50Ms nil with no samples, got %v", *hv.InferenceP50Ms)
	}

	endDispatchInferenceSample(15 * time.Millisecond)

	hv = sampleHostVitals()
	if hv.InferenceP50Ms == nil {
		t.Fatal("expected InferenceP50Ms populated once a sample has been recorded")
	}
}

// TestSampleHostVitals_DiskFreeOnSupportedPlatforms verifies the one gauge
// this PR implements everywhere it can (linux, darwin) is actually sampled
// there, using a plain Statfs on the process's working directory.
func TestSampleHostVitals_DiskFreeOnSupportedPlatforms(t *testing.T) {
	hv := sampleHostVitals()
	switch runtime.GOOS {
	case "linux", "darwin":
		if hv.DiskFreeBytes == nil {
			t.Fatalf("expected disk_free_bytes to be sampled on %s", runtime.GOOS)
		}
		if *hv.DiskFreeBytes == 0 {
			t.Fatalf("disk_free_bytes sampled as exactly zero — suspicious for a live filesystem")
		}
	}
}

// TestSampleHostVitals_UnsupportedSamplersOmitCleanly is the "a failed
// sampler omits cleanly" case from the PR directive, exercised for real (no
// mocking) on darwin: mem_free_bytes and load1 have no non-cgo
// implementation there (host_vitals_darwin.go), so sampleHostVitals must
// leave both nil — never a panic, never a fabricated zero — while
// disk_free_bytes and uptime_seconds still come through.
func TestSampleHostVitals_UnsupportedSamplersOmitCleanly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("exercises the darwin-specific soft-degrade path (mem_free_bytes/load1 unimplemented there)")
	}

	hv := sampleHostVitals()

	if hv.MemFreeBytes != nil {
		t.Errorf("expected mem_free_bytes to be omitted on darwin, got %v", *hv.MemFreeBytes)
	}
	if hv.Load1 != nil {
		t.Errorf("expected load1 to be omitted on darwin, got %v", *hv.Load1)
	}
	if hv.DiskFreeBytes == nil {
		t.Error("expected disk_free_bytes to still be sampled on darwin")
	}
	if hv.UptimeSeconds == nil {
		t.Error("expected uptime_seconds to still be sampled on darwin")
	}

	data, err := json.Marshal(hv)
	if err != nil {
		t.Fatalf("marshal HostVitals: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal HostVitals JSON: %v", err)
	}
	if _, ok := m["mem_free_bytes"]; ok {
		t.Error("mem_free_bytes key must be absent from JSON (omitempty on a nil pointer) when unsupported")
	}
	if _, ok := m["load1"]; ok {
		t.Error("load1 key must be absent from JSON (omitempty on a nil pointer) when unsupported")
	}
	if _, ok := m["disk_free_bytes"]; !ok {
		t.Error("disk_free_bytes key should be present when the sample succeeded")
	}
	if _, ok := m["uptime_seconds"]; !ok {
		t.Error("uptime_seconds key should be present when the sample succeeded")
	}
}

// TestKernelHealthSnapshot_JSONIncludesHostVitals confirms HostVitals rides
// the tick's existing snapshot serialization path end to end (the shape a
// bus consumer / #479 ambient block / future /metrics exporter would see).
func TestKernelHealthSnapshot_JSONIncludesHostVitals(t *testing.T) {
	resetAbandonedInferenceCounterForTest()
	snap := buildKernelHealthSnapshot(context.Background())

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal KernelHealthSnapshot: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal KernelHealthSnapshot JSON: %v", err)
	}
	if _, ok := m["host_vitals"]; !ok {
		t.Fatal("expected host_vitals key in the serialized KernelHealthSnapshot")
	}

	// snapshotToPayload is the actual serialization path emitHealthSnapshot
	// uses to write to bus_kernel_proprio — verify HostVitals survives that
	// round trip too, not just a direct json.Marshal.
	payload, err := snapshotToPayload(snap)
	if err != nil {
		t.Fatalf("snapshotToPayload: %v", err)
	}
	if _, ok := payload["host_vitals"]; !ok {
		t.Fatal("expected host_vitals key in the bus-emitted payload")
	}
}

// TestSampleHostVitals_LocalQueueNilWhenNoBackendsRegistered is the #556
// gauges' soft-degrade contract: with an empty backendQueues registry (no
// local OpenAI-compat backend configured at all), both LocalQueueDepth and
// LocalQueueWaitP50Ms must be nil — never a fabricated zero — matching
// every other gauge in this file's convention.
func TestSampleHostVitals_LocalQueueNilWhenNoBackendsRegistered(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	hv := sampleHostVitals()
	if hv.LocalQueueDepth != nil {
		t.Errorf("expected LocalQueueDepth nil with no registered backend queues, got %v", *hv.LocalQueueDepth)
	}
	if hv.LocalQueueWaitP50Ms != nil {
		t.Errorf("expected LocalQueueWaitP50Ms nil with no registered backend queues, got %v", *hv.LocalQueueWaitP50Ms)
	}
}

// TestSampleHostVitals_LocalQueueDepthRealZeroWhenIdle covers the
// depth-can-be-a-real-zero case: once at least one backend queue is
// registered, LocalQueueDepth must be populated (a real 0, not omitted) even
// though nobody is currently waiting — the same "0 is a real reading"
// convention documented on the field. LocalQueueWaitP50Ms stays nil, since
// there is no currently-waiting caller to compute a median over.
func TestSampleHostVitals_LocalQueueDepthRealZeroWhenIdle(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	q := newBackendQueue("idle-backend", 2)
	backendQueues.Store("idle-backend", q)

	hv := sampleHostVitals()
	if hv.LocalQueueDepth == nil {
		t.Fatal("expected LocalQueueDepth to be populated once a backend queue is registered")
	}
	if *hv.LocalQueueDepth != 0 {
		t.Errorf("LocalQueueDepth = %d, want 0 (queue registered but idle)", *hv.LocalQueueDepth)
	}
	if hv.LocalQueueWaitP50Ms != nil {
		t.Errorf("expected LocalQueueWaitP50Ms nil with no currently-waiting caller, got %v", *hv.LocalQueueWaitP50Ms)
	}
}

// TestSampleHostVitals_LocalQueueDepthAndWaitPooledAcrossBackends verifies
// the pooling behavior sampleLocalQueueVitals documents: depth sums Waiting
// across every registered backend, and the wait-p50 is computed over every
// currently-waiting caller pooled across backends, not per-backend.
func TestSampleHostVitals_LocalQueueDepthAndWaitPooledAcrossBackends(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	qa := newBackendQueue("backend-a", 1)
	qb := newBackendQueue("backend-b", 1)
	backendQueues.Store("backend-a", qa)
	backendQueues.Store("backend-b", qb)

	releaseA, _, _, err := qa.Acquire(context.Background(), "seed-a")
	if err != nil {
		t.Fatalf("seed-a acquire: %v", err)
	}
	t.Cleanup(releaseA)
	releaseB, _, _, err := qb.Acquire(context.Background(), "seed-b")
	if err != nil {
		t.Fatalf("seed-b acquire: %v", err)
	}
	t.Cleanup(releaseB)

	// One waiter on each backend so LocalQueueDepth pools to 2.
	for _, q := range []*backendQueue{qa, qb} {
		go func(q *backendQueue) {
			release, _, _, err := q.Acquire(context.Background(), "waiter")
			if err != nil {
				return
			}
			release()
		}(q)
	}
	deadline := time.Now().Add(2 * time.Second)
	for qa.Snapshot().Waiting != 1 || qb.Snapshot().Waiting != 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for both backends to have one waiter each")
		}
		time.Sleep(time.Millisecond)
	}

	hv := sampleHostVitals()
	if hv.LocalQueueDepth == nil {
		t.Fatal("expected LocalQueueDepth populated")
	}
	if *hv.LocalQueueDepth != 2 {
		t.Errorf("LocalQueueDepth = %d, want 2 (pooled across both backends)", *hv.LocalQueueDepth)
	}
	if hv.LocalQueueWaitP50Ms == nil {
		t.Fatal("expected LocalQueueWaitP50Ms populated with two waiters currently queued")
	}
}

// TestAllGreen_UnaffectedByHostVitals is the N5 normative constraint,
// tested directly: AllGreen() must be a pure function of Counts/Anomalies,
// never HostVitals, no matter how extreme the gauge readings are.
func TestAllGreen_UnaffectedByHostVitals(t *testing.T) {
	base := KernelHealthSnapshot{Counts: HealthCounts{Healthy: 3}}
	if !base.AllGreen() {
		t.Fatal("baseline snapshot with zero degraded/missing/suspended/anomalies should be AllGreen")
	}

	extreme := uint64(0)
	extremeLoad := 999999.0
	extremeP50 := 999999.0
	extremeQueue := 999999
	withExtremeVitals := base
	withExtremeVitals.HostVitals = HostVitals{
		DiskFreeBytes:  &extreme, // zero free disk
		MemFreeBytes:   &extreme, // zero free memory
		Load1:          &extremeLoad,
		UptimeSeconds:  &extreme,
		InferenceP50Ms: &extremeP50,
		InferenceQueue: &extremeQueue,
	}
	if !withExtremeVitals.AllGreen() {
		t.Fatal("RFC-040 N5 violated: extreme HostVitals readings must not affect AllGreen()")
	}

	degradedButVitalsEmpty := base
	degradedButVitalsEmpty.Counts.Degraded = 1
	degradedButVitalsEmpty.HostVitals = HostVitals{} // every gauge omitted
	if degradedButVitalsEmpty.AllGreen() {
		t.Fatal("a degraded provider must still fail AllGreen regardless of HostVitals content")
	}
}
