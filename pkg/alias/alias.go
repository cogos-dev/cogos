// Package alias manages the CogOS workspace alias map stored at
// ~/.cog/node/aliases.yaml.
//
// Aliases are short, human-friendly names for workspace names registered in
// ~/.cog/node/global.yaml.  They appear in the authority slot of a
// cog://authority/path URI:
//
//	cog://cog/mem/semantic/x    — "cog" resolves via alias to "cog-workspace"
//	cog://kernel/adr/067         — "kernel" resolves to "myrgic/cogos"
//
// Aliases are a display/input convenience.  The kernel always stores and logs
// the canonical workspace name; aliases are expanded at parse time.
//
// Schema (YAML):
//
//	version: "1.0"
//	aliases:
//	  cog: cog-workspace                         # short form
//	  m3:                                        # long form with metadata
//	    workspace: myrgic/mod3
//	    description: "mod3 voice server"
//	    node: node-a                             # optional node pin
//
// Validation rules:
//   - Alias name must match ^[a-z][a-z0-9_-]{0,30}$
//   - Target must be a literal workspace name (no alias-of-alias)
//   - Target must not shadow a known projection name (enforced at load time;
//     TODO: load projection set from pkg/substrate/uri/namespace.go after #166 merges)
//   - Stale aliases (target not in global.yaml) are tolerated but flagged
package alias

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	"gopkg.in/yaml.v3"
)

// aliasNameRe validates alias names per the spec.
var aliasNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)

// reservedNames are the cog:// projection namespace names that alias names
// must not shadow.  This list mirrors pkg/substrate/uri/namespace.Namespaces.
// TODO: import pkg/substrate/uri/namespace.Namespaces directly once #166 lands and the
// namespace package stabilises — for now we hardcode to avoid a circular dep.
var reservedNames = map[string]bool{
	"mem": true, "signals": true, "context": true, "thread": true,
	"coherence": true, "identity": true, "src": true, "adr": true,
	"ledger": true, "inference": true, "kernel": true, "hooks": true,
	"spec": true, "specs": true, "status": true, "canonical": true,
	"handoff": true, "handoffs": true, "crystal": true,
	"role": true, "roles": true, "skill": true, "skills": true,
	"agent": true, "agents": true, "conf": true, "config": true,
	"ontology": true, "work": true, "artifact": true, "artifacts": true,
	"docs": true,
}

// ErrStaleAlias is returned by Resolve / used by List when an alias target
// is no longer present in the workspace registry (global.yaml).
var ErrStaleAlias = errors.New("alias: stale alias (target not in registry)")

// ErrUnknownAlias is returned when an alias name is not found.
var ErrUnknownAlias = errors.New("alias: unknown alias")

// ErrInvalidAliasName is returned when a name fails the regex.
var ErrInvalidAliasName = errors.New("alias: invalid alias name")

// ErrReservedName is returned when a name collides with a projection.
var ErrReservedName = errors.New("alias: name is a reserved projection namespace")

// ErrAliasOfAlias is returned when a target is itself an alias name (not
// allowed — targets must be literal workspace names).
var ErrAliasOfAlias = errors.New("alias: target is an alias name (alias-of-alias not allowed)")

// ── Schema types ─────────────────────────────────────────────────────────────

// aliasFile is the on-disk YAML representation.
type aliasFile struct {
	Version string                 `yaml:"version"`
	Aliases map[string]interface{} `yaml:"aliases,omitempty"`
}

// rawEntry is the fully-expanded per-alias representation.
type rawEntry struct {
	Workspace   string `yaml:"workspace"`
	Description string `yaml:"description,omitempty"`
	Node        string `yaml:"node,omitempty"`
}

// AliasOpts carries optional fields for Add.
type AliasOpts struct {
	// Description is a human-readable note stored alongside the alias.
	Description string
	// Node pins the alias to a specific node name (e.g. "node-a").
	// Empty means "any node that hosts the workspace".
	Node string
}

// AliasEntry is a single resolved alias, as returned by List.
type AliasEntry struct {
	Name        string
	Workspace   string
	Node        string
	Description string
	// Stale is true when the target workspace is not present in global.yaml.
	Stale bool
}

