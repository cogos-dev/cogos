// mcp_sessions_identity.go — session→identity binding path for cog_register_session.
//
// G0(b): activate the dormant HarnessBinding path by connecting the session
// registry to the RBAC binding machinery. When cog_register_session is called
// with an optional "subject" field, this file's HarnessAttacher interface is
// invoked to create a HarnessBindingCRD linking sessionID → subject. When
// "subject" is absent, behavior is exactly as before (no binding created).
//
// The HarnessAttacher interface lives here so internal/engine can hold and call
// it without importing the root main package. The concrete implementation
// (RBACProvider) lives in the root package; the wiring happens in serve.go /
// serve_mcp.go via SetHarnessBackend — the same pattern used for
// SetSessionsBackend and SetChannelSessionBackend.
//
// ResolveHarnessBinding is exposed as a read-only resolver for use by G1
// (per-session gateway resolution) and for testing.
package engine

import (
	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// HarnessAttacher is the narrow interface MCPServer requires from the RBAC
// layer. Implemented by *RBACProvider in the root package. Nil-safe: when no
// backend is wired, session register proceeds without creating any binding.
type HarnessAttacher interface {
	// AttachHarness registers an in-memory HarnessBindingCRD for the session.
	AttachHarness(binding *subidentity.HarnessBindingCRD)
	// ResolveHarnessBinding returns the binding for (sessionID, bindingType),
	// or (nil, false) when no binding exists.
	ResolveHarnessBinding(sessionID, bindingType string) (*subidentity.HarnessBindingCRD, bool)
}

// SetHarnessBackend wires the RBAC harness-binding layer into the MCP server
// so cog_register_session can create HarnessBindingCRDs when "subject" is
// supplied. Safe to call post-construction; nil clears any prior backend.
func (m *MCPServer) SetHarnessBackend(h HarnessAttacher) {
	m.harnessBackend = h
}
