package engine

import (
	"net/http"
	"strings"
)

// registerMCPRoutes mounts the MCP Streamable HTTP handler at /mcp.
// Explicit method patterns avoid conflicts with the catch-all GET / dashboard route.
//
// The server's agentController (if set via SetAgentController before or
// after registration) is threaded into the MCP tool registry so the
// cog_list_agents / cog_get_agent_state / cog_trigger_agent_loop tools
// have a backing implementation.
func (s *Server) registerMCPRoutes(mux *http.ServeMux) {
	mcpSrv := NewMCPServerWithAgentController(s.cfg, s.nucleus, s.process, s.agentController)
	// Wire session-management deps so the cog_register_session /
	// cog_offer_handoff / etc. tools can hit the same in-memory registries
	// the HTTP surface uses. The tools fall back to an error message if
	// these are nil — NewMCPServer (used by tests that only care about
	// memory tools) doesn't call this, which is fine.
	mcpSrv.SetSessionsBackend(s.busSessions, s.sessionRegistry, s.handoffRegistry)
	mcpSrv.SetForkRegistry(s.forkRegistry)
	// ADR-082 Wave 3.5: route the mod3 session-family MCP tools through
	// the kernel's shared channel-session methods so session-ID minting
	// happens in exactly one place (this Server). Handlers dispatching
	// to mod3 directly was the Wave 3 divergence this removes.
	mcpSrv.SetChannelSessionBackend(s)
	// G0(b): wire the RBAC harness-binding layer so cog_register_session
	// can create HarnessBindingCRDs for sessions that supply a "subject".
	// Nil when SetHarnessBackend was not called (tests, standalone MCP).
	if s.harnessBackend != nil {
		mcpSrv.SetHarnessBackend(s.harnessBackend)
	}
	// Call any extension hook registered by workspace-root wiring (e.g.
	// eval_wiring.go calling eval.RegisterEvalTools). Nil when not set.
	if RegisterMCPExtensions != nil {
		RegisterMCPExtensions(mcpSrv)
	}
	s.mcpServer = mcpSrv
	h := mcpSrv.Handler()
	s.routeH(mux, "GET /mcp", mcpGetHandler(h))
	s.routeH(mux, "POST /mcp", h)
	s.routeH(mux, "DELETE /mcp", h)
}

// mcpGetHandler wraps the MCP streamable-HTTP handler for GET /mcp.
// A bare GET (no Mcp-Session-Id and Accept does not include text/event-stream)
// is a browser probe — the MCP SDK returns a cryptic 400 in that case, which
// reads as "broken" to smoke-test users. Instead return 405 with a one-liner
// pointing at the JSON-RPC/SSE contract (issue #317).
// Proper MCP GET requests (SSE stream resume) pass through unchanged.
func mcpGetHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		sessionID := r.Header.Get("Mcp-Session-Id")
		isMCPStream := strings.Contains(accept, "text/event-stream")
		if !isMCPStream && sessionID == "" {
			w.Header().Set("Allow", "POST")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "MCP endpoint: use POST /mcp for JSON-RPC or GET /mcp with Accept: text/event-stream + Mcp-Session-Id for SSE stream.", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}
