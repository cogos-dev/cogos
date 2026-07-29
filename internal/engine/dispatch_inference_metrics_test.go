package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDispatchQueueDepth_TracksInFlightSamples exercises the begin/end pair
// directly: depth rises on begin and falls on end, independent of the
// p50 ring (which needs no samples for this to hold).
func TestDispatchQueueDepth_TracksInFlightSamples(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	if got := dispatchQueueDepth(); got != 0 {
		t.Fatalf("want 0 in-flight at start, got %d", got)
	}

	beginDispatchInferenceSample()
	beginDispatchInferenceSample()
	if got := dispatchQueueDepth(); got != 2 {
		t.Fatalf("want 2 in-flight after two begins, got %d", got)
	}

	endDispatchInferenceSample(5 * time.Millisecond)
	if got := dispatchQueueDepth(); got != 1 {
		t.Fatalf("want 1 in-flight after one end, got %d", got)
	}

	endDispatchInferenceSample(5 * time.Millisecond)
	if got := dispatchQueueDepth(); got != 0 {
		t.Fatalf("want 0 in-flight after both ends, got %d", got)
	}
}

// TestDispatchP50Ms_NoSamplesReturnsFalse is RFC-040's soft-degrade contract
// applied to this gauge: a fresh process with no completed internal
// inference calls yet must report "no data" rather than a fabricated zero.
func TestDispatchP50Ms_NoSamplesReturnsFalse(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	if _, ok := dispatchP50Ms(); ok {
		t.Fatal("expected ok=false with zero recorded samples")
	}
}

// TestDispatchP50Ms_ComputesMedianOverRecordedSamples verifies the median
// arithmetic directly against a known set of durations.
func TestDispatchP50Ms_ComputesMedianOverRecordedSamples(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	for _, ms := range []int{10, 50, 20, 40, 30} { // median of {10,20,30,40,50} = 30
		endDispatchInferenceSample(time.Duration(ms) * time.Millisecond)
	}

	p50, ok := dispatchP50Ms()
	if !ok {
		t.Fatal("expected ok=true after recording samples")
	}
	if p50 != 30 {
		t.Fatalf("want median 30ms, got %v", p50)
	}
}

// TestDispatchDurationRing_BoundedCapacity confirms the ring never grows
// past its fixed capacity even under a burst of samples well beyond it —
// the "bounded and lock-cheap" requirement made concrete.
func TestDispatchDurationRing_BoundedCapacity(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	for i := 0; i < dispatchDurationRingCap*3; i++ {
		endDispatchInferenceSample(time.Millisecond)
	}

	dispatchDurationMu.Lock()
	n := dispatchDurationLen
	dispatchDurationMu.Unlock()
	if n != dispatchDurationRingCap {
		t.Fatalf("want ring length capped at %d, got %d", dispatchDurationRingCap, n)
	}
}

// TestCompleteCancelSafeIfSupported_RecordsQueueAndDuration is the
// integration test for the actual chokepoint: calling the exported wrapper
// (as every dispatch/tool-loop/assess-cycle call site does) must move the
// queue depth up during the call and back down after, and leave a sample in
// the p50 ring.
func TestCompleteCancelSafeIfSupported_RecordsQueueAndDuration(t *testing.T) {
	resetDispatchInferenceMetricsForTest()
	t.Cleanup(resetDispatchInferenceMetricsForTest)

	var wg sync.WaitGroup
	const n = 5
	seenNonZero := make(chan struct{}, 1)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			// Each goroutine gets its own StubProvider instance —
			// StubProvider itself is not safe for concurrent Complete()
			// calls (it records lastRequest on the struct), which is a
			// property of the test fixture, not of the code under test.
			p := &StubProvider{name: "stub", available: true, latency: 30 * time.Millisecond}
			_, err := CompleteCancelSafeIfSupported(context.Background(), p, &CompletionRequest{})
			if err != nil {
				t.Errorf("CompleteCancelSafeIfSupported: %v", err)
			}
		}()
	}

	// Poll briefly for a nonzero queue depth while the batch is in flight —
	// best-effort observation of the transient state, not required for the
	// rest of the assertions to hold.
	go func() {
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if dispatchQueueDepth() > 0 {
				select {
				case seenNonZero <- struct{}{}:
				default:
				}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-seenNonZero:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("never observed a nonzero queue depth while calls were in flight")
	}

	wg.Wait()

	if got := dispatchQueueDepth(); got != 0 {
		t.Fatalf("want queue depth 0 once every call has returned, got %d", got)
	}
	p50, ok := dispatchP50Ms()
	if !ok {
		t.Fatal("expected a recorded p50 sample after calls completed")
	}
	if p50 <= 0 {
		t.Fatalf("want a positive p50 given 30ms stub latency, got %v", p50)
	}
}
