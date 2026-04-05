package main

// serve_providers.go — Provider health cache, discovery, and model listing (ADR-046)

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// === PROVIDER HEALTH CACHE ===

// providerHealthCache stores cached health check results
type providerHealthCache struct {
	mu      sync.RWMutex
	results map[ProviderType]*ProviderHealth
	checked map[ProviderType]time.Time
}

var healthCache = &providerHealthCache{
	results: make(map[ProviderType]*ProviderHealth),
	checked: make(map[ProviderType]time.Time),
}

// getProviderDisplayName returns a human-readable name for a provider
func getProviderDisplayName(pt ProviderType) string {
	switch pt {
	case ProviderClaude:
		return "Claude CLI"
	case ProviderCodex:
		return "Codex CLI"
	case ProviderOpenAI:
		return "OpenAI"
	case ProviderOpenRouter:
		return "OpenRouter"
	case ProviderOllama:
		return "Ollama (Local)"
	case ProviderLocal:
		return "Local Kernel"
	case ProviderCustom:
		return "Custom"
	default:
		return string(pt)
	}
}

// getProviderModels returns the default models for a provider
func getProviderModels(pt ProviderType) []string {
	switch pt {
	case ProviderClaude:
		return []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude"}
	case ProviderCodex:
		return []string{"codex", "gpt-5-codex", "codex-mini-latest"}
	case ProviderOpenAI:
		return []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo"}
	case ProviderOpenRouter:
		return []string{"anthropic/claude-3-haiku", "openai/gpt-4o-mini", "google/gemini-pro"}
	case ProviderOllama:
		return []string{"llama3.2", "mistral", "codellama"}
	case ProviderLocal:
		return []string{"claude"} // Routes to Claude by default
	default:
		return []string{}
	}
}

// checkProviderHealth performs a health check on a provider
// Results are cached for 60 seconds to avoid hammering providers
func checkProviderHealth(pt ProviderType, config *ProviderConfig) *ProviderHealth {
	healthCache.mu.RLock()
	if cached, ok := healthCache.results[pt]; ok {
		if checkedAt, ok := healthCache.checked[pt]; ok {
			if time.Since(checkedAt) < 60*time.Second {
				healthCache.mu.RUnlock()
				return cached
			}
		}
	}
	healthCache.mu.RUnlock()

	// Perform fresh health check
	health := &ProviderHealth{}

	// Special handling for local CLIs - check if the binary exists
	if pt == ProviderClaude || pt == ProviderCodex {
		command := claudeCommand
		errMsg := "Claude CLI not found in PATH"
		if pt == ProviderCodex {
			command = codexCommand
			errMsg = "Codex CLI not found in PATH"
		}

		_, err := exec.LookPath(command)
		now := nowISO()
		health.LastCheck = &now
		if err != nil {
			health.Error = &errMsg
		} else {
			latency := 0 // CLI check is instant
			health.LatencyMs = &latency
		}
		healthCache.mu.Lock()
		healthCache.results[pt] = health
		healthCache.checked[pt] = time.Now()
		healthCache.mu.Unlock()
		return health
	}

	// For HTTP providers, make a lightweight request
	if config == nil || config.BaseURL == "" {
		errMsg := "no configuration"
		health.Error = &errMsg
		now := nowISO()
		health.LastCheck = &now
		// Cache the "no configuration" result to avoid repeated checks
		healthCache.mu.Lock()
		healthCache.results[pt] = health
		healthCache.checked[pt] = time.Now()
		healthCache.mu.Unlock()
		return health
	}

	// Make health check request (GET /models is usually lightweight)
	client := &http.Client{Timeout: 5 * time.Second}
	url := config.BaseURL + "/models"

	start := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		now := nowISO()
		health.LastCheck = &now
		errMsg := err.Error()
		health.Error = &errMsg
		return health
	}

	// Add auth header if API key is set
	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if pt == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", "https://cogos.dev")
		req.Header.Set("X-Title", "CogOS Kernel")
	}

	resp, err := client.Do(req)
	latency := int(time.Since(start).Milliseconds())
	now := nowISO()
	health.LastCheck = &now
	health.LatencyMs = &latency

	if err != nil {
		errMsg := err.Error()
		health.Error = &errMsg
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			errMsg := "authentication failed"
			health.Error = &errMsg
		} else if resp.StatusCode >= 400 {
			errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			health.Error = &errMsg
		}
		// Success: health.Error remains nil
	}

	// Cache the result
	healthCache.mu.Lock()
	healthCache.results[pt] = health
	healthCache.checked[pt] = time.Now()
	healthCache.mu.Unlock()

	return health
}

