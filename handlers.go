// Built-in hook handlers for Phase 2
//
// These handlers replace Python scripts for performance-critical operations.
// They implement the same logic but run compiled, with zero import overhead.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Blocked signature patterns
var blockedSignaturePatterns = []*regexp.Regexp{
	regexp.MustCompile(`🤖\s*Generated with \[Claude Code\]`),
	regexp.MustCompile(`Generated with \[Claude Code\]\(https://claude\.com/claude-code\)`),
	regexp.MustCompile(`(?i)Co-Authored-By:\s*Claude`),
}

// handleSignatureBlock implements 10-signature-block.py logic
func handleSignatureBlock(inputData map[string]interface{}) *HookResult {
	// Only check Bash commands
	toolName, _ := inputData["tool_name"].(string)
	if toolName != "Bash" {
		return &HookResult{Decision: "allow"}
	}

	// Extract command
	toolInput, ok := inputData["tool_input"].(map[string]interface{})
	if !ok {
		return &HookResult{Decision: "allow"}
	}

	command, _ := toolInput["command"].(string)

	// Only check git commit and gh pr commands
	if !strings.Contains(command, "git commit") && !strings.Contains(command, "gh pr") {
		return &HookResult{Decision: "allow"}
	}

	// Check for blocked patterns
	for _, pattern := range blockedSignaturePatterns {
		if pattern.MatchString(command) {
			reason := "Claude Code signature detected in commit message. Remove the '🤖 Generated with Claude Code' line and Co-Authored-By header."
			if strings.Contains(command, "gh pr") {
				reason = "Claude Code signature detected in PR body. Remove the '🤖 Generated with Claude Code' line."
			}
			return &HookResult{
				Decision: "block",
				Reason:   reason,
			}
		}
	}

	return &HookResult{Decision: "allow"}
}

