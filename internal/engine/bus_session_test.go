// bus_session_test.go — unit tests for BusSessionManager.
//
// Track 5 Phase 3: verifies hash-chain continuity, bus isolation, and the
// byte-compat storage layout (.cog/.state/buses/{id}/events.jsonl).
package engine

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/pathsafe"
)

// TestBusSessionAppendAndRead covers the basic seq/hash chain and JSONL
// storage layout.
func TestBusSessionAppendAndRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	if err := mgr.EnsureBus("bus-a"); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}

	e1, err := mgr.AppendEvent("bus-a", "message", "alice", map[string]interface{}{"content": "hello"})
	if err != nil {
		t.Fatalf("AppendEvent 1: %v", err)
	}
	if e1.Seq != 1 {
		t.Errorf("seq = %d, want 1", e1.Seq)
	}
	if e1.Hash == "" || len(e1.Hash) != 64 {
		t.Errorf("hash not 64-char hex: %q", e1.Hash)
	}
	if _, err := hex.DecodeString(e1.Hash); err != nil {
		t.Errorf("hash not lowercase hex: %v", err)
	}
	if len(e1.Prev) != 0 || e1.PrevHash != "" {
		t.Errorf("first event should have no prev: prev=%v prev_hash=%q", e1.Prev, e1.PrevHash)
	}

	e2, err := mgr.AppendEvent("bus-a", "message", "bob", map[string]interface{}{"content": "world"})
	if err != nil {
		t.Fatalf("AppendEvent 2: %v", err)
	}
	if e2.Seq != 2 {
		t.Errorf("seq = %d, want 2", e2.Seq)
	}
	if len(e2.Prev) != 1 || e2.Prev[0] != e1.Hash {
		t.Errorf("prev chain broken: got %v, want [%s]", e2.Prev, e1.Hash)
	}
	if e2.PrevHash != e1.Hash {
		t.Errorf("prev_hash = %q, want %q", e2.PrevHash, e1.Hash)
	}

	// Verify storage layout matches root: .cog/.state/buses/{bus_id}/events.jsonl
	eventsFile := filepath.Join(root, ".cog", ".state", "buses", "bus-a", "events.jsonl")
	b, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Errorf("events.jsonl has %d lines, want 2", len(lines))
	}

	// Verify ReadEvents de-dups by seq and preserves order.
	events, err := mgr.ReadEvents("bus-a")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("seq order broken: %d, %d", events[0].Seq, events[1].Seq)
	}
}

// TestBusSessionHashChainRecompute verifies that re-computing the hash of a
// read-back event yields the stored hash (byte-compat with root's hash).
func TestBusSessionHashChainRecompute(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	_, _ = mgr.AppendEvent("chain", "event.a", "sender", map[string]interface{}{"k": "v"})
	_, _ = mgr.AppendEvent("chain", "event.b", "sender", map[string]interface{}{"k": 42.0})
	_, _ = mgr.AppendEvent("chain", "event.c", "sender", map[string]interface{}{"k": true})

	events, err := mgr.ReadEvents("chain")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}

	var prevHash string
	for i, e := range events {
		// Re-compute the hash from the event and compare.
		recomputed := computeBusBlockHash(&e)
		if recomputed != e.Hash {
			t.Errorf("event %d: recomputed hash %q != stored hash %q", i, recomputed, e.Hash)
		}
		// Verify the chain links.
		if i == 0 {
			if e.PrevHash != "" {
				t.Errorf("event 0 PrevHash = %q, want empty", e.PrevHash)
			}
		} else if e.PrevHash != prevHash {
			t.Errorf("event %d PrevHash = %q, want %q", i, e.PrevHash, prevHash)
		}
		prevHash = e.Hash
	}
}

// TestBusSessionIsolation verifies two buses don't cross-contaminate.
func TestBusSessionIsolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	a1, _ := mgr.AppendEvent("bus-a", "m", "x", map[string]interface{}{"v": 1})
	b1, _ := mgr.AppendEvent("bus-b", "m", "x", map[string]interface{}{"v": 1})
	a2, _ := mgr.AppendEvent("bus-a", "m", "x", map[string]interface{}{"v": 2})

	if a1.Seq != 1 || b1.Seq != 1 || a2.Seq != 2 {
		t.Errorf("bus seqs wrong: a1=%d b1=%d a2=%d", a1.Seq, b1.Seq, a2.Seq)
	}
	if len(b1.Prev) != 0 {
		t.Errorf("bus-b first event shouldn't chain from bus-a: prev=%v", b1.Prev)
	}

	aEvents, _ := mgr.ReadEvents("bus-a")
	bEvents, _ := mgr.ReadEvents("bus-b")
	if len(aEvents) != 2 {
		t.Errorf("bus-a has %d events, want 2", len(aEvents))
	}
	if len(bEvents) != 1 {
		t.Errorf("bus-b has %d events, want 1", len(bEvents))
	}
}

