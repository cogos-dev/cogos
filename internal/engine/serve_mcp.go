//go:build mcpserver

package engine

import "net/http"

// registerMCPRoutes mounts the MCP Streamable HTTP handler at /mcp.
// Explicit method patterns avoid conflicts with the catch-all GET / dashboard route.
func (s *Server) registerMCPRoutes(mux *http.ServeMux) {
	mcpSrv := NewMCPServer(s.cfg, s.nucleus, s.process)
	h := mcpSrv.Handler()
	mux.Handle("GET /mcp", h)
	mux.Handle("POST /mcp", h)
	mux.Handle("DELETE /mcp", h)
}
