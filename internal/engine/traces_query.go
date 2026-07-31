// traces_query.go — Unified search across kernel trace JSONL streams.
//
// Per Agent Q's Survey Q design (2026-04-21), this file implements the read
// side of `.cog/run/*.jsonl` observability: a single entry point that scans
// the populated trace sources (attention, proprioceptive, internal-requests),
// normalizes their heterogeneous per-source schemas into one
// `{source, timestamp, session_id?, level?, line}` shape, and applies
// caller-provided filters (time range, session, level, substring).
//
// These are TRACES — semantic metabolites under the kernel-ingestion-and-
// digestion framing — not diagnostic log text. Kernel slog (stderr, text
// handler) is intentionally out of scope; that couples to Windows stderr-
// capture work (Agent K) and gets its own follow-up.
//
// Algorithmic shape mirrors Agent L's QueryLedger: read-side only, per-source
// scan + normalize, filter, limit + truncated flag. The /v1/proprioceptive
// endpoint is left byte-for-byte identical (dashboard.html and canvas.html
// consume its exact shape); this file adds a new surface, it does not reshape
// the old one.

package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TraceSource identifies one of the known trace JSONL streams under .cog/run/.
// "all" expands to every canonical source at query time.
type TraceSource string

const (
	SourceAttention        TraceSource = "attention"
	SourceProprioceptive   TraceSource = "proprioceptive"
	SourceInternalRequests TraceSource = "internal_requests"
	SourceAll              TraceSource = "all"
)

// Query limits mirror the ledger_query.go pattern from Agent L's design.
const (
	defaultTracesLimit = 100
	maxTracesLimit     = 1000
	tracesScanBufSize  = 1 << 20 // 1 MiB; handles rows with large embedded payloads.
	maxSubstringLen    = 1024    // v1 cap on substring filter length.
)

// TraceQuery bundles caller-provided filter parameters.
// Zero values are the defaults: no filter, newest-first, limit=defaultTracesLimit.
type TraceQuery struct {
	Source    TraceSource
	Level     string
	SessionID string
	Substring string // case-insensitive
	Since     time.Time
	Until     time.Time
	Limit     int
	Order     string // "desc" (default) | "asc"
}

// TraceResult is a single normalized row in the unified output.
// Line is the raw JSONL bytes — callers that need per-source fields
// unmarshal themselves.
type TraceResult struct {
	Source    string          `json:"source"`
	Timestamp time.Time       `json:"timestamp"`
	SessionID string          `json:"session_id,omitempty"`
	Level     string          `json:"level,omitempty"`
	Line      json.RawMessage `json:"line"`
}

// TraceQueryResult is the outer envelope returned by QueryTraces.
type TraceQueryResult struct {
	Count          int            `json:"count"`
	Results        []TraceResult  `json:"results"`
	Truncated      bool           `json:"truncated"`
	SourcesChecked []SourceStatus `json:"sources_checked"`
}

// SourceStatus reports per-file scan diagnostics.
// FileExists=false distinguishes "file absent" from "file present but empty",
// which is critical for the "I got nothing — wrong filter or empty file?"
// debugging use case called out in Agent Q §3.2.
type SourceStatus struct {
	Name       string `json:"name"`
	Scanned    int    `json:"scanned"`
	Matched    int    `json:"matched"`
	FileExists bool   `json:"file_exists"`
}

// sourceSpec captures per-source normalization rules.
type sourceSpec struct {
	name         string
	relPath      string
	timestampKey string
	sessionKey   string // "" if the source has no session_id field
	levelKey     string // "" if the source has no level-like field
}

// sourceSpecs is the §3.4 normalization table materialized.
//
// Empirical verification (2026-04-21) against a live workspace's .cog/run:
//   - attention.jsonl:         occurred_at (RFC3339 Z), no session_id, no level
//   - proprioceptive.jsonl:    timestamp (RFC3339 Z), no session_id, event as pseudo-level
//   - internal-requests.jsonl: timestamp is FLOAT UNIX SECONDS (drift from spec's
//     "observed RFC3339"); parseTimestamp handles both string and numeric forms.
var sourceSpecs = map[TraceSource]sourceSpec{
	SourceAttention: {
		name:         "attention",
		relPath:      filepath.Join(".cog", "run", "attention.jsonl"),
		timestampKey: "occurred_at",
	},
	SourceProprioceptive: {
		name:         "proprioceptive",
		relPath:      filepath.Join(".cog", "run", "proprioceptive.jsonl"),
		timestampKey: "timestamp",
		levelKey:     "event",
	},
	SourceInternalRequests: {
		name:         "internal_requests",
		relPath:      filepath.Join(".cog", "run", "internal-requests.jsonl"),
		timestampKey: "timestamp",
		sessionKey:   "session_id",
	},
}

// canonicalSourceOrder preserves a stable iteration over sourceSpecs.
// Map iteration is unstable; tests and diagnostic output benefit from
// a predictable order.
var canonicalSourceOrder = []TraceSource{
	SourceAttention,
	SourceProprioceptive,
	SourceInternalRequests,
}

// resolveSources expands "all" (and the empty default) to every canonical
// source, or returns the single requested spec. Unknown sources return an
// error so callers can surface a 400.
func resolveSources(src TraceSource) ([]sourceSpec, error) {
	if src == "" || src == SourceAll {
		specs := make([]sourceSpec, 0, len(canonicalSourceOrder))
		for _, key := range canonicalSourceOrder {
			specs = append(specs, sourceSpecs[key])
		}
		return specs, nil
	}
	spec, ok := sourceSpecs[src]
	if !ok {
		return nil, fmt.Errorf("unknown source %q (valid: attention, proprioceptive, internal_requests, all)", src)
	}
	return []sourceSpec{spec}, nil
}

