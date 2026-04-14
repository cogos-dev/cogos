// memory_scope_test.go
// Test suite for multi-user memory isolation (D4).
//
// Validates that UserMemoryScope enforces per-user memory boundaries:
// - Path resolution routes to the correct user/shared sector
// - Path traversal attempts are blocked
// - Read/write permissions follow the admin > rw > ro > none hierarchy
// - BuildUserScope correctly maps CRD access rules to scopes

package main

import (
	"testing"
)

// ─── Helper ─────────────────────────────────────────────────────────────────────

// testScope constructs a UserMemoryScope for testing without requiring a CRD.
func testScope(userID, level string) *UserMemoryScope {
	return &UserMemoryScope{
		AgentID:    "exec",
		BaseSector: "cog://mem/semantic/agents/exec/",
		UserID:     userID,
		UserScope:  "users/" + userID,
		Level:      level,
	}
}

// testCRD constructs a minimal AgentCRD for BuildUserScope tests.
func testCRD(users map[string]AgentCRDUserAccess, defaultLevel string) *AgentCRD {
	return &AgentCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "Agent",
		Metadata: AgentCRDMeta{
			Name: "exec",
		},
		Spec: AgentCRDSpec{
			Context: AgentCRDContext{
				Memory: AgentCRDMemory{
					Sector: "cog://mem/semantic/agents/exec/",
				},
			},
			Access: AgentCRDAccess{
				Users:        users,
				DefaultLevel: defaultLevel,
			},
		},
	}
}

// ─── BuildUserScope Tests ────────────────────────────────────────────────────────

func TestMemoryScopeBuildUserScope(t *testing.T) {
	users := map[string]AgentCRDUserAccess{
		"alice": {Level: "admin", MemoryScope: "users/alice"},
		"erin": {Level: "rw", MemoryScope: "users/erin"},
		"dana": {Level: "ro", MemoryScope: "users/dana"},
	}

	t.Run("admin user gets admin scope", func(t *testing.T) {
		crd := testCRD(users, "none")
		scope := BuildUserScope(crd, "alice")
		if scope == nil {
			t.Fatal("expected non-nil scope for admin user")
		}
		if scope.Level != "admin" {
			t.Errorf("level = %q, want %q", scope.Level, "admin")
		}
		if scope.UserScope != "users/alice" {
			t.Errorf("userScope = %q, want %q", scope.UserScope, "users/alice")
		}
		if scope.UserID != "alice" {
			t.Errorf("userID = %q, want %q", scope.UserID, "alice")
		}
	})

	t.Run("rw user gets rw scope", func(t *testing.T) {
		crd := testCRD(users, "none")
		scope := BuildUserScope(crd, "erin")
		if scope == nil {
			t.Fatal("expected non-nil scope for rw user")
		}
		if scope.Level != "rw" {
			t.Errorf("level = %q, want %q", scope.Level, "rw")
		}
		if scope.UserScope != "users/erin" {
			t.Errorf("userScope = %q, want %q", scope.UserScope, "users/erin")
		}
	})

	t.Run("ro user gets ro scope", func(t *testing.T) {
		crd := testCRD(users, "none")
		scope := BuildUserScope(crd, "dana")
		if scope == nil {
			t.Fatal("expected non-nil scope for ro user")
		}
		if scope.Level != "ro" {
			t.Errorf("level = %q, want %q", scope.Level, "ro")
		}
	})

	t.Run("unknown user with defaultLevel=none returns nil", func(t *testing.T) {
		crd := testCRD(users, "none")
		scope := BuildUserScope(crd, "stranger")
		if scope != nil {
			t.Errorf("expected nil scope for unknown user with defaultLevel=none, got level=%q", scope.Level)
		}
	})

	t.Run("unknown user with empty defaultLevel returns nil", func(t *testing.T) {
		crd := testCRD(users, "")
		scope := BuildUserScope(crd, "stranger")
		if scope != nil {
			t.Errorf("expected nil scope for unknown user with empty default, got level=%q", scope.Level)
		}
	})

	t.Run("unknown user with defaultLevel=ro gets ro scope", func(t *testing.T) {
		crd := testCRD(users, "ro")
		scope := BuildUserScope(crd, "guest")
		if scope == nil {
			t.Fatal("expected non-nil scope for user with defaultLevel=ro")
		}
		if scope.Level != "ro" {
			t.Errorf("level = %q, want %q", scope.Level, "ro")
		}
		// Auto-generated scope for unlisted users
		if scope.UserScope != "users/guest" {
			t.Errorf("userScope = %q, want %q", scope.UserScope, "users/guest")
		}
	})

	t.Run("no access rules returns nil", func(t *testing.T) {
		crd := testCRD(nil, "")
		scope := BuildUserScope(crd, "anyone")
		if scope != nil {
			t.Errorf("expected nil scope when no access rules defined, got level=%q", scope.Level)
		}
	})

	t.Run("fallback base sector from agent name", func(t *testing.T) {
		crd := testCRD(users, "none")
		crd.Spec.Context.Memory.Sector = "" // clear explicit sector
		scope := BuildUserScope(crd, "alice")
		if scope == nil {
			t.Fatal("expected non-nil scope")
		}
		if scope.BaseSector != "cog://mem/semantic/agents/exec/" {
			t.Errorf("baseSector = %q, want fallback from agent name", scope.BaseSector)
		}
	})
}

