// Package session is the substrate-canonical home for session-tracking
// schema — the data type describing an in-progress CogOS session and its
// spawned/active/reaped agent rosters as persisted to .cog/status/.session.
//
// Per ADR-100 Step 3d, the Tracking struct extracted from the root
// session.go file lives here. Per RFC-034 §3.3 (diagnostic rule): "if the
// behavior can be tested without a running process, it belongs in the
// substrate library." Tracking is a pure schema type — JSON tags only,
// no behavior beyond what struct serialization provides.
//
// The session command handlers (sessionInit, sessionTrackSpawn,
// sessionTrackReap, sessionStatus, sessionEnd) and the runtime
// SessionManager remain kernel-resident because they depend on workspace
// resolution, git invocation, and concurrent state. They consume this
// schema; they do not define it.
//
// Scope note: this package covers session-tracking schema only. The
// richer context-engine session lifecycle types (SessionState,
// SessionRotation, WorkingMemory, SessionManagerConfig) live in the root
// package's context_engine_types.go. Their extraction is a follow-up
// scoping decision pending an operator resolution of which types are
// pure substrate schema versus engine-internal request-pipeline detail.
package session