// QueryTraces is the entry point. It reads each resolved source, applies the
// normalized filter set, merges results, sorts by timestamp per q.Order, and
// returns the globally-limited slice plus per-source diagnostics.
//
// Missing files are not errors — they surface as file_exists=false in
// SourcesChecked so callers can distinguish "file absent" from "empty match".
func QueryTraces(workspaceRoot string, q TraceQuery) (*TraceQueryResult, error) {
	if len(q.Substring) > maxSubstringLen {
		return nil, fmt.Errorf("substring filter too long (%d > %d)", len(q.Substring), maxSubstringLen)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultTracesLimit
	}
	if limit > maxTracesLimit {
		limit = maxTracesLimit
	}

	order := strings.ToLower(strings.TrimSpace(q.Order))
	if order != "asc" {
		order = "desc"
	}

	specs, err := resolveSources(q.Source)
	if err != nil {
		return nil, err
	}

	result := &TraceQueryResult{
		SourcesChecked: make([]SourceStatus, 0, len(specs)),
	}

	// Per-source pre-limit keeps one pathologically large source from starving
	// the others. We grab up to limit+1 per source; the extra row is what
	// allows Truncated=true to fire on a single-source query that has exactly
	// one more match than requested. The global sort+trim in the final pass
	// produces the correct merged result.
	var merged []TraceResult
	perSourceLimit := limit + 1
	for _, spec := range specs {
		rs, status := scanSource(workspaceRoot, spec, q, perSourceLimit)
		merged = append(merged, rs...)
		result.SourcesChecked = append(result.SourcesChecked, status)
	}

	sortByTimestamp(merged, order)

	if len(merged) > limit {
		merged = merged[:limit]
		result.Truncated = true
	}
	result.Results = merged
	result.Count = len(merged)
	return result, nil
}

// scanSource reads one JSONL file, applies filters, and returns up to
// perSourceLimit matches plus scan diagnostics.
func scanSource(root string, spec sourceSpec, q TraceQuery, perSourceLimit int) ([]TraceResult, SourceStatus) {
	path := filepath.Join(root, spec.relPath)
	status := SourceStatus{Name: spec.name}

	f, err := os.Open(path)
	if err != nil {
		// Missing file is not an error — reported via file_exists=false.
		return nil, status
	}
	defer f.Close()
	status.FileExists = true

	substringLower := strings.ToLower(q.Substring)

	var out []TraceResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), tracesScanBufSize)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		status.Scanned++

		// Cheap byte-level substring check first — no JSON parse needed if it misses.
		if q.Substring != "" && !bytes.Contains(bytes.ToLower(raw), []byte(substringLower)) {
			continue
		}

		// Defensive copy: bufio.Scanner reuses its buffer between Scan() calls,
		// so we must clone before storing in a retained slice.
		lineCopy := make([]byte, len(raw))
		copy(lineCopy, raw)

		row, ok := extractFields(lineCopy, spec)
		if !ok {
			continue
		}
		if !q.Since.IsZero() && row.Timestamp.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && row.Timestamp.After(q.Until) {
			continue
		}
		if q.SessionID != "" && row.SessionID != q.SessionID {
			continue
		}
		if q.Level != "" && !strings.EqualFold(row.Level, q.Level) {
			continue
		}

		out = append(out, row)
		status.Matched++
		if len(out) >= perSourceLimit {
			break
		}
	}
	return out, status
}

// extractFields parses one line and populates the normalized TraceResult.
// Returns (row, false) only if the JSON is unparseable; missing or malformed
// individual fields degrade gracefully (empty string, zero time).
func extractFields(line []byte, spec sourceSpec) (TraceResult, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return TraceResult{}, false
	}
	r := TraceResult{Source: spec.name, Line: json.RawMessage(line)}

	if tsRaw, ok := raw[spec.timestampKey]; ok {
		if t, ok := parseTimestamp(tsRaw); ok {
			r.Timestamp = t.UTC()
		}
	}
	if spec.sessionKey != "" {
		if sRaw, ok := raw[spec.sessionKey]; ok {
			_ = json.Unmarshal(sRaw, &r.SessionID)
		}
	}
	if spec.levelKey != "" {
		if lRaw, ok := raw[spec.levelKey]; ok {
			_ = json.Unmarshal(lRaw, &r.Level)
		}
	}
	return r, true
}

// parseTimestamp accepts:
//   - an RFC3339 / RFC3339Nano string (attention, proprioceptive)
//   - a numeric unix-seconds value, integer or float
//     (internal-requests.jsonl uses this — drift from Agent Q §3.4
//     which specified "observed RFC3339"; handled here without bubbling up
//     a source-specific schema to callers.)
func parseTimestamp(raw json.RawMessage) (time.Time, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return time.Time{}, false
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return time.Time{}, false
		}
		if s == "" {
			return time.Time{}, false
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
		return time.Time{}, false
	default:
		var f float64
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return time.Time{}, false
		}
		if f == 0 {
			return time.Time{}, false
		}
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec), true
	}
}

// sortByTimestamp sorts a merged slice in place. Stable ordering keeps the
// per-source arrival order for ties.
func sortByTimestamp(rows []TraceResult, order string) {
	sort.SliceStable(rows, func(i, j int) bool {
		if order == "asc" {
			return rows[i].Timestamp.Before(rows[j].Timestamp)
		}
		return rows[i].Timestamp.After(rows[j].Timestamp)
	})
}

// ParseTraceDurationOrTime interprets a since/until parameter. Accepts a Go
// duration ("5m", "1h", "24h") — interpreted as "since N ago" — or an
// RFC3339 / RFC3339Nano timestamp. Shared by the HTTP and MCP parsers.
func ParseTraceDurationOrTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp or Go duration (e.g. 5m, 1h): %q", s)
}
