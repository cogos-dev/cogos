package engine

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
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
	withExtremeVitals := base
	withExtremeVitals.HostVitals = HostVitals{
		DiskFreeBytes: &extreme, // zero free disk
		MemFreeBytes:  &extreme, // zero free memory
		Load1:         &extremeLoad,
		UptimeSeconds: &extreme,
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
