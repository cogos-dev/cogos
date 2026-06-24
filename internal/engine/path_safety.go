// path_safety.go — containment helpers for caller-supplied path input.
//
// The kernel's HTTP + MCP surface treats callers as "trusted local", but every
// place that builds a filesystem path from caller input (cogdoc paths, cog: URI
// components, bus ids) is still an arbitrary-read/write surface if the input can
// escape its intended base via "../" or an absolute path. These helpers give one
// consistent guard for those sites.
package engine

import (
	"fmt"
	"path/filepath"
	"strings"
)

// pathWithin reports whether full (after cleaning) is base itself or strictly
// inside base. The trailing-separator suffix means a sibling like "<base>-evil"
// does NOT count as within "<base>". This is a LEXICAL check — it does not
// resolve symlinks, so it assumes no attacker-plantable symlink already exists
// under base (planting one requires a prior write primitive, i.e. second-order).
func pathWithin(base, full string) bool {
	cb := filepath.Clean(base)
	cf := filepath.Clean(full)
	return cf == cb || strings.HasPrefix(cf, cb+string(filepath.Separator))
}

// containedJoin joins base with the untrusted relative component rel and returns
// the cleaned result only if it stays within base — defeating "../" traversal
// (absolute input is absorbed by Join and stays contained). base is trusted; rel
// is caller input. Lexical containment only — see pathWithin.
func containedJoin(base, rel string) (string, error) {
	full := filepath.Join(base, rel)
	if !pathWithin(base, full) {
		return "", fmt.Errorf("path %q escapes base %q", rel, base)
	}
	return filepath.Clean(full), nil
}

// validPathComponent reports whether s is safe to use as a single path segment
// (e.g. a bus id that becomes a directory name): non-empty, not "."/"..", and
// containing no separators, parent refs, or null bytes.
func validPathComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return false
	}
	return !strings.Contains(s, "..")
}
