package identity

import (
	"fmt"
	"strings"
)

// sanitizeSubjectComponent is this module's copy of the path-sanitization
// logic in pkg/pathsafe (main github.com/myrgic/cogos module).
//
// CANONICAL VERSION: pkg/pathsafe.SanitizeComponent. This is a manually-
// kept-in-sync duplicate, not an independent implementation — the same
// pattern already used by sdk/pathsafe.go for the identical reason: this
// package (github.com/myrgic/cogos/pkg/substrate) has its own go.mod with a
// deliberately small dependency set (only google.golang.org/protobuf) so it
// can be imported standalone by pkg/bep and other leaf modules; requiring
// the main module just to reach one of its subpackages would pull that
// whole dependency graph into every program that imports this package. See
// pkg/pathsafe for the full rationale, the NTFS-compatibility background
// (myrgic/cogos#489), and the test corpus this logic is validated against.
//
// myrgic/cogos#489 round 5: this copy exists because LoadCRD (identity.go)
// joins a caller-supplied subject slug into a filesystem path with NO
// sanitization at all — not even the '..'-traversal check every other seam
// fixed in this issue has. The subject originates from cog_register_session
// MCP tool's `in.Subject` field (internal/engine/mcp_sessions.go), stored
// verbatim on a HarnessBindingCRD, and later fed back into LoadCRD via
// resolveBoundIdentity → resolveIdentityExpression (internal/engine/serve.go,
// serve_g3.go) on every inference request whose X-Cogos-Session-Id resolves
// to that binding. A subject of "../../../../etc/some-file" (any string
// ending in a name that, with ".yaml" appended, matches a real file)
// resolves outside {root}/.cog/config/identities/ entirely — this is the
// least-guarded of every seam this issue has touched.
func sanitizeSubjectComponent(raw string) string {
	if raw == "" {
		return raw
	}

	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if isIllegalSubjectByte(c) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()

	if n := len(out); n > 0 && (out[n-1] == '.' || out[n-1] == ' ') {
		out = out[:n-1] + fmt.Sprintf("%%%02X", out[n-1])
	}

	if isReservedSubjectWindowsStem(out) {
		out = fmt.Sprintf("%%%02X", out[0]) + out[1:]
	}

	return out
}

// isIllegalSubjectByte reports whether c is illegal in a Windows/NTFS path
// component. '%' is deliberately NOT included — leaving the escape
// character unescaped is what makes sanitizeSubjectComponent idempotent.
func isIllegalSubjectByte(c byte) bool {
	switch c {
	case '<', '>', ':', '"', '|', '?', '*', '/', '\\':
		return true
	}
	return c < 0x20
}

var reservedSubjectWindowsStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isReservedSubjectWindowsStem reports whether name's pre-extension stem
// matches a Windows-reserved device name, case-insensitively.
func isReservedSubjectWindowsStem(name string) bool {
	stem := name
	if idx := strings.IndexByte(name, '.'); idx != -1 {
		stem = name[:idx]
	}
	return reservedSubjectWindowsStems[strings.ToUpper(stem)]
}
