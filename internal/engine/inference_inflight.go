// inference_inflight.go — retry-dedup and anomaly surfacing for #432
// (local-inference cancellation propagation / abandoned-generation zombies).
//
// Two mechanisms, both process-local (in-memory, not persisted):
//
//  1. In-flight request tracking keyed by RequestMetadata.RequestID. Callers
//     that may be resubmitted (autonomic consult, dispatch, tool-loop
//     re-calls) register their RequestID for the duration of the call and
//     refuse a second registration under the same ID. This is the "never
//     resubmit while a prior attempt may still be generating" retry
//     discipline from #432's expected-fixes list. Request IDs in this
//     codebase are already content-stable where it matters (dispatchSlot
//     hashes systemPrompt+task+tool-names into the ID specifically for
//     KV-cache sharing across fan-out slots), so this doubles as dedup for a
//     genuine retry of identical work.
//
//  2. Abandoned-inference anomaly counting. #432's forensics: the incident
//     ran with kernel vitals reading 0err/0anom for hours while a request sat
//     abandoned server-side — the only trace was a WARN log line nobody was
//     watching. recordAbandonedInference increments a counter that
//     buildKernelHealthSnapshot folds into the next autonomic tick's
//     HealthCounts, so an abandoned/canceled inference is visible in the
//     same proprioceptive surface (bus_kernel_proprio) operators and the
//     autonomic loop already watch, not just in logs.
package engine

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// inflightRequests tracks RequestIDs currently executing a cancel-safe
// completion call. Empty-string IDs are never tracked (some call sites,
// e.g. ad hoc internal completions, may not set one) — dedup only applies
// where the caller has opted in with a real identity.
var inflightRequests sync.Map // map[string]struct{}

// beginInflightInference registers requestID as in-flight. Returns false if
// the ID is already registered (a prior attempt under the same identity may
// still be generating server-side) — the caller must not proceed.
func beginInflightInference(requestID string) bool {
	if requestID == "" {
		return true
	}
	_, loaded := inflightRequests.LoadOrStore(requestID, struct{}{})
	if loaded {
		slog.Warn("inference: retry refused, request already in flight",
			"request_id", requestID,
		)
		return false
	}
	return true
}

// endInflightInference releases requestID. Safe to call even if
// beginInflightInference returned false or was never called (no-op on a
// missing key).
func endInflightInference(requestID string) {
	if requestID == "" {
		return
	}
	inflightRequests.Delete(requestID)
}

// abandonedInferenceCount is incremented every time an internal,
// non-interactive inference call returns an error that traces back to
// context cancellation/deadline (or, conservatively, any error from a
// cancel-safe call site, since a mid-stream error after partial generation
// is the same "we gave up on a request the server may still be working"
// shape). Read by buildKernelHealthSnapshot so the next autonomic tick
// surfaces it as an anomaly rather than only a WARN log line (#432).
var abandonedInferenceCount atomic.Int64

// recordAbandonedInference logs and counts an abandoned/canceled internal
// inference call. site identifies the call site (e.g. "local-harness-assess",
// "chat-complete") for log correlation; requestID may be empty.
func recordAbandonedInference(site, requestID string, err error) {
	abandonedInferenceCount.Add(1)
	slog.Warn("inference: request abandoned or canceled",
		"site", site,
		"request_id", requestID,
		"err", err,
	)
}

// abandonedInferenceSnapshot returns the current cumulative count of
// abandoned/canceled internal inference calls since process start, and the
// delta since the last call to this function (used by
// buildKernelHealthSnapshot to report a per-tick anomaly count while
// AbandonedInferenceTotal keeps a running total for post-hoc audit).
var lastAbandonedInferenceSnapshot atomic.Int64

func abandonedInferenceSnapshot() (total int64, delta int64) {
	total = abandonedInferenceCount.Load()
	prev := lastAbandonedInferenceSnapshot.Swap(total)
	delta = total - prev
	if delta < 0 {
		delta = 0
	}
	return total, delta
}

// abandonedInferencePeek is the non-consuming counterpart to
// abandonedInferenceSnapshot: it returns the same (total, delta-since-last-
// consuming-read) pair but does NOT advance lastAbandonedInferenceSnapshot.
//
// abandonedInferenceSnapshot's swap-and-consume semantics assume a single
// production reader (the autonomic ticker, which needs "delta since my last
// tick" to drive #432 escalation). A second concurrent consumer would steal
// that delta out from under the ticker — it would observe delta=0 on its next
// tick even though a real abandonment happened, silently suppressing
// escalateAbandonedInference. Informational callers (e.g. the ambient-state-
// of-self block surfaced to looped kernel-interior dispatch) must use this
// peek instead so the ticker remains the sole consumer of the watermark.
func abandonedInferencePeek() (total int64, delta int64) {
	total = abandonedInferenceCount.Load()
	prev := lastAbandonedInferenceSnapshot.Load()
	delta = total - prev
	if delta < 0 {
		delta = 0
	}
	return total, delta
}

// resetAbandonedInferenceCounterForTest zeroes the abandoned-inference delta
// baseline so a test that asserts an exact anomaly count (including zero,
// i.e. AllGreen()) is not affected by unrelated tests elsewhere in the
// package that legitimately exercise an error path through
// CompleteCancelSafeIfSupported call sites (recordAbandonedInference is a
// process-global counter by design — the production consumer is "whatever
// accumulated since the last autonomic tick" — but that same global-since-
// last-read semantic makes cross-test pollution possible when many
// unrelated tests call buildKernelHealthSnapshot directly instead of going
// through the ticker). Test-only; not used by production code paths.
func resetAbandonedInferenceCounterForTest() {
	total := abandonedInferenceCount.Load()
	lastAbandonedInferenceSnapshot.Store(total)
}