// TestBusSessionRegistry covers RegisterBus + LoadRegistry round-trip and
// verifies the registry.json format matches root.
func TestBusSessionRegistry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	if err := mgr.EnsureBus("r1"); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}
	if err := mgr.RegisterBus("r1", "sess1", "test"); err != nil {
		t.Fatalf("RegisterBus: %v", err)
	}

	entries := mgr.LoadRegistry()
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].BusID != "r1" || entries[0].State != "active" {
		t.Errorf("registry shape wrong: %+v", entries[0])
	}
	if entries[0].Transport != "file" {
		t.Errorf("Transport = %q, want 'file'", entries[0].Transport)
	}
	if entries[0].Endpoint != filepath.Join(".cog", ".state", "buses", "r1") {
		t.Errorf("Endpoint = %q", entries[0].Endpoint)
	}

	// Append an event, verify registry got updated.
	_, _ = mgr.AppendEvent("r1", "m", "from", map[string]interface{}{"x": 1})
	entries = mgr.LoadRegistry()
	if entries[0].LastEventSeq != 1 || entries[0].EventCount != 1 {
		t.Errorf("registry not updated after append: %+v", entries[0])
	}

	// Verify registry.json is valid JSON with the expected field names.
	regBytes, err := os.ReadFile(mgr.RegistryPath())
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal(regBytes, &parsed); err != nil {
		t.Fatalf("registry.json not valid JSON: %v", err)
	}
	wantFields := []string{"bus_id", "state", "participants", "transport", "endpoint", "created_at", "last_event_seq", "last_event_at", "event_count"}
	for _, f := range wantFields {
		if _, ok := parsed[0][f]; !ok {
			t.Errorf("registry.json missing field %q", f)
		}
	}
}

// ── #489 round 2: colon-bearing bus_id must not reach the filesystem raw ───
//
// cog-review on PR #504 named this seam directly: bus_id is a caller-supplied
// free-form string from an HTTP JSON body (POST /v1/bus/open, POST
// /v1/bus/send), structurally identical to the session keys this PR already
// sanitizes elsewhere, and it was only guarded by validPathComponent — which
// blocks traversal/separators but not colons or the other NTFS-illegal
// characters. These tests mirror TestAppendEventSanitizesColonKey in
// ledger_test.go and the call-site tests in pkg/pathsafe/pathsafe_test.go.

