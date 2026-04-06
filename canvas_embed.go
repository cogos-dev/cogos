// canvas_embed.go — embeds and serves the canvas-based dashboard
//
// The canvas view is served at GET /canvas from the v3 daemon.
// It provides an infinite-canvas spatial interface with draggable nodes,
// real-time chat, and CogDoc visualization.
package main

import (
	_ "embed"
	"net/http"
)

//go:embed canvas.html
var canvasHTML []byte

// handleCanvas serves the embedded canvas dashboard at GET /canvas.
func handleCanvas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(canvasHTML)
}
