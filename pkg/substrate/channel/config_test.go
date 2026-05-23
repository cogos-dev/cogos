package channel_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/channel"
)

// TestLoadBridgeConfig_basic verifies the YAML loader parses a minimal
// channels.yaml from a workspace root.
func TestLoadBridgeConfig_basic(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `channels:
  colony-chat:
    discordId: "1234567890"
    agentId: "whirl"
  ops:
    discordId: "0987654321"
    agentId: "scout"
`
	if err := os.WriteFile(filepath.Join(root, ".cog", "config", "channels.yaml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := channel.LoadBridgeConfig(root)
	if err != nil {
		t.Fatalf("LoadBridgeConfig: %v", err)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(cfg.Channels))
	}

	entry, err := cfg.Lookup("colony-chat")
	if err != nil {
		t.Fatalf("Lookup colony-chat: %v", err)
	}
	if entry.DiscordID != "1234567890" || entry.AgentID != "whirl" {
		t.Errorf("colony-chat entry wrong: %+v", entry)
	}
}

// TestLoadBridgeConfig_envOverride verifies CHANNEL_*_DISCORD_ID env vars
// override per-channel DiscordID values.
func TestLoadBridgeConfig_envOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `channels:
  colony-chat:
    discordId: "original-id"
    agentId: "whirl"
`
	if err := os.WriteFile(filepath.Join(root, ".cog", "config", "channels.yaml"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CHANNEL_COLONY_CHAT_DISCORD_ID", "env-override-id")

	cfg, err := channel.LoadBridgeConfig(root)
	if err != nil {
		t.Fatalf("LoadBridgeConfig: %v", err)
	}
	entry, err := cfg.Lookup("colony-chat")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry.DiscordID != "env-override-id" {
		t.Errorf("expected env override, got DiscordID=%q", entry.DiscordID)
	}
}

// TestLookup_unknown verifies error messages on missing channels.
func TestLookup_unknown(t *testing.T) {
	cfg := &channel.BridgeConfig{Channels: map[string]channel.BridgeEntry{}}
	_, err := cfg.Lookup("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

// TestLookup_emptyDiscordID verifies error when a channel has empty DiscordID.
func TestLookup_emptyDiscordID(t *testing.T) {
	cfg := &channel.BridgeConfig{
		Channels: map[string]channel.BridgeEntry{
			"x": {DiscordID: "", AgentID: "a"},
		},
	}
	_, err := cfg.Lookup("x")
	if err == nil {
		t.Fatal("expected error for empty DiscordID")
	}
}
