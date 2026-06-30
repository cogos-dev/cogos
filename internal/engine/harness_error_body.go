// harness_error_body.go — Structured is_error bodies for harness tool results.
//
// Wave-2 conformance (ADR-031 authority, RFC-020 four-element subset).
// RFC-020 is UNRATIFIED — elements 3 (classifier intent) and 6 (override)
// are deferred until ratification. The four shipped here are:
//   is_error / code / what / why / fix / norm
// (what + fix + norm are the per-code substrate-legible annotations;
// intent + override are deferred.)
package engine

import "encoding/json"

// HarnessToolError is the structured is_error body returned to the model when
// the harness cannot complete a tool invocation. The JSON shape is the
// four-element subset of RFC-020 authorised by ADR-031 §autonomic-fail-fast.
//
// Deferred fields (RFC-020 §3 and §6 — unratified):
//   - intent     (classifier intent, element 3)
//   - override   (structured override path, element 6)
type HarnessToolError struct {
	IsError bool   `json:"is_error"`
	Code    string `json:"code"`
	What    string `json:"what"`
	Why     string `json:"why"`
	Fix     string `json:"fix"`
	Norm    string `json:"norm"`
}

// MarshalJSON always sets is_error:true regardless of the struct field value,
// ensuring the JSON wire shape is unambiguous.
func (e HarnessToolError) MarshalJSON() ([]byte, error) {
	type wire struct {
		IsError bool   `json:"is_error"`
		Code    string `json:"code"`
		What    string `json:"what"`
		Why     string `json:"why"`
		Fix     string `json:"fix"`
		Norm    string `json:"norm"`
	}
	return json.Marshal(wire{
		IsError: true,
		Code:    e.Code,
		What:    e.What,
		Why:     e.Why,
		Fix:     e.Fix,
		Norm:    e.Norm,
	})
}

// LoopExitErrorBody constructs the is_error body for a tool-loop sentinel
// (ErrToolLoopMaxTurns or ErrToolLoopNoProgress). The sentinel's Code() and
// Error() drive the code and why fields; what/fix/norm are per-code constants
// so the model receives actionable substrate-legible guidance.
// (ADR-031 §autonomic-fail-fast, RFC-020 four-element subset)
func LoopExitErrorBody(s *toolLoopSentinel) HarnessToolError {
	e := HarnessToolError{
		IsError: true,
		Code:    s.Code(),
		Why:     s.Error(),
	}
	switch s.Code() {
	case "max_turns":
		e.What = "The tool loop reached the maximum iteration limit without producing a final text response."
		e.Fix = "Reduce the number of tool calls required to complete the task, or break the task into smaller sub-tasks dispatched separately."
		e.Norm = "harness/loop/max_turns"
	case "no_progress":
		e.What = "The tool loop detected a no-progress condition: the same tool call was repeated without advancing state."
		e.Fix = "Vary the arguments or choose a different tool; avoid repeating identical calls in the same dispatch."
		e.Norm = "harness/loop/no_progress"
	default:
		e.What = "The tool loop exited with an unrecognised sentinel code."
		e.Fix = "Inspect the code field and retry with a narrower task scope."
		e.Norm = "harness/loop/unknown"
	}
	return e
}

// GateBDenialErrorBody constructs the is_error body for a Gate-B denial
// (respond invoked more than once in a single user turn). The reason string
// carries the human-readable why; what/fix/norm are constants.
// (ADR-031 §autonomic-fail-fast, RFC-020 four-element subset)
func GateBDenialErrorBody(reason string) HarnessToolError {
	return HarnessToolError{
		IsError: true,
		Code:    "respond_denied",
		What:    "The respond tool was invoked more than once in a single user turn; only the first invocation publishes.",
		Why:     reason,
		Fix:     "Call respond exactly once per user turn. Consolidate all reply content into a single respond invocation.",
		Norm:    "harness/gate_b/respond_denied",
	}
}
