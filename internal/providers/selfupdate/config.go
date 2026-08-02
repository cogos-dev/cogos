// config.go — self-update configuration schema and loader.
//
// The shipped default is DISABLED: an absent config file yields a config with
// Enabled=false, so a stock daemon performs no GitHub traffic and never swaps
// its binary. Opt-in is the presence of <root>/.cog/config/self-update.yaml
// with `enabled: true`.
package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultRepo is the canonical release repository.
const defaultRepo = "myrgic/cogos"

// defaultCheckInterval is the throttle floor for GitHub release queries.
const defaultCheckInterval = time.Hour

// SelfUpdateConfig is the on-disk schema for self-update behaviour.
//
// CheckInterval is parsed from CheckIntervalStr by parse() because gopkg.in/yaml.v3
// does not unmarshal a time.Duration from a string like "1h".
type SelfUpdateConfig struct {
	Enabled          bool          `yaml:"enabled"`        // SHIPPED DEFAULT false (opt-in)
	Channel          string        `yaml:"channel"`        // "stable" | "prerelease"; default "stable"
	Pin              string        `yaml:"pin"`            // exact tag override (e.g. "v0.16.3"); empty = follow channel
	AutoApply        bool          `yaml:"auto_apply"`     // default false
	Repo             string        `yaml:"repo"`           // default "myrgic/cogos"
	CheckIntervalStr string        `yaml:"check_interval"` // raw "1h"/"30m"; parsed into CheckInterval
	CheckInterval    time.Duration `yaml:"-"`              // never serialized; derived from CheckIntervalStr

	// RequireSignature selects the provenance posture: "enforce" | "warn" | "off".
	// See the SignaturePolicy constants for semantics and the migration rationale.
	RequireSignature string `yaml:"require_signature"`

	// signatureKeyPresent records whether require_signature appeared in the file
	// at all, which is what distinguishes "operator chose warn" from "pre-existing
	// config written before this key existed". Never serialized.
	signatureKeyPresent bool `yaml:"-"`

	root string `yaml:"-"` // workspace root this config was loaded from; never serialized
}

// SignaturePolicy values for require_signature.
//
// MIGRATION — why the absent-key default is warn, not enforce.
//
// The task of this setting is to fail closed: an update whose provenance cannot
// be proven must not be applied. But flipping straight to enforce on upgrade is
// a live risk to a node running with auto_apply:true, because several
// legitimate conditions produce an unverifiable-but-honest update:
//
//   - Releases published before provenance.FirstSignedRelease carry no
//     signature at all. A node pinned to such a tag would stop updating.
//   - A pipeline hiccup that drops the signing step would silently freeze the
//     whole fleet's update path with no prior signal.
//   - A missed Sigstore root rotation would do the same (see roots.go).
//
// None of those is an attack, and none should be discovered by an operator
// noticing months later that their node never updated. So the rollout is
// staged, and the stage is inferred from the config file rather than announced:
//
//	Stage 1 (this change) — key ABSENT in an existing config → SignatureWarn.
//	  Verification runs on every update and the result is logged loudly, but a
//	  failure does not block the swap. This buys real telemetry from the live
//	  fleet at zero brick risk, and every cycle emits the deprecation notice
//	  below so the flip cannot arrive unannounced.
//	Stage 2 (next minor) — absent key flips to SignatureEnforce by changing
//	  defaultRequireSignature to SignatureEnforce, a one-line, one-test change.
//	  By then Stage 1's warnings have surfaced any release that would break.
//
// An operator who wants the end state today simply writes
// `require_signature: enforce`, which is the documented recommendation. An
// explicit `warn` or `off` is honoured and is NOT treated as unset, so a
// deliberate choice never silently changes under the operator.
const (
	// SignatureEnforce fails closed: an update whose signature is absent,
	// unverifiable, or bound to the wrong identity is refused and the running
	// binary is left untouched.
	SignatureEnforce = "enforce"

	// SignatureWarn verifies and logs but does not block. Transitional.
	SignatureWarn = "warn"

	// SignatureOff skips verification entirely. Escape hatch for a wedged
	// channel (missed root rotation, broken pipeline). Logs on every cycle so
	// it cannot be set once and forgotten.
	SignatureOff = "off"
)

