// Package conductor is the CogOS conductor: the substrate occupying the ACP
// *client* role, conducting a fleet of orchestrating ACP-*server* cells (Hermes
// first, then others) over Zed's Agent Client Protocol. It is the operational
// realization of RFC-036 ("The Conductor — CogOS-as-ACP-Client as the
// Substrate's Conducting Seat") and ADR-062's Layer 2 (ACP Conductor).
//
// Status: scaffold. This package ports the validated integration spike
// (race-clean, offline) into the kernel as the foundation for the real
// conductor build. What is real and tested here:
//
//   - contract.Conductor — the portable, SDK-free ACP-client interface every
//     RFC-036 verb maps onto (initialize, new_session, prompt, cancel, the fleet
//     verbs fork/list/resume/load, set_mode/set_config_option, close).
//   - adapter.SDKConductor — the only SDK-importing backing, translating between
//     coder/acp-go-sdk wire types and the contract's substrate-shaped types, and
//     wiring the SDK client-side handlers to the substrate collaborators.
//   - The injected collaborators contract.Governor / contract.LedgerSink /
//     contract.FsPolicy — the consent boundary, the stigmergic persistence
//     boundary, and the filesystem policy gate.
//   - The full test suite (in-process fake cell over io.Pipe + a real stdio
//     subprocess), the executable spec for the contract and for any future
//     native ACP-client reimplementation.
//
// What is stubbed (the scaffold boundary — application logic, NOT the contract):
//
//   - TODO(conductor, RFC-036 P3+P2): the fleet manager driving N concurrent
//     sessions. The contract multiplexes sessions cleanly (validated N=8
//     race-clean) but the orchestration loop that owns the fleet lifecycle is
//     not built here.
//   - TODO(conductor, RFC-036 P5 / ADR-013): wire contract.LedgerSink to the
//     real kernel ledger. Today the tests use a capture sink; production routes
//     every session/update to the cogos ledger as a stigmergic trace.
//   - TODO(conductor, RFC-036 P4 / RFC-009): wire contract.Governor to the
//     RFC-009 Proposer/Actor consent boundary. Today the tests inject a func
//     governor; production routes permission requests through the substrate's
//     policy/operator decision.
//   - TODO(conductor, RFC-036 P3 gap 6): populate contract.Identity (_meta
//     iss/sub) from a real HarnessBinding identity. Today the tests inject a
//     fixed test identity; production derives it from the substrate binding
//     model (K8s-RBAC framing).
//
// Relationship to internal/acp: this package is the ACP *client* side (the
// conductor drives cells via coder/acp-go-sdk's ClientSideConnection).
// internal/acp is the claude-subprocess *server/driver* side — ADR-093's
// stream-json ManagedSession primitive that owns a claude process. They sit on
// opposite ends of the ACP relationship. Future unification behind one
// ManagedSession-shaped surface is possible but is explicitly out of scope here;
// this scaffold does not modify internal/acp.
package conductor
