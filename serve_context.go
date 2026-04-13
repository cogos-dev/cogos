package main

// serve_context.go — TAA context visibility, per-session observability, and foveated context rendering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// foveatedMetrics tracks score distributions over time for foveated context requests.
type foveatedMetrics struct {
	mu              sync.Mutex
	requestCount    int64
	totalDocs       int64
	totalCoherence  float64
	avgCoherence    float64
	totalBudget     int64
	totalPressure   float64
	lastRequestTime time.Time
}

const syntheticCanaryHeader = "X-Cog-Synthetic-Canary"

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
	isSyntheticCanary := strings.EqualFold(r.Header.Get(syntheticCanaryHeader), "true")

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
	if !isSyntheticCanary {
		s.taaStateMutex.Lock()
		s.lastTAAState = ctx
		s.taaStateMutex.Unlock()
	}

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

	if !isSyntheticCanary {
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
	}

	log.Printf("[foveated] Response: tokens=%d blocks=%d anchor=%q goal=%q coherence=%.2f pressure=%.1f%%",
		ctx.TotalTokens, len(blocks), ctx.Anchor, ctx.Goal, ctx.CoherenceScore, irisPressure*100)

	// Per-request score distribution logging (B4)
	log.Printf("[foveated] Scores: docs=%d coherence=%.3f budget=%d pressure=%.1f%% session=%s",
		len(blocks), ctx.CoherenceScore, effectiveBudget, irisPressure*100, sessionID)

	// Update rolling metrics
	if !isSyntheticCanary && s.fceMetrics != nil {
		s.fceMetrics.mu.Lock()
		s.fceMetrics.requestCount++
		s.fceMetrics.totalDocs += int64(len(blocks))
		s.fceMetrics.totalCoherence += ctx.CoherenceScore
		if s.fceMetrics.requestCount > 0 {
			s.fceMetrics.avgCoherence = s.fceMetrics.totalCoherence / float64(s.fceMetrics.requestCount)
		}
		s.fceMetrics.totalBudget += int64(effectiveBudget)
		s.fceMetrics.totalPressure += irisPressure
		s.fceMetrics.lastRequestTime = time.Now()
		s.fceMetrics.mu.Unlock()
	}

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

