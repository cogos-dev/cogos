package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTestHCL(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.hcl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHCLParseMinimal(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "123456"
  name = "Test Server"
}

reconciler {
  prune_unmanaged = true
  max_api_calls   = 30
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Guild.ID != "123456" {
		t.Errorf("guild ID = %q, want %q", cfg.Guild.ID, "123456")
	}
	if cfg.Guild.Name != "Test Server" {
		t.Errorf("guild name = %q, want %q", cfg.Guild.Name, "Test Server")
	}
	if cfg.Guild.ManagedBy != "cog" {
		t.Errorf("managed_by = %q, want %q", cfg.Guild.ManagedBy, "cog")
	}
	if cfg.Reconciler.MaxAPICalls != 30 {
		t.Errorf("max_api_calls = %d, want 30", cfg.Reconciler.MaxAPICalls)
	}
	if !cfg.Reconciler.PruneUnmanaged {
		t.Error("prune_unmanaged = false, want true")
	}
}

func TestHCLParseVariables(t *testing.T) {
	path := writeTestHCL(t, `
variable "guild_id" {
  default = "999888"
}

guild {
  id   = var.guild_id
  name = "Var Test"
}

reconciler {}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Guild.ID != "999888" {
		t.Errorf("guild ID = %q, want %q (from variable)", cfg.Guild.ID, "999888")
	}
}

func TestHCLParseLocals(t *testing.T) {
	path := writeTestHCL(t, `
locals {
  admin_deny = ["VIEW_CHANNEL", "CONNECT"]
}

guild {
  id   = "111"
  name = "Locals Test"
}

reconciler {}

category "Admin" {
  permission {
    role = "@everyone"
    deny = local.admin_deny
  }
  channel "logs" {}
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Guild.Categories) != 1 {
		t.Fatalf("got %d categories, want 1", len(cfg.Guild.Categories))
	}
	cat := cfg.Guild.Categories[0]
	if len(cat.PermissionOverwrites) != 1 {
		t.Fatalf("got %d permission overwrites, want 1", len(cat.PermissionOverwrites))
	}
	deny := cat.PermissionOverwrites[0].Deny
	if len(deny) != 2 || deny[0] != "VIEW_CHANNEL" || deny[1] != "CONNECT" {
		t.Errorf("deny = %v, want [VIEW_CHANNEL CONNECT]", deny)
	}
}

func TestHCLDefaults(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "222"
  name = "Defaults Test"
}

reconciler {}

role "Mod" {
  color = "ff0000"
}

category "General" {
  channel "chat" {}
  channel "voice-room" { type = "voice" }
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	// Role defaults
	if len(cfg.Guild.Roles) != 1 {
		t.Fatalf("got %d roles, want 1", len(cfg.Guild.Roles))
	}
	role := cfg.Guild.Roles[0]
	if role.ManagedBy != "cog" {
		t.Errorf("role managed_by = %q, want %q", role.ManagedBy, "cog")
	}

	// Channel defaults
	cat := cfg.Guild.Categories[0]
	if len(cat.Channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(cat.Channels))
	}
	ch := cat.Channels[0]
	if ch.Type != "text" {
		t.Errorf("channel type = %q, want %q (default)", ch.Type, "text")
	}
	if ch.ManagedBy != "cog" {
		t.Errorf("channel managed_by = %q, want %q", ch.ManagedBy, "cog")
	}
	if ch.Slowmode != 0 {
		t.Errorf("channel slowmode = %d, want 0", ch.Slowmode)
	}

	// Voice channel
	voice := cat.Channels[1]
	if voice.Type != "voice" {
		t.Errorf("voice channel type = %q, want %q", voice.Type, "voice")
	}

	// Position from declaration order
	if cat.Channels[0].Position != 0 {
		t.Errorf("ch[0] position = %d, want 0", cat.Channels[0].Position)
	}
	if cat.Channels[1].Position != 1 {
		t.Errorf("ch[1] position = %d, want 1", cat.Channels[1].Position)
	}
}

func TestHCLPermissionInheritance(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "333"
  name = "Perm Test"
}

reconciler {}

category "Admin" {
  permission {
    role = "@everyone"
    deny = ["VIEW_CHANNEL", "CONNECT"]
  }
  channel "logs" {}
  channel "notes" {
    permission {
      role = "@everyone"
      deny = ["VIEW_CHANNEL"]
    }
  }
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	cat := cfg.Guild.Categories[0]

	// logs should inherit category permissions
	logs := cat.Channels[0]
	if len(logs.PermissionOverwrites) != 1 {
		t.Fatalf("logs got %d perms, want 1 (inherited)", len(logs.PermissionOverwrites))
	}
	if logs.PermissionOverwrites[0].Deny[0] != "VIEW_CHANNEL" {
		t.Errorf("logs deny[0] = %q, want VIEW_CHANNEL", logs.PermissionOverwrites[0].Deny[0])
	}
	if logs.PermissionOverwrites[0].Deny[1] != "CONNECT" {
		t.Errorf("logs deny[1] = %q, want CONNECT", logs.PermissionOverwrites[0].Deny[1])
	}

	// notes should have its own permissions (not inherited)
	notes := cat.Channels[1]
	if len(notes.PermissionOverwrites) != 1 {
		t.Fatalf("notes got %d perms, want 1 (explicit)", len(notes.PermissionOverwrites))
	}
	if len(notes.PermissionOverwrites[0].Deny) != 1 || notes.PermissionOverwrites[0].Deny[0] != "VIEW_CHANNEL" {
		t.Errorf("notes deny = %v, want [VIEW_CHANNEL]", notes.PermissionOverwrites[0].Deny)
	}
}

func TestHCLChannelsFrom(t *testing.T) {
	path := writeTestHCL(t, `
locals {
  nsfw_names = ["general", "gifs", "random"]
}

guild {
  id   = "444"
  name = "ChannelsFrom Test"
}

reconciler {}

category "NSFW" {
  permission {
    role = "NSFW"
    allow = ["VIEW_CHANNEL"]
  }

  channels_from = [
    for name in local.nsfw_names :
    { name = "nsfw-${name}" }
  ]
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	cat := cfg.Guild.Categories[0]
	if len(cat.Channels) != 3 {
		t.Fatalf("got %d channels, want 3", len(cat.Channels))
	}

	expectedNames := []string{"nsfw-general", "nsfw-gifs", "nsfw-random"}
	for i, name := range expectedNames {
		if cat.Channels[i].Name != name {
			t.Errorf("channel[%d] = %q, want %q", i, cat.Channels[i].Name, name)
		}
		// Should inherit category permissions
		if len(cat.Channels[i].PermissionOverwrites) != 1 {
			t.Errorf("channel[%d] has %d perms, want 1 (inherited)", i, len(cat.Channels[i].PermissionOverwrites))
		}
	}
}

func TestHCLMixedInlineAndChannelsFrom(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "555"
  name = "Mixed Test"
}

reconciler {}

category "Mixed" {
  channel "static-channel" {}

  channels_from = [
    { name = "dynamic-1" },
    { name = "dynamic-2" },
  ]
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	cat := cfg.Guild.Categories[0]
	if len(cat.Channels) != 3 {
		t.Fatalf("got %d channels, want 3", len(cat.Channels))
	}
	if cat.Channels[0].Name != "static-channel" {
		t.Errorf("ch[0] = %q, want static-channel", cat.Channels[0].Name)
	}
	if cat.Channels[1].Name != "dynamic-1" {
		t.Errorf("ch[1] = %q, want dynamic-1", cat.Channels[1].Name)
	}
	if cat.Channels[2].Name != "dynamic-2" {
		t.Errorf("ch[2] = %q, want dynamic-2", cat.Channels[2].Name)
	}
}

func TestHCLCategoryPositions(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "666"
  name = "Position Test"
}

reconciler {}

category "First" {
  channel "a" {}
}

category "Second" {
  channel "b" {}
}

category "Third" {
  channel "c" {}
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	for i, cat := range cfg.Guild.Categories {
		if cat.Position != i {
			t.Errorf("category %q position = %d, want %d", cat.Name, cat.Position, i)
		}
	}
}

func TestHCLReconcilerDefaults(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "777"
  name = "ReconcilerDefaults"
}

reconciler {}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Reconciler.MaxAPICalls != 60 {
		t.Errorf("max_api_calls = %d, want 60 (default)", cfg.Reconciler.MaxAPICalls)
	}
	if cfg.Reconciler.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q (default)", cfg.Reconciler.LogLevel, "info")
	}
}

func TestHCLMigrationRoundTrip(t *testing.T) {
	// Create a YAML config, convert to HCL, parse back, compare
	yamlContent := `version: "1.0"
guild:
  id: "123"
  name: Test Server
  description: ""
  managed_by: cog
  roles:
    - name: Admin
      color: ff0000
      permissions:
        - ADMINISTRATOR
      hoist: false
      mentionable: false
      position: 1
      managed_by: cog
  categories:
    - name: General
      position: 0
      managed_by: cog
      permission_overwrites: []
      channels:
        - name: chat
          type: text
          topic: ""
          position: 0
          slowmode: 0
          nsfw: false
          managed_by: cog
          permission_overwrites: []
        - name: voice
          type: voice
          topic: ""
          position: 1
          slowmode: 0
          nsfw: false
          managed_by: cog
          permission_overwrites: []
    - name: Admin
      position: 1
      managed_by: cog
      permission_overwrites:
        - target_type: role
          target: "@everyone"
          allow: []
          deny:
            - VIEW_CHANNEL
      channels:
        - name: logs
          type: text
          topic: ""
          position: 0
          slowmode: 0
          nsfw: false
          managed_by: cog
          permission_overwrites:
            - target_type: role
              target: "@everyone"
              allow: []
              deny:
                - VIEW_CHANNEL
reconciler:
  dry_run: false
  prune_unmanaged: true
  respect_user_managed: true
  max_api_calls: 60
  log_level: info
`

	// Write YAML, load it, migrate to HCL, parse HCL back
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate loadDiscordServerConfig with YAML
	origCfg, _, err := loadDiscordServerConfigFromYAML(yamlPath)
	if err != nil {
		t.Fatal(err)
	}

	// Convert to HCL
	hclContent := discordConfigToHCL(origCfg)

	// Write HCL and parse it back
	hclPath := filepath.Join(dir, "server.hcl")
	if err := os.WriteFile(hclPath, []byte(hclContent), 0644); err != nil {
		t.Fatal(err)
	}

	roundTripCfg, err := parseHCLConfig(hclPath)
	if err != nil {
		t.Fatalf("parsing generated HCL: %v\n\nHCL content:\n%s", err, hclContent)
	}

	// Compare key fields
	if origCfg.Guild.ID != roundTripCfg.Guild.ID {
		t.Errorf("guild ID: %q vs %q", origCfg.Guild.ID, roundTripCfg.Guild.ID)
	}
	if origCfg.Guild.Name != roundTripCfg.Guild.Name {
		t.Errorf("guild name: %q vs %q", origCfg.Guild.Name, roundTripCfg.Guild.Name)
	}
	if len(origCfg.Guild.Categories) != len(roundTripCfg.Guild.Categories) {
		t.Fatalf("categories: %d vs %d", len(origCfg.Guild.Categories), len(roundTripCfg.Guild.Categories))
	}
	for i, origCat := range origCfg.Guild.Categories {
		rtCat := roundTripCfg.Guild.Categories[i]
		if origCat.Name != rtCat.Name {
			t.Errorf("cat[%d] name: %q vs %q", i, origCat.Name, rtCat.Name)
		}
		if len(origCat.Channels) != len(rtCat.Channels) {
			t.Errorf("cat[%d] channels: %d vs %d", i, len(origCat.Channels), len(rtCat.Channels))
			continue
		}
		for j, origCh := range origCat.Channels {
			rtCh := rtCat.Channels[j]
			if origCh.Name != rtCh.Name {
				t.Errorf("cat[%d]/ch[%d] name: %q vs %q", i, j, origCh.Name, rtCh.Name)
			}
			if origCh.Type != rtCh.Type {
				t.Errorf("cat[%d]/ch[%d] type: %q vs %q", i, j, origCh.Type, rtCh.Type)
			}
		}
	}

	// Check that logs inherits Admin category permissions
	adminCat := roundTripCfg.Guild.Categories[1]
	logsCh := adminCat.Channels[0]
	if len(logsCh.PermissionOverwrites) != len(adminCat.PermissionOverwrites) {
		t.Errorf("logs perms (%d) should match admin cat perms (%d) via inheritance",
			len(logsCh.PermissionOverwrites), len(adminCat.PermissionOverwrites))
	}
}

// loadDiscordServerConfigFromYAML is a test helper that loads from a specific YAML path
func loadDiscordServerConfigFromYAML(path string) (*DiscordServerConfig, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	var cfg DiscordServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, path, err
	}
	if cfg.Reconciler.MaxAPICalls == 0 {
		cfg.Reconciler.MaxAPICalls = 60
	}
	if cfg.Reconciler.LogLevel == "" {
		cfg.Reconciler.LogLevel = "info"
	}
	return &cfg, path, nil
}

func TestHCLEmptyPermissions(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "888"
  name = "EmptyPerms"
}

reconciler {}

category "Open" {
  channel "general" {}
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	cat := cfg.Guild.Categories[0]
	if len(cat.PermissionOverwrites) != 0 {
		t.Errorf("category perms = %d, want 0", len(cat.PermissionOverwrites))
	}
	// Channel with no category perms and no explicit perms → empty slice
	ch := cat.Channels[0]
	if len(ch.PermissionOverwrites) != 0 {
		t.Errorf("channel perms = %d, want 0 (empty category → empty channel)", len(ch.PermissionOverwrites))
	}
}

func TestHCLChannelTopic(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "999"
  name = "TopicTest"
}

reconciler {}

category "Info" {
  channel "faq" {
    topic = "Frequently asked questions"
  }
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	ch := cfg.Guild.Categories[0].Channels[0]
	if ch.Topic != "Frequently asked questions" {
		t.Errorf("topic = %q, want %q", ch.Topic, "Frequently asked questions")
	}
}

func TestHCLRolePermissions(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "1010"
  name = "RolePermsTest"
}

reconciler {}

role "Owner" {
  color = "f1c40f"
  permissions = ["ADMINISTRATOR", "MANAGE_GUILD"]
  position = 4
}

role "Member" {
  permissions = []
  position = 1
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Guild.Roles) != 2 {
		t.Fatalf("got %d roles, want 2", len(cfg.Guild.Roles))
	}

	owner := cfg.Guild.Roles[0]
	if owner.Position != 4 {
		t.Errorf("owner position = %d, want 4", owner.Position)
	}
	if len(owner.Permissions) != 2 {
		t.Errorf("owner perms = %d, want 2", len(owner.Permissions))
	}

	member := cfg.Guild.Roles[1]
	if len(member.Permissions) != 0 {
		t.Errorf("member perms = %d, want 0", len(member.Permissions))
	}
}

func TestHCLMultiplePermissionsOnCategory(t *testing.T) {
	path := writeTestHCL(t, `
guild {
  id   = "1111"
  name = "MultiPerm"
}

reconciler {}

category "NSFW" {
  permission {
    role = "NSFW"
    allow = ["VIEW_CHANNEL", "CONNECT"]
  }
  permission {
    role = "@everyone"
    deny = ["VIEW_CHANNEL"]
  }
  channel "nsfw-general" {}
}
`)
	cfg, err := parseHCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	cat := cfg.Guild.Categories[0]
	if len(cat.PermissionOverwrites) != 2 {
		t.Fatalf("category perms = %d, want 2", len(cat.PermissionOverwrites))
	}

	// nsfw-general should inherit both
	ch := cat.Channels[0]
	if len(ch.PermissionOverwrites) != 2 {
		t.Fatalf("channel perms = %d, want 2 (inherited)", len(ch.PermissionOverwrites))
	}
	if ch.PermissionOverwrites[0].Target != "NSFW" {
		t.Errorf("perm[0] target = %q, want NSFW", ch.PermissionOverwrites[0].Target)
	}
	if !strings.Contains(strings.Join(ch.PermissionOverwrites[0].Allow, ","), "VIEW_CHANNEL") {
		t.Errorf("perm[0] allow missing VIEW_CHANNEL")
	}
}
