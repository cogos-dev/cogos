// mcp_sessions_correlation.go — transport↔harness session correlation (G2 PART A).
//
// When cog_register_session is called, the MCP transport assigns a per-connection
// session ID to the calling client (req.Session.ID()). That transport ID is a
// random token and is NOT the CogOS harness session ID (in.SessionID). Before
// this file, there was no daemon-side map between them, so tool-call attribution
// in tool_observer.go and mcp_server.go (toolIngest) fell back to the nucleus
// name unconditionally.
//
// This file closes the gap: a sync.Map records transport_session_id → {harness
// session id, subject} at register time and exposes a read-only resolver that
// attribution sites call to get the bound subject for a given transport session.
//
// Design decisions:
//   - sync.Map instead of a mutex-guarded map: the access pattern is "write once
//     at register, read many at every tool call" — exactly the access pattern
//     sync.Map is optimised for.
//   - Entries are never deleted explicitly: transport sessions are
//     connection-scoped; when the MCP connection closes, no more tool calls
//     arrive on that transport session anyway. Memory overhead is bounded by the
//     number of concurrent MCP connections, which is small (typically 1-10 per
//     node). A production hardening pass can add expiry if the daemon sees
//     cardinality pressure.
//   - The map lives on MCPServer (not a package-level global) so tests remain
//     isolated and the correlation store is naturally scoped to the server
//     lifetime.
//
// Attribution is safe even without a correlation: when the map has no entry for
// a given transport session ID (or when the transport session ID is empty, as in
// in-process test calls), the resolver returns (ok=false) and the caller falls
// back to nucleus.Name — unchanged from pre-G2 behaviour.
//
// NOTE (follow-on): MCP clients that never call cog_register_session could declare
// their harness session id in the MCP initialize._meta blob. The server's
// OnInitialize hook could record the mapping there, avoiding the need for an
// explicit register call. This is deferred — the register-session path covers
// all Claude Code clients today and is the right seam for the v0.
package engine

import (
	"sync"
)

// transportCorrelationEntry records the harness-session and identity context
// for one transport session established via cog_register_session.
type transportCorrelationEntry struct {
	// HarnessSessionID is the CogOS session identifier supplied by the caller
	// (the `session_id` field of cog_register_session).
	HarnessSessionID string
	// Subject is the OIDC sub slug supplied via the optional `subject` field.
	// Empty when the session was registered without an identity binding.
	Subject string
}

// transportCorrelationStore is the daemon-side transport↔harness correlation map.
// Keyed by MCP transport session ID (req.Session.ID()). Value is
// *transportCorrelationEntry.
//
// A standalone type alias keeps the field declaration on MCPServer clean and
// makes the zero value usable (sync.Map needs no initialisation).
type transportCorrelationStore struct {
	m sync.Map
}

// record stores the correlation for transportSessionID. No-op when
// transportSessionID is empty (e.g. in-process test calls where the MCP SDK
// returns "" from ServerSession.ID()).
func (s *transportCorrelationStore) record(transportSessionID, harnessSessionID, subject string) {
	if transportSessionID == "" {
		return
	}
	s.m.Store(transportSessionID, &transportCorrelationEntry{
		HarnessSessionID: harnessSessionID,
		Subject:          subject,
	})
}

// resolve returns the correlation entry for transportSessionID.
// Returns (nil, false) when the transport session is unknown or the ID is empty.
func (s *transportCorrelationStore) resolve(transportSessionID string) (*transportCorrelationEntry, bool) {
	if transportSessionID == "" {
		return nil, false
	}
	v, ok := s.m.Load(transportSessionID)
	if !ok {
		return nil, false
	}
	entry, _ := v.(*transportCorrelationEntry)
	return entry, entry != nil
}

// ── MCPServer integration ────────────────────────────────────────────────────

// resolveTransportSession looks up the harness-session and subject for the
// given MCP transport session ID. Returns (entry, true) when found, or
// (nil, false) when the transport session has no registered correlation.
//
// Attribution callers use this to replace the unconditional nucleus.Name
// fallback with a per-session identity when the caller has registered with a
// subject. When the return is (nil, false), nucleus.Name is the correct
// attribution (either no session registered, or the session was registered
// without a subject — both are legitimate operating modes).
func (m *MCPServer) resolveTransportSession(transportSessionID string) (*transportCorrelationEntry, bool) {
	return m.correlation.resolve(transportSessionID)
}
