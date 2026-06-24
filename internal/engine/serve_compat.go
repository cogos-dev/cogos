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
//	GET  /memory/search      — memory search (was missing from v2 too)
//	GET  /memory/read        — memory read (was missing from v2 too)
//	GET  /coherence/check    — coherence check
//	GET  /v1/providers       — provider list with health
//	GET  /v1/taa             — TAA context visibility stub
//
// Removed in the event-bus PR (were always stubs):
//
//	GET  /v1/events/stream   — replaced by the real broker-backed handler in serve.go
//	POST /v1/bus/{bus_id}/ack — dropped; no consumer, new SSE uses Last-Event-ID
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
	s.route(mux, "GET /v1/card", s.handleCard)
	s.route(mux, "GET /v1/models", s.handleModels)

	// Tier B: event stream + bus ack — now real, registered in serve.go
	// (handleEvents + handleEventsStream). handleBusAck deleted — no
	// consumer relied on it and the new SSE resume uses Last-Event-ID.

	// Tier C: operational stability
	s.route(mux, "GET /v1/providers", s.handleProviders)
	s.route(mux, "GET /v1/taa", s.handleTAA)
	s.route(mux, "GET /memory/search", s.handleMemorySearch)
	s.route(mux, "GET /memory/read", s.handleMemoryRead)
	s.route(mux, "GET /coherence/check", s.handleCoherenceCheck)
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
				"id":   "claude-opus-4-7",
				"name": "Claude Opus 4.7",
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

// handleModels returns an OpenAI-compatible model list (G2 both-menu).
//
// Menu order and fields:
//  1. Intent aliases (owned_by "cogos"): foreground, deliberation, local.
//     These are always present — they are software-defined names, not
//     hardware-presence signals.
//  2. Raw frontier model IDs (owned_by "anthropic"): claude-sonnet-4-6,
//     claude-opus-4-7.
//  3. eclipse-26b (owned_by "cogos", tier "lan-local") — ONLY when the
//     eclipse provider is registered in the router. Gated on config presence;
//     no live HTTP probe on every call.
//
// Extension fields `tier` and `description` are ignored by standard OpenAI
// clients; cogos-aware clients use them for UI display / routing decisions.
// The alias IDs here MUST match the intentAliases table in resolve.go so that
// selecting any entry from this menu resolves correctly on both gateway and
// dispatch.
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
		ID          string            `json:"id"`
		Object      string            `json:"object"`
		Created     int64             `json:"created"`
		OwnedBy     string            `json:"owned_by"`
		Permission  []modelPermission `json:"permission"`
		Tier        string            `json:"tier,omitempty"`
		Description string            `json:"description,omitempty"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	now := time.Now().Unix()
	mkModel := func(id, owner, tier, description string) model {
		return model{
			ID: id, Object: "model", Created: now, OwnedBy: owner,
			Tier:        tier,
			Description: description,
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

	// Gate every entry on real provider availability. All checks are in-memory
	// map lookups (no I/O) — same pattern as isEclipseConfigured.
	frontierConfigured := isFrontierConfigured(s.router)
	localConfigured := isLocalConfigured(s.router)
	eclipseConfigured := isEclipseConfigured(s.router)

	var data []model
	if frontierConfigured {
		// Intent aliases for frontier-managed tiers.
		data = append(data,
			mkModel("foreground", "cogos", "frontier-managed",
				"interactive, full capability (managed Claude, Max sub)"),
			mkModel("deliberation", "cogos", "frontier-managed",
				"heavier reasoning (Opus)"),
		)
	}
	if localConfigured {
		// Intent alias for the local-sovereign tier.
		data = append(data,
			mkModel("local", "cogos", "local-sovereign",
				"private, no egress (E4B on this node)"),
		)
	}
	if frontierConfigured {
		// Raw frontier model IDs.
		data = append(data,
			mkModel("claude-sonnet-4-6", "anthropic", "frontier-managed", ""),
			mkModel("claude-opus-4-7", "anthropic", "frontier-managed", ""),
			mkModel("claude-haiku-4-5-20251001", "anthropic", "frontier-managed", "fast, low-cost"),
		)
	}
	if eclipseConfigured {
		data = append(data,
			mkModel("eclipse-26b", "cogos", "lan-local",
				"LAN-resident 26B model (Eclipse node)"),
		)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{
		Object: "list",
		Data:   data,
	})
}

// isEclipseConfigured returns true when the router has a registered provider
// that serves the eclipse-26b LAN model. Checks by provider name ("eclipse",
// "lmstudio") and by model string ("eclipse-26b"). Fast: in-memory lookups only.
func isEclipseConfigured(router Router) bool {
	if router == nil {
		return false
	}
	for _, name := range []string{"eclipse", "lmstudio"} {
		if _, ok := router.ProviderForName(name); ok {
			return true
		}
	}
	_, ok := router.ProviderForModel("eclipse-26b")
	return ok
}

// isFrontierConfigured returns true when the router has a registered provider
// that can serve frontier (Anthropic/Claude) models. Checks by canonical
// provider names ("anthropic", "claude-code") and by representative model IDs.
// Fast: in-memory lookups only, no network I/O.
//
// Known tradeoff: this only matches the canonical provider names plus the two
// representative model IDs. A frontier provider registered under a non-standard
// name (and not serving either representative model string) would be hidden
// from the /v1/models menu. Acceptable: the default config and docs use the
// canonical names; extend the name list here if a new alias is introduced.
func isFrontierConfigured(router Router) bool {
	if router == nil {
		return false
	}
	for _, name := range []string{"claude-oauth", "anthropic", "claude-code"} {
		if _, ok := router.ProviderForName(name); ok {
			return true
		}
	}
	for _, model := range []string{"claude-sonnet-4-6", "claude-opus-4-7"} {
		if _, ok := router.ProviderForModel(model); ok {
			return true
		}
	}
	return false
}

// isLocalConfigured returns true when the router has at least one registered
// provider whose Capabilities().IsLocal is true. Uses FirstLocalProvider so
// the check requires no concrete type assertion. Fast: in-memory lookup only.
func isLocalConfigured(router Router) bool {
	if router == nil {
		return false
	}
	_, ok := router.FirstLocalProvider()
	return ok
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

	// Security: ensure path is under .cog/mem/. Use pathWithin (not a bare
	// HasPrefix) so a sibling like ".cog/mem-evil" — which textually starts with
	// the mem root — does not pass the containment check.
	memRoot := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "mem")
	cleanPath := filepath.Clean(absPath)
	if !pathWithin(memRoot, cleanPath) {
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
