// .cog/rbac.go
// Role-Based Access Control for CogOS
//
// Secure implementation replacing roles.sh (268 LOC)
// Eliminates: eval injection, path traversal, unquoted xargs
// Uses: yaml.v3 for safe parsing, filepath.Clean for path validation

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// === TYPES ===

// Role represents an agent role with capabilities and view constraints
type Role struct {
	// Metadata
	Name   string `yaml:"-"`        // Set from directory name
	Title  string `yaml:"title"`
	Status string `yaml:"status"`
	Layer  int    `yaml:"layer"`

	// Model configuration
	Defaults struct {
		Model    string `yaml:"model"`
		Provider string `yaml:"provider"`
		Toolset  string `yaml:"toolset"`
	} `yaml:"defaults"`

	// Runtime limits
	MaxIterations int `yaml:"maxIterations"`
	Timeout       int `yaml:"timeout"`

	// Capabilities and constraints
	Capabilities []string          `yaml:"capabilities"`
	Constraints  map[string]interface{} `yaml:"constraints"`
	Spawns       []string          `yaml:"spawns"`

	// View constraints (Theorem 8.1)
	View ViewConstraints `yaml:"view"`
}

// ViewConstraints defines what files a role can access
type ViewConstraints struct {
	Include []string `yaml:"include"` // Glob patterns for allowed paths
	Exclude []string `yaml:"exclude"` // Glob patterns to exclude
}

// RoleLoader handles role loading and validation
type RoleLoader struct {
	cogDir string
}

// === ROLE LOADING ===

// NewRoleLoader creates a role loader for the given .cog directory
func NewRoleLoader(cogDir string) (*RoleLoader, error) {
	// Canonicalize path to prevent path traversal
	absPath, err := filepath.Abs(cogDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve .cog directory: %w", err)
	}

	// Verify it's actually a .cog directory
	if filepath.Base(absPath) != ".cog" {
		return nil, fmt.Errorf("not a .cog directory: %s", absPath)
	}

	return &RoleLoader{cogDir: absPath}, nil
}

// LoadRole loads a role definition from .cog/roles/<role>/ROLE.cog.md
// Security: Uses yaml.v3 (NO eval), validates paths with filepath.Clean
func (rl *RoleLoader) LoadRole(roleName string) (*Role, error) {
	// Validate role name (no path traversal)
	if strings.Contains(roleName, "..") || strings.Contains(roleName, "/") {
		return nil, fmt.Errorf("invalid role name: %s", roleName)
	}

	// Construct role file path using secure path joining
	rolePath := filepath.Join(rl.cogDir, "roles", roleName, "ROLE.cog.md")

	// Canonicalize and verify path is within .cog/roles/
	canonicalPath, err := filepath.Abs(rolePath)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve role path: %w", err)
	}

	expectedPrefix := filepath.Join(rl.cogDir, "roles") + string(os.PathSeparator)
	if !strings.HasPrefix(canonicalPath, expectedPrefix) {
		return nil, fmt.Errorf("path traversal detected: %s", roleName)
	}

	// Read role file
	data, err := os.ReadFile(canonicalPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read role file: %w", err)
	}

	// Parse frontmatter (safe YAML parsing)
	role, err := rl.parseFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("cannot parse role frontmatter: %w", err)
	}

	// Set role name from directory
	role.Name = roleName

	return role, nil
}

// parseFrontmatter extracts and parses YAML frontmatter from a cogdoc
// Security: Uses yaml.v3 with strict parsing (no code execution)
func (rl *RoleLoader) parseFrontmatter(data []byte) (*Role, error) {
	// Extract frontmatter between --- markers
	lines := strings.Split(string(data), "\n")

	var frontmatterLines []string
	inFrontmatter := false
	frontmatterCount := 0

	for _, line := range lines {
		if line == "---" {
			frontmatterCount++
			if frontmatterCount == 1 {
				inFrontmatter = true
				continue
			} else if frontmatterCount == 2 {
				break
			}
		}
		if inFrontmatter {
			frontmatterLines = append(frontmatterLines, line)
		}
	}

	if frontmatterCount < 2 {
		return nil, fmt.Errorf("invalid frontmatter: missing --- markers")
	}

	frontmatterYAML := strings.Join(frontmatterLines, "\n")

	// Parse YAML safely (no eval, no code execution)
	var role Role
	decoder := yaml.NewDecoder(strings.NewReader(frontmatterYAML))
	decoder.KnownFields(false) // Allow extra fields (cogn8 metadata)

	if err := decoder.Decode(&role); err != nil {
		return nil, fmt.Errorf("YAML parse error: %w", err)
	}

	return &role, nil
}

// ListRoles returns all available roles in .cog/roles/
func (rl *RoleLoader) ListRoles() ([]string, error) {
	rolesDir := filepath.Join(rl.cogDir, "roles")

	entries, err := os.ReadDir(rolesDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read roles directory: %w", err)
	}

	var roles []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip special directories
		if entry.Name() == ".status" {
			continue
		}

		// Check if ROLE.cog.md exists
		roleFile := filepath.Join(rolesDir, entry.Name(), "ROLE.cog.md")
		if _, err := os.Stat(roleFile); err == nil {
			roles = append(roles, entry.Name())
		}
	}

	return roles, nil
}