// TestBusSessionSanitizesColonBusID covers the concrete repro: a bus_id of
// the "origin:agent" shape (e.g. "http:cog") must round-trip through
// EnsureBus/AppendEvent/ReadEvents while the on-disk directory name is
// NTFS-legal.
func TestBusSessionSanitizesColonBusID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	busID := "http:cog"
	if err := mgr.EnsureBus(busID); err != nil {
		t.Fatalf("EnsureBus(%q): %v", busID, err)
	}

	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	entries, err := os.ReadDir(busesDir)
	if err != nil {
		t.Fatalf("read buses dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("buses dir has %d entries, want 1: %+v", len(entries), entries)
	}
	if strings.Contains(entries[0].Name(), ":") {
		t.Fatalf("on-disk bus dir %q still contains a colon", entries[0].Name())
	}
	want := pathsafe.SanitizeComponent(busID)
	if entries[0].Name() != want {
		t.Fatalf("on-disk bus dir = %q, want %q", entries[0].Name(), want)
	}

	// AppendEvent/ReadEvents must work through the raw (unsanitized) busID —
	// callers never have to sanitize themselves.
	if _, err := mgr.AppendEvent(busID, "message", "alice", map[string]interface{}{"content": "hi"}); err != nil {
		t.Fatalf("AppendEvent(%q): %v", busID, err)
	}
	events, err := mgr.ReadEvents(busID)
	if err != nil {
		t.Fatalf("ReadEvents(%q): %v", busID, err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].BusID != busID {
		t.Errorf("event.BusID = %q, want raw %q (only the on-disk path is sanitized)", events[0].BusID, busID)
	}
}

// TestEventsPathColonBusID_BothSeparators mirrors
// pkg/pathsafe.TestSanitizeComponent_ColonKey_BothSeparators but at the real
// engine call site (EventsPath), proving the actual path bus_session.go
// builds stays colon-free whether treated as a "/"-joined POSIX path or a
// simulated "\"-joined Windows path.
func TestEventsPathColonBusID_BothSeparators(t *testing.T) {
	t.Parallel()
	mgr := NewBusSessionManager(t.TempDir())
	busID := "http:cog"

	forwardSlashPath := mgr.EventsPath(busID)
	if strings.Contains(filepath.Base(filepath.Dir(forwardSlashPath)), ":") {
		t.Fatalf("forward-slash path %q still has a colon in the bus_id component", forwardSlashPath)
	}

	// Simulate the Windows form of the same path: a drive-letter colon
	// ("C:") is legal and expected; sanitizing the bus_id component must not
	// introduce any OTHER colon.
	winPath := "C:\\workspace\\.cog\\.state\\buses\\" + pathsafe.SanitizeComponent(busID) + "\\events.jsonl"
	if n := strings.Count(winPath, ":"); n != 1 {
		t.Fatalf("simulated Windows path has %d colons, want exactly 1 (the drive letter): %q", n, winPath)
	}
}

// TestBusSessionRegistryEndpointSanitizedForColonBusID guards the Endpoint
// metadata field recorded in registry.json: it must match the actual
// sanitized on-disk directory EnsureBus created, not the raw caller-supplied
// bus_id, or the two would silently drift apart for any bus_id containing an
// NTFS-illegal character.
func TestBusSessionRegistryEndpointSanitizedForColonBusID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	busID := "http:cog"
	if err := mgr.EnsureBus(busID); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}
	if err := mgr.RegisterBus(busID, "sess1", "test"); err != nil {
		t.Fatalf("RegisterBus: %v", err)
	}

	entries := mgr.LoadRegistry()
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].Endpoint, ":") {
		t.Fatalf("registry Endpoint %q still contains a colon", entries[0].Endpoint)
	}
	want := filepath.Join(".cog", ".state", "buses", pathsafe.SanitizeComponent(busID))
	if entries[0].Endpoint != want {
		t.Fatalf("Endpoint = %q, want %q", entries[0].Endpoint, want)
	}
}

