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

// SignatureSettings is the provenance posture the detached updater runs under.
type SignatureSettings struct {
	// Mode is "enforce" | "warn" | "off".
	Mode string
	// IdentityRepo is the repository whose CI identity a signature must be
	// bound to. Always the compile-time default unless signature_repo is set.
	IdentityRepo string
	// Explicit reports whether require_signature was actually written in the
	// config, as opposed to being defaulted.
	Explicit bool
}

// SignatureSettingsFor returns the provenance posture configured for a
// workspace.
//
// This is called by the DETACHED updater process, which receives only
// --to/--repo/--port/--workspace on its command line and so must re-read the
// posture itself. Every uncertain path resolves to SignatureEnforce, the safe
// direction: an updater that cannot establish what it is allowed to do must not
// assume it is allowed to skip verification.
//
// An EMPTY root short-circuits before any filesystem access. It must: an empty
// root would otherwise make the config path relative and resolve it against the
// updater's working directory. runSelfUpdateCmd leaves root empty whenever
// LoadConfig finds no .cog/config in any ancestor, so `cogos self-update` run
// from an attacker-writable directory would read a planted config from it.
// Returning enforce here means the worst such a plant can do is refuse to
// update — never silently skip verification.
func SignatureSettingsFor(root string) SignatureSettings {
	safe := SignatureSettings{Mode: SignatureEnforce, IdentityRepo: defaultRepo}
	if root == "" {
		return safe
	}
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil || cfg == nil || cfg.RequireSignature == "" {
		return safe
	}
	return SignatureSettings{
		Mode:         cfg.RequireSignature,
		IdentityRepo: cfg.IdentityRepo(),
		Explicit:     !cfg.SignatureModeUnset(),
	}
}

// VersionAfter reports whether cand is strictly newer than cur (exported wrapper).
func VersionAfter(cand, cur string) bool { return versionAfter(cand, cur) }

// VersionEqual reports whether a and b are the same version (exported wrapper).
func VersionEqual(a, b string) bool { return versionEqual(a, b) }

// NormVersion canonicalises a version string (exported wrapper). Returns "" for
// dev/unknown/invalid builds.
func NormVersion(v string) string { return normVersion(v) }
