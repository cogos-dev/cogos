// Tier 1: Identity Context Loader
//
// Loads the current identity from configuration and provides identity context
// for TAA (Temporal Attention Architecture).
//
// Budget: ~33k tokens (~132k characters)
// Strategy: Load identity card + optional context plugin output
// Output: Identity card content + plugin output (if configured)

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Tier 1 token budget constants
const (
	Tier1MaxTokens = 33000
	Tier1MaxChars  = Tier1MaxTokens * CharsPerToken // ~132k chars
)

// IdentityConfig represents the structure of .cog/config/identity.yaml
type IdentityConfig struct {
	DefaultIdentity      string `yaml:"default_identity"`
	IdentityDirectory    string `yaml:"identity_directory"`
	LoadOnSessionStart   bool   `yaml:"load_on_session_start"`
	InjectContextPlugin  bool   `yaml:"inject_context_plugin"`
}

// IdentityFrontmatter represents the YAML frontmatter in an identity card
type IdentityFrontmatter struct {
	Name           string   `yaml:"name"`
	Role           string   `yaml:"role"`
	ContextPlugin  string   `yaml:"context_plugin"`
	MemoryPath     string   `yaml:"memory_path"`
	MemoryNamespace string  `yaml:"memory_namespace"`
	DerivesFrom    string   `yaml:"derives_from"`
	Dependencies   []string `yaml:"dependencies"`
}

// LoadIdentityContext loads the current identity and returns formatted context.
//
// Algorithm:
// 1. Read identity configuration from .cog/config/identity.yaml
// 2. Load identity card from projects/cog_lab_package/identities/identity_{name}.md
// 3. Execute context plugin if configured and enabled
// 4. Combine identity card + plugin output within budget
//
// Parameters:
// - workspaceRoot: Absolute path to workspace root
// - maxTokens: Token budget (0 = use default Tier1MaxTokens)
//
// Returns:
// - Formatted identity context string
// - Error if any
func LoadIdentityContext(workspaceRoot string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = Tier1MaxTokens
	}
	maxChars := maxTokens * CharsPerToken

	// Load identity configuration
	configPath := filepath.Join(workspaceRoot, ".cog", "config", "identity.yaml")
	config, err := loadIdentityConfig(configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load identity config: %w", err)
	}

	// Construct identity card path
	identityDir := config.IdentityDirectory
	if !filepath.IsAbs(identityDir) {
		identityDir = filepath.Join(workspaceRoot, identityDir)
	}
	cardPath := filepath.Join(identityDir, fmt.Sprintf("identity_%s.md", config.DefaultIdentity))

	// Load identity card
	cardContent, err := os.ReadFile(cardPath)
	if err != nil {
		return "", fmt.Errorf("failed to read identity card at %s: %w", cardPath, err)
	}

	// Parse frontmatter to get context plugin path
	frontmatter, bodyContent, err := parseIdentityCard(cardContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse identity card: %w", err)
	}

	// Build output
	var result strings.Builder

	// Add identity header
	result.WriteString("# Identity Context\n\n")
	result.WriteString(fmt.Sprintf("**Identity:** %s\n", frontmatter.Name))
	result.WriteString(fmt.Sprintf("**Role:** %s\n", frontmatter.Role))
	if frontmatter.MemoryNamespace != "" {
		result.WriteString(fmt.Sprintf("**Memory Namespace:** %s\n", frontmatter.MemoryNamespace))
	}
	result.WriteString("\n---\n\n")

	// Add identity card body
	result.WriteString(bodyContent)

	// Execute context plugin if configured
	var pluginOutput string
	if config.InjectContextPlugin && frontmatter.ContextPlugin != "" {
		pluginPath := frontmatter.ContextPlugin
		if !filepath.IsAbs(pluginPath) {
			pluginPath = filepath.Join(workspaceRoot, pluginPath)
		}

		output, err := ExecuteContextPlugin(pluginPath, workspaceRoot)
		if err != nil {
			// Log warning but don't fail - plugin execution is optional
			result.WriteString("\n\n---\n\n")
			result.WriteString(fmt.Sprintf("*Context plugin execution failed: %v*\n", err))
		} else if output != "" {
			pluginOutput = output
		}
	}

	// Add plugin output if available
	if pluginOutput != "" {
		result.WriteString("\n\n---\n\n")
		result.WriteString("## Workspace Context (from plugin)\n\n")
		result.WriteString("```\n")
		result.WriteString(pluginOutput)
		if !strings.HasSuffix(pluginOutput, "\n") {
			result.WriteString("\n")
		}
		result.WriteString("```\n")
	}

	// Truncate if over budget
	content := result.String()
	if len(content) > maxChars {
		content = truncateToCharLimit(content, maxChars)
	}

	return content, nil
}

