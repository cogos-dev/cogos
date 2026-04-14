// domain_agent_test.go
// Integration tests that validate domain agent CRDs and their projection
// into OpenClaw. Uses the real workspace root for CRD loading.
//
// These tests require the workspace to have agent CRD definitions at
// .cog/bin/agents/definitions/. They are skipped automatically if the
// workspace is not available (e.g. in CI without a mounted workspace).

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveTestWorkspaceRoot returns the workspace root for integration tests.
// It prefers the COGOS_WORKSPACE env var, then the runtime-resolved workspace.
// If neither is available or the CRD directory does not exist, it returns "".
func resolveTestWorkspaceRoot() string {
	if ws := os.Getenv("COGOS_WORKSPACE"); ws != "" {
		return ws
	}
	root, _, err := ResolveWorkspace()
	if err != nil {
		return ""
	}
	return root
}

// requireWorkspaceRoot skips the test if the workspace root or its CRD directory
// is not available on the current machine.
func requireWorkspaceRoot(t *testing.T) string {
	t.Helper()
	root := resolveTestWorkspaceRoot()
	if root == "" {
		t.Skip("workspace root not available; set COGOS_WORKSPACE to run integration tests")
	}
	crdDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if _, err := os.Stat(crdDir); os.IsNotExist(err) {
		t.Skipf("agent CRD directory not found at %s; skipping integration test", crdDir)
	}
	return root
}

// ─── Helper ─────────────────────────────────────────────────────────────────

// containsStr returns true if slice contains s.
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// ─── Test 1: Load all agent CRDs ────────────────────────────────────────────

func TestLoadAllAgentCRDs(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crds, err := ListAgentCRDs(testWorkspaceRoot)
	if err != nil {
		t.Fatalf("ListAgentCRDs failed: %v", err)
	}

	if len(crds) < 5 {
		t.Fatalf("expected at least 5 CRDs, got %d", len(crds))
	}

	for i, crd := range crds {
		if crd.Metadata.Name == "" {
			t.Errorf("CRD[%d]: metadata.name is empty", i)
		}
		if crd.APIVersion == "" {
			t.Errorf("CRD[%d] (%s): apiVersion is empty", i, crd.Metadata.Name)
		}
	}

	t.Logf("loaded %d agent CRDs", len(crds))
}

// ─── Test 2: Pax8 CRD fields ──────────────────────────────────────────────

func TestPax8CRDFields(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crd, err := LoadAgentCRD(testWorkspaceRoot, "pax8")
	if err != nil {
		t.Fatalf("LoadAgentCRD(pax8) failed: %v", err)
	}

	// metadata.name
	if crd.Metadata.Name != "pax8" {
		t.Errorf("metadata.name = %q, want %q", crd.Metadata.Name, "pax8")
	}

	// spec.capabilities.tools.allow
	expectedAllow := []string{"read", "web_fetch", "web_search", "memory_search", "memory_get", "memory_write", "message"}
	for _, tool := range expectedAllow {
		if !containsStr(crd.Spec.Capabilities.Tools.Allow, tool) {
			t.Errorf("spec.capabilities.tools.allow missing %q", tool)
		}
	}

	// spec.capabilities.tools.deny
	expectedDeny := []string{"exec", "process", "browser"}
	for _, tool := range expectedDeny {
		if !containsStr(crd.Spec.Capabilities.Tools.Deny, tool) {
			t.Errorf("spec.capabilities.tools.deny missing %q", tool)
		}
	}

	// spec.identity.name
	if crd.Spec.Identity.Name != "Pax8" {
		t.Errorf("spec.identity.name = %q, want %q", crd.Spec.Identity.Name, "Pax8")
	}

	// spec.identity.emoji
	if crd.Spec.Identity.Emoji == "" {
		t.Error("spec.identity.emoji is empty")
	}

	// spec.context.workspace
	if crd.Spec.Context.Workspace == "" {
		t.Error("spec.context.workspace is empty")
	}
}

// ─── Test 3: HomeAssistant CRD fields ───────────────────────────────────────

