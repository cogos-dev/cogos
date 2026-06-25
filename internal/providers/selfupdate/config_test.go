package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "self-update.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLoadConfigAbsentIsDisabledDefault(t *testing.T) {
	root := t.TempDir()
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("loadSelfUpdateConfig: %v", err)
	}
	if cfg.Enabled {
		t.Error("absent config should be disabled (opt-in)")
	}
	if cfg.Channel != channelStable {
		t.Errorf("default channel = %q; want stable", cfg.Channel)
	}
	if cfg.Repo != defaultRepo {
		t.Errorf("default repo = %q; want %q", cfg.Repo, defaultRepo)
	}
	if cfg.CheckInterval != defaultCheckInterval {
		t.Errorf("default check_interval = %v; want %v", cfg.CheckInterval, defaultCheckInterval)
	}
	if cfg.Root() != root {
		t.Errorf("Root() = %q; want %q", cfg.Root(), root)
	}
}

func TestLoadConfigValidRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `
enabled: true
channel: stable
pin: ""
auto_apply: true
repo: myrgic/cogos
check_interval: 30m
`)
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("loadSelfUpdateConfig: %v", err)
	}
	if !cfg.Enabled || !cfg.AutoApply {
		t.Error("enabled/auto_apply should be true")
	}
	if cfg.CheckInterval != 30*time.Minute {
		t.Errorf("check_interval = %v; want 30m", cfg.CheckInterval)
	}
}

func TestLoadConfigInvalidDuration(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "enabled: true\ncheck_interval: not-a-duration\n")
	if _, err := loadSelfUpdateConfig(root); err == nil {
		t.Fatal("expected error for invalid check_interval")
	}
}

func TestLoadConfigUnknownChannel(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "enabled: true\nchannel: bananas\n")
	if _, err := loadSelfUpdateConfig(root); err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

func TestLoadConfigPartialFillsDefaults(t *testing.T) {
	root := t.TempDir()
	// Only set enabled; everything else should default.
	writeConfig(t, root, "enabled: true\n")
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("loadSelfUpdateConfig: %v", err)
	}
	if cfg.Channel != channelStable {
		t.Errorf("channel = %q; want stable", cfg.Channel)
	}
	if cfg.Repo != defaultRepo {
		t.Errorf("repo = %q; want %q", cfg.Repo, defaultRepo)
	}
	if cfg.CheckInterval != defaultCheckInterval {
		t.Errorf("check_interval = %v; want %v", cfg.CheckInterval, defaultCheckInterval)
	}
}
