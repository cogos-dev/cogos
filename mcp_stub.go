package main

import "net/http"

// MCPSessionManager stub — full implementation in mcp_http.go (WIP).
type MCPSessionManager struct{}

func NewMCPSessionManager(workspaces map[string]*workspaceContext, root string) *MCPSessionManager {
	return &MCPSessionManager{}
}

func (m *MCPSessionManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "MCP not available", http.StatusNotImplemented)
}

func (m *MCPSessionManager) Stop()            {}
func (m *MCPSessionManager) SessionCount() int { return 0 }
