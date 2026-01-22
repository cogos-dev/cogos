// Projection Module - Projects canonical .cog/ state into interface-specific views
//
// This module handles the projection of CogOS kernel state into interface-specific
// working environments (like .claude/ for Claude Code).
//
// The projection pattern (ADR-007):
// 1. .cog/ is canonical (source of truth, kernel-owned)
// 2. .claude/ is a projection (Claude Code's view of the kernel)
// 3. Hooks enforce projections at every interaction
// 4. Projections are tracked in .cog/run/projections.json

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// === PROJECTION CONFIGURATION ===

// Agent projections: .cog/agents/claude-code/* -> .claude/agents/*
const (
	agentSource = "agents/claude-code"
	agentTarget = "agents"
	skillSource = "skills"
	skillTarget = "skills"
)

// Kernel skills that should be projected
var kernelSkills = []string{
	"cogos-memory",
}

// State tracking file
const projectionsStateFile = ".cog/run/projections.json"

// === PROJECTION STATE TYPES ===

// ProjectionEntry represents a single projection mapping
type ProjectionEntry struct {
	Target     string `json:"target"`
	Type       string `json:"type"` // "symlink" or "merge"
	Created    string `json:"created"`
	SourceHash string `json:"source_hash"`
}

// ProjectionState represents the entire projection state
type ProjectionState struct {
	Version        string                     `json:"version"`
	Projections    map[string]ProjectionEntry `json:"projections"`
	LastProjection string                     `json:"last_projection"`
}

// ProjectionResult represents the result of a projection operation
type ProjectionResult struct {
	Success     bool                `json:"success"`
	Messages    []string            `json:"messages"`
	Projections map[string][]string `json:"projections"`
	StateFile   string              `json:"state_file,omitempty"`
}

// === STATE MANAGEMENT ===

// loadProjectionState loads projection tracking state from disk
func loadProjectionState(root string) (*ProjectionState, error) {
	stateFile := filepath.Join(root, projectionsStateFile)

	data, err := os.ReadFile(stateFile)
	if err != nil {
		// Return empty state if file doesn't exist
		return &ProjectionState{
			Version:     "1.0.0",
			Projections: make(map[string]ProjectionEntry),
		}, nil
	}

	var state ProjectionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse projections state: %w", err)
	}

	return &state, nil
}

// saveProjectionState saves projection tracking state to disk
func saveProjectionState(root string, state *ProjectionState) error {
	stateFile := filepath.Join(root, projectionsStateFile)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal projections state: %w", err)
	}

	return writeAtomic(stateFile, data, 0644)
}

// recordProjection records a projection in the state
func recordProjection(state *ProjectionState, source, target, projType string) {
	sourceHash := ""
	// Compute source hash for tracking changes
	if data, err := os.ReadFile(source); err == nil {
		sourceHash = hashShort(data)
	} else if info, err := os.Stat(source); err == nil && info.IsDir() {
		// For directories, compute a simple hash
		sourceHash = computeDirHash(source)
	}

	state.Projections[source] = ProjectionEntry{
		Target:     target,
		Type:       projType,
		Created:    nowISO(),
		SourceHash: sourceHash,
	}
}

// computeDirHash computes a SHA256-based hash for a directory tree
// This matches the Python implementation for compatibility
func computeDirHash(dir string) string {
	hasher := sha256.New()

	// Walk directory tree in sorted order for deterministic hashing
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		// Skip hidden files and directories
		if strings.HasPrefix(filepath.Base(path), ".") && path != dir {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})

	if err != nil {
		return "unknown"
	}

	// Sort for deterministic ordering
	sort.Strings(files)

	// Hash each file's relative path and content
	for _, file := range files {
		relPath, _ := filepath.Rel(dir, file)
		hasher.Write([]byte(relPath))

		// Hash file content
		if f, err := os.Open(file); err == nil {
			io.Copy(hasher, f)
			f.Close()
		}
	}

	// Return first 12 characters of hex digest (matching Python)
	hashBytes := hasher.Sum(nil)
	hashStr := fmt.Sprintf("%x", hashBytes)
	if len(hashStr) > 12 {
		return hashStr[:12]
	}
	return hashStr
}