// ── AliasMap ─────────────────────────────────────────────────────────────────

// AliasMap holds the in-memory alias table loaded from aliases.yaml.
type AliasMap struct {
	entries map[string]*rawEntry   // alias name → resolved entry
	nodeDir string                 // ~/.cog/node/
	known   map[string]bool        // workspace names present in global.yaml
}

// aliasesPath returns the full path to ~/.cog/node/aliases.yaml.
func aliasesPath(nodeDir string) string {
	return filepath.Join(nodeDir, "aliases.yaml")
}

// aliasLockPath returns the companion lock file path.
func aliasLockPath(nodeDir string) string {
	return filepath.Join(nodeDir, "aliases.yaml.lock")
}

// Load reads ~/.cog/node/aliases.yaml and returns an AliasMap.
// If the file does not exist, an empty map is returned (not an error).
// nodeDir is typically filepath.Join(os.UserHomeDir(), ".cog", "node").
//
// The known workspace set is loaded from global.yaml (same nodeDir) for
// stale-alias detection; if global.yaml is absent, all targets are treated
// as live (not stale).
func Load(nodeDir string) (*AliasMap, error) {
	known, _ := loadKnownWorkspaces(nodeDir) // best-effort; nil map is handled

	path := aliasesPath(nodeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &AliasMap{
				entries: make(map[string]*rawEntry),
				nodeDir: nodeDir,
				known:   known,
			}, nil
		}
		return nil, fmt.Errorf("alias: read %q: %w", path, err)
	}

	var f aliasFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("alias: parse %q: %w", path, err)
	}

	entries, err := parseAliasMap(f.Aliases)
	if err != nil {
		return nil, fmt.Errorf("alias: %w", err)
	}

	return &AliasMap{
		entries: entries,
		nodeDir: nodeDir,
		known:   known,
	}, nil
}

// Expand resolves an alias name to (workspace, node, true) or returns
// ("", "", false) if the name is not an alias.
// It does NOT check whether the workspace is stale; callers that care about
// staleness should call List().
func (m *AliasMap) Expand(name string) (workspace, node string, ok bool) {
	e, found := m.entries[name]
	if !found {
		return "", "", false
	}
	return e.Workspace, e.Node, true
}

// Add creates or updates an alias.  All writes are serialised via filelock.
//
// Validation (before taking lock):
//   - name must match aliasNameRe
//   - name must not be a reserved projection namespace
//   - workspace must not itself be a known alias name (no alias-of-alias)
func (m *AliasMap) Add(name, workspace string, opts AliasOpts) error {
	name = strings.TrimSpace(name)
	workspace = strings.TrimSpace(workspace)

	if !aliasNameRe.MatchString(name) {
		return fmt.Errorf("%w: %q (must match ^[a-z][a-z0-9_-]{0,30}$)", ErrInvalidAliasName, name)
	}
	if reservedNames[name] {
		return fmt.Errorf("%w: %q", ErrReservedName, name)
	}
	// Prevent alias-of-alias: reject if workspace resolves to another alias.
	if _, isAlias := m.entries[workspace]; isAlias {
		return fmt.Errorf("%w: %q resolves to another alias", ErrAliasOfAlias, workspace)
	}

	lock, err := filelock.Acquire(aliasLockPath(m.nodeDir), 2*time.Second)
	if err != nil {
		return fmt.Errorf("alias: %w", err)
	}
	defer lock.Release()

	// Re-read under lock to avoid lost-update.
	current, err := readFile(m.nodeDir)
	if err != nil {
		return err
	}

	e := &rawEntry{
		Workspace:   workspace,
		Description: opts.Description,
		Node:        opts.Node,
	}
	current.entries[name] = e

	if err := writeFile(m.nodeDir, current); err != nil {
		return err
	}

	// Update in-memory view.
	m.entries[name] = e
	return nil
}

// Remove deletes an alias by name.  The operation is idempotent — removing
// an alias that does not exist is not an error.
func (m *AliasMap) Remove(name string) error {
	lock, err := filelock.Acquire(aliasLockPath(m.nodeDir), 2*time.Second)
	if err != nil {
		return fmt.Errorf("alias: %w", err)
	}
	defer lock.Release()

	current, err := readFile(m.nodeDir)
	if err != nil {
		return err
	}

	delete(current.entries, name)

	if err := writeFile(m.nodeDir, current); err != nil {
		return err
	}

	delete(m.entries, name)
	return nil
}

