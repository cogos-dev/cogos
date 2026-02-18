// reconcile_events_test.go
// Tests for reconciliation event types and emission helpers.

package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// ─── Event type constant tests ──────────────────────────────────────────────

func TestEventTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant string
		want     string
	}{
		{"PlanStart", EventReconcilePlanStart, "cog.reconcile.plan.start"},
		{"PlanComplete", EventReconcilePlanComplete, "cog.reconcile.plan.complete"},
		{"ApplyStart", EventReconcileApplyStart, "cog.reconcile.apply.start"},
		{"ApplyAction", EventReconcileApplyAction, "cog.reconcile.apply.action"},
		{"ApplyComplete", EventReconcileApplyComplete, "cog.reconcile.apply.complete"},
		{"Drift", EventReconcileDrift, "cog.reconcile.drift.detected"},
		{"Error", EventReconcileError, "cog.reconcile.error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.want {
				t.Errorf("got %q, want %q", tt.constant, tt.want)
			}
		})
	}
}

// ─── EmitReconcileEvent tests ───────────────────────────────────────────────

func TestEmitReconcileEvent_Structure(t *testing.T) {
	evt := EmitReconcileEvent("cog.reconcile.plan.start", "discord", map[string]any{
		"guild_id": "123",
	})

	if evt.Event != "cog.reconcile.plan.start" {
		t.Errorf("Event = %q, want %q", evt.Event, "cog.reconcile.plan.start")
	}
	if evt.ResourceType != "discord" {
		t.Errorf("ResourceType = %q, want %q", evt.ResourceType, "discord")
	}
	if evt.Timestamp == "" {
		t.Error("Timestamp should not be empty")
	}
	if evt.Summary["guild_id"] != "123" {
		t.Errorf("Summary[guild_id] = %v, want 123", evt.Summary["guild_id"])
	}
}

func TestEmitReconcileEvent_NilSummary(t *testing.T) {
	evt := EmitReconcileEvent("cog.reconcile.plan.start", "agent", nil)

	if evt.Event != "cog.reconcile.plan.start" {
		t.Errorf("Event = %q, want %q", evt.Event, "cog.reconcile.plan.start")
	}
	if evt.Summary != nil {
		t.Errorf("Summary should be nil, got %v", evt.Summary)
	}
}

func TestEmitReconcileEvent_JSON(t *testing.T) {
	evt := EmitReconcileEvent("cog.reconcile.drift.detected", "discord", map[string]any{
		"drifts": 3,
	})

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded ReconcileEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if decoded.Event != evt.Event {
		t.Errorf("decoded.Event = %q, want %q", decoded.Event, evt.Event)
	}
	if decoded.ResourceType != evt.ResourceType {
		t.Errorf("decoded.ResourceType = %q, want %q", decoded.ResourceType, evt.ResourceType)
	}
}

// ─── EmitPlanComplete tests ─────────────────────────────────────────────────

func TestEmitPlanComplete(t *testing.T) {
	summary := PlanSummary{
		Creates: 3,
		Updates: 1,
		Deletes: 2,
		Skipped: 5,
	}
	evt := EmitPlanComplete("discord", summary, 450)

	if evt.Event != EventReconcilePlanComplete {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcilePlanComplete)
	}
	if evt.DurationMs != 450 {
		t.Errorf("DurationMs = %d, want 450", evt.DurationMs)
	}
	if evt.Summary["creates"] != 3 {
		t.Errorf("Summary[creates] = %v, want 3", evt.Summary["creates"])
	}
	if evt.Summary["updates"] != 1 {
		t.Errorf("Summary[updates] = %v, want 1", evt.Summary["updates"])
	}
	if evt.Summary["deletes"] != 2 {
		t.Errorf("Summary[deletes] = %v, want 2", evt.Summary["deletes"])
	}
	if evt.Summary["skipped"] != 5 {
		t.Errorf("Summary[skipped] = %v, want 5", evt.Summary["skipped"])
	}
}

func TestEmitPlanComplete_ZeroSummary(t *testing.T) {
	evt := EmitPlanComplete("agent", PlanSummary{}, 100)

	if evt.Summary["creates"] != 0 {
		t.Errorf("Summary[creates] = %v, want 0", evt.Summary["creates"])
	}
	if evt.DurationMs != 100 {
		t.Errorf("DurationMs = %d, want 100", evt.DurationMs)
	}
}

