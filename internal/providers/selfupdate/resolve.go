// resolve.go — CLI-facing release resolution and version helpers.
//
// These exported wrappers let the engine's `cogos self-update` CLI resolve a
// target release and reuse the same semver comparison rules as the provider,
// without re-implementing the GitHub query or the v-prefix/dev normalisation.
package selfupdate

import "context"

// ResolvedTarget is the CLI-facing view of a resolved release.
type ResolvedTarget struct {
	Tag         string
	Prerelease  bool
	AssetName   string
	AssetURL    string
	ChecksumURL string
}

// ResolveTarget resolves the target release for the given repo/channel/pin and
// returns its tag plus asset URLs. interval bounds nothing here (a fresh
// resolver is used per CLI invocation), but the same code path is exercised.
func ResolveTarget(ctx context.Context, repo, channel, pin string) (*ResolvedTarget, error) {
	cfg := &SelfUpdateConfig{
		Enabled:       true,
		Channel:       channel,
		Pin:           pin,
		Repo:          repo,
		CheckInterval: defaultCheckInterval,
	}
	if cfg.Channel == "" {
		cfg.Channel = channelStable
	}
	if cfg.Repo == "" {
		cfg.Repo = defaultRepo
	}
	r := &ReleaseResolver{}
	rel, err := r.Resolve(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &ResolvedTarget{
		Tag:         rel.Tag,
		Prerelease:  rel.Prerelease,
		AssetName:   rel.AssetName,
		AssetURL:    rel.AssetURL,
		ChecksumURL: rel.ChecksumURL,
	}, nil
}

// ResolveTag resolves only the asset URLs for an exact known tag. Used by the
// updater path, which already knows the tag passed by --to.
func ResolveTag(ctx context.Context, repo, tag string) (*ResolvedTarget, error) {
	return ResolveTarget(ctx, repo, "", tag)
}

// VersionAfter reports whether cand is strictly newer than cur (exported wrapper).
func VersionAfter(cand, cur string) bool { return versionAfter(cand, cur) }

// VersionEqual reports whether a and b are the same version (exported wrapper).
func VersionEqual(a, b string) bool { return versionEqual(a, b) }

// NormVersion canonicalises a version string (exported wrapper). Returns "" for
// dev/unknown/invalid builds.
func NormVersion(v string) string { return normVersion(v) }