// ─── ResolveMemoryPath Tests ─────────────────────────────────────────────────────

func TestMemoryScopeResolveMemoryPath(t *testing.T) {
	tests := []struct {
		name     string
		relPath  string
		want     string
		wantErr  bool
	}{
		{
			name:    "user path resolves to user scope",
			relPath: "notes.md",
			want:    "semantic/agents/exec/users/alice/notes.md",
		},
		{
			name:    "nested user path",
			relPath: "projects/alpha/tasks.md",
			want:    "semantic/agents/exec/users/alice/projects/alpha/tasks.md",
		},
		{
			name:    "empty path resolves to user base",
			relPath: "",
			want:    "semantic/agents/exec/users/alice",
		},
		{
			name:    "shared/ path resolves to shared sector",
			relPath: "shared/policies.md",
			want:    "semantic/agents/exec/shared/policies.md",
		},
		{
			name:    "shared without trailing content",
			relPath: "shared",
			want:    "semantic/agents/exec/shared",
		},
		{
			name:    "shared nested path",
			relPath: "shared/templates/weekly.md",
			want:    "semantic/agents/exec/shared/templates/weekly.md",
		},
		{
			name:    "path traversal with leading ..",
			relPath: "../../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "path traversal with embedded ..",
			relPath: "notes/../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "path traversal double dot only",
			relPath: "..",
			wantErr: true,
		},
	}

	scope := testScope("alice", "admin")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scope.ResolveMemoryPath(tt.relPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ResolveMemoryPath(%q) error = %v, wantErr %v", tt.relPath, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ResolveMemoryPath(%q) = %q, want %q", tt.relPath, got, tt.want)
			}
		})
	}
}

// ─── CanRead Tests ───────────────────────────────────────────────────────────────

func TestMemoryScopeCanRead(t *testing.T) {
	tests := []struct {
		name    string
		userID  string
		level   string
		memPath string
		want    bool
	}{
		// admin can read everything
		{
			name:    "admin reads own path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/users/alice/notes.md",
			want:    true,
		},
		{
			name:    "admin reads other users path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/users/erin/private.md",
			want:    true,
		},
		{
			name:    "admin reads shared path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/shared/policies.md",
			want:    true,
		},

		// rw can read own + shared, not others
		{
			name:    "rw reads own path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/users/erin/tasks.md",
			want:    true,
		},
		{
			name:    "rw reads shared path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/shared/templates.md",
			want:    true,
		},
		{
			name:    "rw cannot read other users path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/users/alice/notes.md",
			want:    false,
		},

		// ro can read own + shared, not others
		{
			name:    "ro reads own path",
			userID:  "dana", level: "ro",
			memPath: "semantic/agents/exec/users/dana/info.md",
			want:    true,
		},
		{
			name:    "ro reads shared path",
			userID:  "dana", level: "ro",
			memPath: "semantic/agents/exec/shared/readme.md",
			want:    true,
		},
		{
			name:    "ro cannot read other users path",
			userID:  "dana", level: "ro",
			memPath: "semantic/agents/exec/users/alice/secret.md",
			want:    false,
		},

		// none cannot read anything
		{
			name:    "none cannot read own path",
			userID:  "nobody", level: "none",
			memPath: "semantic/agents/exec/users/nobody/anything.md",
			want:    false,
		},
		{
			name:    "none cannot read shared path",
			userID:  "nobody", level: "none",
			memPath: "semantic/agents/exec/shared/policies.md",
			want:    false,
		},

		// empty level treated as none
		{
			name:    "empty level cannot read",
			userID:  "ghost", level: "",
			memPath: "semantic/agents/exec/shared/anything.md",
			want:    false,
		},

		// rw/ro can read non-user non-shared agent paths
		{
			name:    "rw reads agent-level path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/config.md",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := testScope(tt.userID, tt.level)
			got := scope.CanRead(tt.memPath)
			if got != tt.want {
				t.Errorf("CanRead(%q) [user=%s level=%s] = %v, want %v",
					tt.memPath, tt.userID, tt.level, got, tt.want)
			}
		})
	}
}

// ─── CanWrite Tests ──────────────────────────────────────────────────────────────

