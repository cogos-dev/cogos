package vitalsretention

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// configFileName is the declared-config location, following the existing
// .cog/config/<name>.yaml convention (see .cog/config/observatory.yaml for
// the Conversations Observatory's sibling file — a different subsystem
// despite the similar name).
const configFileName = "vitals-retention.yaml"

// Defaults implement RFC-040 N3's provisional budget/retention numbers,
// "testable defaults, revised by OQ-3's measurement, not vibes."
const (
	defaultRawRetentionHours    = 48
	defaultMidResRetentionDays  = 30
	defaultRawBudgetMB          = 100
	defaultPruneAfterDays       = 0 // 0 = never prune the 1h tier
	defaultCompactCheckInterval = 5 * time.Minute
)

// Config is the optional declared shape of .cog/config/vitals-retention.yaml.
// Every field is optional; a missing file or missing field falls back to the
// RFC-040 N3 provisional defaults above.
type Config struct {
	// BaseDir overrides the on-disk root (default
	// "<workspace>/.cog/observatory/vitals"). Primarily a test seam.
	BaseDir string `yaml:"base_dir,omitempty"`

	// RawRetentionHours: raw-tick data older than this is downsampled to 5m
	// and the raw day-file is pruned. Default 48 (RFC-040 §S2).
	RawRetentionHours int `yaml:"raw_retention_hours,omitempty"`

	// MidResRetentionDays: 5m data older than this is downsampled to 1h and
	// the 5m day-file is pruned. Default 30 (RFC-040 §S2).
	MidResRetentionDays int `yaml:"midres_retention_days,omitempty"`

	// RawBudgetMB is the N3 provisional per-node raw-tier size budget. When
	// exceeded, compaction engages early on the oldest raw day-files
	// regardless of RawRetentionHours. Default 100 (RFC-040 N3).
	RawBudgetMB int `yaml:"raw_budget_mb,omitempty"`

	// PruneAfterDays, if > 0, deletes 1h-tier data older than this many days
	// during compaction ("prunes per config" — RFC-040 §S2). Default 0
	// (never prune the coarsest tier).
	PruneAfterDays int `yaml:"prune_after_days,omitempty"`

	// CompactCheckIntervalSeconds throttles how often the bus-handler
	// dispatch (which fires once per autonomic tick, typically every 60s)
	// actually runs a compaction pass, so compaction cost is bounded
	// regardless of tick frequency. Default 300 (5m).
	CompactCheckIntervalSeconds int `yaml:"compact_check_interval_seconds,omitempty"`
}

func (c Config) rawRetentionHours() time.Duration {
	h := c.RawRetentionHours
	if h <= 0 {
		h = defaultRawRetentionHours
	}
	return time.Duration(h) * time.Hour
}

func (c Config) midResRetentionDays() int {
	if c.MidResRetentionDays <= 0 {
		return defaultMidResRetentionDays
	}
	return c.MidResRetentionDays
}

func (c Config) rawBudgetBytes() int64 {
	mb := c.RawBudgetMB
	if mb <= 0 {
		mb = defaultRawBudgetMB
	}
	return int64(mb) * 1024 * 1024
}

func (c Config) compactCheckInterval() time.Duration {
	if c.CompactCheckIntervalSeconds <= 0 {
		return defaultCompactCheckInterval
	}
	return time.Duration(c.CompactCheckIntervalSeconds) * time.Second
}

// LoadConfig reads .cog/config/vitals-retention.yaml under root. A missing
// file is not an error — it returns the zero-value Config, whose accessor
// methods above apply the N3 provisional defaults.
func LoadConfig(root string) (Config, error) {
	path := filepath.Join(root, ".cog", "config", configFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// --- process-lifetime cache -------------------------------------------
//
// Config is read from disk at most once per (root) per process unless
// invalidated by ReloadConfig. The recorder fires on every autonomic tick
// (default 60s); re-reading and re-parsing a YAML file on that cadence for a
// file that changes essentially never is unnecessary I/O. Tests that swap
// the config file mid-run should call ReloadConfig(root) explicitly.

var (
	configCacheMu sync.Mutex
	configCache   = map[string]Config{}
)

func loadConfigCached(root string) Config {
	configCacheMu.Lock()
	defer configCacheMu.Unlock()
	if cfg, ok := configCache[root]; ok {
		return cfg
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		slog.Warn("vitals-retention: config load failed, using defaults", "root", root, "err", err)
		cfg = Config{}
	}
	configCache[root] = cfg
	return cfg
}

// ReloadConfig invalidates the cached config for root, forcing the next
// loadConfigCached call to re-read the file. Exported for tests.
func ReloadConfig(root string) {
	configCacheMu.Lock()
	defer configCacheMu.Unlock()
	delete(configCache, root)
}