// === SYMLINK OPERATIONS ===

// ensureSymlink ensures a symlink exists from target to source
// Returns (success, message)
func ensureSymlink(source, target string) (bool, string) {
	// Check if target is already a symlink
	if linkTarget, err := os.Readlink(target); err == nil {
		// It's a symlink - check if it points to the right place
		sourcePath, _ := filepath.Abs(source)
		targetPath, _ := filepath.Abs(linkTarget)

		if sourcePath == targetPath {
			return true, fmt.Sprintf("Symlink OK: %s -> %s", target, source)
		}

		// Wrong target - remove and recreate
		if err := os.Remove(target); err != nil {
			return false, fmt.Sprintf("Failed to remove old symlink: %v", err)
		}
	} else if _, err := os.Stat(target); err == nil {
		// Target exists but is not a symlink - back it up
		backup := target + ".bak"
		if err := os.Rename(target, backup); err != nil {
			return false, fmt.Sprintf("Failed to backup existing file: %v", err)
		}
	}

	// Create symlink using relative path for portability
	relSource, err := filepath.Rel(filepath.Dir(target), source)
	if err != nil {
		return false, fmt.Sprintf("Failed to compute relative path: %v", err)
	}

	if err := os.Symlink(relSource, target); err != nil {
		return false, fmt.Sprintf("Failed to create symlink: %v", err)
	}

	return true, fmt.Sprintf("Created symlink: %s -> %s", target, relSource)
}

// === PROJECTION OPERATIONS ===

// projectAgents projects Claude Code agents from .cog/agents/claude-code/ to .claude/agents/
func projectAgents(cogRoot, claudeRoot string, state *ProjectionState) []string {
	var messages []string

	sourceDir := filepath.Join(cogRoot, agentSource)
	targetDir := filepath.Join(claudeRoot, agentTarget)

	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		messages = append(messages, fmt.Sprintf("Agent source not found: %s", sourceDir))
		return messages
	}

	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		messages = append(messages, fmt.Sprintf("Failed to create target dir: %v", err))
		return messages
	}

	// Project each agent
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		messages = append(messages, fmt.Sprintf("Failed to read source dir: %v", err))
		return messages
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		agentSourcePath := filepath.Join(sourceDir, entry.Name())
		agentTargetPath := filepath.Join(targetDir, entry.Name())

		success, msg := ensureSymlink(agentSourcePath, agentTargetPath)
		messages = append(messages, msg)

		if success {
			// Record projection (use relative paths from project root)
			relSource := filepath.Join(".cog", agentSource, entry.Name())
			relTarget := filepath.Join(".claude", agentTarget, entry.Name())
			recordProjection(state, relSource, relTarget, "symlink")
		}
	}

	return messages
}

// projectSkills projects kernel skills from .cog/skills/ to .claude/skills/
func projectSkills(cogRoot, claudeRoot string, state *ProjectionState) []string {
	var messages []string

	sourceDir := filepath.Join(cogRoot, skillSource)
	targetDir := filepath.Join(claudeRoot, skillTarget)

	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		messages = append(messages, fmt.Sprintf("Skill source not found: %s", sourceDir))
		return messages
	}

	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		messages = append(messages, fmt.Sprintf("Failed to create target dir: %v", err))
		return messages
	}

	// Project kernel skills
	for _, skillName := range kernelSkills {
		skillSourcePath := filepath.Join(sourceDir, skillName)
		skillTargetPath := filepath.Join(targetDir, skillName)

		if _, err := os.Stat(skillSourcePath); os.IsNotExist(err) {
			messages = append(messages, fmt.Sprintf("Kernel skill not found: %s", skillSourcePath))
			continue
		}

		success, msg := ensureSymlink(skillSourcePath, skillTargetPath)
		messages = append(messages, msg)

		if success {
			// Record projection (use relative paths from project root)
			relSource := filepath.Join(".cog", skillSource, skillName)
			relTarget := filepath.Join(".claude", skillTarget, skillName)
			recordProjection(state, relSource, relTarget, "symlink")
		}
	}

	return messages
}

