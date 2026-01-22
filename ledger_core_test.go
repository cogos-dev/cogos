package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// === CANONICAL JSON TESTS ===

func TestCanonicalJSON_KeyOrdering(t *testing.T) {
	// Two events with same data but different key order should produce identical bytes
	event1 := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
		Data: map[string]interface{}{
			"zebra": "last",
			"alpha": "first",
			"mike":  "middle",
		},
	}

	event2 := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
		Data: map[string]interface{}{
			"mike":  "middle",
			"alpha": "first",
			"zebra": "last",
		},
	}

	bytes1, err := CanonicalizeEvent(event1)
	if err != nil {
		t.Fatalf("Failed to canonicalize event1: %v", err)
	}

	bytes2, err := CanonicalizeEvent(event2)
	if err != nil {
		t.Fatalf("Failed to canonicalize event2: %v", err)
	}

	if string(bytes1) != string(bytes2) {
		t.Errorf("Canonical bytes differ:\nEvent1: %s\nEvent2: %s", bytes1, bytes2)
	}

	// Verify keys are sorted
	expected := `{"data":{"alpha":"first","mike":"middle","zebra":"last"},"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes1) != expected {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes1, expected)
	}
}

func TestCanonicalJSON_NestedObjects(t *testing.T) {
	event := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
		Data: map[string]interface{}{
			"outer": map[string]interface{}{
				"zebra": "z",
				"alpha": "a",
			},
		},
	}

	bytes, err := CanonicalizeEvent(event)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	// Nested object keys should also be sorted
	expected := `{"data":{"outer":{"alpha":"a","zebra":"z"}},"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes) != expected {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes, expected)
	}
}

