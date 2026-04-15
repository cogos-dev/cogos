// events.go
// Reconciliation event types and emission helpers for the reconciliation framework.
// Events follow the cog.reconcile.* naming convention and are emitted during
// the plan/apply lifecycle.

package reconcile

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Event type constants for the reconciliation lifecycle.
const (
	EventPlanStart     = "cog.reconcile.plan.start"
	EventPlanComplete  = "cog.reconcile.plan.complete"
	EventApplyStart    = "cog.reconcile.apply.start"
	EventApplyAction   = "cog.reconcile.apply.action"
	EventApplyComplete = "cog.reconcile.apply.complete"
	EventDrift         = "cog.reconcile.drift.detected"
	EventError         = "cog.reconcile.error"
)

// Event is the structured payload emitted for reconciliation lifecycle events.
type Event struct {
	EventType    string         `json:"event"`
	ResourceType string         `json:"resource_type"`
	Timestamp    string         `json:"timestamp"`
	DurationMs   int64          `json:"duration_ms,omitempty"`
	Summary      map[string]any `json:"summary,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// EmitEvent creates an Event, logs it to stderr, and returns it for optional forwarding.
func EmitEvent(eventType string, resourceType string, summary map[string]any) *Event {
	evt := &Event{
		EventType:    eventType,
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

// EmitPlanStart emits a plan.start event for the given resource type.
func EmitPlanStart(resourceType string) *Event {
	return EmitEvent(EventPlanStart, resourceType, nil)
}

// EmitPlanComplete emits a plan.complete event with plan summary and duration.
func EmitPlanComplete(resourceType string, summary Summary, durationMs int64) *Event {
	evt := EmitEvent(EventPlanComplete, resourceType, map[string]any{
		"creates": summary.Creates,
		"updates": summary.Updates,
		"deletes": summary.Deletes,
		"skipped": summary.Skipped,
	})
	evt.DurationMs = durationMs
	return evt
}

// EmitApplyStart emits an apply.start event with the number of actions to apply.
func EmitApplyStart(resourceType string, actionCount int) *Event {
	return EmitEvent(EventApplyStart, resourceType, map[string]any{
		"action_count": actionCount,
	})
}

// EmitApplyAction emits an apply.action event for an individual action execution.
func EmitApplyAction(resourceType string, action, name, status string) *Event {
	return EmitEvent(EventApplyAction, resourceType, map[string]any{
		"action": action,
		"name":   name,
		"status": status,
	})
}

// EmitApplyComplete emits an apply.complete event with aggregated results and duration.
func EmitApplyComplete(resourceType string, results []Result, durationMs int64) *Event {
	succeeded := 0
	failed := 0
	skipped := 0
	for _, r := range results {
		switch r.Status {
		case ApplySucceeded:
			succeeded++
		case ApplyFailed:
			failed++
		case ApplySkipped:
			skipped++
		}
	}

	evt := EmitEvent(EventApplyComplete, resourceType, map[string]any{
		"total":     len(results),
		"succeeded": succeeded,
		"failed":    failed,
		"skipped":   skipped,
	})
	evt.DurationMs = durationMs
	return evt
}

// EmitDriftDetected emits a drift.detected event with the number of drifts found.
func EmitDriftDetected(resourceType string, drifts int) *Event {
	return EmitEvent(EventDrift, resourceType, map[string]any{
		"drifts": drifts,
	})
}

// EmitError emits an error event for a failed reconciliation.
func EmitError(resourceType string, err error) *Event {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	evt := EmitEvent(EventError, resourceType, nil)
	evt.Error = errMsg
	return evt
}
