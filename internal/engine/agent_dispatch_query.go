// agent_dispatch_query.go — validation, clamping, and orchestration glue for
// DispatchRequest. Pure functions over the AgentDispatcher contract; the
// concrete dispatcher lives in the root package because it needs *AgentHarness.
package engine

import (
	"context"
	"fmt"
	"strings"
)

// dispatchTimeoutDefault is the per-slot wall-clock budget when the caller
// passed 0 or omitted the field. 30s comfortably covers the resident E4B's
// 10-turn worst case at ~3s/turn while staying well under the
// h.httpClient.Timeout (180s) so the inner HTTP call is the actual cap.
const dispatchTimeoutDefault = 30

// dispatchTimeoutMax caps the per-slot budget. 300s accommodates cold-start
// load of larger local models (gemma4:e4b at ~110s on M4 Pro) plus the
// localHarnessCycleTimeout (300s) for tool-loop dispatches that run multiple
// Ollama round-trips. The previous 120s cap was lifted from the legacy
// TriggerAgent wait limit, but eval sweeps and Reconcilable-driven
// dispatches need headroom for cold cycles.
const dispatchTimeoutMax = 300

// dispatchNDefault is the parallel fan-out when 0 is passed. Mirrors the
// "single dispatch" baseline so a minimal call shape behaves like a normal
// trigger.
const dispatchNDefault = 1

// dispatchNMax caps fan-out. The 48 GB VRAM box comfortably runs 4 concurrent
// E4B requests against the resident weights; tighter caps belong upstream.
const dispatchNMax = 4

// Normalize fills defaults, clamps ranges, and returns the first invalid-input
// error it finds (or nil). Mutates the receiver — callers pass by pointer.
func (r *DispatchRequest) Normalize() error {
	if r.AgentID == "" {
		r.AgentID = DefaultAgentID
	}
	if err := ValidateAgentID(r.AgentID); err != nil {
		return err
	}
	r.Task = strings.TrimSpace(r.Task)
	if r.Task == "" {
		return &AgentControllerError{Code: "invalid_input", Message: "task is required"}
	}
	r.Provider = strings.TrimSpace(r.Provider)
	// Provider unknown-name validation happens in the dispatcher: only it
	// holds the live ProviderResolver. Normalize merely trims and lets a
	// non-empty value through; the dispatcher fails fast on unknown names
	// before any slot starts.
	switch r.Model {
	case "", DispatchModelE4B:
		r.Model = DispatchModelE4B
	case DispatchModel26B:
		// keep
	default:
		// Unknown values silently fall back to e4b per the spec — the
		// dispatcher logs the substitution at its own layer if it cares.
		r.Model = DispatchModelE4B
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = dispatchTimeoutDefault
	}
	if r.TimeoutSeconds > dispatchTimeoutMax {
		r.TimeoutSeconds = dispatchTimeoutMax
	}
	if r.N <= 0 {
		r.N = dispatchNDefault
	}
	if r.N > dispatchNMax {
		r.N = dispatchNMax
	}
	// Tools are validated against the live registry by the dispatcher
	// (only it knows what's registered). Trim/dedupe here to keep the
	// downstream adapter simple.
	if len(r.Tools) > 0 {
		r.Tools = dedupeStrings(r.Tools)
	}
	return nil
}

// QueryDispatchToHarness wraps an AgentDispatcher with normalization and the
// "controller installed?" check. Returns ErrAgentUnavailable when the
// controller is nil or doesn't implement AgentDispatcher.
func QueryDispatchToHarness(ctx context.Context, ctrl AgentController, req DispatchRequest) (*DispatchBatchResult, error) {
	if ctrl == nil {
		return nil, ErrAgentUnavailable
	}
	disp, ok := ctrl.(AgentDispatcher)
	if !ok {
		return nil, &AgentControllerError{
			Code:    "unavailable",
			Message: fmt.Sprintf("agent %q does not support dispatch (controller missing AgentDispatcher)", req.AgentID),
		}
	}
	if err := req.Normalize(); err != nil {
		return nil, err
	}
	return disp.DispatchToHarness(ctx, req)
}

// dedupeStrings returns ss with empty strings removed and order-preserving
// dedupe. Allocates only when the input has duplicates or empties.
func dedupeStrings(ss []string) []string {
	if len(ss) == 0 {
		return ss
	}
	seen := make(map[string]struct{}, len(ss))
	out := ss[:0]
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
