// Package pathsafe sanitizes free-form identifiers (session keys, channel
// keys, conversation IDs, ...) before they become a single filesystem path
// component.
//
// Background (myrgic/cogos#489): session keys throughout the kernel are
// opaque strings assembled from things like a request origin and an agent
// name ("http:cog"), an MCP-client-supplied session_id, or a bus "from"
// field. Several path-construction seams (the event ledger, the turn
// sidecar, the conversation-turns index) join that string directly into a
// directory or file name with no validation. On ext4/APFS a colon is a
// legal byte and the resulting tree looks fine locally, but it is illegal on
// NTFS: any Windows peer (checkout, clone, or BEP folder-binding
// replication) fails the moment it touches a tree containing one of these
// names.
//
// SanitizeComponent makes that seam safe on every OS CogOS peers run on
// today by percent-escaping (RFC 3986 style) the characters NTFS forbids in
// a path component, Windows' reserved device stems, and a trailing dot or
// space. The transform is deterministic AND idempotent: sanitizing an
// already-sanitized value is a no-op. Idempotence is load-bearing, not just
// a nice property — GetLastGlobalEvent, for instance, re-derives a session
// key from a directory listing (already sanitized) and feeds it straight
// back into GetLastEvent (which sanitizes again); a non-idempotent scheme
// would double-escape and silently look in the wrong directory.
//
// Getting both properties at once means NOT escaping the escape character
// ('%') itself: every %XX sequence this function emits uses only bytes that
// are themselves left untouched by a second pass (%, hex digits, letters),
// so re-running the function on its own output is a no-op by construction.
// The one accepted cost: a raw key that already happens to spell out
// another key's escape sequence (e.g. the literal string "http%3Acog")
// maps to the same path as "http:cog". Session keys in this codebase are
// UUIDs, "origin:agent" composites, or MCP-client-chosen slugs — none of
// which plausibly collide this way — so idempotence (a certainty-100% path
// in existing call sites) wins over closing that theoretical, unobserved
// collision window.
//
// The transform is one-way in practice, not because it is lossy (aside from
// the narrow case above, percent-decoding recovers the original), but
// because no caller in this codebase needs to recover the original key FROM
// the path: the authoritative session key is always carried separately (as
// a JSON field, an in-memory map key, ...); the path is only ever a
// write-mostly label that gets compared to itself, never parsed back into
// business logic. See the package tests for the specific NTFS rules
// covered.
package pathsafe

import (
	"fmt"
	"path/filepath"
	"strings"
)

// reservedWindowsStems are the device names NTFS/Windows reserves for a path
// component regardless of case or trailing extension (CON, CON.txt, and
// con.tar.gz are all illegal).
var reservedWindowsStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeComponent transforms raw into a value safe to use as a SINGLE
// filesystem path component (a directory or file name — never a multi-
// segment path; it does not preserve "/" as a separator) on every platform
// CogOS peers run on, in particular Windows/NTFS.
//
// Escaped, in order:
//   - the nine characters NTFS forbids in a path component: < > : " | ? *
//     plus both platform path separators / and \, so a raw value can never
//     smuggle in extra path segments;
//   - all ASCII control bytes (0x00-0x1F);
//   - a trailing '.' or ' ', which Windows silently strips/rejects even
//     though neither character is illegal mid-string (this also defuses
//     "." and ".." as path-traversal components: sanitizing ".." yields
//     ".%2E", not ".." or ".");
//   - Windows-reserved device stems (CON, PRN, AUX, NUL, COM1-9, LPT1-9),
//     matched case-insensitively against the portion of the string before
//     the first '.', by escaping the stem's leading byte so it no longer
//     matches the reserved name.
//
// Empty input is returned unchanged (callers that treat "" as "no session"
// keep doing so).
func SanitizeComponent(raw string) string {
	if raw == "" {
		return raw
	}

	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if isIllegalPathByte(c) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()

	if n := len(out); n > 0 && (out[n-1] == '.' || out[n-1] == ' ') {
		out = out[:n-1] + fmt.Sprintf("%%%02X", out[n-1])
	}

	if isReservedWindowsStem(out) {
		out = fmt.Sprintf("%%%02X", out[0]) + out[1:]
	}

	return out
}

// SanitizeRelPath sanitizes a caller-supplied, possibly multi-segment
// relative path (e.g. a cog: URI path component such as
// "<session-id>/events.jsonl") by running SanitizeComponent over each
// "/"-separated segment independently and rejoining the result with
// filepath.Join.
//
// This is what actually makes it safe to join a caller-supplied path onto a
// base directory: every segment becomes traversal-safe on its own (a ".."
// segment becomes ".%2E", never a parent-directory reference — see
// SanitizeComponent's doc comment), so the joined result can never resolve
// outside the base directory regardless of how filepath.Join's own lexical
// Clean() would otherwise have collapsed it. Empty segments (from a doubled
// "/") are dropped.
//
// myrgic/cogos#489 round 5: added here — the canonical package — because
// internal/engine/uri.go's resolveProjection (the single chokepoint every
// cog: projection in the root module resolves through: mem, adr, role,
// skill, agent, spec, status, ledger, kernel, canonical, conf, ontology,
// work, handoff, artifact, docs, hooks) rejected literal ".." substrings and
// absolute paths, but never escaped NTFS-illegal characters — so a
// colon-bearing path component (the exact #489 shape, e.g. a ledger
// session ID "http:cog") reached filepath.Join raw. The sdk module has an
// intentionally-duplicated private copy of this same function
// (sdk/pathsafe.go's sanitizeRelPath, predating this one) because it cannot
// import this package (separate go.mod, see that file's doc comment); keep
// the two in sync if either changes.
func SanitizeRelPath(relPath string) string {
	if relPath == "" {
		return relPath
	}
	segments := strings.Split(relPath, "/")
	clean := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		clean = append(clean, SanitizeComponent(seg))
	}
	return filepath.Join(clean...)
}

// isIllegalPathByte reports whether c is illegal in a Windows/NTFS path
// component. '%' is deliberately NOT included here — see the package doc
// comment on why leaving the escape character unescaped is what makes
// SanitizeComponent idempotent.
func isIllegalPathByte(c byte) bool {
	switch c {
	case '<', '>', ':', '"', '|', '?', '*', '/', '\\':
		return true
	}
	return c < 0x20
}

// isReservedWindowsStem reports whether name's pre-extension stem matches a
// Windows-reserved device name, case-insensitively.
func isReservedWindowsStem(name string) bool {
	stem := name
	if idx := strings.IndexByte(name, '.'); idx != -1 {
		stem = name[:idx]
	}
	return reservedWindowsStems[strings.ToUpper(stem)]
}
