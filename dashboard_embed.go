// dashboard_embed.go — embeds and serves the web dashboard
//
// The dashboard is a single HTML file served at GET / from the v3 daemon.
// No build step, no external dependencies, no separate process.
package main

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML []byte

// handleDashboard serves the embedded dashboard at GET /.
func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(dashboardHTML)
}
