// version.go — semver comparison helpers for the self-update provider.
//
// All comparisons normalise the v-prefix and treat dev/unknown/empty as the
// empty version (never auto-updated). The underlying ordering is delegated to
// golang.org/x/mod/semver, which requires a leading "v".
package selfupdate

import (
	"strings"

	"golang.org/x/mod/semver"
)

// normVersion canonicalises a version string for comparison.
//
//   - leading/trailing whitespace is trimmed
//   - "", "dev", "unknown" (case-insensitive) normalise to "" (no version)
//   - a missing "v" prefix is added ("0.16.4" → "v0.16.4")
//   - if the result is not a valid semver string, "" is returned
//
// The empty string is the sentinel for "no comparable version" — a dev build,
// an unknown build, or a malformed tag. Callers gate on "" before comparing.
func normVersion(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "dev", "unknown":
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// versionAfter reports whether cand is strictly newer than cur.
// If either normalises to "" (dev/unknown/invalid), it returns false: a build
// with no comparable version is never "after" anything, which keeps the auto
// path from updating or downgrading dev builds.
func versionAfter(cand, cur string) bool {
	nc := normVersion(cand)
	nu := normVersion(cur)
	if nc == "" || nu == "" {
		return false
	}
	return semver.Compare(nc, nu) > 0
}

// versionEqual reports whether a and b are the same version.
// When either normalises to "" the comparison falls back to raw string equality
// of the normalised forms (so "" == "" is true, but ""/dev != a real tag).
func versionEqual(a, b string) bool {
	na := normVersion(a)
	nb := normVersion(b)
	if na == "" || nb == "" {
		return na == nb
	}
	return semver.Compare(na, nb) == 0
}
