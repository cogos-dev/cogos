// serve_dashboard.go — serves the embedded web dashboard at GET /dashboard.
//
// Re-embeds internal/engine/web/dashboard.html so the main kernel server
// (port 6931) can serve the interactive dashboard directly. The cogctl
// dashboard iframe points here.

package main

import (
	_ "embed"
	"net/http"
)

//go:embed internal/engine/web/dashboard.html
var mainDashboardHTML []byte

// handleDashboardPage serves the embedded web dashboard.
func (s *serveServer) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(mainDashboardHTML)
}
