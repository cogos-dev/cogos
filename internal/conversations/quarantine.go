// quarantine.go — quarantine surface for unmapped/unknown ingest components.
//
// Per L0 enforcement invariants:
//   - Records/components that no loaded mapping speaks are quarantined with
//     provenance (never guessed, never silently dropped).
//   - Quarantine files land under <workspace>/.cog/observatory/quarantine/<source>/.
//   - Each quarantine record is a JSON line with the original record plus a
//     _quarantine provenance block.
//
// Quarantine is append-only but idempotent: a record whose dedup key (the
// original record's refs.stable_id, or a content hash when absent) is already
// present in the source's quarantine file is skipped. Reconcile cycles re-read
// an unchanged ingest source repeatedly — without this guard a quarantine-only
// source (every record rejected, e.g. claude-ai-web composer drafts) grows its
// quarantine.jsonl without bound, one full re-append per cycle.
//
// The observatory never reads quarantine files back into the index — they are a
// forensic/diagnostic surface only.
package conversations

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/pathsafe"
)

// QuarantineDir is the default subdirectory under the workspace for quarantine
// output: <workspace>/.cog/observatory/quarantine.
const QuarantineDir = ".cog/observatory/quarantine"

// QuarantineReason describes why a record was quarantined.
type QuarantineReason string

const (
	// QuarantineReasonUnmappedComponent is used when the component class
	// inferred for this record does not appear in any loaded L2 mapping.
	QuarantineReasonUnmappedComponent QuarantineReason = "unmapped_component"

	// QuarantineReasonUnknownSchema is used when an ingest record declares
	// an unknown schema version.
	QuarantineReasonUnknownSchema QuarantineReason = "unknown_schema"

	// QuarantineReasonOntologyMismatch is used when a record declares an
	// (ontology id, major version) for which no L1 instance is loaded.
	QuarantineReasonOntologyMismatch QuarantineReason = "ontology_mismatch"

	// QuarantineReasonDraftRole is used when a record carries a recognized draft
	// role (e.g. "user-draft"): an unsent composer draft that is not a
	// conversation turn, and so is preserved in quarantine rather than ingested.
	QuarantineReasonDraftRole QuarantineReason = "draft_role"
)

// QuarantineProvenance is the provenance block appended to quarantined records.
type QuarantineProvenance struct {
	// Reason is the quarantine reason code.
	Reason QuarantineReason `json:"reason"`

	// Component is the source-declared or inferred component identifier
	// (e.g. "tool_result", "attachment", "system").
	Component string `json:"component,omitempty"`

	// OntologyRef is the loaded ontology version reference at the time
	// of quarantine, e.g. "cogos.conversations@1.0.0".
	OntologyRef string `json:"ontology_ref,omitempty"`

	// MappingRef is the loaded mapping version reference for the source,
	// e.g. "claude-code-jsonl@1.0.0".
	MappingRef string `json:"mapping_ref,omitempty"`

	// QuarantinedAt is the RFC3339 timestamp of when the record was quarantined.
	QuarantinedAt string `json:"quarantined_at"`
}

// quarantineRecord is the JSON line written to the quarantine file.
type quarantineRecord struct {
	// Original is the raw record bytes preserved verbatim.
	Original json.RawMessage `json:"original"`

	// Quarantine is the provenance block.
	Quarantine QuarantineProvenance `json:"_quarantine"`
}

// QuarantineWriter writes quarantined records to the per-source quarantine
// directory. Thread-safe via an internal mutex. Writes are idempotent per
// dedup key (see quarantineDedupKey): repeated quarantining of the same record
// across reconcile cycles is a no-op.
type QuarantineWriter struct {
	mu         sync.Mutex
	quarantine string // <workspace>/.cog/observatory/quarantine

	// seen maps sanitized source name → set of dedup keys already quarantined
	// for that source. Lazily populated from the on-disk quarantine.jsonl the
	// first time a source is written, so idempotency spans process restarts and
	// dedups against any pre-existing backlog.
	seen map[string]map[string]struct{}
}

// NewQuarantineWriter creates a writer backed by quarantineDir.
// quarantineDir is typically <workspace>/.cog/observatory/quarantine.
func NewQuarantineWriter(quarantineDir string) *QuarantineWriter {
	return &QuarantineWriter{
		quarantine: quarantineDir,
		seen:       make(map[string]map[string]struct{}),
	}
}