// Cogdoc configuration
var (
	cogdocRequiredPaths = []string{
		".cog/mem/semantic/",
		".cog/mem/episodic/",
		".cog/mem/procedural/",
		".cog/mem/reflective/",
		".cog/mem/emotional/",
		".cog/mem/temporal/",
		".cog/mem/reference/",
		".cog/mem/waypoints/",
		".cog/adr/",
		".cog/schemas/",
	}

	cogdocExemptPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\.jsonl$`),
		regexp.MustCompile(`\.json$`),
		regexp.MustCompile(`\.yaml$`),
		regexp.MustCompile(`\.yml$`),
		regexp.MustCompile(`-metadata\.json$`),
		regexp.MustCompile(`\.session-`),
		regexp.MustCompile(`/backups/`),
		regexp.MustCompile(`\.gitkeep$`),
	}

	conventionalMDFiles = map[string]bool{
		"README.md":       true,
		"CLAUDE.md":       true,
		"SKILL.md":        true,
		"AGENT.md":        true,
		"CHANGELOG.md":    true,
		"LICENSE.md":      true,
		"CONTRIBUTING.md": true,
	}
)

// requiresCogdocValidation checks if file path needs validation
func requiresCogdocValidation(filePath string) bool {
	// Check if in required paths
	inRequiredPath := false
	for _, path := range cogdocRequiredPaths {
		if strings.Contains(filePath, path) {
			inRequiredPath = true
			break
		}
	}

	if !inRequiredPath {
		return false
	}

	// Check exempt patterns
	for _, pattern := range cogdocExemptPatterns {
		if pattern.MatchString(filePath) {
			return false
		}
	}

	// Check if conventional MD file
	filename := filePath[strings.LastIndex(filePath, "/")+1:]
	if conventionalMDFiles[filename] {
		return true
	}

	// Must be .md or .cog.md
	return strings.HasSuffix(filePath, ".md") || strings.HasSuffix(filePath, ".cog.md")
}

// requiresCogMDExtension checks if file should use .cog.md
func requiresCogMDExtension(filePath string) bool {
	if !requiresCogdocValidation(filePath) {
		return false
	}

	filename := filePath[strings.LastIndex(filePath, "/")+1:]
	if conventionalMDFiles[filename] {
		return false
	}

	return true
}

// validateCogdocContent validates cogdoc format
func validateCogdocContent(content, filePath string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}

	// Check extension
	if requiresCogMDExtension(filePath) {
		if strings.HasSuffix(filePath, ".md") && !strings.HasSuffix(filePath, ".cog.md") {
			newPath := filePath[:len(filePath)-3] + ".cog.md"
			return "Cogdoc extension error: Use '.cog.md' extension instead of '.md'. Rename to '" + newPath + "'."
		}
	}

	// Check frontmatter
	if !strings.HasPrefix(content, "---") {
		return "Missing YAML frontmatter. Files must start with '---' and include type/id fields."
	}

	lines := strings.Split(content, "\n")
	closingIndex := -1
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			closingIndex = i + 1
			break
		}
	}

	if closingIndex == -1 {
		return "Frontmatter not closed. Add '---' after YAML header."
	}

	frontmatter := strings.Join(lines[1:closingIndex], "\n")
	if !strings.Contains(frontmatter, "type:") && !strings.Contains(frontmatter, "type :") {
		// Check for nested type (cogn8 format)
		if !regexp.MustCompile(`(?m)(cogn8|cog):\s*\n\s+.*type:`).MatchString(frontmatter) {
			return "Missing 'type' field in frontmatter."
		}
	}

	return ""
}

// handleCogdocValidate implements 20-cogdoc-validate.py logic
func handleCogdocValidate(inputData map[string]interface{}) *HookResult {
	toolName, _ := inputData["tool_name"].(string)

	toolInput, ok := inputData["tool_input"].(map[string]interface{})
	if !ok {
		return &HookResult{Decision: "allow"}
	}

	filePath, _ := toolInput["file_path"].(string)

	var reason string

	if toolName == "Write" {
		content, _ := toolInput["content"].(string)
		if requiresCogdocValidation(filePath) {
			reason = validateCogdocContent(content, filePath)
		}
	} else if toolName == "Edit" {
		oldString, _ := toolInput["old_string"].(string)
		if requiresCogdocValidation(filePath) {
			if strings.HasPrefix(oldString, "---") && strings.Contains(oldString, "type:") {
				newString, _ := toolInput["new_string"].(string)
				if !strings.HasPrefix(newString, "---") || !strings.Contains(newString, "type:") {
					reason = "Edit would remove cogdoc frontmatter. Preserve the YAML header with type field."
				}
			}
		}
	}

	if reason != "" {
		return &HookResult{
			Decision: "block",
			Reason:   "Cogdoc validation failed for " + filePath + ": " + reason,
		}
	}

	return &HookResult{Decision: "allow"}
}

// handleCoherenceTrack implements 30-coherence-track.py logic
func handleCoherenceTrack(inputData map[string]interface{}) *HookResult {
	toolName, _ := inputData["tool_name"].(string)
	if toolName != "Write" && toolName != "Edit" {
		return &HookResult{Decision: "allow"}
	}

	// Get file path
	toolInput, ok := inputData["tool_input"].(map[string]interface{})
	if !ok {
		return &HookResult{Decision: "allow"}
	}

	filePath, _ := toolInput["file_path"].(string)

	// Check if tracked path
	isTracked := false
	for _, tracked := range trackedPaths {
		if strings.Contains(filePath, ".cog/"+tracked) {
			isTracked = true
			break
		}
	}

	if !isTracked {
		return &HookResult{Decision: "allow"}
	}

	// Record coherence state
	root, _, err := ResolveWorkspace()
	if err != nil {
		return &HookResult{Decision: "allow"}
	}

	state, err := recordCoherenceState(root)
	if err != nil {
		return &HookResult{Decision: "allow"}
	}

	// Log drift (non-blocking)
	if !state.Coherent {
		// Would log to stderr in real implementation
		// fmt.Fprintf(os.Stderr, "Coherence: Drift detected (%d files differ from canonical)\n", len(state.Drift))
	}

	// Never block - this is tracking only
	return &HookResult{Decision: "allow"}
}

// handleSessionProjection implements 10-projection.py logic for SessionStart
func handleSessionProjection(inputData map[string]interface{}) *HookResult {
	// Get project root
	root, _, err := ResolveWorkspace()
	if err != nil {
		// Fallback to Python implementation
		return nil
	}

	// Run full projection
	result := runFullProjection(root)

	// Always allow - projection failures shouldn't block session start
	// But log any issues for debugging
	if !result.Success {
		fmt.Fprintf(os.Stderr, "Projection warnings: %v\n", result.Messages)
	}

	return &HookResult{Decision: "allow"}
}

// handleSessionInit implements session directory initialization
// Creates directory structure needed for session tracking and coordination
func handleSessionInit(inputData map[string]interface{}) *HookResult {
	// Get project root
	root, _, err := ResolveWorkspace()
	if err != nil {
		// Log error but allow (non-fatal)
		fmt.Fprintf(os.Stderr, "Session init: failed to find workspace: %v\n", err)
		return &HookResult{Decision: "allow"}
	}

	cogRoot := filepath.Join(root, ".cog")

	// Define directories to create
	sessionDirs := []string{
		// Session tracking directories
		filepath.Join(cogRoot, "mem", "episodic", "sessions"),
		filepath.Join(cogRoot, "mem", "episodic", "sessions", "backups"),

		// Coordination directories
		filepath.Join(cogRoot, "coordination", "session"),
		filepath.Join(cogRoot, "coordination", "status", "domains"),
		filepath.Join(cogRoot, "coordination", "status", "experts"),
		filepath.Join(cogRoot, "coordination", "queues", "pending"),
		filepath.Join(cogRoot, "coordination", "queues", "claimed"),
		filepath.Join(cogRoot, "coordination", "queues", "completed"),
		filepath.Join(cogRoot, "coordination", "workflow-state"),
	}

	// Create directories atomically (idempotent)
	for _, dir := range sessionDirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			// Log but continue - some directories might be optional
			fmt.Fprintf(os.Stderr, "Session init: failed to create %s: %v\n", dir, err)
		}
	}

	// Extract session ID from input data
	sessionID, _ := inputData["session_id"].(string)
	if sessionID == "" {
		sessionID = "unknown"
	}

	// Log successful initialization
	fmt.Fprintf(os.Stderr, "✓ Session directories initialized for %s\n", sessionID)
	fmt.Fprintf(os.Stderr, "  Created: %d directories\n", len(sessionDirs))

	// Always allow (non-blocking initialization)
	return &HookResult{Decision: "allow"}
}

// handleCoherenceBaseline implements 20-coherence-baseline.py logic for SessionStart
// Records baseline coherence state by computing git tree hash and checking drift
func handleCoherenceBaseline(inputData map[string]interface{}) *HookResult {
	// Get project root
	root, _, err := ResolveWorkspace()
	if err != nil {
		// Log but allow (non-blocking)
		fmt.Fprintf(os.Stderr, "Coherence baseline: failed to find workspace: %v\n", err)
		return &HookResult{Decision: "allow"}
	}

	// Record coherence state (computes git tree hash, saves to .cog/run/coherence/coherence.json)
	state, err := recordCoherenceState(root)
	if err != nil {
		// Log error but don't block session start
		fmt.Fprintf(os.Stderr, "Coherence baseline error: %v\n", err)
		return &HookResult{Decision: "allow"}
	}

	// Report coherence status to user
	if state.Coherent {
		fmt.Fprintf(os.Stderr, "✓ Coherent with canonical\n")
	} else {
		driftCount := len(state.Drift)
		fmt.Fprintf(os.Stderr, "⚠ Session starting with %d files drifted from canonical\n", driftCount)
	}

	// Always allow (non-blocking tracking)
	return &HookResult{Decision: "allow"}
}

// tryBuiltinHandler attempts to handle an event with built-in Go code
// Returns nil if no built-in handler matches
func tryBuiltinHandler(handler Handler, inputData map[string]interface{}) *HookResult {
	// Map handler names to built-in implementations
	switch handler.Name {
	case "10-signature-block":
		return handleSignatureBlock(inputData)
	case "10-projection":
		return handleSessionProjection(inputData)
	case "20-cogdoc-validate":
		return handleCogdocValidate(inputData)
	case "20-coherence-baseline":
		return handleCoherenceBaseline(inputData)
	case "30-coherence-track":
		return handleCoherenceTrack(inputData)
	case "00-session-init":
		return handleSessionInit(inputData)
	default:
		return nil
	}
}