// getProviderStatus determines the status string from health info
func getProviderStatus(health *ProviderHealth, hasAPIKey bool, pt ProviderType) string {
	// Claude CLI doesn't need API key check
	if pt == ProviderClaude || pt == ProviderCodex {
		if health.Error != nil {
			return "offline"
		}
		return "online"
	}

	// For other providers, check if we've done a health check
	if health.LastCheck == nil {
		return "unknown"
	}

	if health.Error != nil {
		// Distinguish between auth errors and connection errors
		if *health.Error == "authentication failed" {
			return "degraded" // We can reach it but auth is wrong
		}
		return "offline"
	}

	return "online"
}

// handleProviders handles GET /v1/providers - list all configured providers
func (s *serveServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get current active provider
	currentActive := getActiveProvider()

	// Get default providers from inference.go
	providers := DefaultProviders()

	// Build response
	var data []ProviderInfo

	// Always include local CLI providers first
	claudeHealth := checkProviderHealth(ProviderClaude, nil)
	claudeStatus := getProviderStatus(claudeHealth, true, ProviderClaude)
	data = append(data, ProviderInfo{
		ID:     string(ProviderClaude),
		Name:   getProviderDisplayName(ProviderClaude),
		Status: claudeStatus,
		Active: currentActive == ProviderClaude,
		Models: getProviderModels(ProviderClaude),
		Config: ProviderPublicConfig{
			BaseURL:   "",   // Claude CLI doesn't use HTTP
			HasAPIKey: true, // Claude CLI uses Anthropic API key internally
		},
		Health: *claudeHealth,
	})

	codexHealth := checkProviderHealth(ProviderCodex, nil)
	codexStatus := getProviderStatus(codexHealth, true, ProviderCodex)
	data = append(data, ProviderInfo{
		ID:     string(ProviderCodex),
		Name:   getProviderDisplayName(ProviderCodex),
		Status: codexStatus,
		Active: currentActive == ProviderCodex,
		Models: getProviderModels(ProviderCodex),
		Config: ProviderPublicConfig{
			BaseURL:   "",
			HasAPIKey: true,
		},
		Health: *codexHealth,
	})

	// Add HTTP providers
	providerOrder := []ProviderType{ProviderOpenAI, ProviderOpenRouter, ProviderOllama, ProviderLocal}
	for _, pt := range providerOrder {
		config := providers[pt]
		if config == nil {
			continue
		}

		health := checkProviderHealth(pt, config)
		hasAPIKey := config.APIKey != ""
		status := getProviderStatus(health, hasAPIKey, pt)

		// Ollama and Local don't require API keys
		if pt == ProviderOllama || pt == ProviderLocal {
			hasAPIKey = true // Mark as "configured" since no key needed
		}

		data = append(data, ProviderInfo{
			ID:     string(pt),
			Name:   getProviderDisplayName(pt),
			Status: status,
			Active: currentActive == pt,
			Models: getProviderModels(pt),
			Config: ProviderPublicConfig{
				BaseURL:   config.BaseURL,
				HasAPIKey: hasAPIKey,
			},
			Health: *health,
		})
	}

	response := ProviderListResponse{
		Object:        "list",
		Data:          data,
		Active:        string(currentActive),
		FallbackChain: []string{string(ProviderOpenRouter), string(ProviderOllama)},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// === ACTIVE PROVIDER STATE ===

// activeProvider tracks the currently active provider (mutable at runtime)
var activeProvider = ProviderClaude
var activeProviderMu sync.RWMutex

// getActiveProvider returns the currently active provider
func getActiveProvider() ProviderType {
	activeProviderMu.RLock()
	defer activeProviderMu.RUnlock()
	return activeProvider
}

// setActiveProvider sets the active provider
func setActiveProvider(pt ProviderType) ProviderType {
	activeProviderMu.Lock()
	defer activeProviderMu.Unlock()
	prev := activeProvider
	activeProvider = pt
	return prev
}

// handleProviderByID handles POST /v1/providers/{id}/activate and /v1/providers/{id}/test
func (s *serveServer) handleProviderByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /v1/providers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		s.writeError(w, http.StatusBadRequest, "Invalid path. Use /v1/providers/{id}/activate or /v1/providers/{id}/test", "invalid_request")
		return
	}

	providerID := parts[0]
	action := parts[1]

	// Validate provider ID
	var providerType ProviderType
	switch providerID {
	case "claude":
		providerType = ProviderClaude
	case "codex":
		providerType = ProviderCodex
	case "openai":
		providerType = ProviderOpenAI
	case "openrouter":
		providerType = ProviderOpenRouter
	case "ollama":
		providerType = ProviderOllama
	case "local":
		providerType = ProviderLocal
	default:
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("Provider '%s' not found", providerID), "not_found")
		return
	}

	switch action {
	case "activate":
		s.handleProviderActivate(w, r, providerType)
	case "test":
		s.handleProviderTest(w, r, providerType)
	default:
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf("Unknown action: %s", action), "invalid_request")
	}
}

