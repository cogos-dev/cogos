package cogfield

import (
	"encoding/json"
	"testing"
)

func TestBlockJSONRoundTrip(t *testing.T) {
	block := Block{
		V:        2,
		ID:       "block-1",
		BusID:    "bus-abc",
		Seq:      1,
		Ts:       "2026-04-14T12:00:00Z",
		From:     "agent-a",
		To:       "agent-b",
		Type:     "bus.message",
		Payload:  map[string]interface{}{"content": "hello"},
		Prev:     []string{"hash-0"},
		PrevHash: "hash-0",
		Hash:     "hash-1",
		Merkle:   "merkle-1",
		Size:     42,
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.V != 2 {
		t.Errorf("V = %d, want 2", decoded.V)
	}
	if decoded.BusID != "bus-abc" {
		t.Errorf("BusID = %q, want %q", decoded.BusID, "bus-abc")
	}
	if decoded.Hash != "hash-1" {
		t.Errorf("Hash = %q, want %q", decoded.Hash, "hash-1")
	}
	if len(decoded.Prev) != 1 || decoded.Prev[0] != "hash-0" {
		t.Errorf("Prev = %v, want [hash-0]", decoded.Prev)
	}
}

func TestBlockOmitsEmptyFields(t *testing.T) {
	block := Block{
		Ts:      "2026-04-14T12:00:00Z",
		From:    "agent-a",
		Type:    "bus.message",
		Payload: map[string]interface{}{},
		Hash:    "hash-1",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// These fields should be omitted when empty
	for _, field := range []string{"id", "bus_id", "to", "prev", "prev_hash", "merkle", "sig"} {
		if _, exists := raw[field]; exists {
			t.Errorf("field %q should be omitted when empty", field)
		}
	}
}

// TestBlockInlinePayloadRoundTrip verifies the legacy Phase-1 envelope shape:
// an inline Payload map with no ADR-084 digest reference.
func TestBlockInlinePayloadRoundTrip(t *testing.T) {
	block := Block{
		V:     2,
		ID:    "block-inline",
		BusID: "bus-inline",
		Seq:   7,
		Ts:    "2026-04-22T10:00:00Z",
		From:  "producer",
		Type:  "bus.message",
		Payload: map[string]interface{}{
			"content": "hello from inline",
			"count":   float64(3),
		},
		Hash: "hash-inline",
		Size: 128,
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// Inline form must serialize payload and omit the ADR-084 reference fields.
	if _, ok := raw["payload"]; !ok {
		t.Error("inline block should serialize payload field")
	}
	for _, field := range []string{"digest", "media_type"} {
		if _, exists := raw[field]; exists {
			t.Errorf("inline block should omit %q when empty", field)
		}
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Payload["content"] != "hello from inline" {
		t.Errorf("Payload.content = %v, want %q", decoded.Payload["content"], "hello from inline")
	}
	if decoded.Digest != "" {
		t.Errorf("Digest = %q, want empty", decoded.Digest)
	}
	if decoded.MediaType != "" {
		t.Errorf("MediaType = %q, want empty", decoded.MediaType)
	}
	if decoded.Size != 128 {
		t.Errorf("Size = %d, want 128", decoded.Size)
	}
}

// TestBlockByReferencePayloadRoundTrip verifies the ADR-084 Phase-2 envelope
// shape: Digest + MediaType + Size carry a by-reference payload pointer, and
// the inline Payload field is left empty so it does not serialize.
func TestBlockByReferencePayloadRoundTrip(t *testing.T) {
	block := Block{
		V:         2,
		ID:        "block-byref",
		BusID:     "bus-byref",
		Seq:       11,
		Ts:        "2026-04-22T10:05:00Z",
		From:      "producer",
		Type:      "trace.assessment",
		Digest:    "sha256:deadbeefcafebabe0000000000000000000000000000000000000000000000ff",
		MediaType: "application/vnd.cogos.trace.assessment.v1+json",
		Size:      512,
		Hash:      "hash-byref",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	// By-reference form must serialize the ADR-084 fields and omit empty payload.
	for _, field := range []string{"digest", "media_type"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("by-reference block should serialize %q", field)
		}
	}
	if _, exists := raw["payload"]; exists {
		t.Errorf("by-reference block should omit empty payload field, got %s", string(raw["payload"]))
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Digest != block.Digest {
		t.Errorf("Digest = %q, want %q", decoded.Digest, block.Digest)
	}
	if decoded.MediaType != block.MediaType {
		t.Errorf("MediaType = %q, want %q", decoded.MediaType, block.MediaType)
	}
	if decoded.Size != 512 {
		t.Errorf("Size = %d, want 512", decoded.Size)
	}
	if len(decoded.Payload) != 0 {
		t.Errorf("Payload should be empty, got %v", decoded.Payload)
	}
}

// resolveBlockPayload is a test-only helper modeling the ADR-084 v1 consumer
// resolution rule: prefer by-reference (Digest) when present, fall back to
// inline Payload otherwise. Returns a label indicating which form was chosen.
func resolveBlockPayload(b *Block) string {
	if b.Digest != "" {
		return "byref"
	}
	if len(b.Payload) > 0 {
		return "inline"
	}
	return "none"
}

// TestBlockCoexistenceBothFormsPopulated verifies that during the ADR-084 v1
// migration window, a Block may carry BOTH an inline Payload AND a Digest/
// MediaType reference. Both forms must serialize and round-trip.
func TestBlockCoexistenceBothFormsPopulated(t *testing.T) {
	block := Block{
		V:         2,
		ID:        "block-both",
		Ts:        "2026-04-22T11:00:00Z",
		From:      "producer",
		Type:      "bus.message",
		Payload:   map[string]interface{}{"content": "inline copy"},
		Digest:    "sha256:cafef00d000000000000000000000000000000000000000000000000000000aa",
		MediaType: "application/vnd.cogos.test.v1+json",
		Size:      256,
		Hash:      "hash-both",
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, field := range []string{"payload", "digest", "media_type"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("coexistence block should serialize %q", field)
		}
	}

	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Payload["content"] != "inline copy" {
		t.Errorf("Payload.content = %v, want %q", decoded.Payload["content"], "inline copy")
	}
	if decoded.Digest != block.Digest {
		t.Errorf("Digest = %q, want %q", decoded.Digest, block.Digest)
	}
}

// TestBlockResolutionPrefersByReference verifies the consumer-side preference
// order: when both forms are present, Digest wins over inline Payload.
func TestBlockResolutionPrefersByReference(t *testing.T) {
	cases := []struct {
		name  string
		block Block
		want  string
	}{
		{
			name:  "both-populated",
			block: Block{Payload: map[string]interface{}{"x": 1}, Digest: "sha256:abc"},
			want:  "byref",
		},
		{
			name:  "only-inline",
			block: Block{Payload: map[string]interface{}{"x": 1}},
			want:  "inline",
		},
		{
			name:  "only-byref",
			block: Block{Digest: "sha256:abc"},
			want:  "byref",
		},
		{
			name:  "neither",
			block: Block{},
			want:  "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBlockPayload(&tc.block); got != tc.want {
				t.Errorf("resolveBlockPayload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBlockSizeMatchesInlinePayloadBytes documents the invariant that Size
// SHOULD equal len(json.Marshal(Payload)) for inline form. The producer is
// responsible for setting Size; this test is skipped because Block itself
// does not auto-compute Size — it's a producer contract, not a struct invariant.
func TestBlockSizeMatchesInlinePayloadBytes(t *testing.T) {
	t.Skip("Size is a producer-set contract, not a struct-computed invariant; " +
		"Block{Payload: {...}} without an explicit Size leaves Size=0. " +
		"See ADR-084 v1 — producers SHOULD set Size to the payload byte length, " +
		"but Block itself does not enforce or compute this. Kept as documentation.")
}

// TestBlockSizeByRefMatchesStoredBytes documents the invariant for the by-ref
// form: Size MUST equal the byte length of the blob stored under Digest.
// We mock this since actual blob storage is in the engine package.
func TestBlockSizeByRefMatchesStoredBytes(t *testing.T) {
	storedBytes := []byte(`{"assessment":"ok","score":0.87}`)
	digest := "sha256:abc123" // mocked — not a real hash of storedBytes
	block := Block{
		V:         2,
		Ts:        "2026-04-22T11:00:00Z",
		From:      "producer",
		Type:      "trace.assessment",
		Digest:    digest,
		MediaType: "application/vnd.cogos.trace.assessment.v1+json",
		Size:      len(storedBytes),
		Hash:      "hash-byref",
	}
	if block.Size != len(storedBytes) {
		t.Errorf("Size = %d, want %d (producer contract)", block.Size, len(storedBytes))
	}

	data, _ := json.Marshal(block)
	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Size != len(storedBytes) {
		t.Errorf("decoded Size = %d, want %d", decoded.Size, len(storedBytes))
	}
}

// TestBlockEmptyMinimalJSON verifies that a Block with no optional fields
// marshals to a minimal JSON object containing only the required fields
// (v, ts, from, type, hash).
func TestBlockEmptyMinimalJSON(t *testing.T) {
	block := Block{}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	// Required fields always present (no omitempty on block.go):
	for _, field := range []string{"v", "ts", "from", "type", "hash"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("required field %q missing from empty block", field)
		}
	}
	// Optional fields omitted:
	for _, field := range []string{"id", "bus_id", "seq", "to", "payload", "digest", "media_type", "prev", "prev_hash", "merkle", "sig", "size"} {
		if _, exists := raw[field]; exists {
			t.Errorf("optional field %q should be omitted from empty block", field)
		}
	}
}

// TestBlockUnmarshalHandAuthoredInlineJSON verifies the json: tags match
// external-consumer expectations for the inline form. The JSON literal below
// is what a non-Go producer (TypeScript, Python, etc.) would emit.
func TestBlockUnmarshalHandAuthoredInlineJSON(t *testing.T) {
	external := `{
		"v": 2,
		"id": "ext-inline-1",
		"bus_id": "bus-ext",
		"seq": 42,
		"ts": "2026-04-22T12:00:00Z",
		"from": "external-producer",
		"type": "bus.message",
		"payload": {"msg": "hello from ts"},
		"hash": "hash-ext-1",
		"size": 28
	}`
	var decoded Block
	if err := json.Unmarshal([]byte(external), &decoded); err != nil {
		t.Fatalf("unmarshal external JSON: %v", err)
	}
	if decoded.ID != "ext-inline-1" || decoded.BusID != "bus-ext" || decoded.Seq != 42 {
		t.Errorf("snake_case tag mismatch: ID=%q BusID=%q Seq=%d", decoded.ID, decoded.BusID, decoded.Seq)
	}
	if decoded.Payload["msg"] != "hello from ts" {
		t.Errorf("Payload.msg = %v, want %q", decoded.Payload["msg"], "hello from ts")
	}
	if decoded.Size != 28 {
		t.Errorf("Size = %d, want 28", decoded.Size)
	}
}

// TestBlockUnmarshalHandAuthoredByRefJSON verifies snake_case tags for the
// ADR-084 by-reference fields (digest, media_type).
func TestBlockUnmarshalHandAuthoredByRefJSON(t *testing.T) {
	external := `{
		"v": 2,
		"ts": "2026-04-22T12:00:00Z",
		"from": "external-producer",
		"type": "trace.assessment",
		"digest": "sha256:0011223344556677889900112233445566778899001122334455667788990011",
		"media_type": "application/vnd.cogos.trace.assessment.v1+json",
		"size": 1024,
		"hash": "hash-ext-2"
	}`
	var decoded Block
	if err := json.Unmarshal([]byte(external), &decoded); err != nil {
		t.Fatalf("unmarshal external JSON: %v", err)
	}
	if decoded.Digest != "sha256:0011223344556677889900112233445566778899001122334455667788990011" {
		t.Errorf("Digest mismatch: %q", decoded.Digest)
	}
	if decoded.MediaType != "application/vnd.cogos.trace.assessment.v1+json" {
		t.Errorf("MediaType mismatch: %q", decoded.MediaType)
	}
	if decoded.Size != 1024 {
		t.Errorf("Size = %d, want 1024", decoded.Size)
	}
}

// TestBlockJSONKeysAreSnakeCase round-trips through map[string]interface{}
// to confirm all emitted keys are lowercase-snake (no camelCase leaks).
func TestBlockJSONKeysAreSnakeCase(t *testing.T) {
	block := Block{
		V:         2,
		BusID:     "bus-snake",
		Ts:        "2026-04-22T12:00:00Z",
		From:      "producer",
		Type:      "bus.message",
		Payload:   map[string]interface{}{"k": "v"},
		Digest:    "sha256:dead",
		MediaType: "application/json",
		PrevHash:  "ph",
		Hash:      "h",
		Size:      10,
	}
	data, _ := json.Marshal(block)
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expected := []string{"v", "bus_id", "ts", "from", "type", "payload", "digest", "media_type", "prev_hash", "hash", "size"}
	for _, k := range expected {
		if _, ok := raw[k]; !ok {
			t.Errorf("expected snake_case key %q not found; got keys %v", k, mapKeys(raw))
		}
	}
	// Negative: no camelCase leak
	for _, bad := range []string{"busId", "mediaType", "prevHash", "BusID", "MediaType"} {
		if _, exists := raw[bad]; exists {
			t.Errorf("unexpected camelCase/PascalCase key %q in JSON", bad)
		}
	}
}

func mapKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBlockMalformedDigestAccepted documents that the Block struct does NOT
// validate the Digest format at the (de)serialization layer. A malformed
// value round-trips as an opaque string. Validation is a higher-layer concern
// (engine/blobstore on ingest), not a struct invariant.
func TestBlockMalformedDigestAccepted(t *testing.T) {
	block := Block{
		V:      2,
		Ts:     "2026-04-22T12:00:00Z",
		From:   "producer",
		Type:   "bus.message",
		Digest: "not-a-hash",
		Hash:   "hash-mal",
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Digest != "not-a-hash" {
		t.Errorf("malformed Digest did not round-trip; got %q", decoded.Digest)
	}
	// Documentation: schema accepts any string for Digest. Format validation
	// (sha256:<64-hex>) lives in engine/blobstore ingest, not here.
}

// TestBlockLargePayloadRoundTrip verifies that a ~1MB inline payload
// round-trips correctly through JSON.
func TestBlockLargePayloadRoundTrip(t *testing.T) {
	const target = 1 << 20 // 1 MB
	big := make([]byte, target)
	for i := range big {
		big[i] = byte('a' + (i % 26))
	}
	block := Block{
		V:       2,
		Ts:      "2026-04-22T12:00:00Z",
		From:    "producer",
		Type:    "bus.message",
		Payload: map[string]interface{}{"blob": string(big)},
		Hash:    "hash-big",
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) < target {
		t.Errorf("serialized size %d is smaller than payload %d (encoding collapse?)", len(data), target)
	}
	var decoded Block
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := decoded.Payload["blob"].(string)
	if !ok {
		t.Fatalf("decoded Payload.blob not a string: %T", decoded.Payload["blob"])
	}
	if len(got) != target {
		t.Errorf("decoded blob length = %d, want %d", len(got), target)
	}
}
