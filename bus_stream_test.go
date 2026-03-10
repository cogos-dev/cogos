// bus_stream_test.go — Integration tests for the CogBus consumer cursor system (ADR-061).
//
// Tests cover the full cursor lifecycle:
//   1. Registry get-or-create semantics (idempotent create, reconnect updates)
//   2. Monotonic ACK advancement and stale/unknown ACK rejection
//   3. Disk persistence and recovery via loadFromDisk
//   4. Staleness sweep marking and exemption
//   5. Minimum-acked-seq computation (skipping stale cursors)
//   6. Consumer removal idempotency
//   7. List filtering by bus and copy-on-read safety

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test 1: getOrCreate semantics
// ---------------------------------------------------------------------------

func TestConsumerRegistry_GetOrCreate(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)

	// First call creates a new cursor at position 0.
	c1 := reg.getOrCreate("bus1", "consumer-A")
	if c1 == nil {
		t.Fatal("expected non-nil cursor from getOrCreate")
	}
	if c1.ConsumerID != "consumer-A" {
		t.Errorf("ConsumerID = %q, want %q", c1.ConsumerID, "consumer-A")
	}
	if c1.BusID != "bus1" {
		t.Errorf("BusID = %q, want %q", c1.BusID, "bus1")
	}
	if c1.LastAckedSeq != 0 {
		t.Errorf("LastAckedSeq = %d, want 0", c1.LastAckedSeq)
	}
	if c1.Stale {
		t.Error("new cursor should not be stale")
	}
	firstConnectedAt := c1.ConnectedAt

	// Small sleep so reconnect timestamp is distinguishable.
	time.Sleep(5 * time.Millisecond)

	// Second call with the same IDs returns the same cursor (pointer identity).
	c2 := reg.getOrCreate("bus1", "consumer-A")
	if c2 != c1 {
		t.Error("expected same *ConsumerCursor pointer for repeat getOrCreate")
	}
	// ConnectedAt should be updated on reconnect.
	if !c2.ConnectedAt.After(firstConnectedAt) {
		t.Errorf("ConnectedAt not updated on reconnect: first=%v, second=%v",
			firstConnectedAt, c2.ConnectedAt)
	}

	// Different consumer ID creates a separate cursor.
	c3 := reg.getOrCreate("bus1", "consumer-B")
	if c3 == c1 {
		t.Error("different consumer should produce a different cursor pointer")
	}
	if c3.ConsumerID != "consumer-B" {
		t.Errorf("ConsumerID = %q, want %q", c3.ConsumerID, "consumer-B")
	}
}

// ---------------------------------------------------------------------------
// Test 2: monotonic ACK advancement
// ---------------------------------------------------------------------------

func TestConsumerRegistry_Ack(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)
	reg.getOrCreate("bus1", "consumer-A")

	// ACK seq=5 → should advance.
	c, err := reg.ack("bus1", "consumer-A", 5)
	if err != nil {
		t.Fatalf("ack(5): unexpected error: %v", err)
	}
	if c.LastAckedSeq != 5 {
		t.Errorf("after ack(5): LastAckedSeq = %d, want 5", c.LastAckedSeq)
	}

	// ACK seq=3 (stale/duplicate) → should be ignored.
	c, err = reg.ack("bus1", "consumer-A", 3)
	if err != nil {
		t.Fatalf("ack(3): unexpected error: %v", err)
	}
	if c.LastAckedSeq != 5 {
		t.Errorf("after stale ack(3): LastAckedSeq = %d, want 5", c.LastAckedSeq)
	}

	// ACK seq=10 → should advance.
	c, err = reg.ack("bus1", "consumer-A", 10)
	if err != nil {
		t.Fatalf("ack(10): unexpected error: %v", err)
	}
	if c.LastAckedSeq != 10 {
		t.Errorf("after ack(10): LastAckedSeq = %d, want 10", c.LastAckedSeq)
	}

	// ACK for unknown consumer → error.
	_, err = reg.ack("bus1", "ghost-consumer", 1)
	if err == nil {
		t.Error("expected error for unknown consumer, got nil")
	}

	// ACK for unknown bus → error.
	_, err = reg.ack("no-such-bus", "consumer-A", 1)
	if err == nil {
		t.Error("expected error for unknown bus, got nil")
	}
}

