// cli_selfupdate_version.go — engine-side wrappers over the selfupdate package's
// semver helpers and resolver, shared by the unix updater and the CLI dispatch.
//
// Centralising these here keeps the v-prefix / dev-build normalisation in ONE
// place (the selfupdate package) so the daemon's ComputePlan and the updater
// subprocess apply identical comparison rules.
package engine

import (
	"context"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
)

// versionFieldEqual reports whether two version strings denote the same release,
// normalising the v-prefix and dev/unknown sentinels.
func versionFieldEqual(a, b string) bool {
	return selfupdate.VersionEqual(a, b)
}

// isDowngrade reports whether tag is strictly older than running.
// Returns false when either is a dev/unknown/invalid build (no comparison).
func isDowngrade(tag, running string) bool {
	if selfupdate.NormVersion(tag) == "" || selfupdate.NormVersion(running) == "" {
		return false
	}
	// tag is a downgrade iff running is after tag.
	return selfupdate.VersionAfter(running, tag)
}

// selfUpdateResolveTag resolves the asset/checksum URLs for an exact tag via the
// selfupdate package's resolver.
func selfUpdateResolveTag(ctx context.Context, repo, tag string) (*selfupdate.ResolvedTarget, error) {
	return selfupdate.ResolveTag(ctx, repo, tag)
}
