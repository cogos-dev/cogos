package selfupdate

import "testing"

// TestGitDescribeVersionIsInertToSelfUpdate pins the regression reported on
// PR #486: a `make`-produced dev-build version string must never be treated as
// merely "behind" a release, because self-update on the auto path would then
// overwrite the developer's binary with the plain release build.
//
// The trap is that a bare `git describe` string such as
// "v0.16.22-6-g4f0acf1-dirty" is VALID semver whose suffix parses as a
// PRERELEASE, and prereleases sort BEFORE their base tag (see TestVersionAfter:
// versionAfter("v0.17.0", "v0.17.0-rc1") == true). So a bare describe string
// passes GATE D (it normalises non-empty) and fails to stop GATE F (the release
// sorts "after" it) — the exact clobber the Makefile guard exists to prevent.
//
// The Makefile therefore prefixes the describe output with "dev-", which makes
// normVersion return "" so GATE D fires and self-update stays inert.
func TestGitDescribeVersionIsInertToSelfUpdate(t *testing.T) {
	// Exactly the shapes `VERSION ?= $(shell ... echo "dev-$$d" ...)` produces.
	devBuildVersions := []string{
		"dev-v0.16.22-6-g4f0acf1",
		"dev-v0.16.22-6-g4f0acf1-dirty",
		"dev-v0.16.22-dirty",
		"dev-v0.16.22",
		"dev-4f0acf1", // no tags reachable: `git describe --always`
		"dev",         // no git at all (source tarball)
	}

	for _, running := range devBuildVersions {
		t.Run(running, func(t *testing.T) {
			// GATE D (provider.go:272) must fire: dev builds are never
			// auto-updated. This is the load-bearing assertion.
			if got := normVersion(running); got != "" {
				t.Errorf("normVersion(%q) = %q, want \"\" so GATE D fires; "+
					"a parseable version lets self-update clobber the dev build",
					running, got)
			}

			// And belt-and-braces: no release may sort "after" a dev build,
			// which is what GATE F would key on if GATE D ever moved.
			for _, target := range []string{"v0.16.22", "v0.17.0", "v1.0.0"} {
				if versionAfter(target, running) {
					t.Errorf("versionAfter(%q, %q) = true, want false; "+
						"release must not be considered an upgrade over a dev build",
						target, running)
				}
			}
		})
	}
}
