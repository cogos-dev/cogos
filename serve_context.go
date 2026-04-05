package main

// serve_context.go — TAA context visibility, per-session observability, and foveated context rendering

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// handleTAA returns the TAA context state for debugging/visibility
// This allows clients like cogcode to see what context was constructed
func (s *serveServer) handleTAA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

	s.taaStateMutex.RLock()
	ctx := s.lastTAAState
	s.taaStateMutex.RUnlock()

	if ctx == nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "no_context",
			"message": "No TAA context available (no inference requests yet)",
		})
		return
	}

	// Build tier breakdown
	tiers := make(map[string]any)
	if ctx.Tier1Identity != nil {
		tiers["tier1_identity"] = map[string]any{
			"tokens": ctx.Tier1Identity.Tokens,
			"source": ctx.Tier1Identity.Source,
		}
	}
	if ctx.Tier2Temporal != nil {
		tiers["tier2_temporal"] = map[string]any{
			"tokens": ctx.Tier2Temporal.Tokens,
			"source": ctx.Tier2Temporal.Source,
		}
	}
	if ctx.Tier3Present != nil {
		tiers["tier3_present"] = map[string]any{
			"tokens": ctx.Tier3Present.Tokens,
			"source": ctx.Tier3Present.Source,
		}
	}
	if ctx.Tier4Semantic != nil {
		tiers["tier4_semantic"] = map[string]any{
			"tokens": ctx.Tier4Semantic.Tokens,
			"source": ctx.Tier4Semantic.Source,
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"total_tokens":    ctx.TotalTokens,
		"coherence_score": ctx.CoherenceScore,
		"should_refresh":  ctx.ShouldRefresh,
		"anchor":          ctx.Anchor,
		"goal":            ctx.Goal,
		"tiers":           tiers,
		"timestamp":       nowISO(),
	})
}

// === PER-SESSION CONTEXT OBSERVABILITY ===

// SessionContextState captures the context state for a single session's most recent foveated request.
// This enables per-session observability via GET /v1/sessions/{session_id}/context.
type SessionContextState struct {
	SessionID      string         `json:"session_id"`
	Profile        string         `json:"profile"`
	TurnNumber     int            `json:"turn_number"`
	IrisSize       int            `json:"iris_size"`
	IrisUsed       int            `json:"iris_used"`
	IrisPressure   float64        `json:"iris_pressure"`
	TotalTokens    int            `json:"total_tokens"`
	Blocks         []ContextBlock `json:"blocks"`
	BlockCount     int            `json:"block_count"`
	CacheHits      int            `json:"cache_hits"`
	LastRequestAt  time.Time      `json:"last_request_at"`
	CoherenceScore float64        `json:"coherence_score,omitempty"`
}

var (
	sessionContextStore   = make(map[string]*SessionContextState)
	sessionContextStoreMu sync.RWMutex
)

// recordSessionContext stores the latest context state for a session.
func recordSessionContext(state *SessionContextState) {
	sessionContextStoreMu.Lock()
	defer sessionContextStoreMu.Unlock()
	sessionContextStore[state.SessionID] = state
}

