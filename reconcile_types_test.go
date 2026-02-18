// reconcile_types_test.go
// Tests for generic reconciliation types and helpers.

package main

import "testing"

func TestReconcileSummaryTotal(t *testing.T) {
	s := ReconcileSummary{Creates: 3, Updates: 2, Deletes: 1, Skipped: 5}
	if s.Total() != 11 {
		t.Errorf("Total() = %d, want 11", s.Total())
	}
}

func TestReconcileSummaryHasChanges(t *testing.T) {
	tests := []struct {
		name    string
		summary ReconcileSummary
		want    bool
	}{
		{"all zeros", ReconcileSummary{}, false},
		{"only skipped", ReconcileSummary{Skipped: 5}, false},
		{"creates", ReconcileSummary{Creates: 1}, true},
		{"updates", ReconcileSummary{Updates: 1}, true},
		{"deletes", ReconcileSummary{Deletes: 1}, true},
		{"mixed", ReconcileSummary{Creates: 1, Skipped: 3}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.summary.HasChanges(); got != tt.want {
				t.Errorf("HasChanges() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewResourceStatus(t *testing.T) {
	s := NewResourceStatus(SyncStatusSynced, HealthHealthy)
	if s.Sync != SyncStatusSynced {
		t.Errorf("Sync = %s, want Synced", s.Sync)
	}
	if s.Health != HealthHealthy {
		t.Errorf("Health = %s, want Healthy", s.Health)
	}
	if s.Operation != OperationIdle {
		t.Errorf("Operation = %s, want Idle", s.Operation)
	}
}

func TestReconcileResourceIndex(t *testing.T) {
	state := &ReconcileState{
		Resources: []ReconcileResource{
			{Address: "role/Admin", Name: "Admin", ExternalID: "111"},
			{Address: "category/General", Name: "General", ExternalID: "222"},
			{Address: "category/General/channel/chat", Name: "chat", ExternalID: "333"},
		},
	}

	idx := ReconcileResourceIndex(state)
	if len(idx) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(idx))
	}
	if idx["role/Admin"].Name != "Admin" {
		t.Error("role/Admin not found or wrong name")
	}
	if idx["category/General/channel/chat"].ExternalID != "333" {
		t.Error("channel lookup failed")
	}
}

func TestReconcileResourceIndexNil(t *testing.T) {
	idx := ReconcileResourceIndex(nil)
	if idx != nil {
		t.Error("Expected nil for nil state")
	}
}

func TestReconcileResourceByExternalID(t *testing.T) {
	state := &ReconcileState{
		Resources: []ReconcileResource{
			{Address: "role/Admin", ExternalID: "111", Name: "Admin"},
			{Address: "role/Mod", ExternalID: "", Name: "Mod"},
			{Address: "category/General", ExternalID: "222", Name: "General"},
		},
	}

	idx := ReconcileResourceByExternalID(state)
	if len(idx) != 2 {
		t.Fatalf("Expected 2 entries (skipping empty ID), got %d", len(idx))
	}
	if idx["111"].Address != "role/Admin" {
		t.Error("ID 111 lookup failed")
	}
	if _, ok := idx[""]; ok {
		t.Error("Empty external ID should not be indexed")
	}
}

func TestStatusEnumValues(t *testing.T) {
	// Verify enum string values are stable (they appear in JSON/state files)
	if SyncStatusSynced != "Synced" {
		t.Error("SyncStatusSynced value changed")
	}
	if HealthHealthy != "Healthy" {
		t.Error("HealthHealthy value changed")
	}
	if OperationIdle != "Idle" {
		t.Error("OperationIdle value changed")
	}
	if ActionCreate != "create" {
		t.Error("ActionCreate value changed")
	}
	if ModeManaged != "managed" {
		t.Error("ModeManaged value changed")
	}
	if ApplySucceeded != "succeeded" {
		t.Error("ApplySucceeded value changed")
	}
}

func TestReconcileActionTypes(t *testing.T) {
	// Verify ActionType values match the strings used in Discord reconciler
	actions := map[ActionType]string{
		ActionCreate: "create",
		ActionUpdate: "update",
		ActionDelete: "delete",
		ActionSkip:   "skip",
	}
	for at, expected := range actions {
		if string(at) != expected {
			t.Errorf("ActionType %v = %q, want %q", at, string(at), expected)
		}
	}
}

func TestReconcilePlanMetadata(t *testing.T) {
	plan := &ReconcilePlan{
		ResourceType: "discord",
		Actions: []ReconcileAction{
			{Action: ActionCreate, ResourceType: "channel", Name: "general"},
			{Action: ActionSkip, ResourceType: "role", Name: "Admin"},
		},
		Summary:  ReconcileSummary{Creates: 1, Skipped: 1},
		Metadata: map[string]any{"guild_id": "123"},
	}
	if plan.ResourceType != "discord" {
		t.Error("ResourceType not set")
	}
	if plan.Metadata["guild_id"] != "123" {
		t.Error("Metadata not preserved")
	}
	if !plan.Summary.HasChanges() {
		t.Error("Plan with creates should have changes")
	}
}
