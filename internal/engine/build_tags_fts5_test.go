//go:build fts5

// build_tags_fts5_test.go — the POSITIVE half of the probe's control pair.
//
// Compiled only when -tags fts5 is present. Its sibling
// build_tags_nofts5_test.go compiles only when the tag is absent and asserts
// the opposite. Together they pin the property that matters: the probe tracks
// the actual build, not a flag echoed back at us.
//
// CI runs `go test ./...` untagged, so the negative half is what guards the
// default pipeline; `make test` (BUILD_TAGS=fts5) runs this half.

package engine

import "testing"

// TestFTS5ProbeTrueWhenTagged asserts that a build carrying -tags fts5 can
// really execute FTS5 DDL. A failure here means the tag was passed but CGO
// was disabled, so the pure-Go go-sqlite3 stub got linked and the module is
// absent despite the flag — a build that lies about its own capabilities.
func TestFTS5ProbeTrueWhenTagged(t *testing.T) {
	if !FTS5Available() {
		t.Fatalf("built with -tags fts5 but the runtime probe says FTS5 is unavailable: %s\n"+
			"The tag was honoured by the compiler but the module is not loadable "+
			"(usually CGO_ENABLED=0). Memory search would silently degrade to an "+
			"unranked scan.", FTS5ProbeError())
	}
	if got := buildTags(); !got.FTS5 || got.Mismatch {
		t.Fatalf("buildTags() = %+v; want FTS5=true, Mismatch=false", got)
	}
}