// ---------------------------------------------------------------------------
// Test 3: persistence round-trip
// ---------------------------------------------------------------------------

func TestConsumerRegistry_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: create registry, advance some cursors, let persistLocked write.
	reg1 := newConsumerRegistry(dir)
	reg1.getOrCreate("bus1", "consumer-A")
	if _, err := reg1.ack("bus1", "consumer-A", 7); err != nil {
		t.Fatalf("phase1 ack: %v", err)
	}
	reg1.getOrCreate("bus1", "consumer-B")
	if _, err := reg1.ack("bus1", "consumer-B", 3); err != nil {
		t.Fatalf("phase1 ack: %v", err)
	}

	// Verify the cursor file was written.
	cursorFile := filepath.Join(dir, "bus1.cursors.jsonl")
	if _, err := os.Stat(cursorFile); err != nil {
		t.Fatalf("cursor file not found: %v", err)
	}

	// Phase 2: new registry from the same directory — cold start.
	reg2 := newConsumerRegistry(dir)
	if err := reg2.loadFromDisk(); err != nil {
		t.Fatalf("loadFromDisk: %v", err)
	}

	// Verify consumer-A was restored at seq=7.
	cursors := reg2.list("bus1")
	found := map[string]int64{}
	for _, c := range cursors {
		found[c.ConsumerID] = c.LastAckedSeq
	}
	if seq, ok := found["consumer-A"]; !ok || seq != 7 {
		t.Errorf("consumer-A: got seq=%d present=%v, want seq=7", seq, ok)
	}
	if seq, ok := found["consumer-B"]; !ok || seq != 3 {
		t.Errorf("consumer-B: got seq=%d present=%v, want seq=3", seq, ok)
	}
}

// ---------------------------------------------------------------------------
// Test 4: staleness sweep
// ---------------------------------------------------------------------------

func TestConsumerRegistry_SweepStale(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)

	// Create an "old" consumer by back-dating its timestamps.
	oldCursor := reg.getOrCreate("bus1", "old-consumer")
	oldTime := time.Now().Add(-10 * time.Minute)
	oldCursor.ConnectedAt = oldTime
	oldCursor.LastAckAt = oldTime

	// Create a fresh consumer.
	reg.getOrCreate("bus1", "fresh-consumer")

	// Sweep with a short connected window (1 minute) and short disconnected window (1 minute).
	marked := reg.sweepStale(1*time.Minute, 1*time.Minute)
	if marked != 1 {
		t.Errorf("sweepStale: marked = %d, want 1", marked)
	}

	// Verify the old consumer is now stale.
	for _, c := range reg.list("bus1") {
		switch c.ConsumerID {
		case "old-consumer":
			if !c.Stale {
				t.Error("old-consumer should be marked stale after sweep")
			}
		case "fresh-consumer":
			if c.Stale {
				t.Error("fresh-consumer should NOT be marked stale")
			}
		}
	}

	// A second sweep should not re-mark already-stale cursors.
	marked2 := reg.sweepStale(1*time.Minute, 1*time.Minute)
	if marked2 != 0 {
		t.Errorf("second sweepStale: marked = %d, want 0 (already stale)", marked2)
	}
}

// ---------------------------------------------------------------------------
// Test 5: minAckedSeq
// ---------------------------------------------------------------------------

