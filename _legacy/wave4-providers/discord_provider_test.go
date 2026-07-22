// discord_provider_test.go
// Tests for the DiscordProvider Reconcilable adapter.

package main

import (
	"context"
	"testing"
)

func TestDiscordProviderType(t *testing.T) {
	p := &DiscordProvider{}
	if p.Type() != "discord" {
		t.Errorf("Type() = %q, want %q", p.Type(), "discord")
	}
}

func TestDiscordProviderRegistered(t *testing.T) {
	if !HasProvider("discord") {
		t.Fatal("discord provider not registered")
	}
	p, err := GetProvider("discord")
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
	if h.Health != HealthMissing {
		t.Errorf("Health = %s, want Missing with no token", h.Health)
	}
}

func TestDiscordProviderHealthWithToken(t *testing.T) {
	p := &DiscordProvider{Token: "fake"}
	h := p.Health()
	if h.Health != HealthHealthy {
		t.Errorf("Health = %s, want Healthy with token set", h.Health)
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
	if generic.Actions[0].Action != ActionCreate {
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
