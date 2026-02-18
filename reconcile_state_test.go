// reconcile_state_test.go
// Tests for generic reconciliation state management.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileStatePath(t *testing.T) {
	got := reconcileStatePath("/workspace", "discord")
	want := filepath.Join("/workspace", ".cog", "config", "discord", ".state.json")
	if got != want {
		t.Errorf("reconcileStatePath = %q, want %q", got, want)
	}
}

func TestNewReconcileState(t *testing.T) {
	state := NewReconcileState("discord")
	if state.Version != 1 {
		t.Errorf("Version = %d, want 1", state.Version)
	}
	if state.Lineage == "" {
		t.Error("Lineage should not be empty")
	}
	if len(state.Lineage) != 32 {
		t.Errorf("Lineage length = %d, want 32 hex chars", len(state.Lineage))
	}
	if state.ResourceType != "discord" {
		t.Errorf("ResourceType = %q, want %q", state.ResourceType, "discord")
	}
	if state.Serial != 0 {
		t.Errorf("Serial = %d, want 0", state.Serial)
	}
}

func TestLoadReconcileStateMissing(t *testing.T) {
	tmpDir := t.TempDir()
	state, err := LoadReconcileState(tmpDir, "discord")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Error("expected nil state for missing file")
	}
}

func TestWriteAndLoadReconcileState(t *testing.T) {
	tmpDir := t.TempDir()

	state := NewReconcileState("test-provider")
	state.Resources = []ReconcileResource{
		{
			Address:    "role/admin",
			Type:       "role",
			Mode:       ModeManaged,
			ExternalID: "123",
			Name:       "admin",
		},
	}

	err := WriteReconcileState(tmpDir, "test-provider", state)
	if err != nil {
		t.Fatalf("WriteReconcileState failed: %v", err)
	}

	// Verify file exists at correct path
	expectedPath := filepath.Join(tmpDir, ".cog", "config", "test-provider", ".state.json")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("state file not found at %s", expectedPath)
	}

	// Load it back
	loaded, err := LoadReconcileState(tmpDir, "test-provider")
	if err != nil {
		t.Fatalf("LoadReconcileState failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded state is nil")
	}

	// Serial should be incremented
	if loaded.Serial != 1 {
		t.Errorf("Serial = %d, want 1", loaded.Serial)
	}
	if loaded.ResourceType != "test-provider" {
		t.Errorf("ResourceType = %q, want %q", loaded.ResourceType, "test-provider")
	}
	if loaded.GeneratedAt == "" {
		t.Error("GeneratedAt should be set")
	}
	if len(loaded.Resources) != 1 {
		t.Fatalf("Resources count = %d, want 1", len(loaded.Resources))
	}
	if loaded.Resources[0].ExternalID != "123" {
		t.Errorf("ExternalID = %q, want %q", loaded.Resources[0].ExternalID, "123")
	}
}

func TestWriteReconcileStateSerialIncrement(t *testing.T) {
	tmpDir := t.TempDir()
	state := NewReconcileState("counter")

	// Write 3 times
	for i := 0; i < 3; i++ {
		if err := WriteReconcileState(tmpDir, "counter", state); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	loaded, err := LoadReconcileState(tmpDir, "counter")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.Serial != 3 {
		t.Errorf("Serial = %d, want 3 after 3 writes", loaded.Serial)
	}
}

func TestWriteReconcileStateCreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	state := NewReconcileState("new-provider")

	err := WriteReconcileState(tmpDir, "new-provider", state)
	if err != nil {
		t.Fatalf("failed to write state to new dir: %v", err)
	}

	// Directory should exist now
	dir := filepath.Join(tmpDir, ".cog", "config", "new-provider")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestReconcileStateJSON(t *testing.T) {
	state := NewReconcileState("discord")
	state.Metadata = map[string]any{"guild_id": "12345"}
	state.Resources = []ReconcileResource{
		{Address: "role/admin", ExternalID: "999", Mode: ModeManaged},
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Verify JSON structure
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if parsed["resource_type"] != "discord" {
		t.Error("resource_type not in JSON")
	}
	meta, ok := parsed["metadata"].(map[string]any)
	if !ok {
		t.Fatal("metadata not in JSON")
	}
	if meta["guild_id"] != "12345" {
		t.Error("metadata.guild_id not preserved")
	}
}

func TestGenerateLineage(t *testing.T) {
	a := GenerateLineage()
	b := GenerateLineage()
	if a == b {
		t.Error("two lineages should not be equal")
	}
	if len(a) != 32 {
		t.Errorf("lineage length = %d, want 32", len(a))
	}
}
