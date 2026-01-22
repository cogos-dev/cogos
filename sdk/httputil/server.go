// Package httputil provides HTTP interfaces for the CogOS SDK.
//
// It includes:
//   - Server: HTTP server exposing SDK operations as REST endpoints
//   - Client: HTTP client for connecting to remote SDK servers
//   - OpenAI-compatible endpoint for inference
//
// # Server Usage
//
//	kernel, _ := sdk.Connect(".")
//	server := httputil.NewServer(kernel)
//	server.ListenAndServe(":8080")
//
// # Routes
//
//	GET  /resolve?uri=cog://...  - Resolve a URI
//	POST /mutate                 - Apply a mutation
//	GET  /health                 - Health check
//	GET  /ws/watch?uri=cog://... - WebSocket for watch events
//	POST /v1/chat/completions    - OpenAI-compatible inference
package httputil

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// SRC Constants for signal decay calculations (from ontological crystal)
const (
	Tau1          = 0.693147180559945 // ln(2) - single distinction cost
	Tau2          = 1.386294361119891 // 2*ln(2) - stability threshold
	VarianceRatio = 6.0               // Thought/action efficiency (2×3)
	GEff          = 0.333333333333333 // Self-reference coupling (1/3)
	Rho0          = 0.816496580927726 // √(2/3) - correlation coefficient
)

// Server wraps a kernel as HTTP endpoints.
type Server struct {
	kernel  *sdk.Kernel
	mux     *http.ServeMux
	openai  *OpenAIHandler
	server  *http.Server
	mu      sync.Mutex
	started bool
}

// NewServer creates a new HTTP server for the given kernel.
func NewServer(k *sdk.Kernel) *Server {
	s := &Server{
		kernel: k,
		mux:    http.NewServeMux(),
		openai: NewOpenAIHandler(k),
	}

	// Register routes
	s.mux.HandleFunc("GET /resolve", s.handleResolve)
	s.mux.HandleFunc("POST /mutate", s.handleMutate)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /ws/watch", s.handleWatch)

	// Whirlpool endpoints (workspace state for widgets)
	s.mux.HandleFunc("GET /state", s.handleState)
	s.mux.HandleFunc("GET /signals", s.handleSignals)

	// OpenAI-compatible endpoint
	s.mux.HandleFunc("POST /v1/chat/completions", s.openai.ServeHTTP)

	// Convenience routes
	s.mux.HandleFunc("GET /", s.handleRoot)

	return s
}

// Handler returns the HTTP handler for use with custom servers.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe starts the HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("server already started")
	}
	s.started = true
	s.server = &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	s.mu.Unlock()

	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// handleRoot returns basic server info.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	info := map[string]any{
		"name":    "CogOS SDK Server",
		"version": sdk.Version,
		"endpoints": []string{
			"GET /resolve?uri=cog://...",
			"POST /mutate",
			"GET /health",
			"GET /state",
			"GET /signals",
			"GET /ws/watch?uri=cog://...",
			"POST /v1/chat/completions",
		},
	}

	writeJSON(w, http.StatusOK, info)
}

// handleResolve handles GET /resolve?uri=cog://...
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		writeError(w, http.StatusBadRequest, "missing 'uri' query parameter")
		return
	}

	resource, err := s.kernel.ResolveContext(r.Context(), uri)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "invalid") {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resource)
}

