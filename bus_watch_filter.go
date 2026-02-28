// bus_watch_filter.go — Filter parsing and evaluation for cog bus watch.
//
// Filters are composable: multiple flags of the same type are OR'd;
// different flag types are AND'd.
//
//   -t "chat.*"              → type glob
//   -f "user:*"              → from glob
//   -F "tokens_used>100"     → payload field comparison
//   --trigger "error_type"   → break condition (same syntax as -F)

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FilterOp is a comparison operator for payload field filters.
type FilterOp int

const (
	OpGlob   FilterOp = iota // default: glob match on string value
	OpEq                     // ==
	OpNeq                    // !=
	OpGt                     // >
	OpLt                     // <
	OpGte                    // >=
	OpLte                    // <=
	OpExists                 // field exists (any value)
)

// FieldFilter is a single payload field comparison.
type FieldFilter struct {
	Key string   // payload field name (dot-path: "metadata.username")
	Op  FilterOp // comparison operator
	Val string   // comparison value (parsed to number for numeric ops)
}

// WatchFilter holds the parsed filter criteria for bus watch.
type WatchFilter struct {
	Types    []string      // glob patterns for evt.Type
	From     []string      // glob patterns for evt.From
	To       []string      // glob patterns for evt.To
	Fields   []FieldFilter // payload field filters
	Since    time.Time     // only events after this time
	NoReplay bool          // skip historical events
}

// ParseFieldFilter parses a field filter string into a FieldFilter.
//
// Syntax:
//
//	"key"                 → OpExists (field exists, any value)
//	"key=val*"            → OpGlob (glob match)
//	"key!=val"            → OpNeq
//	"key>=val"            → OpGte
//	"key<=val"            → OpLte
//	"key>val"             → OpGt
//	"key<val"             → OpLt
func ParseFieldFilter(s string) (FieldFilter, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return FieldFilter{}, fmt.Errorf("empty field filter")
	}

	// Try two-char operators first (!=, >=, <=)
	for _, op := range []struct {
		tok string
		op  FilterOp
	}{
		{"!=", OpNeq},
		{">=", OpGte},
		{"<=", OpLte},
	} {
		if idx := strings.Index(s, op.tok); idx > 0 {
			return FieldFilter{
				Key: s[:idx],
				Op:  op.op,
				Val: s[idx+2:],
			}, nil
		}
	}

	// Single-char operators (=, >, <) — check after two-char to avoid false matches
	for _, op := range []struct {
		tok string
		op  FilterOp
	}{
		{"=", OpGlob}, // = uses glob matching (so "model=claude*" works)
		{">", OpGt},
		{"<", OpLt},
	} {
		if idx := strings.Index(s, op.tok); idx > 0 {
			return FieldFilter{
				Key: s[:idx],
				Op:  op.op,
				Val: s[idx+1:],
			}, nil
		}
	}

	// No operator — OpExists (field exists with any value)
	return FieldFilter{
		Key: s,
		Op:  OpExists,
	}, nil
}

// Match returns true if the event passes all filter criteria.
// Empty filters match everything.
func (f *WatchFilter) Match(evt *CogBlock) bool {
	if evt == nil {
		return false
	}

	// Time filter
	if !f.Since.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, evt.Ts)
		if err == nil && t.Before(f.Since) {
			return false
		}
	}

	// Type filter (OR within group)
	if len(f.Types) > 0 {
		if !matchAnyGlob(f.Types, evt.Type) {
			return false
		}
	}

	// From filter (OR within group)
	if len(f.From) > 0 {
		if !matchAnyGlob(f.From, evt.From) {
			return false
		}
	}

	// To filter (OR within group)
	if len(f.To) > 0 {
		if !matchAnyGlob(f.To, evt.To) {
			return false
		}
	}

	// Field filters (AND between fields)
	for _, ff := range f.Fields {
		if !matchFieldFilter(ff, evt.Payload) {
			return false
		}
	}

	return true
}

// matchAnyGlob returns true if value matches any of the glob patterns.
func matchAnyGlob(patterns []string, value string) bool {
	for _, p := range patterns {
		if matchGlob(p, value) {
			return true
		}
	}
	return false
}

// matchGlob performs filepath.Match-style glob matching.
// Returns true on match or on pattern parse error (permissive).
func matchGlob(pattern, value string) bool {
	matched, err := filepath.Match(pattern, value)
	if err != nil {
		// Fall back to exact match on malformed patterns
		return pattern == value
	}
	return matched
}

// matchFieldFilter evaluates a single field filter against a payload.
func matchFieldFilter(ff FieldFilter, payload map[string]interface{}) bool {
	val, ok := extractNestedField(payload, ff.Key)

	// For *.message events, the payload "message" field may be a JSON string
	// containing nested fields. Try parsing it if direct extraction fails.
	if !ok && payload != nil {
		if msgStr, isStr := payload["message"].(string); isStr {
			var msgObj map[string]interface{}
			if json.Unmarshal([]byte(msgStr), &msgObj) == nil {
				val, ok = extractNestedField(msgObj, ff.Key)
			}
		}
	}

	switch ff.Op {
	case OpExists:
		return ok
	case OpGlob:
		if !ok {
			return false
		}
		return matchGlob(ff.Val, fieldToString(val))
	case OpEq:
		if !ok {
			return false
		}
		return fieldToString(val) == ff.Val
	case OpNeq:
		if !ok {
			return true // field doesn't exist, so it's != anything
		}
		return fieldToString(val) != ff.Val
	case OpGt, OpLt, OpGte, OpLte:
		if !ok {
			return false
		}
		return compareNumeric(val, ff.Op, ff.Val)
	}
	return false
}

// extractNestedField retrieves a value from a nested map using dot-path notation.
// Example: extractNestedField(payload, "metadata.username") returns payload["metadata"]["username"].
func extractNestedField(payload map[string]interface{}, dotPath string) (interface{}, bool) {
	parts := strings.Split(dotPath, ".")
	var current interface{} = payload

	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// fieldToString converts a payload field value to its string representation.
func fieldToString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

// compareNumeric performs numeric comparison between a payload value and a filter value.
func compareNumeric(fieldVal interface{}, op FilterOp, filterVal string) bool {
	var fv float64
	switch v := fieldVal.(type) {
	case float64:
		fv = v
	case int:
		fv = float64(v)
	case int64:
		fv = float64(v)
	case string:
		var err error
		fv, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
	default:
		return false
	}

	target, err := strconv.ParseFloat(filterVal, 64)
	if err != nil {
		return false
	}

	switch op {
	case OpGt:
		return fv > target
	case OpLt:
		return fv < target
	case OpGte:
		return fv >= target
	case OpLte:
		return fv <= target
	}
	return false
}