// handleProviderActivate sets a provider as the active default
func (s *serveServer) handleProviderActivate(w http.ResponseWriter, r *http.Request, pt ProviderType) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Set the provider as active
	previous := setActiveProvider(pt)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"active":   string(pt),
		"previous": string(previous),
	})
}

// handleProviderTest performs a health check on a specific provider
func (s *serveServer) handleProviderTest(w http.ResponseWriter, r *http.Request, pt ProviderType) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get provider config
	providers := DefaultProviders()
	config := providers[pt]

	// For local CLI providers, we just check if the binary exists
	if pt == ProviderClaude || pt == ProviderCodex {
		start := time.Now()
		command := claudeCommand
		testModel := "claude"
		errLabel := "Claude CLI not found in PATH"
		if pt == ProviderCodex {
			command = codexCommand
			testModel = "codex"
			errLabel = "Codex CLI not found in PATH"
		}

		_, err := exec.LookPath(command)
		latency := int(time.Since(start).Milliseconds())

		status := "online"
		var errMsg *string
		if err != nil {
			status = "offline"
			msg := errLabel
			errMsg = &msg
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"provider":   string(pt),
			"status":     status,
			"latency_ms": latency,
			"test_model": testModel,
			"error":      errMsg,
		})
		return
	}

	// For HTTP providers, make a test request
	if config == nil {
		s.writeError(w, http.StatusInternalServerError, "No configuration for provider", "server_error")
		return
	}

	// Health check by hitting the models endpoint
	client := &http.Client{Timeout: 10 * time.Second}
	url := config.BaseURL + "/models"

	start := time.Now()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	if config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+config.APIKey)
	}

	// OpenRouter-specific headers
	if pt == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", "https://cogos.dev")
		req.Header.Set("X-Title", "CogOS Kernel")
	}

	resp, err := client.Do(req)
	latency := int(time.Since(start).Milliseconds())

	status := "online"
	var errMsg *string

	if err != nil {
		status = "offline"
		msg := err.Error()
		errMsg = &msg
	} else {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = "degraded"
			msg := "authentication failed"
			errMsg = &msg
		} else if resp.StatusCode >= 400 {
			status = "offline"
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			errMsg = &msg
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"provider":   string(pt),
		"status":     status,
		"latency_ms": latency,
		"test_model": config.Model,
		"error":      errMsg,
	})
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	response := ModelListResponse{
		Object: "list",
		Data: []ModelInfo{
			{
				ID:      "claude-opus-4-5-20250929",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
			{
				ID:      "claude-sonnet-4-5-20250929",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
			{
				ID:      "claude",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "anthropic",
			},
			{
				ID:      "codex",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "openai",
			},
			{
				ID:      "gpt-5-codex",
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "openai",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
