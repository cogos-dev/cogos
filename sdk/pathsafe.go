package sdk

import (
	"fmt"
	"path/filepath"
	"strings"
)

// sanitizePathComponent and sanitizeRelPath are the sdk module's copy of the
// path-sanitization logic in pkg/pathsafe (main github.com/myrgic/cogos
// module).
//
// CANONICAL VERSION: pkg/pathsafe/pathsafe.go. This is a manually-kept-in-
// sync duplicate, not an independent implementation — same pattern already
// used a few lines away in uri.go for the Namespaces whitelist ("The sdk
// module cannot import pkg/substrate/uri directly (separate Go modules)").
// The sdk module (github.com/myrgic/cogos/sdk) has its own go.mod with a
// deliberately small dependency set so it can be imported standalone,
// separate from the main module's; requiring the main module just to reach
// one of its subpackages would pull that whole dependency graph into every
// program that imports the SDK. See pkg/pathsafe for the full rationale,
// the NTFS-compatibility background (myrgic/cogos#489), and the test
// corpus this logic is validated against.
//
// myrgic/cogos#489 round 2: this copy exists because the sdk's own
// ledgerProjector (cogos.go) joins caller-supplied URI path segments into a
// ledger filesystem path via filepath.Join with no sanitization, which is a
// path-traversal read/write reachable unauthenticated through
// sdk/httputil.Server's GET /resolve and POST /mutate. Sanitizing every
// segment closes that: escaping ".." turns it into the literal, non-
// traversing component ".%2E" instead of a parent-directory reference (see
// isIllegalSDKPathByte's doc comment for why '/' and '\\' are escaped too,
// which is what stops a single segment from smuggling in extra path
// segments of its own). ParseURI (uri.go) also rejects raw ".." segments at
// parse time as an independent, defense-in-depth layer ahead of this one.

// reservedSDKWindowsStems are the device names NTFS/Windows reserves for a
// path component regardless of case or trailing extension.
var reservedSDKWindowsStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizePathComponent transforms raw into a value safe to use as a SINGLE
// filesystem path component (never a multi-segment path — it does not
// preserve "/" as a separator). It is deterministic and idempotent:
// sanitizing an already-sanitized value is a no-op.
func sanitizePathComponent(raw string) string {
	if raw == "" {
		return raw
	}

	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if isIllegalSDKPathByte(c) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()

	if n := len(out); n > 0 && (out[n-1] == '.' || out[n-1] == ' ') {
		out = out[:n-1] + fmt.Sprintf("%%%02X", out[n-1])
	}

	if isReservedSDKWindowsStem(out) {
		out = fmt.Sprintf("%%%02X", out[0]) + out[1:]
	}

	return out
}

// isIllegalSDKPathByte reports whether c is illegal in a Windows/NTFS path
// component. '%' is deliberately NOT included — leaving the escape
// character unescaped is what makes sanitizePathComponent idempotent.
func isIllegalSDKPathByte(c byte) bool {
	switch c {
	case '<', '>', ':', '"', '|', '?', '*', '/', '\\':
		return true
	}
	return c < 0x20
}

// isReservedSDKWindowsStem reports whether name's pre-extension stem
// matches a Windows-reserved device name, case-insensitively.
func isReservedSDKWindowsStem(name string) bool {
	stem := name
	if idx := strings.IndexByte(name, '.'); idx != -1 {
		stem = name[:idx]
	}
	return reservedSDKWindowsStems[strings.ToUpper(stem)]
}

// sanitizeRelPath sanitizes a caller-supplied, possibly multi-segment
// relative path (e.g. a ParsedURI.Path such as "<session-id>/events.jsonl")
// by running sanitizePathComponent over each "/"-separated segment
// independently and rejoining the result with filepath.Join.
//
// This is what actually makes it safe to join a URI path onto a base
// directory: every segment becomes traversal-safe on its own (a ".."
// segment becomes ".%2E", never a parent-directory reference), so the
// joined result can never resolve outside the base directory regardless of
// how filepath.Join's own lexical Clean() would otherwise have collapsed
// it. Empty segments (from a doubled "/") are dropped.
func sanitizeRelPath(relPath string) string {
	if relPath == "" {
		return relPath
	}
	segments := strings.Split(relPath, "/")
	clean := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		clean = append(clean, sanitizePathComponent(seg))
	}
	return filepath.Join(clean...)
}