// TestAppendEventRotationSanitizesColonBusID covers the size-rotation archive
// path in AppendEvent — the one busID-to-directory join that did not route
// through EventsPath before this fix. Before the fix, rotation for a
// colon-bearing busID would build the archive path under an unsanitized
// "http:cog" directory instead of the one EnsureBus/EventsPath actually use;
// this test would fail with "no such file or directory" against that old
// behavior because it looks for the archive under the SANITIZED bus dir.
func TestAppendEventRotationSanitizesColonBusID(t *testing.T) {
	orig := eventsFileMaxBytes
	eventsFileMaxBytes = 1 // rotate after the very first write
	t.Cleanup(func() { eventsFileMaxBytes = orig })

	root := t.TempDir()
	mgr := NewBusSessionManager(root)
	busID := "http:cog"

	if _, err := mgr.AppendEvent(busID, "message", "alice", map[string]interface{}{"content": "hi"}); err != nil {
		t.Fatalf("AppendEvent 1: %v", err)
	}
	if _, err := mgr.AppendEvent(busID, "message", "bob", map[string]interface{}{"content": "again"}); err != nil {
		t.Fatalf("AppendEvent 2: %v", err)
	}

	busDir := filepath.Join(root, ".cog", ".state", "buses", pathsafe.SanitizeComponent(busID))
	entries, err := os.ReadDir(busDir)
	if err != nil {
		t.Fatalf("read bus dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "events.") && strings.HasSuffix(e.Name(), ".jsonl") && e.Name() != "events.jsonl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rotated archive file found in sanitized bus dir %s: %+v", busDir, entries)
	}

	// No sibling raw "http:cog" directory should exist alongside registry
	// bookkeeping files from an unsanitized join.
	busesDir := filepath.Join(root, ".cog", ".state", "buses")
	top, err := os.ReadDir(busesDir)
	if err != nil {
		t.Fatalf("read buses dir: %v", err)
	}
	dirCount := 0
	for _, e := range top {
		if strings.Contains(e.Name(), ":") {
			t.Fatalf("buses dir entry %q contains a colon: %+v", e.Name(), top)
		}
		if e.IsDir() {
			dirCount++
		}
	}
	if dirCount != 1 {
		t.Fatalf("buses dir has %d subdirectories, want exactly 1: %+v", dirCount, top)
	}
}

// TestBusSessionByteCompatJSONShape verifies that the JSON encoding of a bus
// event matches the shape captured from the live cogos-v3 daemon:
//
//	{v, bus_id, seq, ts, from, type, payload, prev?, prev_hash?, hash}
func TestBusSessionByteCompatJSONShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	// First event — no prev chain.
	e1, err := mgr.AppendEvent("phase3-test", "message", "phase3-test",
		map[string]interface{}{"content": "shape-probe"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	raw, _ := json.Marshal(e1)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Required fields that the bridge (cogos_bridge.py) reads on every
	// event. If any of these go missing, the bridge breaks.
	wantRequired := []string{"v", "bus_id", "seq", "ts", "from", "type", "payload", "hash"}
	for _, f := range wantRequired {
		if _, ok := parsed[f]; !ok {
			t.Errorf("first event missing required field %q", f)
		}
	}
	// On the first event `prev` + `prev_hash` should be omitted (omitempty).
	if _, has := parsed["prev"]; has {
		t.Errorf("first event unexpectedly has 'prev': %v", parsed["prev"])
	}
	if _, has := parsed["prev_hash"]; has {
		t.Errorf("first event unexpectedly has 'prev_hash': %v", parsed["prev_hash"])
	}

	// Second event — should carry prev and prev_hash.
	e2, _ := mgr.AppendEvent("phase3-test", "message", "phase3-test",
		map[string]interface{}{"content": "second"})
	raw2, _ := json.Marshal(e2)
	var p2 map[string]interface{}
	_ = json.Unmarshal(raw2, &p2)
	if _, ok := p2["prev"]; !ok {
		t.Errorf("second event missing 'prev'")
	}
	if _, ok := p2["prev_hash"]; !ok {
		t.Errorf("second event missing 'prev_hash'")
	}

	// Value-level checks on the first event.
	if parsed["v"].(float64) != 2 {
		t.Errorf("v = %v, want 2", parsed["v"])
	}
	if parsed["bus_id"].(string) != "phase3-test" {
		t.Errorf("bus_id = %v", parsed["bus_id"])
	}
	if parsed["seq"].(float64) != 1 {
		t.Errorf("seq = %v, want 1", parsed["seq"])
	}
	if parsed["type"].(string) != "message" {
		t.Errorf("type = %v", parsed["type"])
	}
	// Ts must be RFC3339-nano style.
	ts, ok := parsed["ts"].(string)
	if !ok || !strings.Contains(ts, "T") || !strings.HasSuffix(ts, "Z") {
		t.Errorf("ts shape wrong: %q", ts)
	}
	// Hash is lowercase hex, 64 chars.
	h, ok := parsed["hash"].(string)
	if !ok || len(h) != 64 {
		t.Errorf("hash shape wrong: %q", h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("hash not hex: %v", err)
	}
}

// TestAppendEvent_CacheAvoidsScan verifies that the in-memory seq/hash cache
// is populated after a successful AppendEvent and that subsequent appends do
// not need to scan events.jsonl.
//
// We confirm the cache works by:
//  1. Appending 3 events to prime the cache.
//  2. Clearing the cache entry manually (simulating a cold process restart).
//  3. Appending a 4th event — this causes one file scan to reprime the cache.
//  4. Appending 1000 more events — each should hit the cache (no additional
//     file scan).  The test asserts that the chain stays intact and that wall
//     time is O(n) rather than O(n²); no instrumentation hook is needed
//     because a 1000-event O(n²) scan at 182 MB would take seconds on any
//     CI machine, while the O(n) path finishes in milliseconds.
func TestAppendEvent_CacheAvoidsScan(t *testing.T) {
	// Not parallel: sets eventsFileMaxBytes to prevent rotation interference.
	root := t.TempDir()

	// Ensure rotation does not trigger during the 1000-event bulk run.
	original := eventsFileMaxBytes
	eventsFileMaxBytes = 1 << 30 // 1 GiB — won't be reached in this test
	t.Cleanup(func() { eventsFileMaxBytes = original })

	mgr := NewBusSessionManager(root)

	if err := mgr.EnsureBus("cache-bus"); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}

	// Prime: append 3 events normally.
	for i := 0; i < 3; i++ {
		if _, err := mgr.AppendEvent("cache-bus", "m", "tester", map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("AppendEvent prime %d: %v", i, err)
		}
	}

	// Manually clear the cache to simulate a cold process restart.
	mgr.mu.Lock()
	delete(mgr.lastSeq, "cache-bus")
	delete(mgr.lastHash, "cache-bus")
	mgr.mu.Unlock()

	// This append scans the file once to reprime the cache.
	e4, err := mgr.AppendEvent("cache-bus", "m", "tester", map[string]interface{}{"i": 3})
	if err != nil {
		t.Fatalf("AppendEvent after cache clear: %v", err)
	}
	if e4.Seq != 4 {
		t.Errorf("expected seq=4 after reprime, got %d", e4.Seq)
	}

	// Append 1000 more events — all should use the cache (no file scan per call).
	start := time.Now()
	const n = 1000
	for i := 0; i < n; i++ {
		if _, err := mgr.AppendEvent("cache-bus", "m", "tester", map[string]interface{}{"i": i + 4}); err != nil {
			t.Fatalf("bulk AppendEvent %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// With the cache, 1000 appends should complete in well under 2 seconds on
	// any modern machine.  Without the cache (O(n) scan per append on a growing
	// file) this would be O(n²) and would take many seconds even on tiny files.
	if elapsed > 2*time.Second {
		t.Errorf("1000 cache-hit appends took %v; expected O(n) (cache miss would be O(n²))", elapsed)
	}

	// Verify chain integrity on the last event.
	events, err := mgr.ReadEvents("cache-bus")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	want := 4 + n
	if len(events) != want {
		t.Errorf("want %d events, got %d", want, len(events))
	}
	// Sequential seqs, no gaps.
	for i, ev := range events {
		if ev.Seq != i+1 {
			t.Errorf("event %d: seq=%d, want %d", i, ev.Seq, i+1)
		}
	}
}

// TestAppendEvent_RotatesAtThreshold verifies that events.jsonl is renamed to a
// timestamped archive and a fresh file is created when the size threshold is
// reached.
func TestAppendEvent_RotatesAtThreshold(t *testing.T) {
	// Not parallel: mutates package-level eventsFileMaxBytes.
	root := t.TempDir()

	// Lower the threshold so we don't need to write gigabytes.
	original := eventsFileMaxBytes
	eventsFileMaxBytes = 512
	t.Cleanup(func() { eventsFileMaxBytes = original })

	mgr := NewBusSessionManager(root)
	busID := "rot-bus"

	if err := mgr.EnsureBus(busID); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}

	eventsFile := mgr.EventsPath(busID)
	busDir := filepath.Join(mgr.BusesDir(), busID)

	// Append events until the file has been rotated at least once.
	rotated := false
	for i := 0; i < 200; i++ {
		if _, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"data": strings.Repeat("x", 20)}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
		// Check whether any rotated file exists yet.
		entries, _ := os.ReadDir(busDir)
		for _, e := range entries {
			name := e.Name()
			if name != "events.jsonl" && strings.HasPrefix(name, "events.") && strings.HasSuffix(name, ".jsonl") {
				rotated = true
			}
		}
		if rotated {
			break
		}
	}

	if !rotated {
		t.Fatal("expected at least one rotation to occur, but none did")
	}

	// The live events.jsonl must still exist (created fresh after rotation).
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		t.Error("events.jsonl should exist after rotation (fresh file)")
	}

	// Verify the rotated archive has non-zero content.
	entries, err := os.ReadDir(busDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var archivePaths []string
	for _, e := range entries {
		name := e.Name()
		if name != "events.jsonl" && strings.HasPrefix(name, "events.") && strings.HasSuffix(name, ".jsonl") {
			archivePaths = append(archivePaths, filepath.Join(busDir, name))
		}
	}
	if len(archivePaths) == 0 {
		t.Fatal("no archive file found after rotation")
	}
	for _, ap := range archivePaths {
		fi, err := os.Stat(ap)
		if err != nil {
			t.Errorf("stat archive %s: %v", ap, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("archive %s is empty; expected rotated events to be present", ap)
		}
		// Verify the archive is valid JSONL (every line parses as a BusBlock).
		data, _ := os.ReadFile(ap)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var b BusBlock
			if err := json.Unmarshal([]byte(line), &b); err != nil {
				t.Errorf("archive %s: invalid JSON line: %v", ap, err)
			}
		}
	}
}

// TestAppendEvent_SeqResetsAcrossRotation verifies that after a size-based
// rotation the next event starts at seq=1 (per-file semantics): rotation
// clears the cache (lastSeq[busID]=0, lastHash[busID]="") so the next
// AppendEvent builds a new chain from seq=1, matching AppendEvent's
// size-based rotation branch which explicitly sets LastEventSeq=0 and
// EventCount=0 (both in-memory and, via resetRegistrySeq, in the registry)
// after the rename.
func TestAppendEvent_SeqResetsAcrossRotation(t *testing.T) {
	// Not parallel: mutates package-level eventsFileMaxBytes.
	root := t.TempDir()

	original := eventsFileMaxBytes
	eventsFileMaxBytes = 512
	t.Cleanup(func() { eventsFileMaxBytes = original })

	mgr := NewBusSessionManager(root)
	busID := "seq-reset-bus"

	if err := mgr.EnsureBus(busID); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}

	busDir := filepath.Join(mgr.BusesDir(), busID)

	// Append events until at least one rotation has occurred.
	var lastBeforeRotation *BusBlock
	for i := 0; i < 200; i++ {
		evt, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"data": strings.Repeat("y", 20)})
		if err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}

		entries, _ := os.ReadDir(busDir)
		rotated := false
		for _, e := range entries {
			name := e.Name()
			if name != "events.jsonl" && strings.HasPrefix(name, "events.") && strings.HasSuffix(name, ".jsonl") {
				rotated = true
			}
		}
		if rotated && lastBeforeRotation == nil {
			// Next append will be post-rotation.
			lastBeforeRotation = evt
			break
		}
	}

	if lastBeforeRotation == nil {
		t.Fatal("no rotation occurred; cannot test seq reset")
	}

	// First append after rotation must start a new chain at seq=1.
	post, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"data": "post-rotation"})
	if err != nil {
		t.Fatalf("AppendEvent post-rotation: %v", err)
	}
	if post.Seq != 1 {
		t.Errorf("post-rotation seq = %d, want 1 (per-file reset semantics)", post.Seq)
	}
	if post.PrevHash != "" {
		t.Errorf("post-rotation PrevHash = %q, want empty (new chain)", post.PrevHash)
	}
	if len(post.Prev) != 0 {
		t.Errorf("post-rotation Prev = %v, want empty (new chain)", post.Prev)
	}
}