// WriteRecord appends one quarantined record to
// <quarantineDir>/<source>/quarantine.jsonl, unless a record with the same
// dedup key has already been quarantined for this source (in which case it is
// a no-op and returns nil). The file is created and the directory is mkdir'd
// if absent. Returns an error only for I/O failures; callers may log and
// continue.
func (q *QuarantineWriter) WriteRecord(source string, original json.RawMessage, prov QuarantineProvenance) error {
	key := quarantineDedupKey(original)

	q.mu.Lock()
	defer q.mu.Unlock()

	safeSource := sanitizeSourceName(source)
	dir := filepath.Join(q.quarantine, safeSource)
	path := filepath.Join(dir, "quarantine.jsonl")

	// Lazily load the set of dedup keys already on disk for this source so
	// idempotency holds across process restarts and against a pre-existing
	// backlog.
	seen, ok := q.seen[safeSource]
	if !ok {
		seen = loadQuarantineKeys(path)
		q.seen[safeSource] = seen
	}
	if _, dup := seen[key]; dup {
		return nil // already quarantined — idempotent no-op
	}

	prov.QuarantinedAt = time.Now().UTC().Format(time.RFC3339)
	qr := quarantineRecord{
		Original:   original,
		Quarantine: prov,
	}
	line, err := json.Marshal(qr)
	if err != nil {
		return fmt.Errorf("quarantine: marshal record: %w", err)
	}
	line = append(line, '\n')

	if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
		return fmt.Errorf("quarantine: mkdir %s: %w", dir, mkdirErr)
	}

	f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("quarantine: open %s: %w", path, openErr)
	}
	defer f.Close()

	if _, writeErr := f.Write(line); writeErr != nil {
		return fmt.Errorf("quarantine: write %s: %w", path, writeErr)
	}

	seen[key] = struct{}{}
	return nil
}

// quarantineDedupKey derives the idempotency key for a raw ingest record. It
// prefers refs.stable_id (the same stable identity the ingest parser dedups
// turns by); when absent it falls back to a SHA-256 of the raw record bytes so
// byte-identical records still dedup. The "s:"/"h:" prefix keeps a stable_id
// from ever colliding with a content hash.
func quarantineDedupKey(original json.RawMessage) string {
	var probe struct {
		Refs struct {
			StableID any `json:"stable_id"`
		} `json:"refs"`
	}
	if json.Unmarshal(original, &probe) == nil && probe.Refs.StableID != nil {
		switch v := probe.Refs.StableID.(type) {
		case string:
			if v != "" {
				return "s:" + v
			}
		case float64:
			return "s:" + fmt.Sprintf("%.0f", v)
		default:
			if b, mErr := json.Marshal(v); mErr == nil {
				return "s:" + string(b)
			}
		}
	}
	sum := sha256.Sum256(original)
	return "h:" + hex.EncodeToString(sum[:16])
}

// loadQuarantineKeys reads an existing quarantine.jsonl and returns the set of
// dedup keys it already contains. A missing or unreadable file yields an empty
// set (fail-open: worst case is one redundant append, never a lost record).
func loadQuarantineKeys(path string) map[string]struct{} {
	keys := make(map[string]struct{})
	f, err := os.Open(path)
	if err != nil {
		return keys
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxScannerTokenSize), maxScannerTokenSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var qr quarantineRecord
		if json.Unmarshal(line, &qr) != nil || len(qr.Original) == 0 {
			continue
		}
		keys[quarantineDedupKey(qr.Original)] = struct{}{}
	}
	return keys
}

// sanitizeSourceName makes an ingest-record-supplied source name safe to use
// as a single quarantine directory-name component.
//
// This used to hand-roll its own escaping (only '/' and NUL), which was
// narrower than pkg/pathsafe.SanitizeComponent used everywhere else in this
// codebase for the same problem (myrgic/cogos#489 round 4): it left NTFS's
// other illegal characters (notably ':') unescaped, so a WriteRecord call
// with source "http:cog" — reachable from ingest_parser.go's quarantine
// branches for unknown-schema/missing-fields records — created the exact
// colon-bearing directory name #489 targets. It also left a bare ".."
// source name unescaped, which combined with the historical '/'-only
// replacement here (".." contains no '/') let filepath.Join(quarantineDir,
// "..") resolve one level above the intended quarantine root. Delegating to
// pathsafe.SanitizeComponent closes both: it escapes the full NTFS-illegal
// set including ':', and its trailing-dot rule turns ".." into the
// non-traversing literal ".%2E" (see pathsafe.go for why).
func sanitizeSourceName(name string) string {
	return pathsafe.SanitizeComponent(name)
}
