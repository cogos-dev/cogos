// memory_scope.go — Zero-churn root alias shim + BuildUserScope.
// Pure helpers (UserMemoryScope, ResolveMemoryPath, CanRead, CanWrite,
// IsUserScopedPath, ExtractUserFromPath) live in internal/identity per
// ADR-100 P0 extraction.
//
// BuildUserScope stays here because it couples to the root *AgentCRD type
// and is superseded in a later wave.

package main

import (
	"github.com/myrgic/cogos/internal/identity"
)

// UserMemoryScope resolves memory paths for user-scoped agent memory.
// Canonical home: internal/identity.UserMemoryScope.
type UserMemoryScope = identity.UserMemoryScope

// IsUserScopedPath returns true if the path falls within a user-specific
// memory sector (i.e., contains a "/users/" segment).
// Canonical home: internal/identity.IsUserScopedPath.
func IsUserScopedPath(p string) bool {
	return identity.IsUserScopedPath(p)
}

// ExtractUserFromPath extracts the user ID from a user-scoped path.
// Canonical home: internal/identity.ExtractUserFromPath.
func ExtractUserFromPath(p string) string {
	return identity.ExtractUserFromPath(p)
}

// BuildUserScope creates a UserMemoryScope from an agent CRD and user identity.
// Returns nil if:
//   - the CRD has no user access rules
//   - the user is not listed and defaultLevel is empty or "none"
//
// Stays in root because it couples to the root *AgentCRD type.
// Superseded in a later extraction wave.
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