// projectCommands projects commands from .cog/commands/ to .claude/commands/
func projectCommands(cogRoot, claudeRoot string, state *ProjectionState) []string {
	var messages []string

	sourceDir := filepath.Join(cogRoot, "commands")
	targetDir := filepath.Join(claudeRoot, "commands")

	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		messages = append(messages, fmt.Sprintf("Commands source not found: %s", sourceDir))
		return messages
	}

	success, msg := ensureSymlink(sourceDir, targetDir)
	messages = append(messages, msg)

	if success {
		// Record projection
		recordProjection(state,
			".cog/commands",
			".claude/commands",
			"symlink")
	}

	return messages
}

// runFullProjection runs full projection from .cog/ to .claude/
// Called by SessionStart hook to ensure interface view is current
func runFullProjection(root string) *ProjectionResult {
	cogRoot := filepath.Join(root, ".cog")
	claudeRoot := filepath.Join(root, ".claude")

	result := &ProjectionResult{
		Success: true,
		Projections: map[string][]string{
			"commands": {},
			"agents":   {},
			"skills":   {},
		},
		Messages: []string{},
	}

	if _, err := os.Stat(cogRoot); os.IsNotExist(err) {
		result.Success = false
		result.Messages = append(result.Messages, ".cog/ directory not found")
		return result
	}

	// Load projection state for tracking
	state, err := loadProjectionState(root)
	if err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Warning: Failed to load state: %v", err))
		state = &ProjectionState{
			Version:     "1.0.0",
			Projections: make(map[string]ProjectionEntry),
		}
	}

	// Ensure .claude/ exists
	if err := os.MkdirAll(claudeRoot, 0755); err != nil {
		result.Success = false
		result.Messages = append(result.Messages, fmt.Sprintf("Failed to create .claude/: %v", err))
		return result
	}

	// Project commands
	result.Projections["commands"] = projectCommands(cogRoot, claudeRoot, state)

	// Project agents
	result.Projections["agents"] = projectAgents(cogRoot, claudeRoot, state)

	// Project skills
	result.Projections["skills"] = projectSkills(cogRoot, claudeRoot, state)

	// Update timestamp and save state
	state.LastProjection = nowISO()
	if err := saveProjectionState(root, state); err != nil {
		result.Messages = append(result.Messages, fmt.Sprintf("Warning: Failed to save state: %v", err))
	}

	// Collect all messages
	for _, msgs := range result.Projections {
		result.Messages = append(result.Messages, msgs...)
	}

	// Check for failures
	for _, msg := range result.Messages {
		if strings.Contains(msg, "Failed") {
			result.Success = false
			break
		}
	}

	result.StateFile = filepath.Join(root, projectionsStateFile)

	return result
}