// === CAPABILITY CHECKS ===

// CanSpawn checks if a role can spawn another role
func (role *Role) CanSpawn(childRole string) bool {
	// Root (no role) can spawn anything
	if role == nil {
		return true
	}

	// Check spawns list
	for _, allowed := range role.Spawns {
		if allowed == childRole {
			return true
		}
	}

	return false
}

// HasCapability checks if a role has a specific capability
func (role *Role) HasCapability(capability string) bool {
	for _, cap := range role.Capabilities {
		if cap == capability {
			return true
		}
	}
	return false
}

// LayerAllows checks if a role meets minimum layer requirement
func (role *Role) LayerAllows(requiredLayer int) bool {
	if role == nil {
		return true // Root has all permissions
	}
	return role.Layer >= requiredLayer
}

// === VIEW PROJECTION (Theorem 8.1) ===

// PathChecker validates paths against role view constraints
type PathChecker struct {
	role   *Role
	cogDir string
}

// NewPathChecker creates a path checker for a role
func NewPathChecker(role *Role, cogDir string) (*PathChecker, error) {
	absPath, err := filepath.Abs(cogDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve .cog directory: %w", err)
	}

	return &PathChecker{
		role:   role,
		cogDir: absPath,
	}, nil
}

// CheckPath validates if a role can access a path
// Security: Uses filepath.Clean and canonical path validation
func (pc *PathChecker) CheckPath(path string) error {
	// Clean and canonicalize path
	cleanPath := filepath.Clean(path)

	// Make absolute if relative
	var absPath string
	if filepath.IsAbs(cleanPath) {
		absPath = cleanPath
	} else {
		absPath = filepath.Join(pc.cogDir, cleanPath)
	}

	// Canonicalize
	canonicalPath, err := filepath.Abs(absPath)
	if err != nil {
		return fmt.Errorf("cannot resolve path: %w", err)
	}

	// Verify path is within .cog/
	if !strings.HasPrefix(canonicalPath, pc.cogDir+string(os.PathSeparator)) {
		return fmt.Errorf("path outside .cog directory: %s", path)
	}

	// If no view constraints, allow everything in .cog/
	if len(pc.role.View.Include) == 0 && len(pc.role.View.Exclude) == 0 {
		return nil
	}

	// Get relative path for pattern matching
	relPath, err := filepath.Rel(pc.cogDir, canonicalPath)
	if err != nil {
		return fmt.Errorf("cannot compute relative path: %w", err)
	}

	// Check include patterns (default deny if includes specified)
	included := false
	if len(pc.role.View.Include) > 0 {
		for _, pattern := range pc.role.View.Include {
			matched, err := filepath.Match(pattern, relPath)
			if err != nil {
				continue // Invalid pattern, skip
			}
			if matched || strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "*")) {
				included = true
				break
			}
		}
		if !included {
			return fmt.Errorf("path not in role view: %s", relPath)
		}
	} else {
		included = true // No includes means allow all (unless excluded)
	}

	// Check exclude patterns
	for _, pattern := range pc.role.View.Exclude {
		matched, err := filepath.Match(pattern, relPath)
		if err != nil {
			continue
		}
		if matched || strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "*")) {
			return fmt.Errorf("path excluded from role view: %s", relPath)
		}
	}

	return nil
}

// GetVisibleFiles returns all files visible to a role
// Security: Safe traversal with path validation at every step
func (pc *PathChecker) GetVisibleFiles() ([]string, error) {
	var files []string

	// If no view constraints, return all files
	if len(pc.role.View.Include) == 0 {
		return pc.getAllFiles()
	}

	// Walk each include pattern
	for _, pattern := range pc.role.View.Include {
		patternPath := filepath.Join(pc.cogDir, pattern)

		// Handle glob patterns
		if strings.Contains(pattern, "*") {
			matches, err := filepath.Glob(patternPath)
			if err != nil {
				continue
			}
			for _, match := range matches {
				// Verify canonical path
				canonical, err := filepath.Abs(match)
				if err != nil {
					continue
				}
				if !strings.HasPrefix(canonical, pc.cogDir+string(os.PathSeparator)) {
					continue
				}

				// Check if excluded
				relPath, _ := filepath.Rel(pc.cogDir, canonical)
				if !pc.isExcluded(relPath) {
					files = append(files, canonical)
				}
			}
		} else {
			// Direct path
			canonical, err := filepath.Abs(patternPath)
			if err != nil {
				continue
			}
			if strings.HasPrefix(canonical, pc.cogDir+string(os.PathSeparator)) {
				relPath, _ := filepath.Rel(pc.cogDir, canonical)
				if !pc.isExcluded(relPath) {
					if _, err := os.Stat(canonical); err == nil {
						files = append(files, canonical)
					}
				}
			}
		}
	}

	return files, nil
}

// isExcluded checks if a relative path matches any exclude pattern
func (pc *PathChecker) isExcluded(relPath string) bool {
	for _, pattern := range pc.role.View.Exclude {
		matched, err := filepath.Match(pattern, relPath)
		if err != nil {
			continue
		}
		if matched || strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// getAllFiles returns all files in .cog/ (no view constraints)
func (pc *PathChecker) getAllFiles() ([]string, error) {
	var files []string

	err := filepath.Walk(pc.cogDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})

	return files, err
}
