// memory_scope.go
// Per-user memory scoping for shared agents.
//
// Shared agents (e.g., Exec Assistant) serve multiple users. Each user gets
// a private memory sector under the agent's memory space, with a shared sector
// visible to all authorized users. This file resolves memory paths to the
// correct user-scoped location and enforces read/write permissions based on
// the user's access level (admin, rw, ro, none).
//
// Layout:
//
//	cog://mem/semantic/agents/{agent}/
//	    shared/          # visible to all authorized users
//	    users/
//	        {user}/      # private to that user

package main

import (
	"fmt"
	"path"
	"strings"
)

// UserMemoryScope resolves memory paths for user-scoped agent memory.
// Given an agent's base memory sector and a user's memory scope,
// it produces scoped paths for memory operations.
type UserMemoryScope struct {
	AgentID    string // e.g., "exec-assistant"
	BaseSector string // e.g., "cog://mem/semantic/agents/exec-assistant/"
	UserID     string // e.g., "alice"
	UserScope  string // e.g., "users/alice" (from CRD)
	Level      string // e.g., "admin", "rw", "ro", "none"
}

// ResolveMemoryPath takes a relative memory path and returns the user-scoped
// absolute path. If the path starts with "shared/", it resolves to the
// agent's shared memory sector (accessible to all users). Otherwise the path
// is placed under the user's private scope.
//
// Examples (BaseSector = "agents/exec-assistant", UserScope = "users/alice"):
//
//	"notes.md"         → "agents/exec-assistant/users/alice/notes.md"
//	"shared/policies"  → "agents/exec-assistant/shared/policies"
//	""                 → "agents/exec-assistant/users/alice"
func (s *UserMemoryScope) ResolveMemoryPath(relativePath string) (string, error) {
	// Normalize base sector: strip cog://mem/ prefix and trailing slash
	base := s.BaseSector
	base = strings.TrimPrefix(base, "cog://mem/")
	base = strings.TrimSuffix(base, "/")

	// Block path traversal
	cleaned := path.Clean(relativePath)
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("path traversal blocked: %q attempts to escape scope", relativePath)
	}

	// Shared paths resolve directly under the agent's base sector
	if strings.HasPrefix(relativePath, "shared/") || relativePath == "shared" {
		return path.Join(base, relativePath), nil
	}

	// User paths resolve under the user's scope
	userBase := path.Join(base, s.UserScope)
	if relativePath == "" {
		return userBase, nil
	}
	return path.Join(userBase, relativePath), nil
}

// CanRead returns whether this user can read the given path.
//
// Permission matrix:
//
//	admin: can read own scope, shared, and other users' scopes
//	rw:    can read own scope and shared
//	ro:    can read own scope and shared
//	none:  cannot read anything
func (s *UserMemoryScope) CanRead(memPath string) bool {
	if s.Level == "none" || s.Level == "" {
		return false
	}

	// Admin can read everything
	if s.Level == "admin" {
		return true
	}

	// rw and ro can read shared memory
	if isSharedPath(memPath) {
		return true
	}

	// rw and ro can read their own user scope
	if s.isOwnPath(memPath) {
		return true
	}

	// rw and ro cannot read other users' memory
	if IsUserScopedPath(memPath) {
		return false
	}

	// Non-user, non-shared paths within the agent sector are readable
	return true
}

// CanWrite returns whether this user can write to the given path.
//
// Permission matrix:
//
//	admin: can write own scope, shared, and other users' scopes
//	rw:    can write own scope and shared
//	ro:    cannot write anything
//	none:  cannot write anything
func (s *UserMemoryScope) CanWrite(memPath string) bool {
	switch s.Level {
	case "none", "", "ro":
		return false
	case "admin":
		return true
	case "rw":
		// rw can write to own scope
		if s.isOwnPath(memPath) {
			return true
		}
		// rw can write to shared
		if isSharedPath(memPath) {
			return true
		}
		// rw cannot write to other users' memory or unscoped agent paths
		return false
	default:
		return false
	}
}

// isOwnPath checks if a path belongs to this user's scope.
func (s *UserMemoryScope) isOwnPath(memPath string) bool {
	if s.UserScope == "" {
		return false
	}
	// Normalize: the user scope pattern we look for in the path
	// e.g., "users/alice/" or path ends with "users/alice"
	scope := s.UserScope
	scope = strings.TrimSuffix(scope, "/")

	return strings.Contains(memPath, scope+"/") || strings.HasSuffix(memPath, scope)
}

// isSharedPath checks if a path is within a shared memory sector.
func isSharedPath(memPath string) bool {
	// Match "shared/" segment or path ending with "/shared"
	return strings.Contains(memPath, "/shared/") ||
		strings.HasSuffix(memPath, "/shared") ||
		strings.HasPrefix(memPath, "shared/") ||
		memPath == "shared"
}

// BuildUserScope creates a UserMemoryScope from an agent CRD and user identity.
// Returns nil if:
//   - the CRD has no user access rules
//   - the user is not listed and defaultLevel is empty or "none"
func BuildUserScope(crd *AgentCRD, userID string) *UserMemoryScope {
	access := crd.Spec.Access

	// No user access rules defined at all
	if len(access.Users) == 0 && access.DefaultLevel == "" {
		return nil
	}

	// Look up the user
	userAccess, found := access.Users[userID]

	var level string
	var memoryScope string

	if found {
		level = userAccess.Level
		memoryScope = userAccess.MemoryScope
	} else {
		// Use default level for unlisted users
		level = access.DefaultLevel
		if level == "" {
			level = "none"
		}
		// Generate a default memory scope for unlisted users
		memoryScope = "users/" + userID
	}

	if level == "none" {
		return nil
	}

	// Resolve base sector from CRD context
	baseSector := crd.Spec.Context.Memory.Sector
	if baseSector == "" {
		// Fallback: construct from agent name
		baseSector = "cog://mem/semantic/agents/" + crd.Metadata.Name + "/"
	}

	return &UserMemoryScope{
		AgentID:    crd.Metadata.Name,
		BaseSector: baseSector,
		UserID:     userID,
		UserScope:  memoryScope,
		Level:      level,
	}
}

// IsUserScopedPath returns true if the path falls within a user-specific
// memory sector (i.e., contains a "/users/" segment).
func IsUserScopedPath(p string) bool {
	return strings.Contains(p, "/users/") ||
		strings.HasPrefix(p, "users/")
}

// ExtractUserFromPath extracts the user ID from a user-scoped path.
// It finds the first "users/" segment and returns the next path component.
//
// Examples:
//
//	"agents/exec/users/alice/notes.md" → "alice"
//	"users/erin/drafts.md"            → "erin"
//	"agents/exec/shared/policies.md"  → "" (not user-scoped)
func ExtractUserFromPath(p string) string {
	// Look for "/users/" or leading "users/"
	idx := strings.Index(p, "/users/")
	if idx >= 0 {
		rest := p[idx+len("/users/"):]
		return firstPathSegment(rest)
	}
	if strings.HasPrefix(p, "users/") {
		rest := strings.TrimPrefix(p, "users/")
		return firstPathSegment(rest)
	}
	return ""
}

// firstPathSegment returns the first component of a slash-separated path.
// e.g., "alice/notes.md" → "alice", "alice" → "alice", "" → ""
func firstPathSegment(p string) string {
	if p == "" {
		return ""
	}
	if idx := strings.Index(p, "/"); idx >= 0 {
		return p[:idx]
	}
	return p
}
