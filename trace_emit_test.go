// trace_emit_test.go — Integration tests for the cycle-trace emit site
// (G3 pilot of ADR-084 Phase 2: dataref emit via BlobStore).

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/trace"
)

// resetTraceEmitterForTest clears the package-level atomics that
// InstallTraceEmitter and the dataref branch consult. Tests that flip the
// emit-site env var MUST call this on setup and cleanup so the global state
// doesn't leak across subtests (or into unrelated tests that happen to also
// exercise the trace path).
func resetTraceEmitterForTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		traceBusMgr.Store(nil)
		traceBlobStore.Store(nil)
		_ = os.Unsetenv(traceDatarefEnvVar)
	})
	traceBusMgr.Store(nil)
	traceBlobStore.Store(nil)
	_ = os.Unsetenv(traceDatarefEnvVar)
}

// waitForBusEvents spins briefly until the bus's events.jsonl has at least n
// lines or the deadline expires. emitCycleEvent dispatches the actual append
// on a goroutine, so a direct read right after the emit races the writer.
func waitForBusEvents(t *testing.T, root, busID string, n int, deadline time.Duration) []CogBlock {
	t.Helper()
	end := time.Now().Add(deadline)
	path := filepath.Join(root, ".cog", ".state", "buses", busID, "events.jsonl")
	for time.Now().Before(end) {
		data, err := os.ReadFile(path)
		if err == nil {
			var lines []string
			for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				if l != "" {
					lines = append(lines, l)
				}
			}
			if len(lines) >= n {
				out := make([]CogBlock, 0, len(lines))
				for _, l := range lines {
					var b CogBlock
					if err := json.Unmarshal([]byte(l), &b); err == nil {
						out = append(out, b)
					}
				}
				return out
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events on bus %s", n, busID)
	return nil
}

// TestEmitCycleEvent_InlinePayload_Legacy verifies the pre-ADR-084 default:
// with no env flag and no BlobStore, emitCycleEvent writes the trace payload
// inline on the envelope (Payload populated, Digest/MediaType empty). This
// pins backward compatibility for consumers that have not been updated.
func TestEmitCycleEvent_InlinePayload_Legacy(t *testing.T) {
	resetTraceEmitterForTest(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", ".state", "buses"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	mgr := newBusSessionManager(root)
	// No BlobStore → the dataref branch is never taken.
	InstallTraceEmitter(mgr, root, nil)

	ev, err := trace.NewAssessment("cog", "cycle-legacy-1", "observe", 0.9, "legacy-inline")
	if err != nil {
		t.Fatalf("build assessment: %v", err)
	}
	emitCycleEvent(ev)

	evts := waitForBusEvents(t, root, traceBusID, 1, 2*time.Second)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	got := evts[0]
	if got.Type != "cycle.assessment" {
		t.Errorf("type=%q want cycle.assessment", got.Type)
	}
	if got.Payload == nil {
		t.Fatal("legacy emit: Payload is nil; expected inline map")
	}
	if got.Digest != "" || got.MediaType != "" {
		t.Errorf("legacy emit: unexpected digest=%q media_type=%q", got.Digest, got.MediaType)
	}
	if got.Payload["kind"] != "assessment" {
		t.Errorf("payload.kind=%v want assessment", got.Payload["kind"])
	}
}

// TestEmitCycleEvent_DatarefViaBlobStore is the G3 pilot roundtrip: the
// cycle_trace site is allow-listed via COGOS_DATAREF_EMIT, a BlobStore is
// wired, and emitCycleEvent is expected to (a) write the payload bytes into
// the blob store, (b) emit an envelope carrying only Digest + MediaType +
// Size, and (c) produce bytes that a consumer can resolve through the
// BlobStore back to the original trace.CycleEvent JSON.
func TestEmitCycleEvent_DatarefViaBlobStore(t *testing.T) {
	resetTraceEmitterForTest(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", ".state", "buses"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	bs := engine.NewBlobStore(root)
	if err := bs.Init(); err != nil {
		t.Fatalf("blob store init: %v", err)
	}
	mgr := newBusSessionManager(root)

	// Gate the new emit behind the env var so only this test's emit site
	// takes the dataref path; every other emit in the binary is unaffected.
	t.Setenv(traceDatarefEnvVar, "cycle_trace")
	InstallTraceEmitter(mgr, root, bs)

	ev, err := trace.NewAssessment("cog", "cycle-dataref-1", "propose", 0.42, "g3-pilot")
	if err != nil {
		t.Fatalf("build assessment: %v", err)
	}
	// Re-marshal via the canonical trace JSON shape to know what bytes
	// should land in the blob store. emitCycleEvent does the exact same
	// json.Marshal(ev) internally, so the digests must match.
	wantBytes, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal want-bytes: %v", err)
	}

	emitCycleEvent(ev)

	evts := waitForBusEvents(t, root, traceBusID, 1, 2*time.Second)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	got := evts[0]

	if got.Type != "cycle.assessment" {
		t.Errorf("type=%q want cycle.assessment", got.Type)
	}
	if got.Payload != nil {
		t.Errorf("dataref emit: Payload should be nil; got %v", got.Payload)
	}
	if got.MediaType != traceMediaType {
		t.Errorf("media_type=%q want %q", got.MediaType, traceMediaType)
	}
	if got.Size != len(wantBytes) {
		t.Errorf("size=%d want %d", got.Size, len(wantBytes))
	}
	if !strings.HasPrefix(got.Digest, "sha256:") || len(got.Digest) != len("sha256:")+64 {
		t.Fatalf("digest=%q does not look like sha256:<hex64>", got.Digest)
	}

	// Consumer-side resolution: strip the "sha256:" prefix and round-trip
	// through the shared blob store to recover the original payload bytes.
	hexHash := strings.TrimPrefix(got.Digest, "sha256:")
	raw, err := bs.Get(hexHash)
	if err != nil {
		t.Fatalf("blob store Get: %v", err)
	}
	if string(raw) != string(wantBytes) {
		t.Fatalf("blob roundtrip mismatch:\n got %s\nwant %s", raw, wantBytes)
	}

	// A decoded CycleEvent from the resolved bytes should exactly equal
	// what the caller emitted — this is what an updated consumer sees.
	var decoded trace.CycleEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode resolved bytes: %v", err)
	}
	if decoded.CycleID != "cycle-dataref-1" || decoded.Kind != trace.KindAssessment {
		t.Errorf("decoded event cycle_id=%q kind=%q mismatch",
			decoded.CycleID, decoded.Kind)
	}
}

// TestEmitCycleEvent_DatarefOtherSiteNotMatched verifies that the env var
// check is a substring-safe token match: setting COGOS_DATAREF_EMIT to an
// unrelated site ("chat_response") leaves the cycle_trace emit on the legacy
// inline path. This is the property that makes the flag safe to roll out one
// site at a time without accidentally capturing neighbouring emit sites.
func TestEmitCycleEvent_DatarefOtherSiteNotMatched(t *testing.T) {
	resetTraceEmitterForTest(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", ".state", "buses"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	bs := engine.NewBlobStore(root)
	if err := bs.Init(); err != nil {
		t.Fatalf("blob store init: %v", err)
	}
	mgr := newBusSessionManager(root)

	t.Setenv(traceDatarefEnvVar, "chat_response,some_other_site")
	InstallTraceEmitter(mgr, root, bs)

	ev, err := trace.NewAssessment("cog", "cycle-not-matched", "observe", 0.1, "other-site")
	if err != nil {
		t.Fatalf("build assessment: %v", err)
	}
	emitCycleEvent(ev)

	evts := waitForBusEvents(t, root, traceBusID, 1, 2*time.Second)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	got := evts[0]
	if got.Digest != "" {
		t.Errorf("non-matching token should fall back to inline; got digest=%q", got.Digest)
	}
	if got.Payload == nil {
		t.Errorf("non-matching token should populate inline Payload; got nil")
	}
}