// MutateRequest is the request body for POST /mutate.
type MutateRequest struct {
	URI      string         `json:"uri"`
	Op       string         `json:"op"`
	Content  string         `json:"content,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// handleMutate handles POST /mutate
func (s *Server) handleMutate(w http.ResponseWriter, r *http.Request) {
	var req MutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.URI == "" {
		writeError(w, http.StatusBadRequest, "missing 'uri' field")
		return
	}

	// Convert to SDK mutation
	var mutation *sdk.Mutation
	content := []byte(req.Content)
	switch sdk.MutationOp(req.Op) {
	case sdk.MutationSet:
		mutation = sdk.NewSetMutation(content)
	case sdk.MutationPatch:
		mutation = sdk.NewPatchMutation(content)
	case sdk.MutationAppend:
		mutation = sdk.NewAppendMutation(content)
	case sdk.MutationDelete:
		mutation = sdk.NewDeleteMutation()
	default:
		writeError(w, http.StatusBadRequest, "invalid 'op' field: "+req.Op)
		return
	}

	if req.Metadata != nil {
		for k, v := range req.Metadata {
			mutation.WithMetadata(k, v)
		}
	}

	if err := s.kernel.MutateContext(r.Context(), req.URI, mutation); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "read-only") {
			status = http.StatusMethodNotAllowed
		}
		writeError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"uri":     req.URI,
	})
}

// handleHealth handles GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]any{
		"status":  "healthy",
		"version": sdk.Version,
		"root":    s.kernel.Root(),
	}

	// Check coherence
	coherence, err := s.kernel.Resolve("cog://coherence")
	if err == nil {
		var state map[string]any
		if err := coherence.JSON(&state); err == nil {
			health["coherence"] = state
		}
	}

	writeJSON(w, http.StatusOK, health)
}

// handleWatch handles GET /ws/watch?uri=cog://...
// This upgrades to a WebSocket connection for real-time watch events.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		writeError(w, http.StatusBadRequest, "missing 'uri' query parameter")
		return
	}

	// Upgrade to WebSocket
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to upgrade: "+err.Error())
		return
	}
	defer c.CloseNow()

	// Create watcher
	ctx := r.Context()
	watcher, err := s.kernel.WatchURI(ctx, uri)
	if err != nil {
		c.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer watcher.Close()

	// Send initial message
	wsjson.Write(ctx, c, map[string]any{
		"type":    "connected",
		"uri":     uri,
		"message": "watching for changes",
	})

	// Forward events to WebSocket
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "context cancelled")
			return
		case event, ok := <-watcher.Events:
			if !ok {
				c.Close(websocket.StatusNormalClosure, "watcher closed")
				return
			}

			// Send event as JSON
			msg := map[string]any{
				"type":      "event",
				"uri":       event.URI,
				"eventType": event.Type,
				"timestamp": event.Timestamp,
			}
			if event.Resource != nil {
				msg["resource"] = event.Resource
			}

			if err := wsjson.Write(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
	})
}

// === Whirlpool Endpoints ===
// These endpoints expose workspace state for cog-native widgets.

// WorkspaceState represents the full workspace snapshot for widgets.
type WorkspaceState struct {
	Coherent       bool                           `json:"coherent"`
	CanonicalHash  string                         `json:"canonical_hash,omitempty"`
	CurrentHash    string                         `json:"current_hash,omitempty"`
	Drift          []string                       `json:"drift,omitempty"`
	Signals        map[string][]SignalWithDecay   `json:"signals"`
	SignalCount    int                            `json:"signal_count"`
	TotalRelevance float64                        `json:"total_relevance"`
	SessionID      string                         `json:"session_id,omitempty"`
	Constants      map[string]float64             `json:"constants"`
	Timestamp      string                         `json:"timestamp"`
}

// SignalWithDecay extends signal data with calculated relevance.
type SignalWithDecay struct {
	SignalType  string         `json:"signal_type"`
	Strength    float64        `json:"strength"`
	DepositedBy string         `json:"deposited_by"`
	DepositedAt float64        `json:"deposited_at"`
	HalfLife    float64        `json:"half_life"`
	DecayType   string         `json:"decay_type"`
	Metadata    map[string]any `json:"metadata"`
	AgeHours    float64        `json:"age_hours"`
	AgeTurns    float64        `json:"age_turns"`
	Relevance   float64        `json:"relevance"`
}

// handleState returns full workspace state for widgets.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state := WorkspaceState{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Constants: map[string]float64{
			"tau1":           Tau1,
			"tau2":           Tau2,
			"variance_ratio": VarianceRatio,
			"g_eff":          GEff,
			"rho0":           Rho0,
		},
		Signals: make(map[string][]SignalWithDecay),
	}

	// Get coherence state via SDK
	coherence, err := s.kernel.Resolve("cog://coherence")
	if err == nil {
		var cohState map[string]any
		if err := coherence.JSON(&cohState); err == nil {
			if coherent, ok := cohState["coherent"].(bool); ok {
				state.Coherent = coherent
			}
			if canonical, ok := cohState["canonical_hash"].(string); ok {
				state.CanonicalHash = canonical
			}
			if current, ok := cohState["current_hash"].(string); ok {
				state.CurrentHash = current
			}
			if drift, ok := cohState["drift"].([]any); ok {
				for _, d := range drift {
					if ds, ok := d.(string); ok {
						state.Drift = append(state.Drift, ds)
					}
				}
			}
		}
	} else {
		state.Coherent = true // Default if can't check
	}

	// Get signals via SDK
	signals, err := s.kernel.Resolve("cog://signals")
	if err == nil {
		var sigField map[string]any
		if err := signals.JSON(&sigField); err == nil {
			now := float64(time.Now().Unix())
			if sigs, ok := sigField["signals"].(map[string]any); ok {
				for location, sigList := range sigs {
					if sigArr, ok := sigList.([]any); ok {
						for _, sig := range sigArr {
							if sigMap, ok := sig.(map[string]any); ok {
								depositedAt, _ := sigMap["deposited_at"].(float64)
								strength, _ := sigMap["strength"].(float64)
								if strength == 0 {
									strength = 1.0
								}
								ageHours := (now - depositedAt) / 3600.0
								ageTurns := ageHours / 4.0 // 4 hours per turn
								relevance := strength * math.Exp(-ageTurns/Tau2)

								swd := SignalWithDecay{
									AgeHours:  ageHours,
									AgeTurns:  ageTurns,
									Relevance: relevance,
									Strength:  strength,
								}
								if st, ok := sigMap["signal_type"].(string); ok {
									swd.SignalType = st
								}
								if db, ok := sigMap["deposited_by"].(string); ok {
									swd.DepositedBy = db
								}
								swd.DepositedAt = depositedAt
								if hl, ok := sigMap["half_life"].(float64); ok {
									swd.HalfLife = hl
								}
								if dt, ok := sigMap["decay_type"].(string); ok {
									swd.DecayType = dt
								}
								if md, ok := sigMap["metadata"].(map[string]any); ok {
									swd.Metadata = md
								}

								state.Signals[location] = append(state.Signals[location], swd)
								state.SignalCount++
								state.TotalRelevance += relevance
							}
						}
					}
				}
			}
		}
	}

	// Get session ID via SDK
	identity, err := s.kernel.Resolve("cog://identity")
	if err == nil {
		var idData map[string]any
		if err := identity.JSON(&idData); err == nil {
			if sid, ok := idData["session_id"].(string); ok {
				state.SessionID = sid
			}
		}
	}

	writeJSON(w, http.StatusOK, state)
}

// handleSignals returns just the signal field with relevance calculations.
func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	result := struct {
		Signals        map[string][]SignalWithDecay `json:"signals"`
		SignalCount    int                          `json:"signal_count"`
		TotalRelevance float64                      `json:"total_relevance"`
		Constants      map[string]float64           `json:"constants"`
		Timestamp      string                       `json:"timestamp"`
	}{
		Signals: make(map[string][]SignalWithDecay),
		Constants: map[string]float64{
			"tau2": Tau2,
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Get signals via SDK
	signals, err := s.kernel.Resolve("cog://signals")
	if err != nil {
		// Return empty signals rather than error
		writeJSON(w, http.StatusOK, result)
		return
	}

	var sigField map[string]any
	if err := signals.JSON(&sigField); err != nil {
		writeJSON(w, http.StatusOK, result)
		return
	}

	now := float64(time.Now().Unix())
	if sigs, ok := sigField["signals"].(map[string]any); ok {
		for location, sigList := range sigs {
			if sigArr, ok := sigList.([]any); ok {
				for _, sig := range sigArr {
					if sigMap, ok := sig.(map[string]any); ok {
						depositedAt, _ := sigMap["deposited_at"].(float64)
						strength, _ := sigMap["strength"].(float64)
						if strength == 0 {
							strength = 1.0
						}
						ageHours := (now - depositedAt) / 3600.0
						ageTurns := ageHours / 4.0
						relevance := strength * math.Exp(-ageTurns/Tau2)

						swd := SignalWithDecay{
							AgeHours:  ageHours,
							AgeTurns:  ageTurns,
							Relevance: relevance,
							Strength:  strength,
						}
						if st, ok := sigMap["signal_type"].(string); ok {
							swd.SignalType = st
						}
						if db, ok := sigMap["deposited_by"].(string); ok {
							swd.DepositedBy = db
						}
						swd.DepositedAt = depositedAt
						if hl, ok := sigMap["half_life"].(float64); ok {
							swd.HalfLife = hl
						}
						if dt, ok := sigMap["decay_type"].(string); ok {
							swd.DecayType = dt
						}
						if md, ok := sigMap["metadata"].(map[string]any); ok {
							swd.Metadata = md
						}

						result.Signals[location] = append(result.Signals[location], swd)
						result.SignalCount++
						result.TotalRelevance += relevance
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, result)
}