func TestHomeAssistantCRDFields(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crd, err := LoadAgentCRD(testWorkspaceRoot, "homeassistant")
	if err != nil {
		t.Fatalf("LoadAgentCRD(homeassistant) failed: %v", err)
	}

	if crd.Metadata.Name != "homeassistant" {
		t.Errorf("metadata.name = %q, want %q", crd.Metadata.Name, "homeassistant")
	}

	if crd.Spec.Identity.Name != "Home" {
		t.Errorf("spec.identity.name = %q, want %q", crd.Spec.Identity.Name, "Home")
	}

	if crd.Spec.Identity.Emoji == "" {
		t.Error("spec.identity.emoji is empty")
	}

	if crd.Spec.Identity.Role != "Home Automation Agent" {
		t.Errorf("spec.identity.role = %q, want %q", crd.Spec.Identity.Role, "Home Automation Agent")
	}

	if crd.Spec.Context.Workspace == "" {
		t.Error("spec.context.workspace is empty")
	}

	// Verify deny list has restrictive entries
	expectedDeny := []string{"exec", "write", "edit", "process", "browser"}
	for _, tool := range expectedDeny {
		if !containsStr(crd.Spec.Capabilities.Tools.Deny, tool) {
			t.Errorf("spec.capabilities.tools.deny missing %q", tool)
		}
	}

	// Verify allow list contains expected tools
	expectedAllow := []string{"read", "web_fetch", "memory_search", "memory_get", "message"}
	for _, tool := range expectedAllow {
		if !containsStr(crd.Spec.Capabilities.Tools.Allow, tool) {
			t.Errorf("spec.capabilities.tools.allow missing %q", tool)
		}
	}

	// Verify sandbox is read-only
	if crd.Spec.Runtime.Sandbox.Workspace != "ro" {
		t.Errorf("spec.runtime.sandbox.workspace = %q, want %q", crd.Spec.Runtime.Sandbox.Workspace, "ro")
	}
}

// ─── Test 4: Exec CRD access model ─────────────────────────────────────────

func TestExecCRDAccessModel(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crd, err := LoadAgentCRD(testWorkspaceRoot, "exec")
	if err != nil {
		t.Fatalf("LoadAgentCRD(exec) failed: %v", err)
	}

	// Verify structured access (not flat): defaultLevel should be set
	if crd.Spec.Access.DefaultLevel == "" {
		t.Error("spec.access.defaultLevel is empty; expected structured format")
	}
	if crd.Spec.Access.DefaultLevel != "none" {
		t.Errorf("spec.access.defaultLevel = %q, want %q", crd.Spec.Access.DefaultLevel, "none")
	}

	// Verify agents block exists
	if crd.Spec.Access.Agents == nil {
		t.Fatal("spec.access.agents is nil; expected structured agents map")
	}
	if crd.Spec.Access.Agents["whirl"] != "admin" {
		t.Errorf("spec.access.agents[whirl] = %q, want %q", crd.Spec.Access.Agents["whirl"], "admin")
	}

	// Verify users block exists with at least one user
	if crd.Spec.Access.Users == nil {
		t.Fatal("spec.access.users is nil; expected user entries")
	}
	if len(crd.Spec.Access.Users) < 1 {
		t.Fatal("spec.access.users has no entries; expected at least one user")
	}

	// Verify at least one admin user exists with a memoryScope set
	foundAdmin := false
	for uid, u := range crd.Spec.Access.Users {
		if u.Level == "admin" {
			foundAdmin = true
			wantScope := "users/" + uid
			if u.MemoryScope != wantScope {
				t.Errorf("users[%s].memoryScope = %q, want %q", uid, u.MemoryScope, wantScope)
			}
			break
		}
	}
	if !foundAdmin {
		t.Error("spec.access.users has no admin-level user; expected at least one")
	}
}

// ─── Test 5: Sentinel cron entries ──────────────────────────────────────────

func TestSentinelCronEntries(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crd, err := LoadAgentCRD(testWorkspaceRoot, "sentinel")
	if err != nil {
		t.Fatalf("LoadAgentCRD(sentinel) failed: %v", err)
	}

	if len(crd.Spec.Scheduling.Cron) == 0 {
		t.Fatal("spec.scheduling.cron is empty; expected at least one cron entry")
	}

	for i, entry := range crd.Spec.Scheduling.Cron {
		if entry.Schedule == "" {
			t.Errorf("cron[%d].schedule is empty", i)
		}
		if entry.Task == "" {
			t.Errorf("cron[%d].task is empty", i)
		}
	}

	// Verify the specific cron entry
	first := crd.Spec.Scheduling.Cron[0]
	if first.Schedule != "0 */4 * * *" {
		t.Errorf("cron[0].schedule = %q, want %q", first.Schedule, "0 */4 * * *")
	}
	if first.Channel != "#sentinel" {
		t.Errorf("cron[0].channel = %q, want %q", first.Channel, "#sentinel")
	}

	// Verify event subscriptions are also present
	if len(crd.Spec.Scheduling.EventSubscriptions) == 0 {
		t.Error("spec.scheduling.eventSubscriptions is empty; expected at least one")
	}
}

