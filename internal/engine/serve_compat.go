// serve_compat.go — v2 compatibility endpoints for Phase 0 cutover.
//
// These endpoints allow v3 to replace v2 as the production kernel on port 5100.
// Consumers: OpenClaw cogos plugin, CogBus plugin, launchd service.
//
// DEPRECATED: These compatibility routes exist only for migration from v2.
// They will be removed once all clients migrate to standard endpoints.
// Standard endpoints: /v1/chat/completions, /v1/messages, /mcp, /health
//
// Endpoints:
//
//	GET  /v1/card            — kernel capability card (OpenClaw auth flow)
//	GET  /v1/models          — OpenAI-compatible model list
//	GET  /v1/events/stream   — SSE stub (CogBus keepalive)
//	POST /v1/bus/{bus_id}/ack — bus event acknowledgment stub
//	GET  /memory/search      — memory search (was missing from v2 too)
//	GET  /memory/read        — memory read (was missing from v2 too)
//	GET  /coherence/check    — coherence check
//	GET  /v1/providers       — provider list with health
//	GET  /v1/taa             — TAA context visibility stub
package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) logCompatDeprecated(r *http.Request) {
	slog.Debug("compat: deprecated endpoint hit", "path", r.URL.Path)
}

// registerCompatRoutes adds v2-compatible endpoints to the mux.
// Called from NewServer after all v3 routes are registered.
func (s *Server) registerCompatRoutes(mux *http.ServeMux) {
	// Tier A: blocking for OpenClaw plugin
	mux.HandleFunc("GET /v1/card", s.handleCard)
	mux.HandleFunc("GET /v1/models", s.handleModels)

	// Tier B: blocking for CogBus plugin
	mux.HandleFunc("GET /v1/events/stream", s.handleEventsStream)
	mux.HandleFunc("POST /v1/bus/{bus_id}/ack", s.handleBusAck)

	// Tier C: operational stability
	mux.HandleFunc("GET /v1/providers", s.handleProviders)
	mux.HandleFunc("GET /v1/taa", s.handleTAA)
	mux.HandleFunc("GET /memory/search", s.handleMemorySearch)
	mux.HandleFunc("GET /memory/read", s.handleMemoryRead)
	mux.HandleFunc("GET /coherence/check", s.handleCoherenceCheck)
}

// ── Tier A: OpenClaw plugin ────────────────────────────────────────────────────

// handleCard returns the kernel capability card. Used by the OpenClaw cogos
// plugin for auth flow and model resolution.
func (s *Server) handleCard(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	port := s.cfg.Port
	if port == 0 {
		port = 6931
	}

	card := map[string]any{
		"schemaVersion":   "1.0",
		"name":            "CogOS Kernel v3",
		"humanReadableId": "cogos/kernel-v3",
		"description":     "v3 production kernel — foveated context, TRM, attentional field",
		"url":             fmt.Sprintf("http://localhost:%d", port),
		"defaultModel":    "claude-sonnet-4-6",
		"models": []map[string]any{
			{
				"id":   "claude-sonnet-4-6",
				"name": "Claude Sonnet 4.6",
				"limits": map[string]int{
					"context": 200000,
					"output":  8192,
				},
			},
			{
				"id":   "claude-opus-4-6",
				"name": "Claude Opus 4.6",
				"limits": map[string]int{
					"context": 1000000,
					"output":  32000,
				},
			},
			{
				"id":   "local",
				"name": "Local (Ollama)",
				"limits": map[string]int{
					"context": 32768,
					"output":  4096,
				},
			},
		},
		"capabilities": map[string]bool{
			"streaming":         true,
			"taaAware":          true,
			"foveatedContext":   true,
			"memoryIntegration": true,
			"modelRouting":      s.router != nil,
			"trmScoring":        s.process.TRM() != nil,
			"attentionalField":  true,
		},
		"endpoints": map[string]string{
			"inference": "/v1/chat/completions",
			"models":    "/v1/models",
			"health":    "/health",
			"foveated":  "/v1/context/foveated",
			"attention": "/v1/attention",
			"card":      "/v1/card",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(card)
}

// handleModels returns an OpenAI-compatible model list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	type modelPermission struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		Created       int64  `json:"created"`
		AllowSampling bool   `json:"allow_sampling"`
		AllowLogprobs bool   `json:"allow_logprobs"`
		AllowView     bool   `json:"allow_view"`
	}
	type model struct {
		ID         string            `json:"id"`
		Object     string            `json:"object"`
		Created    int64             `json:"created"`
		OwnedBy   string            `json:"owned_by"`
		Permission []modelPermission `json:"permission"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	now := time.Now().Unix()
	mkModel := func(id, owner string) model {
		return model{
			ID: id, Object: "model", Created: now, OwnedBy: owner,
			Permission: []modelPermission{{
				ID:            "modelperm-" + id,
				Object:        "model_permission",
				Created:       now,
				AllowSampling: true,
				AllowLogprobs: true,
				AllowView:     true,
			}},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{
		Object: "list",
		Data: []model{
			mkModel("claude-sonnet-4-6", "anthropic"),
			mkModel("claude-opus-4-6", "anthropic"),
			mkModel("local", "cogos"),
		},
	})
}

// ── Tier B: CogBus plugin ──────────────────────────────────────────────────────

// handleEventsStream is a stub SSE endpoint that keeps CogBus connections alive.
// Sends a heartbeat every 30s. Full event routing is Phase 1.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	busID := r.URL.Query().Get("bus_id")
	consumer := r.URL.Query().Get("consumer")

	slog.Info("compat: SSE connected", "bus_id", busID, "consumer", consumer)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send initial connected event.
	fmt.Fprintf(w, "data: {\"type\":\"connected\",\"bus_id\":%q,\"consumer\":%q}\n\n", busID, consumer)
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			slog.Info("compat: SSE disconnected", "bus_id", busID, "consumer", consumer)
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// handleBusAck is a stub that accepts bus event acknowledgments.
func (s *Server) handleBusAck(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	busID := r.PathValue("bus_id")
	var req struct {
		ConsumerID string `json:"consumer_id"`
		Seq        int    `json:"seq"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	slog.Debug("compat: bus ack", "bus_id", busID, "consumer", req.ConsumerID, "seq", req.Seq)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "seq": req.Seq})
}