// handleHealthCanary runs a synthetic smoke-test foveated context request.
// GET /v1/health/canary
func (s *serveServer) handleHealthCanary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	started := time.Now()
	canaryCtx, canaryCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer canaryCancel()

	body, err := json.Marshal(map[string]any{
		"prompt":     "canary health check",
		"profile":    "default",
		"session_id": "health-canary",
	})
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     "failed to build canary request: " + err.Error(),
			"timestamp": nowISO(),
		})
		return
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/context/foveated", bytes.NewReader(body)).WithContext(canaryCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(syntheticCanaryHeader, "true")

	rec := httptest.NewRecorder()
	s.handleFoveatedContext(rec, req)

	if err := canaryCtx.Err(); err != nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     "canary timed out: " + err.Error(),
			"timestamp": nowISO(),
		})
		return
	}

	if rec.Code != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil || strings.TrimSpace(errResp.Error.Message) == "" {
			errResp.Error.Message = fmt.Sprintf("foveated context returned HTTP %d", rec.Code)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     errResp.Error.Message,
			"timestamp": nowISO(),
		})
		return
	}

	var canaryResp struct {
		Context        string  `json:"context"`
		Tokens         int     `json:"tokens"`
		CoherenceScore float64 `json:"coherence_score"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &canaryResp); err != nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     "invalid foveated response: " + err.Error(),
			"timestamp": nowISO(),
		})
		return
	}

	switch {
	case strings.TrimSpace(canaryResp.Context) == "":
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     "foveated response missing context",
			"timestamp": nowISO(),
		})
		return
	case canaryResp.Tokens <= 0:
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     fmt.Sprintf("invalid token count: %d", canaryResp.Tokens),
			"timestamp": nowISO(),
		})
		return
	case canaryResp.CoherenceScore <= 0:
		json.NewEncoder(w).Encode(map[string]any{
			"status":    "fail",
			"error":     fmt.Sprintf("invalid coherence score: %.3f", canaryResp.CoherenceScore),
			"timestamp": nowISO(),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":     "pass",
		"latency_ms": float64(time.Since(started)) / float64(time.Millisecond),
		"tokens":     canaryResp.Tokens,
		"coherence":  canaryResp.CoherenceScore,
		"timestamp":  nowISO(),
	})
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, claudeErr := exec.LookPath(claudeCommand)
	_, codexErr := exec.LookPath(codexCommand)

	status := "healthy"
	var degradedReasons []string

	checks := map[string]any{
		"claude_cli": claudeErr == nil,
		"codex_cli":  codexErr == nil,
	}

	if claudeErr != nil && codexErr != nil {
		status = "degraded"
		degradedReasons = append(degradedReasons, "no CLI providers available")
	}

	// Embedding service check: Ollama at localhost:11434 (2s timeout)
	embeddingOK := false
	ollamaCtx, ollamaCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ollamaCancel()
	ollamaReq, err := http.NewRequestWithContext(ollamaCtx, "GET", "http://localhost:11434/api/tags", nil)
	if err == nil {
		ollamaClient := &http.Client{}
		resp, err := ollamaClient.Do(ollamaReq)
		if err == nil {
			resp.Body.Close()
			embeddingOK = resp.StatusCode == http.StatusOK
		}
	}
	checks["embedding_service"] = embeddingOK
	if !embeddingOK {
		if status == "healthy" {
			status = "degraded"
		}
		degradedReasons = append(degradedReasons, "embedding_service unavailable")
	}

	// TRM model check: look for the ONNX model file
	trmOK := false
	homeDir, err := os.UserHomeDir()
	if err == nil {
		trmPath := filepath.Join(homeDir, ".cache", "cogos-autoresearch", "model_mamba.onnx")
		if fi, err := os.Stat(trmPath); err == nil && fi.Size() > 0 {
			trmOK = true
		}
	}
	checks["trm_model"] = trmOK
	if !trmOK {
		if status == "healthy" {
			status = "degraded"
		}
		degradedReasons = append(degradedReasons, "trm_model not found")
	}

	// Workspace check: kernel root must exist on disk
	workspaceOK := false
	if s.kernel != nil {
		root := s.kernel.Root()
		if root != "" {
			if fi, err := os.Stat(root); err == nil && fi.IsDir() {
				workspaceOK = true
			}
		}
	}
	checks["workspace"] = workspaceOK
	if !workspaceOK {
		status = "unhealthy"
		degradedReasons = append(degradedReasons, "workspace root missing or inaccessible")
	}

	// Bus count: count active buses from the registry
	busCount := 0
	if s.busChat != nil && s.busChat.manager != nil {
		entries := s.busChat.manager.loadRegistry()
		for _, e := range entries {
			if e.State == "active" {
				busCount++
			}
		}
	}
	checks["bus_count"] = busCount

	resp := map[string]any{
		"status":    status,
		"timestamp": nowISO(),
		"checks":    checks,
		"debug":     DebugMode.Load(),
	}
	if len(degradedReasons) > 0 {
		resp["degraded_reasons"] = degradedReasons
	}
	if s.mcpManager != nil {
		resp["mcp"] = map[string]any{
			"sessions": s.mcpManager.SessionCount(),
		}
	}
	if s.pipeline != nil {
		resp["modality_pipeline"] = s.pipeline.Status()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleFoveatedMetrics returns rolling metrics for foveated context requests.
// GET /v1/metrics/foveated
func (s *serveServer) handleFoveatedMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.fceMetrics == nil {
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "no_metrics",
			"message": "Foveated metrics not initialized",
		})
		return
	}

	s.fceMetrics.mu.Lock()
	reqCount := s.fceMetrics.requestCount
	totalDocs := s.fceMetrics.totalDocs
	avgCoherence := s.fceMetrics.avgCoherence
	avgDocs := float64(0)
	if reqCount > 0 {
		avgDocs = float64(totalDocs) / float64(reqCount)
	}
	avgBudget := float64(0)
	if reqCount > 0 {
		avgBudget = float64(s.fceMetrics.totalBudget) / float64(reqCount)
	}
	avgPressure := float64(0)
	if reqCount > 0 {
		avgPressure = s.fceMetrics.totalPressure / float64(reqCount)
	}
	lastReq := s.fceMetrics.lastRequestTime
	s.fceMetrics.mu.Unlock()

	result := map[string]any{
		"request_count": reqCount,
		"total_docs":    totalDocs,
		"avg_docs":      avgDocs,
		"avg_coherence": avgCoherence,
		"avg_budget":    avgBudget,
		"avg_pressure":  avgPressure,
		"timestamp":     nowISO(),
	}
	if !lastReq.IsZero() {
		result["last_request_at"] = lastReq.Format(time.RFC3339)
	}

	json.NewEncoder(w).Encode(result)
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
