// bus_session_generation_fencing_test.go — regression coverage for the
// registry-corruption race the cog-review AI reviewer flagged as CONFIRMED
// against PR #534 (internal/engine/bus_session.go:602): resetRegistrySeq's
// write and updateRegistrySeqIfNewer's write both run outside m.mu with no
// fencing between them, so a non-rotating append's registry advance
// (captured for the PRE-rotation generation) can land AFTER a rotating
// append's registry reset (for the POST-rotation generation), silently
// reinstating the pre-rotation terminal seq and freezing the registry
// against every subsequent append — the exact bug PR #534 set out to fix,
// reached via a race instead of a deterministic gap.
//
// TestAppendEvent_GenerationFencing_StaleAdvanceRejectedAfterRotationReset
// reproduces this deterministically using appendEventPostUnlockHook (see its
// doc comment in bus_session.go) to force the ordering the reviewer
// described:
//
//  1. "A" appends the last event of the pre-rotation generation (does NOT
//     itself cross the rotation threshold). It releases m.mu, then blocks
//     in the hook — mirroring A being descheduled between m.mu.Unlock() and
//     its registry write call.
//  2. "B" appends the next event, crosses the rotation threshold, rotates,
//     and runs resetRegistrySeq to completion synchronously on the main
//     test goroutine — mirroring B's rotate+prune+reset path finishing
//     first.
//  3. A is released and its delayed, stale-generation registry write is
//     allowed to proceed.
//
// The test asserts the registry reflects B's reset (not A's stale advance)
// immediately after step 3, and that a real post-rotation append still
// advances the registry correctly afterward (proving this isn't just a
// smaller-window version of the same freeze, but a genuine fix).
package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendEvent_GenerationFencing_StaleAdvanceRejectedAfterRotationReset(t *testing.T) {
	// Not parallel: mutates package-level eventsFileMaxBytes and installs
	// appendEventPostUnlockHook — same constraint as the other rotation
	// tests in bus_session_test.go.
	root := t.TempDir()

	originalMaxBytes := eventsFileMaxBytes
	t.Cleanup(func() {
		eventsFileMaxBytes = originalMaxBytes
		appendEventPostUnlockHook = nil
	})

	m := NewBusSessionManager(root)
	const busID = "gen-fence-bus"
	if err := m.RegisterBus(busID, "sess", "test"); err != nil {
		t.Fatalf("RegisterBus: %v", err)
	}

	payload := map[string]interface{}{"data": strings.Repeat("y", 500)}
	appendPlain := func() {
		t.Helper()
		if _, err := m.AppendEvent(busID, "m", "tester", payload); err != nil {
			t.Fatalf("priming AppendEvent: %v", err)
		}
	}

	// Reach steady state (a real prev-hash present in every line, so line
	// size is stable) at seq=1, then measure the per-line size increment
	// from a second, steady-state line (seq=2 -> seq=3).
	appendPlain() // seq=1
	sizeAfter1 := statSizeT(t, m.EventsPath(busID))
	appendPlain() // seq=2
	sizeAfter2 := statSizeT(t, m.EventsPath(busID))
	appendPlain() // seq=3
	sizeAfter3 := statSizeT(t, m.EventsPath(busID))
	lineSize := sizeAfter3 - sizeAfter2
	if lineSize <= 0 {
		t.Fatalf("could not measure a positive per-line size (sizeAfter2=%d sizeAfter3=%d)", sizeAfter2, sizeAfter3)
	}
	_ = sizeAfter1

	// Threshold sits comfortably between "current size + 1 line" (seq=4,
	// this test's A — must NOT rotate) and "current size + 2 lines" (seq=5,
	// this test's B — must rotate). RFC3339Nano timestamp width varies by a
	// few bytes call to call; lineSize/2 of headroom on each side dwarfs
	// that.
	eventsFileMaxBytes = sizeAfter3 + lineSize + lineSize/2

	// Install the hook: pause any NON-rotating append's registry write
	// until released, and report when the first one has reached that point.
	aInHook := make(chan struct{})
	var aInHookOnce sync.Once
	released := make(chan struct{})
	appendEventPostUnlockHook = func(hbusID string, rotated bool, gen int64) {
		if hbusID != busID || rotated {
			return
		}
		aInHookOnce.Do(func() { close(aInHook) })
		<-released
	}

	// A: seq=4, non-rotating (threshold not yet crossed). Runs on its own
	// goroutine so it can be parked in the hook while the test drives B.
	aDone := make(chan error, 1)
	go func() {
		_, err := m.AppendEvent(busID, "m", "tester", payload)
		aDone <- err
	}()

	select {
	case <-aInHook:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for A to reach the post-unlock hook")
	}

	// B: seq=5, crosses the threshold computed above, rotates. Runs
	// synchronously on the test goroutine — by the time this call returns,
	// resetRegistrySeq has completed (it blocks on the registry lock; see
	// its doc comment), and does so uncontended since A is parked before
	// its own registryFileMu attempt.
	if _, err := m.AppendEvent(busID, "m", "tester", payload); err != nil {
		t.Fatalf("B (rotating) AppendEvent: %v", err)
	}

	entry := lookupRegistryEntryT(t, m, busID)
	if entry.Generation != 1 {
		t.Fatalf("rotation did not advance the persisted generation: got %d, want 1", entry.Generation)
	}
	if entry.LastEventSeq != 0 {
		t.Fatalf("registry not reset by rotation before A's delayed write landed: LastEventSeq=%d, want 0", entry.LastEventSeq)
	}

	// Release A: its delayed, stale (pre-rotation-generation) advance now
	// races to land on top of B's already-completed reset.
	close(released)
	select {
	case err := <-aDone:
		if err != nil {
			t.Fatalf("A (delayed advance) AppendEvent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for A to complete after release")
	}
	appendEventPostUnlockHook = nil

	// THE ASSERTION: A's stale gen=0 advance (seq=4) must be rejected, not
	// silently reinstate the pre-rotation terminal seq over B's gen=1
	// reset. Without generation fencing, this is exactly where the bug
	// reproduces: entry.LastEventSeq(0) >= 4 is false, so the old
	// (seq-only) guard lets A's write through, corrupting the registry back
	// to the archived file's terminal value.
	entry = lookupRegistryEntryT(t, m, busID)
	if entry.LastEventSeq != 0 {
		t.Fatalf("registry corrupted by a stale cross-generation advance landing after the rotation reset: "+
			"LastEventSeq=%d, want 0 (A's gen=0 write for seq=4 must be rejected once the registry is at gen=1)",
			entry.LastEventSeq)
	}
	if entry.Generation != 1 {
		t.Fatalf("stale advance regressed the persisted generation: got %d, want 1", entry.Generation)
	}

	// Prove this isn't just "coincidentally still zero" but genuinely
	// unfrozen: a real post-rotation append (seq=1 in the new generation)
	// must be correctly recorded. Pre-fix, this is where the freeze shows
	// up for good — every future small post-rotation seq would fail
	// entry.LastEventSeq(4) >= seq and never be recorded again.
	if _, err := m.AppendEvent(busID, "m", "tester", map[string]interface{}{"data": "post-rotation-settle"}); err != nil {
		t.Fatalf("post-rotation settle append: %v", err)
	}
	entry = lookupRegistryEntryT(t, m, busID)
	if entry.LastEventSeq != 1 {
		t.Fatalf("registry frozen against the new generation's own appends: LastEventSeq=%d, want 1 "+
			"(the exact staleness bug this fencing closes — a corrupted registry never recovers "+
			"until the NEXT rotation, if this were left unfixed)", entry.LastEventSeq)
	}
}

func statSizeT(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func lookupRegistryEntryT(t *testing.T, m *BusSessionManager, busID string) BusRegistryEntry {
	t.Helper()
	for _, e := range m.LoadRegistry() {
		if e.BusID == busID {
			return e
		}
	}
	t.Fatalf("bus %q missing from registry", busID)
	return BusRegistryEntry{}
}

// TestBusRegistryEntry_LegacyJSONWithoutGeneration covers item 4 of the
// generation-fencing fix: a registry.json written before the Generation
// field existed has no "generation" key at all for any entry. Go's
// json.Unmarshal leaves a struct field absent from the input at its zero
// value, so this must load as Generation=0 — which is also the value a
// bus that has genuinely never rotated carries (see the field's doc
// comment on cogfield.BusRegistryEntry) — and the write paths must treat
// that exactly like any other generation-0 bus, not refuse to advance it.
func TestBusRegistryEntry_LegacyJSONWithoutGeneration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewBusSessionManager(root)

	const busID = "legacy-bus"
	busesDir := m.BusesDir()
	if err := os.MkdirAll(busesDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Hand-written registry.json in the pre-Generation-field shape — no
	// "generation" key present anywhere in the JSON, simulating a
	// registry.json written by a binary built before this fix. Registered
	// but never appended-to (last_event_seq=0): the events.jsonl created
	// below starts genuinely empty, so this matches what a real legacy
	// entry in that state would look like — no manufactured mismatch
	// between the registry's stale metadata and the actual file.
	legacyJSON := `[
  {
    "bus_id": "` + busID + `",
    "state": "active",
    "participants": ["test:session:sess"],
    "transport": "file",
    "endpoint": ".cog/.state/buses/legacy-bus",
    "created_at": "2026-01-01T00:00:00Z",
    "last_event_seq": 0,
    "last_event_at": "2026-01-01T00:00:00Z",
    "event_count": 0
  }
]`
	if err := os.WriteFile(m.RegistryPath(), []byte(legacyJSON), 0644); err != nil {
		t.Fatalf("write legacy registry.json: %v", err)
	}

	// 1. Loading must succeed and zero-value the missing field, not error
	// or drop the entry.
	loaded := m.LoadRegistry()
	if len(loaded) != 1 {
		t.Fatalf("legacy registry.json did not load as 1 entry: got %d", len(loaded))
	}
	if loaded[0].Generation != 0 {
		t.Fatalf("legacy entry's missing generation field did not zero-value: got %d, want 0", loaded[0].Generation)
	}
	if loaded[0].LastEventSeq != 0 {
		t.Fatalf("legacy entry's other fields corrupted by the load: LastEventSeq=%d, want 0", loaded[0].LastEventSeq)
	}

	// 2. The bus directory + events.jsonl don't exist yet for this
	// hand-written entry (only registry.json does) — EnsureBus/AppendEvent
	// must be able to create them and advance the registry normally. This
	// is the actual behavioral check: a legacy entry's implicit
	// generation=0 must be usable by the write paths, not just parseable.
	if err := os.MkdirAll(filepath.Join(busesDir, busID), 0755); err != nil {
		t.Fatalf("MkdirAll bus dir: %v", err)
	}
	if _, err := m.AppendEvent(busID, "m", "tester", map[string]interface{}{"x": 1}); err != nil {
		t.Fatalf("AppendEvent against a legacy (pre-Generation-field) registry entry: %v", err)
	}

	entry := lookupRegistryEntryT(t, m, busID)
	if entry.Generation != 0 {
		t.Fatalf("Generation changed by a non-rotating append: got %d, want 0", entry.Generation)
	}
	// Confirms the write path advances a legacy entry the same way it
	// would a native generation=0 entry, not specially or incorrectly.
	if entry.LastEventSeq != 1 {
		t.Fatalf("registry not advanced past a legacy entry: LastEventSeq=%d, want 1", entry.LastEventSeq)
	}

	// 3. Round-trip: writing the entry back out (already exercised by
	// AppendEvent's saveRegistry call above) must now include an explicit
	// "generation" key, confirmed by re-parsing the raw bytes — proves this
	// process's writes upgrade the on-disk shape rather than silently
	// preserving the old one forever.
	raw, err := os.ReadFile(m.RegistryPath())
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var rawEntries []map[string]interface{}
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		t.Fatalf("registry.json not valid JSON after write-back: %v", err)
	}
	found := false
	for _, e := range rawEntries {
		if e["bus_id"] == busID {
			found = true
			if _, ok := e["generation"]; !ok {
				t.Errorf("write-back did not add an explicit \"generation\" key: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("legacy entry missing from write-back")
	}
}