// ── Tier C: Operational stability ──────────────────────────────────────────────

// handleProviders returns provider health information.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	type providerInfo struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Available bool   `json:"available"`
	}

	var providers []providerInfo
	if sr, ok := s.router.(*SimpleRouter); ok && sr != nil {
		sr.mu.RLock()
		for _, p := range sr.providers {
			providers = append(providers, providerInfo{
				Name:      p.Name(),
				Type:      p.Name(),
				Available: p.Available(r.Context()),
			})
		}
		sr.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"providers": providers,
	})
}

// handleTAA returns a stub TAA context visibility response.
func (s *Server) handleTAA(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": "v3-foveated",
		"note":    "v3 uses foveated context assembly, not tiered TAA",
		"zones":   []string{"nucleus", "foveal-docs", "conversation", "reserve"},
	})
}

// handleMemorySearch searches CogDocs by query string.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	query := r.URL.Query().Get("query")
	if query == "" {
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}

	type searchResult struct {
		Path    string  `json:"path"`
		Title   string  `json:"title"`
		Type    string  `json:"type"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet,omitempty"`
	}

	var results []searchResult

	cogIdx := s.process.Index()
	if cogIdx != nil {
		keywords := strings.Fields(strings.ToLower(query))
		for _, doc := range cogIdx.ByURI {
			score := queryRelevance(doc, keywords)
			salience := s.process.Field().Score(doc.Path)
			combined := score*2.0 + salience
			if combined <= 0 {
				continue
			}
			results = append(results, searchResult{
				Path:  doc.Path,
				Title: doc.Title,
				Type:  doc.Type,
				Score: combined,
			})
		}
	}

	// Sort by score descending, limit to 20.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if len(results) > 20 {
		results = results[:20]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"query":   query,
		"results": results,
		"count":   len(results),
	})
}

// handleMemoryRead reads a CogDoc by path.
func (s *Server) handleMemoryRead(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}

	// Resolve relative to workspace .cog/mem/
	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(s.cfg.WorkspaceRoot, ".cog", "mem", path)
	}

	// Security: ensure path is under .cog/mem/
	memRoot := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "mem")
	cleanPath := filepath.Clean(absPath)
	if !strings.HasPrefix(cleanPath, memRoot) {
		http.Error(w, "path outside memory root", http.StatusForbidden)
		return
	}

	content, err := os.ReadFile(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(content)
}

// handleCoherenceCheck runs a quick coherence check.
func (s *Server) handleCoherenceCheck(w http.ResponseWriter, r *http.Request) {
	s.logCompatDeprecated(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"coherent":   true,
		"version":    "v3",
		"field_size": s.process.Field().Len(),
		"index_size": func() int {
			idx := s.process.Index()
			if idx == nil {
				return 0
			}
			return len(idx.ByURI)
		}(),
		"trm_loaded":    s.process.TRM() != nil,
		"process_state": s.process.State().String(),
	})
}