// ─── EmitApplyComplete tests ────────────────────────────────────────────────

func TestEmitApplyComplete_Counts(t *testing.T) {
	results := []ApplyResult{
		{Phase: "roles", Action: "create", Name: "Admin", Status: "succeeded"},
		{Phase: "roles", Action: "create", Name: "Mod", Status: "succeeded"},
		{Phase: "channels", Action: "create", Name: "general", Status: "failed", Error: "permission denied"},
		{Phase: "channels", Action: "skip", Name: "voice-chat", Status: "skipped"},
		{Phase: "channels", Action: "create", Name: "announcements", Status: "succeeded"},
	}
	evt := EmitApplyComplete("discord", results, 1200)

	if evt.Event != EventReconcileApplyComplete {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcileApplyComplete)
	}
	if evt.DurationMs != 1200 {
		t.Errorf("DurationMs = %d, want 1200", evt.DurationMs)
	}
	if evt.Summary["total"] != 5 {
		t.Errorf("Summary[total] = %v, want 5", evt.Summary["total"])
	}
	if evt.Summary["succeeded"] != 3 {
		t.Errorf("Summary[succeeded] = %v, want 3", evt.Summary["succeeded"])
	}
	if evt.Summary["failed"] != 1 {
		t.Errorf("Summary[failed] = %v, want 1", evt.Summary["failed"])
	}
	if evt.Summary["skipped"] != 1 {
		t.Errorf("Summary[skipped] = %v, want 1", evt.Summary["skipped"])
	}
}

func TestEmitApplyComplete_Empty(t *testing.T) {
	evt := EmitApplyComplete("discord", nil, 50)

	if evt.Summary["total"] != 0 {
		t.Errorf("Summary[total] = %v, want 0", evt.Summary["total"])
	}
	if evt.Summary["succeeded"] != 0 {
		t.Errorf("Summary[succeeded] = %v, want 0", evt.Summary["succeeded"])
	}
}

// ─── Other helper tests ────────────────────────────────────────────────────

func TestEmitPlanStart(t *testing.T) {
	evt := EmitPlanStart("discord")
	if evt.Event != EventReconcilePlanStart {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcilePlanStart)
	}
	if evt.ResourceType != "discord" {
		t.Errorf("ResourceType = %q, want %q", evt.ResourceType, "discord")
	}
}

func TestEmitApplyStart(t *testing.T) {
	evt := EmitApplyStart("discord", 7)
	if evt.Event != EventReconcileApplyStart {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcileApplyStart)
	}
	if evt.Summary["action_count"] != 7 {
		t.Errorf("Summary[action_count] = %v, want 7", evt.Summary["action_count"])
	}
}

func TestEmitApplyAction(t *testing.T) {
	evt := EmitApplyAction("discord", "create", "general", "succeeded")
	if evt.Event != EventReconcileApplyAction {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcileApplyAction)
	}
	if evt.Summary["action"] != "create" {
		t.Errorf("Summary[action] = %v, want create", evt.Summary["action"])
	}
	if evt.Summary["name"] != "general" {
		t.Errorf("Summary[name] = %v, want general", evt.Summary["name"])
	}
	if evt.Summary["status"] != "succeeded" {
		t.Errorf("Summary[status] = %v, want succeeded", evt.Summary["status"])
	}
}

func TestEmitDriftDetected(t *testing.T) {
	evt := EmitDriftDetected("discord", 4)
	if evt.Event != EventReconcileDrift {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcileDrift)
	}
	if evt.Summary["drifts"] != 4 {
		t.Errorf("Summary[drifts] = %v, want 4", evt.Summary["drifts"])
	}
}

func TestEmitReconcileError_WithError(t *testing.T) {
	evt := EmitReconcileError("discord", errors.New("token expired"))
	if evt.Event != EventReconcileError {
		t.Errorf("Event = %q, want %q", evt.Event, EventReconcileError)
	}
	if evt.Error != "token expired" {
		t.Errorf("Error = %q, want %q", evt.Error, "token expired")
	}
}

func TestEmitReconcileError_NilError(t *testing.T) {
	evt := EmitReconcileError("discord", nil)
	if evt.Error != "" {
		t.Errorf("Error = %q, want empty string", evt.Error)
	}
}
