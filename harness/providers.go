// providers.go defines inference provider types and model routing.
//
// ParseModelProvider is the routing function — it takes a model string from the
// request and returns which provider to use. This is called early in both
// RunInference and RunInferenceStream to decide the Claude CLI vs HTTP path.
//
// DefaultProviders returns the built-in provider configs. API keys come from
// environment variables (OPENAI_API_KEY, OPENROUTER_API_KEY). Ollama and local
// providers don't require keys.
package harness

import (
	"os"
	"strings"
)

// ProviderType identifies the inference provider.
type ProviderType string

const (
	ProviderClaude     ProviderType = "claude"     // Claude CLI (default)
	ProviderOpenAI     ProviderType = "openai"     // OpenAI API
	ProviderOpenRouter ProviderType = "openrouter" // OpenRouter API
	ProviderOllama     ProviderType = "ollama"     // Ollama (local)
	ProviderLocal      ProviderType = "local"      // Local kernel endpoint (self-reference)
	ProviderCustom     ProviderType = "custom"     // Any OpenAI-compatible endpoint
)

// ProviderConfig holds configuration for an inference provider
type ProviderConfig struct {
	Type    ProviderType `json:"type"`
	BaseURL string       `json:"base_url"`
	APIKey  string       `json:"api_key"`
	Model   string       `json:"model"` // Default model for this provider
}

// DefaultProviders returns the default provider configurations.
// API keys are read from environment variables.
func DefaultProviders() map[ProviderType]*ProviderConfig {
	// Ollama port can be customized via OLLAMA_HOST
	ollamaHost := os.Getenv("OLLAMA_HOST")
	if ollamaHost == "" {
		ollamaHost = "http://localhost:11434"
	}

	// Local kernel port
	localPort := os.Getenv("COG_KERNEL_PORT")
	if localPort == "" {
		localPort = "5100"
	}

	return map[ProviderType]*ProviderConfig{
		ProviderOpenAI: {
			Type:    ProviderOpenAI,
			BaseURL: "https://api.openai.com/v1",
			APIKey:  os.Getenv("OPENAI_API_KEY"),
			Model:   "gpt-4o-mini",
		},
		ProviderOpenRouter: {
			Type:    ProviderOpenRouter,
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  os.Getenv("OPENROUTER_API_KEY"),
			Model:   "anthropic/claude-3-haiku",
		},
		ProviderOllama: {
			Type:    ProviderOllama,
			BaseURL: ollamaHost + "/v1", // Ollama's OpenAI-compatible endpoint
			APIKey:  "",                 // Ollama doesn't require API key
			Model:   "llama3.2",
		},
		ProviderLocal: {
			Type:    ProviderLocal,
			BaseURL: "http://localhost:" + localPort + "/v1",
			APIKey:  "",       // Local kernel doesn't require API key
			Model:   "claude", // Route to Claude by default
		},
	}
}

// ParseModelProvider extracts the provider and model from a model string.
// Formats:
//   - "claude" or "" -> (ProviderClaude, "claude")
//   - "openai/gpt-4o" -> (ProviderOpenAI, "gpt-4o")
//   - "openrouter/anthropic/claude-3-haiku" -> (ProviderOpenRouter, "anthropic/claude-3-haiku")
//   - "ollama/llama3.2" -> (ProviderOllama, "llama3.2")
//   - "local/claude" -> (ProviderLocal, "claude")
//   - "http://localhost:8080|model-name" -> (ProviderCustom, model with custom URL)
func ParseModelProvider(model string) (ProviderType, string, *ProviderConfig) {
	if model == "" || model == "claude" {
		return ProviderClaude, "claude", nil
	}

	// Check for URL-based custom provider
	if strings.HasPrefix(model, "http://") || strings.HasPrefix(model, "https://") {
		// Format: "http://localhost:8080|model-name"
		parts := strings.SplitN(model, "|", 2)
		baseURL := parts[0]
		modelName := ""
		if len(parts) > 1 {
			modelName = parts[1]
		}
		return ProviderCustom, modelName, &ProviderConfig{
			Type:    ProviderCustom,
			BaseURL: baseURL,
			Model:   modelName,
		}
	}

	// Check for prefixed providers
	if strings.HasPrefix(model, "openai/") {
		return ProviderOpenAI, strings.TrimPrefix(model, "openai/"), nil
	}
	if strings.HasPrefix(model, "openrouter/") {
		return ProviderOpenRouter, strings.TrimPrefix(model, "openrouter/"), nil
	}
	if strings.HasPrefix(model, "ollama/") {
		return ProviderOllama, strings.TrimPrefix(model, "ollama/"), nil
	}
	if strings.HasPrefix(model, "local/") {
		return ProviderLocal, strings.TrimPrefix(model, "local/"), nil
	}

	// Default to Claude CLI for anything else
	return ProviderClaude, model, nil
}