// TestAppendEvent_RegistrySeqAdvancesAcrossRotation is a regression guard
// for the bug where a bus's registry entry stopped advancing forever after
// its FIRST size-based rotation: AppendEvent's rotation path reset the
// in-memory cursor (m.lastSeq[busID] = 0) but never touched the registry,
// so updateRegistrySeqIfNewer's monotonic guard (entry.LastEventSeq >= seq)
// rejected every later, smaller per-file seq — last_event_at froze at the
// moment of the first rotation even though the bus kept being written to.
//
// Rotates the bus at least twice and asserts the registry's LastEventSeq /
// EventCount / LastEventAt keep tracking live writes after BOTH rotations,
// not just surviving the first one — a fix that only patched the first
// rotation (e.g. a one-shot flag) would pass a single-rotation check and
// still be broken in production, where buses rotate repeatedly over time.
func TestAppendEvent_RegistrySeqAdvancesAcrossRotation(t *testing.T) {
	// Not parallel: mutates package-level eventsFileMaxBytes.
	root := t.TempDir()

	original := eventsFileMaxBytes
	eventsFileMaxBytes = 512
	t.Cleanup(func() { eventsFileMaxBytes = original })

	mgr := NewBusSessionManager(root)
	busID := "reg-seq-bus"

	if err := mgr.EnsureBus(busID); err != nil {
		t.Fatalf("EnsureBus: %v", err)
	}
	// Seed a registry entry — AppendEvent's registry-seq paths only update an
	// EXISTING entry (they never create one), matching production where
	// RegisterBus always runs before a bus is written to.
	if err := mgr.RegisterBus(busID, "sess1", "test"); err != nil {
		t.Fatalf("RegisterBus: %v", err)
	}

	// Detect rotations by watching evt.Seq drop between consecutive appends
	// (per-file seq semantics: the first event of a new generation always
	// has a smaller seq than the last event of the generation it followed —
	// see TestAppendEvent_SeqResetsAcrossRotation). This is more reliable
	// than counting archive files on disk: the archive filename has
	// one-second resolution (events.<RFC3339-ish-UTC>.jsonl), so multiple
	// rotations within the same wall-clock second — routine at this test's
	// tiny 512-byte threshold — collide on the same filename and would
	// undercount rotations if counted by directory listing.
	var lastEvt *BusBlock
	rotations := 0
	for i := 0; i < 5000 && rotations < 2; i++ {
		evt, err := mgr.AppendEvent(busID, "m", "tester", map[string]interface{}{"data": strings.Repeat("z", 20)})
		if err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}

		if lastEvt != nil && evt.Seq < lastEvt.Seq {
			rotations++

			// evt is the FIRST write of the new generation — exactly the
			// write that used to be silently dropped by
			// updateRegistrySeqIfNewer's monotonic guard once the registry
			// was left pinned at the old generation's terminal seq.
			entries := mgr.LoadRegistry()
			if len(entries) != 1 {
				t.Fatalf("after rotation #%d: registry has %d entries, want 1", rotations, len(entries))
			}
			got := entries[0]
			if got.LastEventSeq != evt.Seq {
				t.Errorf("after rotation #%d: registry LastEventSeq = %d, want %d (evt seq) — registry stopped advancing, staleness bug reproduced", rotations, got.LastEventSeq, evt.Seq)
			}
			if got.EventCount != evt.Seq {
				t.Errorf("after rotation #%d: registry EventCount = %d, want %d", rotations, got.EventCount, evt.Seq)
			}
			if got.LastEventAt != evt.Ts {
				t.Errorf("after rotation #%d: registry LastEventAt = %q, want %q (the just-appended event's ts) — a bus written to moments ago must not report as stale", rotations, got.LastEventAt, evt.Ts)
			}
		}
		lastEvt = evt
	}

	if rotations < 2 {
		t.Fatalf("only %d rotation(s) occurred in 5000 appends; need at least 2 to exercise the second-rotation regression", rotations)
	}

	// Final sanity check against the very last event appended, independent
	// of the per-rotation assertions above.
	entries := mgr.LoadRegistry()
	got := entries[0]
	if got.LastEventAt != lastEvt.Ts {
		t.Fatalf("final registry LastEventAt = %q, want %q (last appended event's ts)", got.LastEventAt, lastEvt.Ts)
	}
	if got.LastEventSeq != lastEvt.Seq {
		t.Fatalf("final registry LastEventSeq = %d, want %d (last appended event's seq)", got.LastEventSeq, lastEvt.Seq)
	}
}

