// discord_provider_test.go
// Tests for the DiscordProvider reconcile.Reconcilable adapter.

package discord

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// Test-only registration: production wiring happens in
// internal/providers/daemon's init() (daemon.go), which constructs a real
// *discord.DiscordProvider and registers it at the package's existing
// "discord" RegisterProvider call site. This package does not self-register
// in a production init() (that would double-register under
// reconcile.RegisterProvider's panic-on-duplicate guard once daemon.go also
// registers "discord"). Registering here, in a _test.go file, only affects
// this package's own test binary and preserves coverage that DiscordProvider
// actually satisfies reconcile.Reconcilable end to end through the registry.
func init() {
	reconcile.RegisterProvider("discord", &DiscordProvider{})
}

func TestDiscordProviderType(t *testing.T) {
	p := &DiscordProvider{}
	if p.Type() != "discord" {
		t.Errorf("Type() = %q, want %q", p.Type(), "discord")
	}
}

func TestDiscordProviderRegistered(t *testing.T) {
	if !reconcile.HasProvider("discord") {
		t.Fatal("discord provider not registered")
	}
	p, err := reconcile.GetProvider("discord")
	if err != nil {
		t.Fatalf("GetProvider(discord) failed: %v", err)
	}
	if p.Type() != "discord" {
		t.Errorf("registered provider Type() = %q, want %q", p.Type(), "discord")
	}
}

func TestDiscordProviderLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// LoadConfig with no config file should error
	_, err := (&DiscordProvider{}).LoadConfig(tmpDir)
	if err == nil {
		t.Error("expected error when no config exists")
	}
}

func TestDiscordProviderFetchLiveNoToken(t *testing.T) {
	p := &DiscordProvider{} // no token
	cfg := &DiscordServerConfig{Guild: GuildConfig{ID: "123"}}
	_, err := p.FetchLive(context.Background(), cfg)
	if err == nil {
		t.Error("expected error with no token")
	}
}

func TestDiscordProviderFetchLiveWrongType(t *testing.T) {
	p := &DiscordProvider{Token: "fake"}
	_, err := p.FetchLive(context.Background(), "not a config")
	if err == nil {
		t.Error("expected error for wrong config type")
	}
}

func TestDiscordProviderComputePlanWrongTypes(t *testing.T) {
	p := &DiscordProvider{}
	_, err := p.ComputePlan("bad", nil, nil)
	if err == nil {
		t.Error("expected error for wrong config type")
	}

	cfg := &DiscordServerConfig{}
	_, err = p.ComputePlan(cfg, "bad", nil)
	if err == nil {
		t.Error("expected error for wrong live type")
	}
}

func TestDiscordProviderHealthNoToken(t *testing.T) {
	p := &DiscordProvider{}
	h := p.Health()
	if h.Health != reconcile.HealthMissing {
		t.Errorf("Health = %s, want Missing with no token", h.Health)
	}
}

func TestDiscordProviderHealthWithToken(t *testing.T) {
	p := &DiscordProvider{Token: "fake"}
	h := p.Health()
	if h.Health != reconcile.HealthHealthy {
		t.Errorf("Health = %s, want Healthy with token set", h.Health)
	}
}