// ─── Test 6: Agent tool policies ────────────────────────────────────────────

func TestAgentToolPolicies(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	t.Run("sentinel", func(t *testing.T) {
		pol, err := GetAgentCRDToolPolicy(testWorkspaceRoot, "sentinel")
		if err != nil {
			t.Fatalf("GetAgentCRDToolPolicy(sentinel) failed: %v", err)
		}
		if pol == nil {
			t.Fatal("sentinel tool policy is nil")
		}

		expectedDeny := []string{"exec", "write", "edit", "process", "browser"}
		for _, tool := range expectedDeny {
			if !containsStr(pol.DenyTools, tool) {
				t.Errorf("sentinel deny list missing %q", tool)
			}
		}
	})

	t.Run("pax8", func(t *testing.T) {
		pol, err := GetAgentCRDToolPolicy(testWorkspaceRoot, "pax8")
		if err != nil {
			t.Fatalf("GetAgentCRDToolPolicy(pax8) failed: %v", err)
		}
		if pol == nil {
			t.Fatal("pax8 tool policy is nil")
		}
		if len(pol.DenyTools) == 0 {
			t.Error("pax8 should have a non-empty deny list")
		}
	})

	t.Run("homeassistant", func(t *testing.T) {
		pol, err := GetAgentCRDToolPolicy(testWorkspaceRoot, "homeassistant")
		if err != nil {
			t.Fatalf("GetAgentCRDToolPolicy(homeassistant) failed: %v", err)
		}
		if pol == nil {
			t.Fatal("homeassistant tool policy is nil")
		}
		if len(pol.DenyTools) == 0 {
			t.Error("homeassistant should have a non-empty deny list")
		}
	})

	t.Run("exec", func(t *testing.T) {
		pol, err := GetAgentCRDToolPolicy(testWorkspaceRoot, "exec")
		if err != nil {
			t.Fatalf("GetAgentCRDToolPolicy(exec) failed: %v", err)
		}
		if pol == nil {
			t.Fatal("exec tool policy is nil")
		}
		if len(pol.DenyTools) == 0 {
			t.Error("exec should have a non-empty deny list")
		}
	})

	t.Run("whirl", func(t *testing.T) {
		pol, err := GetAgentCRDToolPolicy(testWorkspaceRoot, "whirl")
		if err != nil {
			t.Fatalf("GetAgentCRDToolPolicy(whirl) failed: %v", err)
		}
		if pol == nil {
			t.Fatal("whirl tool policy is nil")
		}
		// Whirl is unrestricted: deny list should be empty or nil
		if len(pol.DenyTools) > 0 {
			t.Errorf("whirl deny list should be empty (unrestricted), got %v", pol.DenyTools)
		}
	})
}

// ─── Test 7: Agent projection ───────────────────────────────────────────────