// validateProjection validates that projections are correctly set up
func validateProjection(root string) map[string]interface{} {
	cogRoot := filepath.Join(root, ".cog")
	claudeRoot := filepath.Join(root, ".claude")

	issues := []string{}

	// Check commands symlink
	commandsTarget := filepath.Join(claudeRoot, "commands")
	commandsSource := filepath.Join(cogRoot, "commands")

	if linkTarget, err := os.Readlink(commandsTarget); err == nil {
		// It's a symlink - check if it points to the right place
		if absLink, err := filepath.Abs(filepath.Join(filepath.Dir(commandsTarget), linkTarget)); err == nil {
			if absSource, err := filepath.Abs(commandsSource); err == nil {
				if absLink != absSource {
					issues = append(issues, "commands symlink points to wrong location")
				}
			}
		}
	} else if _, err := os.Stat(commandsTarget); err == nil {
		issues = append(issues, "commands is not a symlink")
	}

	// Check agent symlinks
	agentsTarget := filepath.Join(claudeRoot, agentTarget)
	agentsSource := filepath.Join(cogRoot, agentSource)

	if entries, err := os.ReadDir(agentsSource); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			agentSourcePath := filepath.Join(agentsSource, entry.Name())
			agentTargetPath := filepath.Join(agentsTarget, entry.Name())

			// Check if source is accessible
			if _, err := os.Stat(agentSourcePath); err != nil {
				issues = append(issues, fmt.Sprintf("Agent source %s is not accessible: %v", entry.Name(), err))
				continue
			}

			// Check if target is a symlink
			if _, err := os.Readlink(agentTargetPath); err != nil {
				if _, err := os.Stat(agentTargetPath); err == nil {
					issues = append(issues, fmt.Sprintf("Agent %s is not a symlink", entry.Name()))
				} else {
					issues = append(issues, fmt.Sprintf("Agent %s target is missing", entry.Name()))
				}
			}
		}
	} else {
		// Report if agent source directory is inaccessible
		issues = append(issues, fmt.Sprintf("Agent source directory is not accessible: %v", err))
	}

	return map[string]interface{}{
		"valid":  len(issues) == 0,
		"issues": issues,
	}
}

// detectProjectionDrift detects drift between recorded projections and current state
func detectProjectionDrift(root string) map[string]interface{} {
	state, err := loadProjectionState(root)
	if err != nil {
		return map[string]interface{}{
			"error":     err.Error(),
			"has_drift": false,
		}
	}

	drift := map[string]interface{}{
		"changed":         []map[string]string{},
		"missing_source":  []string{},
		"missing_target":  []string{},
		"broken_symlinks": []string{},
		"has_drift":       false,
	}

	for source, projection := range state.Projections {
		sourcePath := filepath.Join(root, source)
		targetPath := filepath.Join(root, projection.Target)

		// Check source existence
		if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
			drift["missing_source"] = append(drift["missing_source"].([]string), source)
			continue
		}

		// Check target existence
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			drift["missing_target"] = append(drift["missing_target"].([]string), projection.Target)
			continue
		}

		// Check for broken symlinks
		if linkTarget, err := os.Readlink(targetPath); err == nil {
			// It's a symlink - check if it's broken
			// Resolve relative symlink target from the symlink's directory
			var absLinkTarget string
			if filepath.IsAbs(linkTarget) {
				absLinkTarget = linkTarget
			} else {
				absLinkTarget = filepath.Join(filepath.Dir(targetPath), linkTarget)
			}
			if _, err := os.Stat(absLinkTarget); os.IsNotExist(err) {
				drift["broken_symlinks"] = append(drift["broken_symlinks"].([]string), projection.Target)
				continue
			}
		}

		// TODO: Check for content changes using source_hash
		// This would require more sophisticated hashing for directories
	}

	// Determine if there's any drift
	drift["has_drift"] = len(drift["changed"].([]map[string]string)) > 0 ||
		len(drift["missing_source"].([]string)) > 0 ||
		len(drift["missing_target"].([]string)) > 0 ||
		len(drift["broken_symlinks"].([]string)) > 0

	return drift
}

// cleanOrphanProjections removes targets whose sources no longer exist
func cleanOrphanProjections(root string) []string {
	state, err := loadProjectionState(root)
	if err != nil {
		return []string{}
	}

	orphans := []string{}

	for source, projection := range state.Projections {
		sourcePath := filepath.Join(root, source)
		targetPath := filepath.Join(root, projection.Target)

		// Check if source is gone but target remains
		if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
			// Use Lstat to examine the symlink itself, not its target
			// This prevents errors when the symlink is broken
			if info, err := os.Lstat(targetPath); err == nil {
				// Target exists - remove it if it's a symlink
				if info.Mode()&os.ModeSymlink != 0 {
					if err := os.Remove(targetPath); err == nil {
						orphans = append(orphans, targetPath)
						// Remove from state
						delete(state.Projections, source)
					}
				}
			}
		}
	}

	// Save updated state if we cleaned anything
	if len(orphans) > 0 {
		saveProjectionState(root, state)
	}

	return orphans
}
