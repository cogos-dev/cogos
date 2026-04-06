// nucleus.go — CogOS v3 nucleus
//
// The nucleus is the always-loaded identity context: the runtime object that
// is never evicted from memory. It replaces the v2 pattern of loading the
// identity card from disk at session start.
//
// In v3, the nucleus is loaded once at daemon startup and held in memory for
// the lifetime of the process. It is the "floor" of the attentional field.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Nucleus is the always-above-threshold identity context.
// It holds the parsed identity card and the workspace root.
type Nucleus struct {
	mu sync.RWMutex

	// Name is the identity name (e.g. "Cog", "Sandy").
	Name string

	// Role is the identity role descriptor.
	Role string

	// Card is the full text of the identity card (markdown).
	Card string

	// WorkspaceRoot is the absolute path to the workspace.
	WorkspaceRoot string

	// LoadedAt records when this nucleus was loaded.
	LoadedAt time.Time
}

// identityYAML is the on-disk structure of .cog/config/identity.yaml.
type identityYAML struct {
	DefaultIdentity   string `yaml:"default_identity"`
	IdentityDirectory string `yaml:"identity_directory"`
}

// identityFrontmatter is the YAML header in an identity card file.
type identityFrontmatter struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
}

// LoadNucleus reads the current identity from .cog/config/identity.yaml and
// loads the corresponding identity card file. Falls back to an embedded
// default identity if no config or card exists.
func LoadNucleus(cfg *Config) (*Nucleus, error) {
	// Try to read identity config.
	identCfgPath := filepath.Join(cfg.CogDir, "config", "identity.yaml")
	identData, err := os.ReadFile(identCfgPath)

	var name string
	var identDir string

	if err == nil {
		var identCfg identityYAML
		if parseErr := yaml.Unmarshal(identData, &identCfg); parseErr == nil {
			name = identCfg.DefaultIdentity
			identDir = identCfg.IdentityDirectory
		}
	}

	if name == "" {
		name = "cogos"
	}

	// Resolve identity card path.
	if identDir == "" {
		identDir = filepath.Join(cfg.WorkspaceRoot, ".cog", "agents", "identities")
	} else if !filepath.IsAbs(identDir) {
		identDir = filepath.Join(cfg.WorkspaceRoot, identDir)
	}

	cardPath := filepath.Join(identDir, fmt.Sprintf("identity_%s.md", strings.ToLower(name)))
	cardData, err := os.ReadFile(cardPath)
	if err != nil {
		// Fall back to embedded default identity.
		cardData, err = defaultsFS.ReadFile("defaults/identity.md")
		if err != nil {
			return nil, fmt.Errorf("no identity card found and embedded default unavailable: %w", err)
		}
	}

	// Parse frontmatter.
	fm, body := parseIdentityFrontmatter(string(cardData))

	n := &Nucleus{
		Name:          fm.Name,
		Role:          fm.Role,
		Card:          body,
		WorkspaceRoot: cfg.WorkspaceRoot,
		LoadedAt:      time.Now(),
	}

	if n.Name == "" {
		n.Name = name
	}

	return n, nil
}

// Summary returns a compact one-line description of the nucleus for logging.
func (n *Nucleus) Summary() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return fmt.Sprintf("identity=%s role=%q loaded=%s", n.Name, n.Role, n.LoadedAt.Format(time.RFC3339))
}

// parseIdentityFrontmatter splits YAML frontmatter (delimited by ---) from body.
func parseIdentityFrontmatter(content string) (identityFrontmatter, string) {
	var fm identityFrontmatter

	if !strings.HasPrefix(content, "---") {
		return fm, content
	}

	// Find closing ---
	rest := content[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return fm, content
	}

	fmRaw := rest[:idx]
	body := rest[idx+4:]
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	}

	_ = yaml.Unmarshal([]byte(fmRaw), &fm)
	return fm, body
}
