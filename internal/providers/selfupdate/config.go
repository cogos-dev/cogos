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

	root string `yaml:"-"` // workspace root this config was loaded from; never serialized
}

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
		Enabled:       false,
		Channel:       channelStable,
		AutoApply:     false,
		Repo:          defaultRepo,
		CheckInterval: defaultCheckInterval,
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
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("self-update: parsing %s: %w", path, err)
	}

	if cfg.Channel == "" {
		cfg.Channel = channelStable
	}
	if cfg.Repo == "" {
		cfg.Repo = defaultRepo
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