func TestAgentProjection(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crds, err := ListAgentCRDs(testWorkspaceRoot)
	if err != nil {
		t.Fatalf("ListAgentCRDs failed: %v", err)
	}

	// Build a map of CRDs by name for easy lookup
	crdByName := make(map[string]AgentCRD)
	for _, crd := range crds {
		crdByName[crd.Metadata.Name] = crd
	}

	t.Run("whirl_maps_to_main", func(t *testing.T) {
		crd, ok := crdByName["whirl"]
		if !ok {
			t.Fatal("whirl CRD not found")
		}
		entry := projectCRDToEntry(crd)

		if entry.ID != "main" {
			t.Errorf("whirl projected ID = %q, want %q", entry.ID, "main")
		}
		if !entry.Default {
			t.Error("whirl projected entry should have Default=true")
		}
		if entry.Name != "Whirl" {
			t.Errorf("whirl projected Name = %q, want %q", entry.Name, "Whirl")
		}
		if entry.Identity == nil {
			t.Fatal("whirl projected Identity is nil")
		}
		if entry.Identity.Name != "Whirl" {
			t.Errorf("whirl Identity.Name = %q, want %q", entry.Identity.Name, "Whirl")
		}
		if entry.Identity.Emoji == "" {
			t.Error("whirl Identity.Emoji is empty")
		}
	})

	t.Run("pax8_projection", func(t *testing.T) {
		crd, ok := crdByName["pax8"]
		if !ok {
			t.Fatal("pax8 CRD not found")
		}
		entry := projectCRDToEntry(crd)

		// CRD name maps directly to OpenClaw ID (not "whirl" -> "main")
		if entry.ID != "pax8" {
			t.Errorf("pax8 projected ID = %q, want %q", entry.ID, "pax8")
		}
		if entry.Default {
			t.Error("pax8 should not be default")
		}

		// Identity fields
		if entry.Identity == nil {
			t.Fatal("pax8 Identity is nil")
		}
		if entry.Identity.Name != crd.Spec.Identity.Name {
			t.Errorf("Identity.Name = %q, want %q", entry.Identity.Name, crd.Spec.Identity.Name)
		}
		if entry.Identity.Emoji != crd.Spec.Identity.Emoji {
			t.Errorf("Identity.Emoji = %q, want %q", entry.Identity.Emoji, crd.Spec.Identity.Emoji)
		}

		// Workspace
		if entry.Workspace != crd.Spec.Context.Workspace {
			t.Errorf("Workspace = %q, want %q", entry.Workspace, crd.Spec.Context.Workspace)
		}

		// Tools should use shell-specific override (openclaw toolPolicy)
		if entry.Tools == nil {
			t.Fatal("pax8 Tools is nil; expected shell-specific tool policy")
		}
		// The openclaw shell toolPolicy.allow should be projected
		if len(entry.Tools.Allow) == 0 {
			t.Error("pax8 Tools.Allow is empty")
		}
	})

	t.Run("sentinel_projection", func(t *testing.T) {
		crd, ok := crdByName["sentinel"]
		if !ok {
			t.Fatal("sentinel CRD not found")
		}
		entry := projectCRDToEntry(crd)

		if entry.ID != "sentinel" {
			t.Errorf("sentinel projected ID = %q, want %q", entry.ID, "sentinel")
		}
		if entry.Identity == nil {
			t.Fatal("sentinel Identity is nil")
		}
		if entry.Identity.Name != "Sentinel" {
			t.Errorf("Identity.Name = %q, want %q", entry.Identity.Name, "Sentinel")
		}

		// Sentinel should have deny tools projected
		if entry.Tools == nil {
			t.Fatal("sentinel Tools is nil")
		}
		if len(entry.Tools.Deny) == 0 {
			t.Error("sentinel Tools.Deny is empty; expected deny list")
		}
	})

	t.Run("homeassistant_projection", func(t *testing.T) {
		crd, ok := crdByName["homeassistant"]
		if !ok {
			t.Fatal("homeassistant CRD not found")
		}
		entry := projectCRDToEntry(crd)

		if entry.ID != "homeassistant" {
			t.Errorf("homeassistant projected ID = %q, want %q", entry.ID, "homeassistant")
		}
		if entry.Workspace != crd.Spec.Context.Workspace {
			t.Errorf("Workspace = %q, want %q", entry.Workspace, crd.Spec.Context.Workspace)
		}
	})
}

// ─── Test 8: CRD schema compliance ─────────────────────────────────────────

func TestCRDSchemaCompliance(t *testing.T) {
	testWorkspaceRoot := requireWorkspaceRoot(t)
	crds, err := ListAgentCRDs(testWorkspaceRoot)
	if err != nil {
		t.Fatalf("ListAgentCRDs failed: %v", err)
	}

	for _, crd := range crds {
		name := crd.Metadata.Name
		t.Run(name, func(t *testing.T) {
			// apiVersion
			if crd.APIVersion != "cog.os/v1alpha1" {
				t.Errorf("apiVersion = %q, want %q", crd.APIVersion, "cog.os/v1alpha1")
			}

			// kind
			if crd.Kind != "Agent" {
				t.Errorf("kind = %q, want %q", crd.Kind, "Agent")
			}

			// metadata.name
			if crd.Metadata.Name == "" {
				t.Error("metadata.name is empty")
			}

			// spec.capabilities must be present (tools block)
			if len(crd.Spec.Capabilities.Tools.Allow) == 0 && len(crd.Spec.Capabilities.Tools.Deny) == 0 {
				t.Error("spec.capabilities.tools has no allow or deny entries")
			}

			// spec.type should be set
			if crd.Spec.Type == "" {
				t.Error("spec.type is empty")
			}
			validTypes := map[string]bool{"interactive": true, "declarative": true, "headless": true}
			if !validTypes[crd.Spec.Type] {
				t.Errorf("spec.type = %q, want one of interactive/declarative/headless", crd.Spec.Type)
			}

			// spec.identity.name should be set
			if crd.Spec.Identity.Name == "" {
				t.Error("spec.identity.name is empty")
			}

			// spec.modelConfig.model should be set (except headless agents which don't do inference)
			if crd.Spec.ModelConfig.Model == "" && crd.Spec.Type != "headless" {
				t.Error("spec.modelConfig.model is empty")
			}
		})
	}
}