// handleListSessions returns summary metadata for all known sessions.
// GET /v1/sessions
func (s *serveServer) handleListSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	sessionContextStoreMu.RLock()
	defer sessionContextStoreMu.RUnlock()

	type sessionSummary struct {
		SessionID      string    `json:"session_id"`
		Profile        string    `json:"profile"`
		TurnNumber     int       `json:"turn_number"`
		IrisPressure   float64   `json:"iris_pressure"`
		TotalTokens    int       `json:"total_tokens"`
		BlockCount     int       `json:"block_count"`
		CoherenceScore float64   `json:"coherence_score,omitempty"`
		LastRequestAt  time.Time `json:"last_request_at"`
	}

	sessions := make([]sessionSummary, 0, len(sessionContextStore))
	for _, state := range sessionContextStore {
		sessions = append(sessions, sessionSummary{
			SessionID:      state.SessionID,
			Profile:        state.Profile,
			TurnNumber:     state.TurnNumber,
			IrisPressure:   state.IrisPressure,
			TotalTokens:    state.TotalTokens,
			BlockCount:     state.BlockCount,
			CoherenceScore: state.CoherenceScore,
			LastRequestAt:  state.LastRequestAt,
		})
	}

	json.NewEncoder(w).Encode(map[string]any{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// handleSessionContext returns the full context state for a specific session.
// GET /v1/sessions/{session_id}/context
func (s *serveServer) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract session_id from URL path: /v1/sessions/{session_id}/context
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	// path is now "{session_id}/context" or "{session_id}"
	sessionID := strings.TrimSuffix(path, "/context")
	sessionID = strings.TrimSuffix(sessionID, "/")

	if sessionID == "" {
		s.writeError(w, http.StatusBadRequest, "session_id is required in path: /v1/sessions/{session_id}/context", "invalid_request")
		return
	}

	sessionContextStoreMu.RLock()
	state, ok := sessionContextStore[sessionID]
	sessionContextStoreMu.RUnlock()

	if !ok {
		s.writeError(w, http.StatusNotFound,
			fmt.Sprintf("No context found for session %q. Use GET /v1/sessions to list known sessions.", sessionID),
			"not_found")
		return
	}

	json.NewEncoder(w).Encode(state)
}

// handleFoveatedContext renders context at variable resolution driven by iris signals.
//
// The iris (agent's context window state) determines the effective budget,
// and score-based thresholds determine which content gets full vs. reduced resolution.
//
// POST /v1/context/foveated
// Body: { prompt, iris: { size, used }, profile, session_id, user_id }
// Response: { context, tokens, anchor, goal, coherence_score, tier_breakdown, effective_budget, iris_pressure }
func (s *serveServer) handleFoveatedContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18) // 256KB limit
	var req struct {
		Prompt string `json:"prompt"`
		Iris   struct {
			Size int `json:"size"`
			Used int `json:"used"`
		} `json:"iris"`
		Profile   string `json:"profile"`
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	if req.Prompt == "" {
		s.writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request")
		return
	}

	// Defaults
	profileName := req.Profile
	if profileName == "" {
		profileName = "default"
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "foveated-" + nowISO()
	}

	workspaceRoot := ""
	if s.kernel != nil {
		workspaceRoot = s.kernel.Root()
	} else if ws := s.getWorkspace(""); ws != nil {
		workspaceRoot = ws.root
	}

	if workspaceRoot == "" {
		s.writeError(w, http.StatusInternalServerError, "No workspace root available", "server_error")
		return
	}

	// Build messages from prompt (minimal — the plugin sends just the prompt)
	promptJSON, _ := json.Marshal(req.Prompt)
	messages := []ChatMessage{
		{Role: "user", Content: json.RawMessage(promptJSON)},
	}

	// Compute iris pressure
	irisPressure := float64(0)
	if req.Iris.Size > 0 {
		irisPressure = float64(req.Iris.Used) / float64(req.Iris.Size)
	}

	log.Printf("[foveated] Request: iris_size=%d iris_used=%d pressure=%.1f%% profile=%s session=%s",
		req.Iris.Size, req.Iris.Used, irisPressure*100, profileName, sessionID)

	// Construct context with iris-driven budgets
	var ctx *ContextState
	var err error

	if req.Iris.Size > 0 {
		ctx, err = ConstructContextStateWithIris(messages, sessionID, workspaceRoot, profileName, req.Iris.Size, req.Iris.Used)
	} else {
		ctx, err = ConstructContextStateWithProfile(messages, sessionID, workspaceRoot, profileName)
	}

	if err != nil {
		log.Printf("[foveated] Construction error (partial result returned): %v", err)
		// Continue — partial results are still useful
	}

	if ctx == nil {
		s.writeError(w, http.StatusInternalServerError, "Context construction returned nil", "server_error")
		return
	}

	// Store as last TAA state for /v1/taa visibility
	s.taaStateMutex.Lock()
	s.lastTAAState = ctx
	s.taaStateMutex.Unlock()

	// Build response — stability-ordered blocks
	contextStr, blocks := ctx.BuildOrderedContextString()
	if contextStr == "" {
		// Fallback to legacy tier-ordered output if decomposition yields nothing
		contextStr = ctx.BuildContextString()
	}

	tierBreakdown := map[string]int{}
	if ctx.Tier1Identity != nil {
		tierBreakdown["tier1"] = ctx.Tier1Identity.Tokens
	}
	if ctx.Tier2Temporal != nil {
		tierBreakdown["tier2"] = ctx.Tier2Temporal.Tokens
	}
	if ctx.Tier3Present != nil {
		tierBreakdown["tier3"] = ctx.Tier3Present.Tokens
	}
	if ctx.Tier4Semantic != nil {
		tierBreakdown["tier4"] = ctx.Tier4Semantic.Tokens
	}

	effectiveBudget := ctx.TotalTokens
	if req.Iris.Size > 0 {
		available := req.Iris.Size - req.Iris.Used
		if available > 0 {
			effectiveBudget = available
		}
	}

	// Record per-session context state for observability
	turnNumber := 0
	cacheHits := 0
	sessionContextStoreMu.RLock()
	if prev, ok := sessionContextStore[sessionID]; ok {
		turnNumber = prev.TurnNumber + 1
		// Count cache hits: blocks whose hash matches a block in the previous state
		prevHashes := make(map[string]bool, len(prev.Blocks))
		for _, b := range prev.Blocks {
			prevHashes[b.Hash] = true
		}
		for _, b := range blocks {
			if prevHashes[b.Hash] {
				cacheHits++
			}
		}
	}
	sessionContextStoreMu.RUnlock()

	recordSessionContext(&SessionContextState{
		SessionID:      sessionID,
		Profile:        profileName,
		TurnNumber:     turnNumber,
		IrisSize:       req.Iris.Size,
		IrisUsed:       req.Iris.Used,
		IrisPressure:   irisPressure,
		TotalTokens:    ctx.TotalTokens,
		Blocks:         blocks,
		BlockCount:     len(blocks),
		CacheHits:      cacheHits,
		LastRequestAt:  time.Now(),
		CoherenceScore: ctx.CoherenceScore,
	})

	log.Printf("[foveated] Response: tokens=%d blocks=%d anchor=%q goal=%q coherence=%.2f pressure=%.1f%%",
		ctx.TotalTokens, len(blocks), ctx.Anchor, ctx.Goal, ctx.CoherenceScore, irisPressure*100)

	json.NewEncoder(w).Encode(map[string]any{
		"context":          contextStr,
		"tokens":           ctx.TotalTokens,
		"anchor":           ctx.Anchor,
		"goal":             ctx.Goal,
		"coherence_score":  ctx.CoherenceScore,
		"tier_breakdown":   tierBreakdown,
		"effective_budget": effectiveBudget,
		"iris_pressure":    irisPressure,
		"blocks":           blocks,
	})
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, claudeErr := exec.LookPath(claudeCommand)
	_, codexErr := exec.LookPath(codexCommand)
	status := "healthy"
	if claudeErr != nil && codexErr != nil {
		status = "degraded"
	}

	resp := map[string]any{
		"status":    status,
		"timestamp": nowISO(),
		"claude":    claudeErr == nil,
		"codex":     codexErr == nil,
		"debug":     DebugMode.Load(),
	}
	if s.mcpManager != nil {
		resp["mcp"] = map[string]any{
			"sessions": s.mcpManager.SessionCount(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *serveServer) handleDebug(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

	switch r.Method {
	case "GET":
		// Return current debug state
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode.Load(),
		})
	case "POST":
		// Toggle or set debug mode
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64KB limit
		var req struct {
			Debug *bool `json:"debug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Toggle if no body (or body too large)
			DebugMode.Store(!DebugMode.Load())
		} else if req.Debug != nil {
			DebugMode.Store(*req.Debug)
		} else {
			DebugMode.Store(!DebugMode.Load())
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"debug": DebugMode.Load(),
		})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

// handleServices provides service status and management via launchd
func (s *serveServer) handleServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// CORS is handled by corsMiddleware

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case "GET":
		// Return status of all services
		services := s.getServicesStatus()
		json.NewEncoder(w).Encode(services)

	case "POST":
		// Restart a service
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64KB limit
		var req struct {
			Service string `json:"service"` // "kernel" or "cog-chat"
			Action  string `json:"action"`  // "restart", "start", "stop"
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
			return
		}

		// Validate service name
		validServices := map[string]string{
			"kernel":   "com.cogos.kernel",
			"cog-chat": "com.cogos.cog-chat",
		}
		launchdLabel, ok := validServices[req.Service]
		if !ok {
			s.writeError(w, http.StatusBadRequest, "Unknown service: "+req.Service, "invalid_request")
			return
		}

		// Execute launchctl command with timeout to prevent kernel hang
		var cmd *exec.Cmd
		uid := os.Getuid()
		launchctlCtx, launchctlCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer launchctlCancel()
		switch req.Action {
		case "restart":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "start":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		case "stop":
			cmd = exec.CommandContext(launchctlCtx, "launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		default:
			s.writeError(w, http.StatusBadRequest, "Unknown action: "+req.Action, "invalid_request")
			return
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   err.Error(),
				"output":  string(output),
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"service": req.Service,
			"action":  req.Action,
			"output":  string(output),
		})

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

// getServicesStatus checks the status of managed services
// Service ports are registered in cog://conf/ports
func (s *serveServer) getServicesStatus() map[string]interface{} {
	// Port assignments from cog://conf/ports registry
	services := []struct {
		name   string
		label  string
		port   int
		health string
	}{
		{"kernel", "com.cogos.kernel", 5100, "http://localhost:5100/health"},         // cog://conf/ports#kernel
		{"cog-chat", "com.cogos.cog-chat", 8765, "http://localhost:8765/api/health"}, // cog://conf/ports#cog-chat
	}

	result := make(map[string]interface{})
	serviceList := make([]map[string]interface{}, 0)

	for _, svc := range services {
		status := map[string]interface{}{
			"name":     svc.name,
			"label":    svc.label,
			"port":     svc.port,
			"running":  false,
			"healthy":  false,
			"launchd":  false,
			"pid":      nil,
			"exitCode": nil,
		}

		// Check launchd status (with timeout to prevent kernel hang)
		listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(listCtx, "launchctl", "list", svc.label)
		output, err := cmd.Output()
		listCancel()
		if err == nil {
			status["launchd"] = true
			// Parse output: "PID\tStatus\tLabel"
			lines := strings.Split(string(output), "\n")
			if len(lines) > 0 {
				fields := strings.Fields(lines[0])
				if len(fields) >= 2 {
					if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
						status["pid"] = pid
						status["running"] = true
					}
					if exitCode, err := strconv.Atoi(fields[1]); err == nil {
						status["exitCode"] = exitCode
					}
				}
			}
		}

		// Check health endpoint
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(svc.health)
		if err == nil {
			status["healthy"] = resp.StatusCode == http.StatusOK
			resp.Body.Close()
			status["running"] = true // If health responds, it's running
		}

		serviceList = append(serviceList, status)
	}

	result["services"] = serviceList
	result["timestamp"] = time.Now().Format(time.RFC3339)

	return result
}