// defaultRequireSignature is the posture applied when require_signature is
// absent from an existing config file. Stage 2 of the migration flips this
// single constant to SignatureEnforce.
const defaultRequireSignature = SignatureWarn

// SignatureModeUnset reports whether the operator has expressed a choice. The
// provider uses this to emit the one-time-per-cycle migration notice.
func (c *SelfUpdateConfig) SignatureModeUnset() bool { return !c.signatureKeyPresent }

// Root returns the workspace root this config was loaded from (empty when the
// config was constructed in-memory rather than via loadSelfUpdateConfig).
func (c *SelfUpdateConfig) Root() string { return c.root }

// Channel values.
const (
	channelStable     = "stable"
	channelPrerelease = "prerelease"
)

// defaultConfig returns the safe shipped default: DISABLED, stable channel,
// no auto-apply, 1h check interval. Used when no config file is present.
func defaultConfig() *SelfUpdateConfig {
	return &SelfUpdateConfig{
		Enabled:   false,
		Channel:   channelStable,
		AutoApply: false,
		Repo:      defaultRepo,
		// A config created fresh (no file on disk) has no legacy to protect, so
		// it gets the end-state posture immediately. Only an EXISTING file that
		// predates the key is granted the transitional warn default, in
		// loadSelfUpdateConfig below.
		RequireSignature: SignatureEnforce,
		CheckInterval:    defaultCheckInterval,
	}
}

// selfUpdateConfigPath returns the absolute config path for a workspace root.
func selfUpdateConfigPath(root string) string {
	return filepath.Join(root, ".cog", "config", "self-update.yaml")
}

// loadSelfUpdateConfig reads <root>/.cog/config/self-update.yaml.
//
//   - file absent (ENOENT)  → defaultConfig(), nil  (SAFE: disabled, no traffic)
//   - read / parse error    → nil, err              (surfaces as a reconcile WARN)
//   - success               → parsed config with defaults filled and validated
func loadSelfUpdateConfig(root string) (*SelfUpdateConfig, error) {
	path := selfUpdateConfigPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			dc := defaultConfig()
			dc.root = root
			return dc, nil
		}
		return nil, fmt.Errorf("self-update: reading %s: %w", path, err)
	}

	cfg := defaultConfig()
	cfg.root = root
	// Reset duration so an explicit (or missing) check_interval is honoured by parse().
	cfg.CheckInterval = 0
	// Reset the posture so an ABSENT require_signature key is distinguishable
	// from an explicit one after unmarshalling (see SignaturePolicy migration).
	cfg.RequireSignature = ""
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("self-update: parsing %s: %w", path, err)
	}

	if cfg.Channel == "" {
		cfg.Channel = channelStable
	}
	if cfg.Repo == "" {
		cfg.Repo = defaultRepo
	}
	// Provenance posture: an existing file that predates the key gets the
	// transitional default; an explicit value is always honoured verbatim.
	cfg.signatureKeyPresent = cfg.RequireSignature != ""
	if !cfg.signatureKeyPresent {
		cfg.RequireSignature = defaultRequireSignature
	}
	switch cfg.RequireSignature {
	case SignatureEnforce, SignatureWarn, SignatureOff:
	default:
		return nil, fmt.Errorf("self-update: %s: unknown require_signature %q (want enforce|warn|off)",
			path, cfg.RequireSignature)
	}
	if err := cfg.parse(); err != nil {
		return nil, fmt.Errorf("self-update: %s: %w", path, err)
	}
	if cfg.Channel != channelStable && cfg.Channel != channelPrerelease {
		return nil, fmt.Errorf("self-update: %s: unknown channel %q (want stable|prerelease)", path, cfg.Channel)
	}
	return cfg, nil
}

// parse derives CheckInterval from CheckIntervalStr. An empty string keeps the
// default; a malformed duration is a hard config error.
func (c *SelfUpdateConfig) parse() error {
	if c.CheckIntervalStr == "" {
		if c.CheckInterval == 0 {
			c.CheckInterval = defaultCheckInterval
		}
		return nil
	}
	d, err := time.ParseDuration(c.CheckIntervalStr)
	if err != nil {
		return fmt.Errorf("invalid check_interval %q: %w", c.CheckIntervalStr, err)
	}
	if d <= 0 {
		return fmt.Errorf("invalid check_interval %q: must be positive", c.CheckIntervalStr)
	}
	c.CheckInterval = d
	return nil
}
