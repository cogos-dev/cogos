//go:build !mcpserver

package engine

import "net/http"

// registerMCPRoutes is a no-op when built without the mcpserver tag.
func (s *Server) registerMCPRoutes(_ *http.ServeMux) {}
