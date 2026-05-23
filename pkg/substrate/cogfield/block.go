package cogfield

// ADR-084 dataref migration policy (Phase 1: schema-additive).
//
// The Block envelope below is the single source of truth for the two
// mutually-compatible payload forms that coexist during the migration:
//
//   - Inline (pre-ADR-084): Payload map[string]interface{}
//   - By-reference (ADR-084): Digest + MediaType + Size pointing into BlobStore
//
// Phase 1 emit paths continue to populate Payload. Phase 2 pilot sites
// (gated by COGOS_DATAREF_EMIT=<site-name>) store bytes via BlobStore and
// leave Payload empty. Consumers MUST tolerate both shapes and should:
//
//   1. Prefer Digest + BlobStore.Get(digest) if Digest != ""
//   2. Fall back to inline Payload otherwise
//
// Full cut-over happens in Phase 2 (ADR-084 revision pending) once all
// consumers have been updated to the byref-first resolution path.
//
// See: .cog/adr/084-bus-payloads-as-cogblocks.cog.md

// Block is the canonical content atom for the CogOS bus protocol (ADR-059).
// V1 blocks use PrevHash (string); V2 blocks use Prev ([]string) for DAG-style linking.
// Both fields are written during the transition period for backward compatibility.
//
// ADR-084 v1 — by-reference payload. When Digest and MediaType are set, the
// envelope carries a content-addressed reference to the payload bytes stored
// in BlobStore; Payload may be empty, and the actual bytes are fetched via
// GET /v1/blobs/:digest. The inline Payload field is preserved for backward
// compatibility during the Phase 1 schema-additive migration — producers may
// emit either shape, and consumers MUST tolerate both. Size is shared by
// both forms and indicates the payload size in bytes.
type Block struct {
	V         int                    `json:"v"`
	ID        string                 `json:"id,omitempty"`
	BusID     string                 `json:"bus_id,omitempty"`
	Seq       int                    `json:"seq,omitempty"`
	Ts        string                 `json:"ts"`
	From      string                 `json:"from"`
	To        string                 `json:"to,omitempty"`
	Type      string                 `json:"type"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
	Digest    string                 `json:"digest,omitempty"`     // ADR-084: "sha256:<hex>" content hash of by-reference payload
	MediaType string                 `json:"media_type,omitempty"` // ADR-084: OCI media type of the payload (e.g. application/vnd.cogos.trace.assessment.v1+json)
	Prev      []string               `json:"prev,omitempty"`
	PrevHash  string                 `json:"prev_hash,omitempty"` // V1 compat
	Hash      string                 `json:"hash"`
	Merkle    string                 `json:"merkle,omitempty"`
	Sig       string                 `json:"sig,omitempty"`
	Size      int                    `json:"size,omitempty"`
}

// GraphBlock is the intermediate representation for CogField graph rendering.
// Adapters convert their native data into GraphBlocks for visualization.
type GraphBlock struct {
	URI      string                 // cog://bus/{busID}/{seq}
	Type     string                 // bus.message, session.turn, etc.
	From     string
	Ts       string
	Hash     string
	PrevHash string
	Payload  map[string]interface{}
	Meta     map[string]interface{}
}