func TestCanonicalJSON_Arrays(t *testing.T) {
	event := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
		Data: map[string]interface{}{
			"items": []interface{}{"third", "first", "second"},
		},
	}

	bytes, err := CanonicalizeEvent(event)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	// Arrays should preserve order
	expected := `{"data":{"items":["third","first","second"]},"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes) != expected {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes, expected)
	}
}

func TestCanonicalJSON_OptionalFields(t *testing.T) {
	// Event without data field
	event1 := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
	}

	bytes1, err := CanonicalizeEvent(event1)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	// Should not include data field
	expected1 := `{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes1) != expected1 {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes1, expected1)
	}

	// Event with prior_hash
	event2 := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
		PriorHash: "abc123",
	}

	bytes2, err := CanonicalizeEvent(event2)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	// Should include prior_hash
	expected2 := `{"prior_hash":"abc123","session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes2) != expected2 {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes2, expected2)
	}
}

// === HASHING TESTS ===

func TestHashEvent_SHA256(t *testing.T) {
	payload := []byte(`{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`)

	hash, err := HashEvent(payload, "sha256")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	// Verify hash format (64 hex chars for SHA256)
	if len(hash) != 64 {
		t.Errorf("Expected 64-char hash, got %d chars: %s", len(hash), hash)
	}

	// Same input should produce same hash
	hash2, err := HashEvent(payload, "sha256")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	if hash != hash2 {
		t.Errorf("Same input produced different hashes:\nHash1: %s\nHash2: %s", hash, hash2)
	}
}

func TestHashEvent_SHA512(t *testing.T) {
	payload := []byte(`{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`)

	hash, err := HashEvent(payload, "sha512")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	// Verify hash format (128 hex chars for SHA512)
	if len(hash) != 128 {
		t.Errorf("Expected 128-char hash, got %d chars: %s", len(hash), hash)
	}
}

func TestHashEvent_DefaultAlgorithm(t *testing.T) {
	payload := []byte(`{"type":"test"}`)

	hash1, err := HashEvent(payload, "")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	hash2, err := HashEvent(payload, "sha256")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	// Empty algorithm should default to sha256
	if hash1 != hash2 {
		t.Errorf("Default algorithm differs from sha256:\nDefault: %s\nSHA256:  %s", hash1, hash2)
	}
}

// === APPEND & CHAIN TESTS ===

func TestAppendEvent_FirstEvent(t *testing.T) {
	// Create temporary workspace
	tmpDir := t.TempDir()

	sessionID := "test-session-001"
	event := NewEventEnvelope("test.event", sessionID)
	event.WithData("message", "Hello, world!")

	err := AppendEvent(tmpDir, sessionID, event)
	if err != nil {
		t.Fatalf("Failed to append event: %v", err)
	}

	// Verify event was written
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events file: %v", err)
	}

	var written EventEnvelope
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("Failed to parse event: %v", err)
	}

	// First event should have seq=1 and no prior_hash
	if written.Metadata.Seq != 1 {
		t.Errorf("Expected seq=1, got %d", written.Metadata.Seq)
	}

	if written.HashedPayload.PriorHash != "" {
		t.Errorf("First event should have empty prior_hash, got %s", written.HashedPayload.PriorHash)
	}

	// Verify hash was computed
	if written.Metadata.Hash == "" {
		t.Error("Event hash was not computed")
	}
}

func TestAppendEvent_HashChaining(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-002"

	// Append first event
	event1 := NewEventEnvelope("test.event", sessionID)
	event1.WithData("number", 1)

	if err := AppendEvent(tmpDir, sessionID, event1); err != nil {
		t.Fatalf("Failed to append event1: %v", err)
	}

	// Append second event
	event2 := NewEventEnvelope("test.event", sessionID)
	event2.WithData("number", 2)

	if err := AppendEvent(tmpDir, sessionID, event2); err != nil {
		t.Fatalf("Failed to append event2: %v", err)
	}

	// Append third event
	event3 := NewEventEnvelope("test.event", sessionID)
	event3.WithData("number", 3)

	if err := AppendEvent(tmpDir, sessionID, event3); err != nil {
		t.Fatalf("Failed to append event3: %v", err)
	}

	// Read all events
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events: %v", err)
	}

	lines := splitLines(string(data))
	if len(lines) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(lines))
	}

	// Parse events
	var events []*EventEnvelope
	for _, line := range lines {
		var e EventEnvelope
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("Failed to parse event: %v", err)
		}
		events = append(events, &e)
	}

	// Verify sequence numbers
	for i, e := range events {
		expectedSeq := int64(i + 1)
		if e.Metadata.Seq != expectedSeq {
			t.Errorf("Event %d: expected seq=%d, got %d", i, expectedSeq, e.Metadata.Seq)
		}
	}

	// Verify hash chain
	if events[0].HashedPayload.PriorHash != "" {
		t.Errorf("Event 0: expected empty prior_hash, got %s", events[0].HashedPayload.PriorHash)
	}

	if events[1].HashedPayload.PriorHash != events[0].Metadata.Hash {
		t.Errorf("Event 1: prior_hash mismatch:\nExpected: %s\nGot:      %s",
			events[0].Metadata.Hash, events[1].HashedPayload.PriorHash)
	}

	if events[2].HashedPayload.PriorHash != events[1].Metadata.Hash {
		t.Errorf("Event 2: prior_hash mismatch:\nExpected: %s\nGot:      %s",
			events[1].Metadata.Hash, events[2].HashedPayload.PriorHash)
	}
}

// === VERIFICATION TESTS ===

func TestVerifyLedger_ValidChain(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-003"

	// Create genesis event
	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		t.Fatalf("Failed to append genesis: %v", err)
	}

	// Append more events
	for i := 1; i <= 5; i++ {
		event := NewEventEnvelope("test.event", sessionID)
		event.WithData("index", i)
		if err := AppendEvent(tmpDir, sessionID, event); err != nil {
			t.Fatalf("Failed to append event %d: %v", i, err)
		}
	}

	// Verify chain integrity
	if err := VerifyLedger(tmpDir, sessionID); err != nil {
		t.Errorf("Verification failed for valid chain: %v", err)
	}
}

func TestVerifyLedger_DetectsTampering(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-004"

	// Create valid chain
	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		t.Fatalf("Failed to append genesis: %v", err)
	}

	event1 := NewEventEnvelope("test.event", sessionID)
	event1.WithData("value", "original")
	if err := AppendEvent(tmpDir, sessionID, event1); err != nil {
		t.Fatalf("Failed to append event1: %v", err)
	}

	event2 := NewEventEnvelope("test.event", sessionID)
	event2.WithData("value", "second")
	if err := AppendEvent(tmpDir, sessionID, event2); err != nil {
		t.Fatalf("Failed to append event2: %v", err)
	}

	// Tamper with middle event
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events: %v", err)
	}

	lines := splitLines(string(data))
	if len(lines) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(lines))
	}

	// Modify the second event's data (breaking the hash)
	var tampered EventEnvelope
	if err := json.Unmarshal([]byte(lines[1]), &tampered); err != nil {
		t.Fatalf("Failed to parse event: %v", err)
	}

	tampered.HashedPayload.Data["value"] = "TAMPERED"
	tamperedJSON, _ := json.Marshal(&tampered)
	lines[1] = string(tamperedJSON)

	// Write back tampered data
	tamperedData := []byte(lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n")
	if err := os.WriteFile(eventsFile, tamperedData, 0644); err != nil {
		t.Fatalf("Failed to write tampered data: %v", err)
	}

	// Verification should detect tampering
	err = VerifyLedger(tmpDir, sessionID)
	if err == nil {
		t.Error("Verification should have detected tampering")
	}
}

func TestVerifyLedger_DetectsBrokenChain(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-005"

	// Create chain
	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		t.Fatalf("Failed to append genesis: %v", err)
	}

	event1 := NewEventEnvelope("test.event", sessionID)
	if err := AppendEvent(tmpDir, sessionID, event1); err != nil {
		t.Fatalf("Failed to append event1: %v", err)
	}

	event2 := NewEventEnvelope("test.event", sessionID)
	if err := AppendEvent(tmpDir, sessionID, event2); err != nil {
		t.Fatalf("Failed to append event2: %v", err)
	}

	// Break the chain by corrupting prior_hash
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events: %v", err)
	}

	lines := splitLines(string(data))
	var broken EventEnvelope
	if err := json.Unmarshal([]byte(lines[2]), &broken); err != nil {
		t.Fatalf("Failed to parse event: %v", err)
	}

	// Corrupt the prior_hash
	broken.HashedPayload.PriorHash = "0000000000000000000000000000000000000000000000000000000000000000"
	brokenJSON, _ := json.Marshal(&broken)
	lines[2] = string(brokenJSON)

	brokenData := []byte(lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n")
	if err := os.WriteFile(eventsFile, brokenData, 0644); err != nil {
		t.Fatalf("Failed to write broken data: %v", err)
	}

	// Verification should detect broken chain
	err = VerifyLedger(tmpDir, sessionID)
	if err == nil {
		t.Error("Verification should have detected broken chain")
	}
}

// === HELPER FUNCTIONS ===

func splitLines(s string) []string {
	var lines []string
	current := ""
	for _, ch := range s {
		if ch == '\n' {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// === BENCHMARK TESTS ===

func BenchmarkCanonicalizeEvent(b *testing.B) {
	event := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Data: map[string]interface{}{
			"field1": "value1",
			"field2": 42,
			"field3": map[string]interface{}{
				"nested1": "value",
				"nested2": []interface{}{1, 2, 3},
			},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := CanonicalizeEvent(event)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashEvent_SHA256(b *testing.B) {
	payload := []byte(`{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := HashEvent(payload, "sha256")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendEvent(b *testing.B) {
	tmpDir := b.TempDir()
	sessionID := "bench-session"

	// Create genesis
	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		event := NewEventEnvelope("test.event", sessionID)
		event.WithData("iteration", i)
		if err := AppendEvent(tmpDir, sessionID, event); err != nil {
			b.Fatal(err)
		}
	}
}