// ExecuteContextPlugin runs a context plugin script and returns its output.
//
// Parameters:
// - pluginPath: Absolute path to the plugin script
// - workspaceRoot: Workspace root (passed to plugin as CLAUDE_PROJECT_DIR)
//
// Returns:
// - Plugin stdout output
// - Error if plugin fails or times out
func ExecuteContextPlugin(pluginPath string, workspaceRoot string) (string, error) {
	// Check if plugin exists and is executable
	info, err := os.Stat(pluginPath)
	if err != nil {
		return "", fmt.Errorf("plugin not found: %w", err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("plugin path is a directory, not a file")
	}

	// Check if executable (on Unix-like systems)
	if info.Mode()&0111 == 0 {
		return "", fmt.Errorf("plugin is not executable")
	}

	// Create context with timeout (5 seconds)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Prepare command
	cmd := exec.CommandContext(ctx, pluginPath)
	cmd.Dir = workspaceRoot
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CLAUDE_PROJECT_DIR=%s", workspaceRoot),
	)

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the plugin
	err = cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("plugin timed out after 5 seconds")
		}
		return "", fmt.Errorf("plugin execution failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

// loadIdentityConfig reads and parses the identity configuration file
func loadIdentityConfig(configPath string) (*IdentityConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config IdentityConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Set defaults if not specified
	if config.IdentityDirectory == "" {
		config.IdentityDirectory = "projects/cog_lab_package/identities"
	}

	return &config, nil
}

// parseIdentityCard extracts YAML frontmatter and markdown body from an identity card
func parseIdentityCard(content []byte) (*IdentityFrontmatter, string, error) {
	contentStr := string(content)

	// Check for frontmatter delimiter
	if !strings.HasPrefix(contentStr, "---") {
		// No frontmatter, return empty frontmatter and full content as body
		return &IdentityFrontmatter{}, contentStr, nil
	}

	// Find the closing delimiter
	// Pattern: starts with ---, ends with ---\n
	pattern := regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)$`)
	matches := pattern.FindStringSubmatch(contentStr)
	if matches == nil {
		return nil, "", fmt.Errorf("malformed frontmatter: missing closing delimiter")
	}

	frontmatterYAML := matches[1]
	body := matches[2]

	// Parse frontmatter
	var frontmatter IdentityFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatterYAML), &frontmatter); err != nil {
		return nil, "", fmt.Errorf("failed to parse frontmatter YAML: %w", err)
	}

	return &frontmatter, body, nil
}

// truncateToCharLimit truncates content to fit within character limit,
// adding a truncation notice at the end
func truncateToCharLimit(content string, maxChars int) string {
	notice := "\n\n[... content truncated to fit token budget ...]\n"
	noticeLen := len(notice)

	if len(content) <= maxChars {
		return content
	}

	// Find a good break point (end of line) near the limit
	cutoff := maxChars - noticeLen
	if cutoff < 0 {
		cutoff = 0
	}

	// Try to cut at a newline to avoid mid-line truncation
	lastNewline := strings.LastIndex(content[:cutoff], "\n")
	if lastNewline > cutoff/2 {
		// Found a reasonable newline break point
		cutoff = lastNewline
	}

	return content[:cutoff] + notice
}

// GetIdentityName returns just the identity name from configuration
// This is a utility function for quick identity lookup
func GetIdentityName(workspaceRoot string) (string, error) {
	configPath := filepath.Join(workspaceRoot, ".cog", "config", "identity.yaml")
	config, err := loadIdentityConfig(configPath)
	if err != nil {
		return "", err
	}
	return config.DefaultIdentity, nil
}

// GetIdentityFrontmatter returns the parsed frontmatter for the current identity
// This is a utility function for accessing identity metadata
func GetIdentityFrontmatter(workspaceRoot string) (*IdentityFrontmatter, error) {
	configPath := filepath.Join(workspaceRoot, ".cog", "config", "identity.yaml")
	config, err := loadIdentityConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load identity config: %w", err)
	}

	identityDir := config.IdentityDirectory
	if !filepath.IsAbs(identityDir) {
		identityDir = filepath.Join(workspaceRoot, identityDir)
	}
	cardPath := filepath.Join(identityDir, fmt.Sprintf("identity_%s.md", config.DefaultIdentity))

	cardContent, err := os.ReadFile(cardPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read identity card: %w", err)
	}

	frontmatter, _, err := parseIdentityCard(cardContent)
	if err != nil {
		return nil, fmt.Errorf("failed to parse identity card: %w", err)
	}

	return frontmatter, nil
}