// List returns a sorted slice of all aliases with staleness flags set.
func (m *AliasMap) List() []AliasEntry {
	out := make([]AliasEntry, 0, len(m.entries))
	for name, e := range m.entries {
		stale := len(m.known) > 0 && !m.known[e.Workspace]
		out = append(out, AliasEntry{
			Name:        name,
			Workspace:   e.Workspace,
			Node:        e.Node,
			Description: e.Description,
			Stale:       stale,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ── I/O helpers ──────────────────────────────────────────────────────────────

// readFile reads aliases.yaml without taking a lock (caller holds lock).
func readFile(nodeDir string) (*AliasMap, error) {
	path := aliasesPath(nodeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &AliasMap{entries: make(map[string]*rawEntry), nodeDir: nodeDir}, nil
		}
		return nil, fmt.Errorf("alias: read %q: %w", path, err)
	}
	var f aliasFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("alias: parse %q: %w", path, err)
	}
	entries, err := parseAliasMap(f.Aliases)
	if err != nil {
		return nil, fmt.Errorf("alias: %w", err)
	}
	return &AliasMap{entries: entries, nodeDir: nodeDir}, nil
}

// writeFile serialises and atomically writes aliases.yaml (caller holds lock).
func writeFile(nodeDir string, m *AliasMap) error {
	// Build canonical YAML-friendly map.
	out := make(map[string]interface{}, len(m.entries))
	for name, e := range m.entries {
		if e.Description == "" && e.Node == "" {
			// Short form: just the workspace string.
			out[name] = e.Workspace
		} else {
			out[name] = map[string]string{
				"workspace":   e.Workspace,
				"description": e.Description,
				"node":        e.Node,
			}
		}
	}
	f := aliasFile{Version: "1.0", Aliases: out}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("alias: marshal: %w", err)
	}

	path := aliasesPath(nodeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("alias: mkdir %q: %w", filepath.Dir(path), err)
	}

	tmp := path + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("alias: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("alias: rename: %w", err)
	}
	return nil
}

// parseAliasMap converts the raw YAML aliases field (map[string]interface{})
// into the internal entries map.  Both short form (string value) and long
// form (map with workspace/description/node) are accepted.
func parseAliasMap(raw map[string]interface{}) (map[string]*rawEntry, error) {
	entries := make(map[string]*rawEntry, len(raw))
	for name, val := range raw {
		if !aliasNameRe.MatchString(name) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidAliasName, name)
		}
		switch v := val.(type) {
		case string:
			entries[name] = &rawEntry{Workspace: v}
		case map[string]interface{}:
			e := &rawEntry{}
			if ws, ok := v["workspace"].(string); ok {
				e.Workspace = ws
			}
			if desc, ok := v["description"].(string); ok {
				e.Description = desc
			}
			if node, ok := v["node"].(string); ok {
				e.Node = node
			}
			if e.Workspace == "" {
				return nil, fmt.Errorf("alias %q: missing workspace field", name)
			}
			entries[name] = e
		default:
			return nil, fmt.Errorf("alias %q: unexpected value type %T", name, val)
		}
	}
	return entries, nil
}

// ── GlobalConfig adapter ──────────────────────────────────────────────────────

// globalRegistryEntry is the minimal shape of a global.yaml workspace entry
// that we need for stale detection.
type globalRegistryEntry struct {
	Path string `yaml:"path"`
}

// globalRegistry is the shape of global.yaml (subset we need).
type globalRegistry struct {
	Workspaces map[string]*globalRegistryEntry `yaml:"workspaces,omitempty"`
}

// loadKnownWorkspaces reads global.yaml and returns the set of registered
// workspace names.  Returns nil map on any error (caller treats nil as
// "no information available" — don't flag anything as stale).
func loadKnownWorkspaces(nodeDir string) (map[string]bool, error) {
	path := filepath.Join(nodeDir, "global.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg globalRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(reg.Workspaces))
	for name := range reg.Workspaces {
		known[name] = true
	}
	return known, nil
}