// TestBusSessionEventHandlerDispatch verifies that registered handlers
// fire after AppendEvent, outside the lock.
func TestBusSessionEventHandlerDispatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	seen := make(chan *BusBlock, 4)
	mgr.AddEventHandler("test", func(busID string, block *BusBlock) {
		seen <- block
	})

	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 1})
	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 2})

	for i := 0; i < 2; i++ {
		select {
		case evt := <-seen:
			if evt.Seq != i+1 {
				t.Errorf("handler received seq=%d, want %d", evt.Seq, i+1)
			}
		default:
			t.Errorf("handler didn't receive event %d", i+1)
		}
	}

	mgr.RemoveEventHandler("test")
	_, _ = mgr.AppendEvent("bus-h", "m", "x", map[string]interface{}{"v": 3})
	select {
	case evt := <-seen:
		t.Errorf("handler fired after removal: %+v", evt)
	default:
		// ok — handler was properly removed
	}
}

// TestSaveRegistryAtomic verifies the registry write goes through a temp file
// and rename (no truncate-before-write window), leaves no .tmp behind, and
// round-trips through loadRegistry.
func TestSaveRegistryAtomic(t *testing.T) {
	root := t.TempDir()
	mgr := NewBusSessionManager(root)

	entries := []BusRegistryEntry{{BusID: "bus_test", State: "active", LastEventSeq: 3}}
	if err := mgr.saveRegistry(entries); err != nil {
		t.Fatalf("saveRegistry: %v", err)
	}

	if _, err := os.Stat(mgr.RegistryPath() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after atomic write: stat err=%v", err)
	}

	got := mgr.loadRegistry()
	if len(got) != 1 || got[0].BusID != "bus_test" || got[0].LastEventSeq != 3 {
		t.Errorf("loadRegistry = %+v, want one bus_test entry with seq=3", got)
	}
}