func TestMemoryScopeCanWrite(t *testing.T) {
	tests := []struct {
		name    string
		userID  string
		level   string
		memPath string
		want    bool
	}{
		// admin can write everywhere
		{
			name:    "admin writes own path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/users/alice/notes.md",
			want:    true,
		},
		{
			name:    "admin writes other users path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/users/erin/override.md",
			want:    true,
		},
		{
			name:    "admin writes shared path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/shared/new-policy.md",
			want:    true,
		},
		{
			name:    "admin writes agent-level path",
			userID:  "alice", level: "admin",
			memPath: "semantic/agents/exec/config.md",
			want:    true,
		},

		// rw can write own + shared
		{
			name:    "rw writes own path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/users/erin/tasks.md",
			want:    true,
		},
		{
			name:    "rw writes shared path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/shared/contrib.md",
			want:    true,
		},
		{
			name:    "rw cannot write other users path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/users/alice/notes.md",
			want:    false,
		},
		{
			name:    "rw cannot write agent-level path",
			userID:  "erin", level: "rw",
			memPath: "semantic/agents/exec/config.md",
			want:    false,
		},

		// ro cannot write anything
		{
			name:    "ro cannot write own path",
			userID:  "dana", level: "ro",
			memPath: "semantic/agents/exec/users/dana/info.md",
			want:    false,
		},
		{
			name:    "ro cannot write shared path",
			userID:  "dana", level: "ro",
			memPath: "semantic/agents/exec/shared/policies.md",
			want:    false,
		},

		// none cannot write anything
		{
			name:    "none cannot write",
			userID:  "nobody", level: "none",
			memPath: "semantic/agents/exec/users/nobody/anything.md",
			want:    false,
		},

		// empty level treated as none
		{
			name:    "empty level cannot write",
			userID:  "ghost", level: "",
			memPath: "semantic/agents/exec/shared/anything.md",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := testScope(tt.userID, tt.level)
			got := scope.CanWrite(tt.memPath)
			if got != tt.want {
				t.Errorf("CanWrite(%q) [user=%s level=%s] = %v, want %v",
					tt.memPath, tt.userID, tt.level, got, tt.want)
			}
		})
	}
}

// ─── IsUserScopedPath Tests ──────────────────────────────────────────────────────

func TestMemoryScopeIsUserScopedPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"semantic/agents/exec/users/alice/notes.md", true},
		{"users/erin/tasks.md", true},
		{"semantic/agents/exec/shared/policies.md", false},
		{"semantic/agents/exec/config.md", false},
		{"shared/readme.md", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsUserScopedPath(tt.path)
			if got != tt.want {
				t.Errorf("IsUserScopedPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// ─── ExtractUserFromPath Tests ───────────────────────────────────────────────────

func TestMemoryScopeExtractUserFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"agents/exec/users/alice/notes.md", "alice"},
		{"users/erin/drafts.md", "erin"},
		{"semantic/agents/exec/users/dana/info.md", "dana"},
		{"users/alice", "alice"},
		{"agents/exec/shared/policies.md", ""},
		{"shared/readme.md", ""},
		{"config.md", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := ExtractUserFromPath(tt.path)
			if got != tt.want {
				t.Errorf("ExtractUserFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// ─── Cross-User Isolation Tests ──────────────────────────────────────────────────

// TestMemoryScopeCrossUserIsolation is an integration-style test that validates
// the full isolation guarantee: two rw users cannot access each other's memory.
func TestMemoryScopeCrossUserIsolation(t *testing.T) {
	erin := testScope("erin", "rw")
	alice := testScope("alice", "rw")

	// Each user resolves their own path
	erinPath, err := erin.ResolveMemoryPath("secret.md")
	if err != nil {
		t.Fatalf("erin resolve: %v", err)
	}
	alicePath, err := alice.ResolveMemoryPath("secret.md")
	if err != nil {
		t.Fatalf("alice resolve: %v", err)
	}

	// Paths must be different
	if erinPath == alicePath {
		t.Errorf("isolation violated: both users resolved to %q", erinPath)
	}

	// Erin can read/write her own path
	if !erin.CanRead(erinPath) {
		t.Error("erin should read her own path")
	}
	if !erin.CanWrite(erinPath) {
		t.Error("erin should write her own path")
	}

	// Erin cannot read/write alice's path
	if erin.CanRead(alicePath) {
		t.Error("erin should NOT read alice's path")
	}
	if erin.CanWrite(alicePath) {
		t.Error("erin should NOT write alice's path")
	}

	// Chaz can read/write his own path
	if !alice.CanRead(alicePath) {
		t.Error("alice should read his own path")
	}
	if !alice.CanWrite(alicePath) {
		t.Error("alice should write his own path")
	}

	// Chaz cannot read/write erin's path
	if alice.CanRead(erinPath) {
		t.Error("alice should NOT read erin's path")
	}
	if alice.CanWrite(erinPath) {
		t.Error("alice should NOT write erin's path")
	}

	// Both can read/write shared
	sharedPath := "semantic/agents/exec/shared/collab.md"
	if !erin.CanRead(sharedPath) {
		t.Error("erin should read shared")
	}
	if !erin.CanWrite(sharedPath) {
		t.Error("erin should write shared")
	}
	if !alice.CanRead(sharedPath) {
		t.Error("alice should read shared")
	}
	if !alice.CanWrite(sharedPath) {
		t.Error("alice should write shared")
	}
}
