package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// IsProtectedArchivePath reports whether path points into an archived or
// attic region of a workspace: any path component named "_attic" or
// "output", or any ancestor directory carrying an ARCHIVED.txt marker.
//
// Context: on 2026-06-01 a frontmatter round-trip pass rewrote tracked files
// inside output/architecture-corpus-archive-2026-04-06/ (yaml quote-style
// churn on an immutable point-in-time archive), and on 2026-06-03 a sections
// sweep bulk-touched mem/working/. Archives must stay frozen: writers that
// go through the shared memory write helpers refuse these paths, and the
// mem walkers skip the directories outright. Verified 2026-06-09: no
// legitimate mem path contains an "output" component, so the simple
// component rule is safe. See the self-reconciliation plan (R-1):
// cog:mem/episodic/audits/2026-06-09-self-reconciliation-plan.cog.md
func IsProtectedArchivePath(path string) bool {
	abs := path
	if a, err := filepath.Abs(path); err == nil {
		abs = a
	}

	for _, part := range strings.Split(abs, string(filepath.Separator)) {
		if part == "_attic" || part == "output" {
			return true
		}
	}

	// Ancestor ARCHIVED.txt marker check (bounded walk toward root).
	dir := filepath.Dir(abs)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "ARCHIVED.txt")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return false
}

// skipProtectedDir is the walker-side companion to IsProtectedArchivePath:
// returns true when a directory entry should be skipped entirely
// (filepath.SkipDir) during mem walks.
func skipProtectedDir(name string) bool {
	return name == "_attic" || name == "output"
}
