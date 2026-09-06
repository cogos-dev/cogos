//go:build !fts5

// build_tags_nofts5_test.go — the NEGATIVE half of the probe's control pair.
//
// Compiled only when -tags fts5 is ABSENT. This is the half that runs in the
// default CI pipeline (`go test ./...`), and it is the one that proves the
// probe can return false. A capability check that can only ever answer "yes"
// is precisely the failure this package exists to prevent: the previous
// `cog health` guard reported the same thing on healthy and broken kernels,
// so it never detected the grep-fallback kernel of 2026-09-06.

package engine

import "testing"

// TestFTS5ProbeFalseWithoutTag asserts that omitting -tags fts5 is detected.
// If this ever passes with FTS5Available() == true, the probe is not measuring
// the build and every guard downstream of it is decorative.
func TestFTS5ProbeFalseWithoutTag(t *testing.T) {
	if FTS5Available() {
		t.Fatal("built WITHOUT -tags fts5 yet the runtime probe reports FTS5 available; " +
			"the probe is not measuring this build's real capability")
	}
	if err := FTS5ProbeError(); err == "" {
		t.Fatal("probe reports unavailable but gives no reason; an operator would " +
			"see fts5=false with nothing to act on")
	}
}

// TestBuildTagMismatchFiresOnUntaggedBuild pins the exact 2026-09-06 defect:
// a binary that DECLARES fts5 via ldflags while lacking the module must be
// flagged, not believed.
func TestBuildTagMismatchFiresOnUntaggedBuild(t *testing.T) {
	orig := DeclaredBuildTags
	t.Cleanup(func() { DeclaredBuildTags = orig })

	DeclaredBuildTags = "fts5"
	rep := buildTags()

	if !rep.Mismatch {
		t.Fatal("declared 'fts5' on a build with no fts5 module, but Mismatch=false; " +
			"the report would hide exactly the defect it exists to catch")
	}
	if rep.FTS5 {
		t.Fatal("FTS5=true on an untagged build")
	}
}
