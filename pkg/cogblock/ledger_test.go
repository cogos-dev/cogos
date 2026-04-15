package cogblock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// === CANONICAL JSON TESTS ===

func TestCanonicalizeEvent_KeyOrdering(t *testing.T) {
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

	expected := `{"data":{"alpha":"first","mike":"middle","zebra":"last"},"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes1) != expected {
		t.Errorf("Unexpected canonical form:\nGot:      %s\nExpected: %s", bytes1, expected)
	}
}

func TestCanonicalizeEvent_OptionalFields(t *testing.T) {
	// Without data
	event := &EventPayload{
		Type:      "test.event",
		SessionID: "session-123",
		Timestamp: "2026-01-16T18:30:00Z",
	}

	bytes, err := CanonicalizeEvent(event)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	expected := `{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes) != expected {
		t.Errorf("Got: %s\nWant: %s", bytes, expected)
	}

	// With prior_hash
	event.PriorHash = "abc123"
	bytes, err = CanonicalizeEvent(event)
	if err != nil {
		t.Fatalf("Failed to canonicalize: %v", err)
	}

	expected = `{"prior_hash":"abc123","session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`
	if string(bytes) != expected {
		t.Errorf("Got: %s\nWant: %s", bytes, expected)
	}
}

// === HASHING TESTS ===

func TestHashEvent_SHA256(t *testing.T) {
	payload := []byte(`{"session_id":"session-123","timestamp":"2026-01-16T18:30:00Z","type":"test.event"}`)

	hash, err := HashEvent(payload, "sha256")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	if len(hash) != 64 {
		t.Errorf("Expected 64-char hash, got %d chars: %s", len(hash), hash)
	}

	// Deterministic
	hash2, _ := HashEvent(payload, "sha256")
	if hash != hash2 {
		t.Errorf("Non-deterministic hash: %s vs %s", hash, hash2)
	}
}

func TestHashEvent_SHA512(t *testing.T) {
	payload := []byte(`{"type":"test"}`)

	hash, err := HashEvent(payload, "sha512")
	if err != nil {
		t.Fatalf("Failed to hash: %v", err)
	}

	if len(hash) != 128 {
		t.Errorf("Expected 128-char hash, got %d chars", len(hash))
	}
}

func TestHashEvent_DefaultIsSHA256(t *testing.T) {
	payload := []byte(`{"type":"test"}`)

	h1, _ := HashEvent(payload, "")
	h2, _ := HashEvent(payload, "sha256")

	if h1 != h2 {
		t.Errorf("Default differs from sha256: %s vs %s", h1, h2)
	}
}

func TestHashEvent_UnsupportedAlgorithm(t *testing.T) {
	_, err := HashEvent([]byte("x"), "md5")
	if err == nil {
		t.Error("Expected error for unsupported algorithm")
	}
}

// === ENVELOPE CONSTRUCTOR TESTS ===

func TestNewEventEnvelope(t *testing.T) {
	before := time.Now().UTC()
	env := NewEventEnvelope("test.event", "session-42")
	after := time.Now().UTC()

	if env.HashedPayload.Type != "test.event" {
		t.Errorf("Type = %q; want test.event", env.HashedPayload.Type)
	}
	if env.HashedPayload.SessionID != "session-42" {
		t.Errorf("SessionID = %q; want session-42", env.HashedPayload.SessionID)
	}

	ts, err := time.Parse(time.RFC3339Nano, env.HashedPayload.Timestamp)
	if err != nil {
		t.Fatalf("Bad timestamp: %v", err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("Timestamp %v not between %v and %v", ts, before, after)
	}

	if env.HashedPayload.Data == nil {
		t.Error("Data map should be initialized")
	}
}

func TestEventEnvelope_WithData(t *testing.T) {
	env := NewEventEnvelope("test", "s1")
	env.WithData("key1", "value1").WithData("key2", 42)

	if env.HashedPayload.Data["key1"] != "value1" {
		t.Errorf("key1 = %v; want value1", env.HashedPayload.Data["key1"])
	}
	if env.HashedPayload.Data["key2"] != 42 {
		t.Errorf("key2 = %v; want 42", env.HashedPayload.Data["key2"])
	}
}

func TestEventEnvelope_WithSource(t *testing.T) {
	env := NewEventEnvelope("test", "s1").WithSource("kernel")

	if env.Metadata.Source != "kernel" {
		t.Errorf("Source = %q; want kernel", env.Metadata.Source)
	}
}

// === APPEND & VERIFY INTEGRATION TESTS ===

func TestAppendEvent_FirstEvent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-001"

	event := NewEventEnvelope("test.event", sessionID)
	event.WithData("message", "Hello, world!")

	err := AppendEvent(tmpDir, sessionID, event)
	if err != nil {
		t.Fatalf("Failed to append event: %v", err)
	}

	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events file: %v", err)
	}

	var written EventEnvelope
	if err := json.Unmarshal(data[:len(data)-1], &written); err != nil { // trim trailing newline
		t.Fatalf("Failed to parse event: %v", err)
	}

	if written.Metadata.Seq != 1 {
		t.Errorf("Expected seq=1, got %d", written.Metadata.Seq)
	}
	if written.HashedPayload.PriorHash != "" {
		t.Errorf("First event should have empty prior_hash, got %s", written.HashedPayload.PriorHash)
	}
	if written.Metadata.Hash == "" {
		t.Error("Event hash was not computed")
	}
}