func TestDiscordProviderHealthWorkspaceRootFallback(t *testing.T) {
	root := t.TempDir()
	hclDir := filepath.Join(root, ".cog", "config", "discord")
	if err := os.MkdirAll(hclDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hclDir, "config.hcl"), []byte("guild { id = \"1\" }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p := &DiscordProvider{WorkspaceRoot: root} // no token, no env var
	h := p.Health()
	if h.Health != reconcile.HealthHealthy {
		t.Errorf("Health = %s, want Healthy via config.hcl presence fallback", h.Health)
	}
}

func TestDiscordPlanConversionRoundTrip(t *testing.T) {
	original := &Plan{
		GuildID:     "123",
		GuildName:   "Test",
		GeneratedAt: "2026-01-01T00:00:00Z",
		ConfigPath:  "/config/test",
		Actions: []PlanAction{
			{Action: "create", ResourceType: "channel", Name: "general", Details: map[string]any{"type": "text"}},
			{Action: "delete", ResourceType: "role", Name: "old-role"},
		},
		Summary:  PlanSummary{Creates: 1, Deletes: 1},
		Warnings: []string{"test warning"},
	}

	// Discord → Generic
	generic := discordPlanToReconcilePlan(original)
	if generic.ResourceType != "discord" {
		t.Errorf("ResourceType = %q, want discord", generic.ResourceType)
	}
	if len(generic.Actions) != 2 {
		t.Fatalf("Actions count = %d, want 2", len(generic.Actions))
	}
	if generic.Actions[0].Action != reconcile.ActionCreate {
		t.Errorf("Action[0] = %s, want create", generic.Actions[0].Action)
	}
	if generic.Metadata["guild_id"] != "123" {
		t.Error("guild_id not preserved in metadata")
	}

	// Generic → Discord
	roundTripped := reconcilePlanToDiscordPlan(generic)
	if roundTripped.GuildID != "123" {
		t.Errorf("GuildID = %q, want 123", roundTripped.GuildID)
	}
	if len(roundTripped.Actions) != 2 {
		t.Fatalf("Actions count = %d, want 2", len(roundTripped.Actions))
	}
	if roundTripped.Summary.Creates != 1 || roundTripped.Summary.Deletes != 1 {
		t.Errorf("Summary not preserved: %+v", roundTripped.Summary)
	}
}

func TestDiscordStateConversionRoundTrip(t *testing.T) {
	original := &DiscordState{
		Version:     1,
		Lineage:     "abc123",
		Serial:      5,
		GuildID:     "guild-999",
		GeneratedAt: "2026-01-01T00:00:00Z",
		Resources: []StateResource{
			{
				Address:   "role/Admin",
				Type:      "role",
				Mode:      "managed",
				DiscordID: "111",
				Name:      "Admin",
			},
			{
				Address:       "category/General/channel/chat",
				Type:          "channel",
				Mode:          "managed",
				DiscordID:     "222",
				Name:          "chat",
				ParentAddress: "category/General",
				ParentID:      "333",
			},
		},
	}

	// Discord → Generic
	generic := discordStateToReconcileState(original)
	if generic.ResourceType != "discord" {
		t.Errorf("ResourceType = %q, want discord", generic.ResourceType)
	}
	if generic.Lineage != "abc123" {
		t.Errorf("Lineage = %q, want abc123", generic.Lineage)
	}
	if len(generic.Resources) != 2 {
		t.Fatalf("Resources count = %d, want 2", len(generic.Resources))
	}
	if generic.Resources[0].ExternalID != "111" {
		t.Error("DiscordID → ExternalID not mapped")
	}
	if generic.Metadata["guild_id"] != "guild-999" {
		t.Error("guild_id not in metadata")
	}

	// Generic → Discord
	roundTripped := reconcileStateToDiscordState(generic)
	if roundTripped.GuildID != "guild-999" {
		t.Errorf("GuildID = %q, want guild-999", roundTripped.GuildID)
	}
	if roundTripped.Resources[0].DiscordID != "111" {
		t.Error("ExternalID → DiscordID not mapped back")
	}
	if roundTripped.Resources[1].ParentAddress != "category/General" {
		t.Error("ParentAddress not preserved")
	}
}

func TestDiscordExportConfig(t *testing.T) {
	catID := "cat-100"
	live := &DiscordLiveState{
		Roles: []DiscordRole{
			// @everyone (skipped: id == guildID)
			{ID: "guild-1", Name: "@everyone", Position: 0},
			// discord-managed bot role (skipped)
			{ID: "bot-9", Name: "SomeBot", Managed: true, Position: 4},
			// managed role: VIEW_CHANNEL(1<<10) | SEND_MESSAGES(1<<11)
			{ID: "role-2", Name: "Friend", Color: 3447003, Permissions: "3072", Hoist: true, Mentionable: true, Position: 2},
		},
		Channels: []DiscordChannel{
			// category (type 4)
			{ID: catID, Type: 4, Name: "claw-colony", Position: 0,
				PermissionOverwrites: []DiscordPermOverwrite{
					// role overwrite: allow VIEW_CHANNEL
					{ID: "role-2", Type: 0, Allow: "1024", Deny: "0"},
				}},
			// text channel under category
			{ID: "ch-1", Type: 0, Name: "whirl", Topic: "Main channel", Position: 1,
				RateLimitPerUser: 5, NSFW: true, ParentID: &catID},
			// voice channel under category
			{ID: "ch-2", Type: 2, Name: "voice-lab", Position: 0, ParentID: &catID},
			// uncategorized channel (skipped)
			{ID: "ch-3", Type: 0, Name: "orphan", Position: 0, ParentID: nil},
		},
	}

	cfg := buildDiscordConfigFromLive(live, "guild-1", "Test Guild", "a description")

	// Guild mapping
	if cfg.Version != "1.0" {
		t.Errorf("version = %q, want 1.0", cfg.Version)
	}
	if cfg.Guild.ID != "guild-1" || cfg.Guild.Name != "Test Guild" {
		t.Errorf("guild id/name mismatch: %+v", cfg.Guild)
	}
	if cfg.Guild.ManagedBy != "cog" {
		t.Errorf("guild managed_by = %q, want cog", cfg.Guild.ManagedBy)
	}

	// Roles: only Friend survives (@everyone + managed bot dropped)
	if len(cfg.Guild.Roles) != 1 {
		t.Fatalf("roles = %d, want 1: %+v", len(cfg.Guild.Roles), cfg.Guild.Roles)
	}
	r := cfg.Guild.Roles[0]
	if r.Name != "Friend" || r.ManagedBy != "cog" || !r.Hoist || !r.Mentionable {
		t.Errorf("role mapping wrong: %+v", r)
	}
	if r.Color != "3498db" {
		t.Errorf("role color = %q, want 3498db", r.Color)
	}
	if len(r.Permissions) != 2 {
		t.Errorf("role perms = %v, want [VIEW_CHANNEL SEND_MESSAGES]", r.Permissions)
	}

	// Categories
	if len(cfg.Guild.Categories) != 1 {
		t.Fatalf("categories = %d, want 1", len(cfg.Guild.Categories))
	}
	cat := cfg.Guild.Categories[0]
	if cat.Name != "claw-colony" || cat.ManagedBy != "cog" {
		t.Errorf("category mapping wrong: %+v", cat)
	}
	if len(cat.PermissionOverwrites) != 1 || cat.PermissionOverwrites[0].Target != "Friend" {
		t.Errorf("category perm overwrite target not resolved to role name: %+v", cat.PermissionOverwrites)
	}

	// Channels: 2 under category (orphan excluded), sorted by position → voice-lab(0), whirl(1)
	if len(cat.Channels) != 2 {
		t.Fatalf("channels = %d, want 2: %+v", len(cat.Channels), cat.Channels)
	}
	if cat.Channels[0].Name != "voice-lab" || cat.Channels[0].Type != "voice" {
		t.Errorf("channel[0] mapping wrong: %+v", cat.Channels[0])
	}
	whirl := cat.Channels[1]
	if whirl.Name != "whirl" || whirl.Type != "text" || whirl.Topic != "Main channel" {
		t.Errorf("channel whirl mapping wrong: %+v", whirl)
	}
	if whirl.Slowmode != 5 || !whirl.NSFW || whirl.ManagedBy != "cog" {
		t.Errorf("channel whirl attrs wrong: %+v", whirl)
	}

	// Write + header + round-trip
	root := t.TempDir()
	if err := writeDiscordServerYAML(root, cfg); err != nil {
		t.Fatalf("writeDiscordServerYAML: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".cog", "config", "discord", "server.yaml"))
	if err != nil {
		t.Fatalf("read written yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# Generated by: cog snapshot discord") {
		t.Error("missing header line")
	}
	if !strings.Contains(content, "managed_by: cog") {
		t.Error("missing managed_by: cog")
	}
	if !strings.Contains(content, "name: whirl") {
		t.Error("missing channel in output")
	}
	if !strings.Contains(content, "cogos reconcile discord --snapshot") {
		t.Error("missing current cogos reconcile usage syntax in header")
	}

	// Round-trips back into a DiscordServerConfig
	var back DiscordServerConfig
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.Guild.ID != "guild-1" || len(back.Guild.Categories) != 1 {
		t.Errorf("round-trip mismatch: %+v", back.Guild)
	}
}
