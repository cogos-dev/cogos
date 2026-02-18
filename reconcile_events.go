// reconcile_events.go
// Reconciliation event types and emission helpers for the CogOS provider model.
// Events follow the cog.reconcile.* naming convention and are emitted during
// the plan/apply lifecycle. They are logged to stderr and returned to callers
// for optional forwarding to CogBus (see task C2).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ─── Reconciliation event type constants ────────────────────────────────────

const (
	// EventReconcilePlanStart is emitted when plan computation begins.
	EventReconcilePlanStart = "cog.reconcile.plan.start"

	// EventReconcilePlanComplete is emitted when a plan is ready with its summary.
	EventReconcilePlanComplete = "cog.reconcile.plan.complete"

	// EventReconcileApplyStart is emitted when plan application begins.
	EventReconcileApplyStart = "cog.reconcile.apply.start"

	// EventReconcileApplyAction is emitted for each individual action executed.
	EventReconcileApplyAction = "cog.reconcile.apply.action"

	// EventReconcileApplyComplete is emitted when plan application finishes.
	EventReconcileApplyComplete = "cog.reconcile.apply.complete"

	// EventReconcileDrift is emitted when drift is detected during refresh or watch.
	EventReconcileDrift = "cog.reconcile.drift.detected"

	// EventReconcileError is emitted when a reconciliation fails.
	EventReconcileError = "cog.reconcile.error"
)

// ─── ReconcileEvent struct ──────────────────────────────────────────────────

// ReconcileEvent is the structured payload emitted for reconciliation lifecycle events.
type ReconcileEvent struct {
	Event        string         `json:"event"`
	ResourceType string         `json:"resource_type"`
	Timestamp    string         `json:"timestamp"`
	DurationMs   int64          `json:"duration_ms,omitempty"`
	Summary      map[string]any `json:"summary,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// ─── Core emission function ─────────────────────────────────────────────────

// EmitReconcileEvent creates a ReconcileEvent, logs it to stderr, and returns
// it for callers who want to forward it to CogBus.
func EmitReconcileEvent(eventType string, resourceType string, summary map[string]any) *ReconcileEvent {
	evt := &ReconcileEvent{
		Event:        eventType,
		ResourceType: resourceType,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Summary:      summary,
	}

	data, err := json.Marshal(evt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[reconcile] event=%s resource=%s error=marshal_failed\n", eventType, resourceType)
		return evt
	}
	fmt.Fprintf(os.Stderr, "[reconcile] %s\n", string(data))
	return evt
}

// ─── Typed helper functions ─────────────────────────────────────────────────

// EmitPlanStart emits a plan.start event for the given resource type.
func EmitPlanStart(resourceType string) *ReconcileEvent {
	return EmitReconcileEvent(EventReconcilePlanStart, resourceType, nil)
}

// EmitPlanComplete emits a plan.complete event with plan summary and duration.
func EmitPlanComplete(resourceType string, summary PlanSummary, durationMs int64) *ReconcileEvent {
	evt := EmitReconcileEvent(EventReconcilePlanComplete, resourceType, map[string]any{
		"creates": summary.Creates,
		"updates": summary.Updates,
		"deletes": summary.Deletes,
		"skipped": summary.Skipped,
	})
	evt.DurationMs = durationMs
	return evt
}

// EmitApplyStart emits an apply.start event with the number of actions to apply.
func EmitApplyStart(resourceType string, actionCount int) *ReconcileEvent {
	return EmitReconcileEvent(EventReconcileApplyStart, resourceType, map[string]any{
		"action_count": actionCount,
	})
}

// EmitApplyAction emits an apply.action event for an individual action execution.
func EmitApplyAction(resourceType string, action, name, status string) *ReconcileEvent {
	return EmitReconcileEvent(EventReconcileApplyAction, resourceType, map[string]any{
		"action": action,
		"name":   name,
		"status": status,
	})
}

// EmitApplyComplete emits an apply.complete event with aggregated results and duration.
func EmitApplyComplete(resourceType string, results []ApplyResult, durationMs int64) *ReconcileEvent {
	succeeded := 0
	failed := 0
	skipped := 0
	for _, r := range results {
		switch r.Status {
		case "succeeded":
			succeeded++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}

	evt := EmitReconcileEvent(EventReconcileApplyComplete, resourceType, map[string]any{
		"total":     len(results),
		"succeeded": succeeded,
		"failed":    failed,
		"skipped":   skipped,
	})
	evt.DurationMs = durationMs
	return evt
}

// EmitDriftDetected emits a drift.detected event with the number of drifts found.
func EmitDriftDetected(resourceType string, drifts int) *ReconcileEvent {
	return EmitReconcileEvent(EventReconcileDrift, resourceType, map[string]any{
		"drifts": drifts,
	})
}

// EmitReconcileError emits an error event for a failed reconciliation.
func EmitReconcileError(resourceType string, err error) *ReconcileEvent {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	evt := EmitReconcileEvent(EventReconcileError, resourceType, nil)
	evt.Error = errMsg
	return evt
}
