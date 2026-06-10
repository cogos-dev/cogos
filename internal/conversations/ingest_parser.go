// ingest_parser.go — streaming parser for the normalized ingest surface.
//
// Handles records conforming to cogos.observatory.conversations/v0.1.
// Records with an unknown schema value are rejected and logged; the parser
// never guesses at schema semantics.
//
// Dedup on the normalized path:
//   - When refs contains a "stable_id" key: dedup by that value.
//   - Otherwise: dedup by SHA-256 of "<role>\x00<timestamp>\x00<text>".
//
// Monotonic turn_index is assigned per (source, session_id) when absent from
// the record.
package conversations

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// knownIngestSchemas is the set of schema values this parser speaks.
// Records declaring any other schema value are rejected.
var knownIngestSchemas = map[string]bool{
	"cogos.observatory.conversations/v0.1": true,
}

// validIngestRoles is the allowed set of role values per the schema contract.
var validIngestRoles = map[string]Role{
	"user":      RoleUser,
	"assistant": RoleAssistant,
	"tool":      RoleTool,
	"system":    RoleSystem,
}

// ingestRecord is the decoded form of one line in a normalized ingest JSONL.
type ingestRecord struct {
	Schema       string          `json:"schema"`
	Source       string          `json:"source"`
	SessionID    string          `json:"session_id"`
	SessionTitle string          `json:"session_title,omitempty"`
	TurnIndex    *int            `json:"turn_index,omitempty"` // pointer so we can detect absence
	Role         string          `json:"role"`
	Timestamp    string          `json:"timestamp"`
	Text         string          `json:"text"`
	Identity     string          `json:"identity,omitempty"`
	Refs         json.RawMessage `json:"refs,omitempty"`
}

// ingestRefs is the optional refs object within an ingest record.
type ingestRefs struct {
	StableID any    `json:"stable_id,omitempty"` // stable per-record id for dedup
	DB       string `json:"db,omitempty"`
	MessageID any   `json:"message_id,omitempty"`
}

// ParseIngestSession streams turns from a normalized ingest JSONL. r is the
// open file (caller controls open/close). The indexKey is the composite
// "<source>/<session_id>" used as the session key in the index.
//
// Behaviour:
//   - Records with unknown schema → rejected, logged, counted in stats.
//   - Records with unknown role  → rejected, logged.
//   - Missing required fields (source, session_id, role, timestamp, text) → rejected, logged.
//   - Duplicate records (by stable_id or content hash) → silently skipped.
//   - turn_index absent           → assigned monotonically from 0.
//   - text longer than maxTurnLen → truncated with " [truncated]" suffix.
//
// Returns the number of records rejected due to schema mismatch.
func ParseIngestSession(r io.Reader, indexKey string, maxTurnLen int, meta *SessionMeta, callback func(Turn) bool) (rejectedSchemas int, err error) {
	if maxTurnLen <= 0 {
		maxTurnLen = defaultMaxTurnLen
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxScannerTokenSize), maxScannerTokenSize)

	seen := make(map[string]struct{}) // dedup keys seen so far
	turnIndex := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rec ingestRecord
		if jsonErr := json.Unmarshal(line, &rec); jsonErr != nil {
			// Skip unparseable lines gracefully.
			continue
		}

		// Schema validation — reject and log unknown schemas.
		if !knownIngestSchemas[rec.Schema] {
			log.Printf("conversations/ingest: rejected record with unknown schema %q in session %s", rec.Schema, indexKey)
			rejectedSchemas++
			continue
		}

		// Required field validation.
		if rec.Source == "" || rec.SessionID == "" || rec.Role == "" || rec.Timestamp == "" || rec.Text == "" {
			log.Printf("conversations/ingest: rejected record missing required fields in session %s (source=%q session_id=%q role=%q timestamp=%q text_len=%d)",
				indexKey, rec.Source, rec.SessionID, rec.Role, rec.Timestamp, len(rec.Text))
			continue
		}

		// Role validation.
		role, roleOK := validIngestRoles[rec.Role]
		if !roleOK {
			log.Printf("conversations/ingest: rejected record with unknown role %q in session %s", rec.Role, indexKey)
			continue
		}

		// Dedup key.
		dedupKey := ingestDedupKey(rec)
		if _, dup := seen[dedupKey]; dup {
			continue
		}
		seen[dedupKey] = struct{}{}

		// Assign monotonic turn_index when absent.
		idx := turnIndex
		if rec.TurnIndex != nil {
			idx = *rec.TurnIndex
		}

		// Truncate text.
		text := rec.Text
		if len(text) > maxTurnLen {
			text = text[:maxTurnLen] + " [truncated]"
		}

		ts := parseTimestamp(rec.Timestamp)
		updateTimeBounds(meta, ts)

		// Session title from any record that carries one.
		if rec.SessionTitle != "" && meta.Title == "" {
			meta.Title = rec.SessionTitle
		}
		// Identity from any record that carries one.
		if rec.Identity != "" && meta.Identity == "" {
			meta.Identity = rec.Identity
		}

		turn := Turn{
			UUID:      dedupKey, // stable identifier within the index
			SessionID: indexKey,
			TurnIndex: idx,
			Role:      role,
			Timestamp: ts,
			Text:      text,
		}

		if !callback(turn) {
			return rejectedSchemas, nil
		}
		turnIndex++
		meta.TurnCount = turnIndex
	}

	return rejectedSchemas, scanner.Err()
}

// ingestDedupKey returns the dedup key for an ingest record.
//
// Priority:
//  1. refs.stable_id (if present and non-empty after string conversion)
//  2. SHA-256 of "<role>\x00<timestamp>\x00<text>"
func ingestDedupKey(rec ingestRecord) string {
	// Try to extract stable_id from refs.
	if len(rec.Refs) > 0 {
		var refs ingestRefs
		if json.Unmarshal(rec.Refs, &refs) == nil && refs.StableID != nil {
			var stableStr string
			switch v := refs.StableID.(type) {
			case string:
				stableStr = v
			case float64:
				stableStr = fmt.Sprintf("%.0f", v)
			default:
				b, _ := json.Marshal(v)
				stableStr = string(b)
			}
			if stableStr != "" {
				return "stable:" + rec.Source + ":" + stableStr
			}
		}
	}

	// Fall back to content hash.
	h := sha256.New()
	h.Write([]byte(rec.Role))
	h.Write([]byte{0})
	h.Write([]byte(rec.Timestamp))
	h.Write([]byte{0})
	h.Write([]byte(rec.Text))
	return "hash:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// indexKeyForIngest builds the composite session key used in the index for
// normalized ingest sessions: "<source>/<session_id>".
func indexKeyForIngest(source, sessionID string) string {
	return source + "/" + sessionID
}

// parseIngestTimestamp parses an ingest record timestamp. Accepts RFC3339 with
// or without sub-second precision, and Python isoformat strings that use a
// space separator instead of 'T'.
func parseIngestTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Normalise Python isoformat space separator to 'T'.
	normalised := strings.Replace(s, " ", "T", 1)
	return parseTimestamp(normalised)
}