// TestArchiveRetentionFor verifies the per-bus retention lookup: exact names,
// family prefixes, and the keep-everything default for undeclared buses.
func TestArchiveRetentionFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		busID string
		want  int
	}{
		{"bus_traces", 8},         // exact match, prefixed
		{"traces", 8},             // exact match, unprefixed
		{"bus_kernel_proprio", 8}, // exact match wins over the "kernel" segment
		{"bus_peer_awareness", 8}, // exact match wins over the "peer" segment
		{"bus_chat_abc-123", -1},  // conversation content: never auto-pruned
		{"bus_mcp_deadbeef", -1},  // conversation content: never auto-pruned
		{"bus_sessions", -1},      // undeclared: keep everything
		{"", -1},                  // degenerate input must not panic or prune
	}
	for _, tc := range cases {
		if got := archiveRetentionFor(tc.busID); got != tc.want {
			t.Errorf("archiveRetentionFor(%q) = %d, want %d", tc.busID, got, tc.want)
		}
	}
}

// TestPruneBusArchives_KeepsNewest verifies that pruning removes the oldest
// archives, keeps the newest `keep`, and never touches the live events.jsonl.
func TestPruneBusArchives_KeepsNewest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)
	busID := "bus_prunetest"
	if _, err := mgr.AppendEvent(busID, "test", "seed", map[string]any{"n": 1}); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	busDir := filepath.Join(mgr.BusesDir(), busID)

	// Five archives, lexically ordered oldest -> newest.
	stamps := []string{
		"2026-01-01T000000Z", "2026-02-01T000000Z", "2026-03-01T000000Z",
		"2026-04-01T000000Z", "2026-05-01T000000Z",
	}
	for _, ts := range stamps {
		p := filepath.Join(busDir, "events."+ts+".jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write archive %s: %v", ts, err)
		}
	}

	mgr.pruneBusArchives(busID, 2)

	for _, ts := range stamps[:3] {
		if _, err := os.Stat(filepath.Join(busDir, "events."+ts+".jsonl")); !os.IsNotExist(err) {
			t.Errorf("archive %s should have been pruned, stat err = %v", ts, err)
		}
	}
	for _, ts := range stamps[3:] {
		if _, err := os.Stat(filepath.Join(busDir, "events."+ts+".jsonl")); err != nil {
			t.Errorf("archive %s should have been kept: %v", ts, err)
		}
	}
	// The live file must survive pruning.
	if _, err := os.Stat(filepath.Join(busDir, "events.jsonl")); err != nil {
		t.Errorf("live events.jsonl must never be pruned: %v", err)
	}
}

// TestPruneBusArchives_KeepAllIsNoop is the regression guard that matters most:
// a bus with no declared retention must never lose an archive.
func TestPruneBusArchives_KeepAllIsNoop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)
	busID := "bus_chat_keepme"
	if _, err := mgr.AppendEvent(busID, "test", "seed", map[string]any{"n": 1}); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	busDir := filepath.Join(mgr.BusesDir(), busID)
	for _, ts := range []string{"2026-01-01T000000Z", "2026-02-01T000000Z"} {
		if err := os.WriteFile(filepath.Join(busDir, "events."+ts+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write archive: %v", err)
		}
	}

	mgr.pruneBusArchives(busID, archiveRetentionFor(busID))

	entries, err := os.ReadDir(busDir)
	if err != nil {
		t.Fatalf("read bus dir: %v", err)
	}
	var archives int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "events.") && e.Name() != "events.jsonl" {
			archives++
		}
	}
	if archives != 2 {
		t.Errorf("undeclared bus must keep all archives, got %d want 2", archives)
	}
}