func TestAppendEvent_HashChaining(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-002"

	for i := 1; i <= 3; i++ {
		event := NewEventEnvelope("test.event", sessionID)
		event.WithData("number", i)
		if err := AppendEvent(tmpDir, sessionID, event); err != nil {
			t.Fatalf("Failed to append event %d: %v", i, err)
		}
	}

	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("Failed to read events: %v", err)
	}

	lines := splitNonEmpty(string(data))
	if len(lines) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(lines))
	}

	var events []*EventEnvelope
	for _, line := range lines {
		var e EventEnvelope
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("Failed to parse event: %v", err)
		}
		events = append(events, &e)
	}

	// Verify chain linkage
	if events[0].HashedPayload.PriorHash != "" {
		t.Errorf("Event 0: expected empty prior_hash")
	}
	if events[1].HashedPayload.PriorHash != events[0].Metadata.Hash {
		t.Errorf("Event 1: prior_hash mismatch")
	}
	if events[2].HashedPayload.PriorHash != events[1].Metadata.Hash {
		t.Errorf("Event 2: prior_hash mismatch")
	}
}

func TestVerifyLedger_ValidChain(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-003"

	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		t.Fatalf("Failed to append genesis: %v", err)
	}

	for i := 1; i <= 5; i++ {
		event := NewEventEnvelope("test.event", sessionID)
		event.WithData("index", i)
		if err := AppendEvent(tmpDir, sessionID, event); err != nil {
			t.Fatalf("Failed to append event %d: %v", i, err)
		}
	}

	if err := VerifyLedger(tmpDir, sessionID); err != nil {
		t.Errorf("Verification failed for valid chain: %v", err)
	}
}

func TestVerifyLedger_DetectsTampering(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-004"

	genesis := NewEventEnvelope("workspace.genesis", sessionID)
	genesis.WithData("hash_algorithm", "sha256")
	if err := AppendEvent(tmpDir, sessionID, genesis); err != nil {
		t.Fatal(err)
	}

	event := NewEventEnvelope("test.event", sessionID)
	event.WithData("value", "original")
	if err := AppendEvent(tmpDir, sessionID, event); err != nil {
		t.Fatal(err)
	}

	// Tamper with the second event
	eventsFile := filepath.Join(tmpDir, ".cog", "ledger", sessionID, "events.jsonl")
	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatal(err)
	}

	lines := splitNonEmpty(string(data))
	var tampered EventEnvelope
	json.Unmarshal([]byte(lines[1]), &tampered)
	tampered.HashedPayload.Data["value"] = "TAMPERED"
	tamperedJSON, _ := json.Marshal(&tampered)
	lines[1] = string(tamperedJSON)

	os.WriteFile(eventsFile, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	err = VerifyLedger(tmpDir, sessionID)
	if err == nil {
		t.Error("Verification should have detected tampering")
	}
}

// === HELPERS ===

func splitNonEmpty(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
