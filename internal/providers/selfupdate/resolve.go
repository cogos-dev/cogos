// resolve.go — CLI-facing release resolution and version helpers.
//
// These exported wrappers let the engine's `cogos self-update` CLI resolve a
// target release and reuse the same semver comparison rules as the provider,
// without re-implementing the GitHub query or the v-prefix/dev normalisation.
package selfupdate

import (
	"context"

	"github.com/myrgic/cogos/internal/providers/selfupdate/provenance"
)

// ResolvedTarget is the CLI-facing view of a resolved release.
type ResolvedTarget struct {
	Tag         string
	Prerelease  bool
	AssetName   string
	AssetURL    string
	ChecksumURL string
	// SignatureURL / CertificateURL carry the Sigstore material the updater
	// verifies checksums.txt against before trusting any digest inside it.
	SignatureURL   string
	CertificateURL string
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
		Tag:            rel.Tag,
		Prerelease:     rel.Prerelease,
		AssetName:      rel.AssetName,
		AssetURL:       rel.AssetURL,
		ChecksumURL:    rel.ChecksumURL,
		SignatureURL:   rel.SignatureURL,
		CertificateURL: rel.CertificateURL,
	}, nil
}

// ResolveTag resolves only the asset URLs for an exact known tag. Used by the
// updater path, which already knows the tag passed by --to.
func ResolveTag(ctx context.Context, repo, tag string) (*ResolvedTarget, error) {
	return ResolveTarget(ctx, repo, "", tag)
}

// FirstSignedReleaseTag returns the first tag whose release carries a Sigstore
// signature, for CLI help text. It re-exports the provenance constant so the
// engine's flag definitions need not import that package directly.
func FirstSignedReleaseTag() string { return provenance.FirstSignedRelease }

// SignatureModeFor returns the provenance posture configured for a workspace
// ("enforce" | "warn" | "off").
//
// This is called by the DETACHED updater process, which receives only
// --to/--repo/--port/--workspace on its command line and so must re-read the
// posture itself. Failing to read or parse the config yields SignatureEnforce,
// the safe direction: an updater that cannot establish what it is allowed to do
// must not assume it is allowed to skip verification. An absent config file is
// not an error — it yields the shipped default, which is also enforce.
func SignatureModeFor(root string) string {
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil || cfg == nil || cfg.RequireSignature == "" {
		return SignatureEnforce
	}
	return cfg.RequireSignature
}

// VersionAfter reports whether cand is strictly newer than cur (exported wrapper).
func VersionAfter(cand, cur string) bool { return versionAfter(cand, cur) }

// VersionEqual reports whether a and b are the same version (exported wrapper).
func VersionEqual(a, b string) bool { return versionEqual(a, b) }

// NormVersion canonicalises a version string (exported wrapper). Returns "" for
// dev/unknown/invalid builds.
func NormVersion(v string) string { return normVersion(v) }
