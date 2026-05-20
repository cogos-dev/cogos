// Package acp implements a Go-native bridge between the CogOS kernel and a
// long-lived Claude Code subprocess driven via stream-json IO.
//
// This is the substrate-side realisation of ADR-093's ManagedSession
// primitive: the kernel owns the claude process, mediates user input into
// it as JSON-lines on stdin, and translates the NDJSON stream-json events
// emitted on stdout into ACP-shaped notifications the dashboard can render.
//
// Status: spike. The package currently provides the minimum viable
// subprocess driver + translator needed to validate the architecture
// end-to-end (one prompt → one response). The full event-type coverage,
// state machine, and substrate integration land in follow-up commits if
// the spike confirms the wire shape is workable.
//
// Wire shape:
//
//	┌──────────────────┐ acp.Subprocess.Send()  ┌────────────────────────┐
//	│  CogOS kernel    │ ───────stdin─────────▶ │  claude --print        │
//	│  (this package)  │                        │  --input-format        │
//	│                  │ ◀──────stdout────────  │  stream-json           │
//	│                  │ acp.Subprocess.Events()│  --output-format       │
//	│                  │                        │  stream-json           │
//	│                  │                        │  --resume <session_id> │
//	└──────────────────┘                        └────────────────────────┘
//
// The wire on the right is Claude Code's stream-json format
// (https://code.claude.com/docs/en/headless). The left side is what the
// kernel hands to upstream consumers — currently a typed event channel,
// later (once ADR-093 lands) wrapped behind acp.ManagedSession and
// optionally surfaced as ACP JSON-RPC over a separate transport.
package acp
