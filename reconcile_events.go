// reconcile_events.go
// Thin re-export layer for event types and constants. Also provides
// compatibility helpers that bridge legacy Discord types (PlanSummary,
// ApplyResult) to the extracted package's types.

package main

import "github.com/cogos-dev/cogos/pkg/reconcile"

// --- Re-exported event type aliases ---

type ReconcileEvent = reconcile.Event

// --- Re-exported constants ---

const (
	EventReconcilePlanStart    = reconcile.EventPlanStart
	EventReconcilePlanComplete = reconcile.EventPlanComplete
	EventReconcileApplyStart   = reconcile.EventApplyStart
	EventReconcileApplyAction  = reconcile.EventApplyAction
	EventReconcileApplyComplete = reconcile.EventApplyComplete
	EventReconcileDrift        = reconcile.EventDrift
	EventReconcileError        = reconcile.EventError

	// BlockComponentDrift is the CogBus block type for component drift events.
	BlockComponentDrift = "component.drift"

	// reconcileBusID is the bus channel for reconciliation events.
	reconcileBusID = "bus_chat_system_capabilities"
)

// --- Re-exported functions ---

var (
	EmitReconcileEvent = reconcile.EmitEvent
	EmitPlanStart      = reconcile.EmitPlanStart
	EmitApplyAction    = reconcile.EmitApplyAction
	EmitDriftDetected  = reconcile.EmitDriftDetected
	EmitReconcileError = reconcile.EmitError
)

// --- Compatibility wrappers for legacy Discord types ---

// EmitPlanComplete bridges the legacy PlanSummary type to the extracted package.
func EmitPlanComplete(resourceType string, summary PlanSummary, durationMs int64) *ReconcileEvent {
	return reconcile.EmitPlanComplete(resourceType, reconcile.Summary{
		Creates: summary.Creates,
		Updates: summary.Updates,
		Deletes: summary.Deletes,
		Skipped: summary.Skipped,
	}, durationMs)
}

// EmitApplyStart emits an apply.start event with the number of actions to apply.
func EmitApplyStart(resourceType string, actionCount int) *ReconcileEvent {
	return reconcile.EmitApplyStart(resourceType, actionCount)
}

// EmitApplyComplete bridges the legacy ApplyResult type to the extracted package.
func EmitApplyComplete(resourceType string, results []ApplyResult, durationMs int64) *ReconcileEvent {
	pkgResults := make([]reconcile.Result, len(results))
	for i, r := range results {
		pkgResults[i] = reconcile.Result{
			Phase:  r.Phase,
			Action: r.Action,
			Name:   r.Name,
			Status: reconcile.ApplyStatus(r.Status),
			Error:  r.Error,
		}
	}
	return reconcile.EmitApplyComplete(resourceType, pkgResults, durationMs)
}
