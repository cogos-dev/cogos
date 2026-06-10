// quarantine.go — quarantine surface for unmapped/unknown ingest components.
//
// Per L0 enforcement invariants:
//   - Records/components that no loaded mapping speaks are quarantined with
//     provenance (never guessed, never silently dropped).
//   - Quarantine files land under <workspace>/.cog/observatory/quarantine/<source>/.
//   - Each quarantine record is a JSON line with the original record plus a
//     _quarantine provenance block.
//
// Quarantine is append-only. The observatory never reads quarantine files back
// into the index — they are a forensic/diagnostic surface only.
package conversations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
// directory. Thread-safe via an internal mutex.
type QuarantineWriter struct {
	mu         sync.Mutex
	quarantine string // <workspace>/.cog/observatory/quarantine
}

// NewQuarantineWriter creates a writer backed by quarantineDir.
// quarantineDir is typically <workspace>/.cog/observatory/quarantine.
func NewQuarantineWriter(quarantineDir string) *QuarantineWriter {
	return &QuarantineWriter{quarantine: quarantineDir}
}

// WriteRecord appends one quarantined record to
// <quarantineDir>/<source>/quarantine.jsonl.
// The file is created and the directory is mkdir'd if absent.
// Returns an error only for I/O failures; callers may log and continue.
func (q *QuarantineWriter) WriteRecord(source string, original json.RawMessage, prov QuarantineProvenance) error {
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

	q.mu.Lock()
	defer q.mu.Unlock()

	dir := filepath.Join(q.quarantine, sanitizeSourceName(source))
	if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
		return fmt.Errorf("quarantine: mkdir %s: %w", dir, mkdirErr)
	}

	path := filepath.Join(dir, "quarantine.jsonl")
	f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("quarantine: open %s: %w", path, openErr)
	}
	defer f.Close()

	if _, writeErr := f.Write(line); writeErr != nil {
		return fmt.Errorf("quarantine: write %s: %w", path, writeErr)
	}
	return nil
}

// sanitizeSourceName replaces characters that are not safe in directory names.
// Only forward-slashes and null bytes are replaced; other characters are kept
// as-is since source names are typically simple identifiers.
func sanitizeSourceName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '/' || c == 0 {
			out = append(out, '_')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
