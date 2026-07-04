// inference_inflight_test.go — retry-dedup and anomaly-counting tests (#432)
package engine

import (
	"testing"
)

// TestBeginInflightInferenceDedupesConcurrentRequestID verifies the core
// retry-discipline guarantee from #432: a second beginInflightInference call
// under the same non-empty RequestID is refused while the first is still
// registered, so a caller cannot resubmit a request whose prior attempt may
// still be generating server-side.
func TestBeginInflightInferenceDedupesConcurrentRequestID(t *testing.T) {
	const id = "test-request-dedupe-1"
	t.Cleanup(func() { endInflightInference(id) })

	if !beginInflightInference(id) {
		t.Fatal("first beginInflightInference should succeed")
	}
	if beginInflightInference(id) {
		t.Error("second beginInflightInference under the same ID should be refused while the first is in flight")
	}
}

// TestBeginInflightInferenceAllowsResubmitAfterEnd verifies that once a
// request completes (endInflightInference releases the ID), the same
// RequestID can be legitimately reused — dedup only blocks concurrent
// overlap, not sequential retries of a completed attempt.
func TestBeginInflightInferenceAllowsResubmitAfterEnd(t *testing.T) {
	const id = "test-request-dedupe-2"

	if !beginInflightInference(id) {
		t.Fatal("first beginInflightInference should succeed")
	}
	endInflightInference(id)

	if !beginInflightInference(id) {
		t.Error("beginInflightInference should succeed again after the prior attempt ended")
	}
	endInflightInference(id)
}

// TestBeginInflightInferenceIgnoresEmptyRequestID verifies that call sites
// which don't set a RequestID (empty string) are never deduped against each
// other — dedup only applies where the caller opted in with a real identity.
func TestBeginInflightInferenceIgnoresEmptyRequestID(t *testing.T) {
	if !beginInflightInference("") {
		t.Error("beginInflightInference(\"\") should always succeed")
	}
	if !beginInflightInference("") {
		t.Error("beginInflightInference(\"\") should always succeed, even called twice")
	}
	// No cleanup needed: empty ID is never tracked.
}

// TestRecordAbandonedInferenceIncrementsSnapshotDelta verifies that
// recordAbandonedInference's count is visible via abandonedInferenceSnapshot
// as a delta since the last read, and that the delta resets to zero once
// consumed (mirroring how buildKernelHealthSnapshot reads it once per tick).
func TestRecordAbandonedInferenceIncrementsSnapshotDelta(t *testing.T) {
	// Baseline read to zero out any delta left by other tests sharing the
	// package-level counter (tests in this package do not run in parallel
	// with each other for this counter by convention — see below).
	_, _ = abandonedInferenceSnapshot()

	recordAbandonedInference("test-site", "req-1", errCanceledForTest{})
	recordAbandonedInference("test-site", "req-2", errCanceledForTest{})

	total, delta := abandonedInferenceSnapshot()
	if delta != 2 {
		t.Errorf("delta = %d; want 2", delta)
	}
	if total < 2 {
		t.Errorf("total = %d; want >= 2", total)
	}

	// A second read with no new abandonment in between must report zero delta.
	_, delta2 := abandonedInferenceSnapshot()
	if delta2 != 0 {
		t.Errorf("delta2 = %d; want 0 (no new abandonment since last read)", delta2)
	}
}

// errCanceledForTest is a minimal error implementation for
// recordAbandonedInference test calls.
type errCanceledForTest struct{}

func (errCanceledForTest) Error() string { return "canceled for test" }