func TestConsumerRegistry_MinAckedSeq(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)

	// No consumers → min is 0.
	if got := reg.minAckedSeq("bus1"); got != 0 {
		t.Errorf("empty bus: minAckedSeq = %d, want 0", got)
	}

	// Create consumers at different positions.
	reg.getOrCreate("bus1", "c1")
	reg.ack("bus1", "c1", 10)

	reg.getOrCreate("bus1", "c2")
	reg.ack("bus1", "c2", 5)

	reg.getOrCreate("bus1", "c3")
	reg.ack("bus1", "c3", 20)

	// Minimum across active consumers = 5.
	if got := reg.minAckedSeq("bus1"); got != 5 {
		t.Errorf("minAckedSeq = %d, want 5", got)
	}

	// Mark c2 (the minimum) as stale — should be skipped.
	for _, c := range reg.list("bus1") {
		if c.ConsumerID == "c2" {
			// Directly manipulate the real cursor through getOrCreate
			// since list returns copies.
			break
		}
	}
	// Use sweepStale to mark c2 by back-dating it.
	cursor := reg.getOrCreate("bus1", "c2")
	cursor.ConnectedAt = time.Now().Add(-1 * time.Hour)
	cursor.LastAckAt = time.Now().Add(-1 * time.Hour)
	reg.sweepStale(1*time.Minute, 1*time.Minute)

	// Now minimum should be 10 (c1), since c2 is stale and skipped.
	if got := reg.minAckedSeq("bus1"); got != 10 {
		t.Errorf("after stale sweep: minAckedSeq = %d, want 10", got)
	}

	// Unknown bus → 0.
	if got := reg.minAckedSeq("no-such-bus"); got != 0 {
		t.Errorf("unknown bus: minAckedSeq = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Test 6: remove
// ---------------------------------------------------------------------------

func TestConsumerRegistry_Remove(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)

	reg.getOrCreate("bus1", "doomed")

	// First remove → true.
	if !reg.remove("doomed") {
		t.Error("first remove should return true")
	}

	// Second remove → false (already gone).
	if reg.remove("doomed") {
		t.Error("second remove should return false")
	}

	// List should be empty.
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("list after remove: len = %d, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// Test 7: list filtering and copy safety
// ---------------------------------------------------------------------------

func TestConsumerRegistry_List(t *testing.T) {
	dir := t.TempDir()
	reg := newConsumerRegistry(dir)

	reg.getOrCreate("bus1", "c1")
	reg.ack("bus1", "c1", 10)
	reg.getOrCreate("bus1", "c2")
	reg.ack("bus1", "c2", 20)
	reg.getOrCreate("bus2", "c3")
	reg.ack("bus2", "c3", 30)

	// list("") returns all.
	all := reg.list("")
	if len(all) != 3 {
		t.Errorf("list(''): len = %d, want 3", len(all))
	}

	// list("bus1") returns only bus1 consumers.
	bus1Only := reg.list("bus1")
	if len(bus1Only) != 2 {
		t.Errorf("list('bus1'): len = %d, want 2", len(bus1Only))
	}
	for _, c := range bus1Only {
		if c.BusID != "bus1" {
			t.Errorf("list('bus1') returned cursor with BusID=%q", c.BusID)
		}
	}

	// list("bus2") returns only bus2.
	bus2Only := reg.list("bus2")
	if len(bus2Only) != 1 {
		t.Errorf("list('bus2'): len = %d, want 1", len(bus2Only))
	}

	// Verify returned cursors are copies — mutating them must not affect the registry.
	for _, c := range bus1Only {
		c.LastAckedSeq = 9999
		c.Stale = true
	}
	// Re-read from registry: original values should be unchanged.
	fresh := reg.list("bus1")
	for _, c := range fresh {
		if c.LastAckedSeq == 9999 {
			t.Errorf("list returned a reference instead of a copy for consumer=%s", c.ConsumerID)
		}
		if c.Stale {
			t.Errorf("list-returned cursor Stale flag leaked back into registry for consumer=%s", c.ConsumerID)
		}
	}
}
