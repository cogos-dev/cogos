// components.go — Component registry, indexer, and CLI for CogOS kernel.
// Part of ADR-060 Phase 1: kernel-level component topology awareness.
//
// Provides:
//   - Data types for component registry, blobs, and Merkle index
//   - Git helpers for submodule discovery and tree hashing
//   - Content-addressable component indexer
//   - CLI: cog components [list|status|index|register]

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ────────────────────────────────────────────────────────────────────────────
// Registry Types (parsed from .cog/conf/components.cog.md)
// ────────────────────────────────────────────────────────────────────────────

// ComponentRegistry represents the full component manifest.
type ComponentRegistry struct {
	Reconciler ComponentReconcilerConfig `yaml:"reconciler"`
	Components map[string]ComponentDecl  `yaml:"components"`
}

// ComponentReconcilerConfig controls auto-discovery and pruning behaviour.
type ComponentReconcilerConfig struct {
	AutoDiscover       bool   `yaml:"auto_discover"`
	PruneUnregistered  bool   `yaml:"prune_unregistered"`
	HealthCheckTimeout string `yaml:"health_check_timeout"`
}

// ComponentDecl is a single declared component entry.
type ComponentDecl struct {
	Role       string             `yaml:"role"        json:"role"`
	URL        string             `yaml:"url"         json:"url,omitempty"`
	Kind       string             `yaml:"kind"        json:"kind"`
	Language   string             `yaml:"language"    json:"language,omitempty"`
	Required   bool               `yaml:"required"    json:"required"`
	Services   []ComponentService `yaml:"services"    json:"services,omitempty"`
	SyncPolicy string             `yaml:"sync_policy" json:"sync_policy"`
	Tags       []string           `yaml:"tags"        json:"tags,omitempty"`
	Notes      string             `yaml:"notes"       json:"notes,omitempty"`
}

// ComponentService describes a network service exposed by a component.
type ComponentService struct {
	Name   string `yaml:"name"   json:"name"`
	Port   int    `yaml:"port"   json:"port"`
	Health string `yaml:"health" json:"health,omitempty"`
}

// ────────────────────────────────────────────────────────────────────────────
// Blob and Index Types (content-addressable state)
// ────────────────────────────────────────────────────────────────────────────

// ComponentBlob is the indexed snapshot of a single component.
type ComponentBlob struct {
	Version      int            `json:"version"`
	Path         string         `json:"path"`
	URI          string         `json:"uri"`
	Kind         string         `json:"kind"`
	TreeHash     string         `json:"tree_hash"`
	CommitHash   string         `json:"commit_hash,omitempty"`
	BlobHash     string         `json:"blob_hash"`
	Dirty        bool           `json:"dirty"`
	Language     string         `json:"language,omitempty"`
	BuildSystem  string         `json:"build_system,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	IndexedAt    string         `json:"indexed_at"`
	Declared     *ComponentDecl `json:"declared,omitempty"`
}

// ComponentIndex is the Merkle-rooted index over all component blobs.
type ComponentIndex struct {
	Version    int               `json:"version"`
	RootHash   string            `json:"root_hash"`
	Count      int               `json:"count"`
	Components map[string]string `json:"components"` // path → blob_hash
	IndexedAt  string            `json:"indexed_at"`
}

// ────────────────────────────────────────────────────────────────────────────
// Git Helpers
// ────────────────────────────────────────────────────────────────────────────

// SubmoduleEntry represents one [submodule] section from .gitmodules.
type SubmoduleEntry struct {
	Path string
	URL  string
}

// parseGitmodules reads .gitmodules and returns all submodule entries.
func parseGitmodules(root string) ([]SubmoduleEntry, error) {
	path := filepath.Join(root, ".gitmodules")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []SubmoduleEntry
	var current *SubmoduleEntry

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "[submodule ") {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &SubmoduleEntry{}
			continue
		}

		if current == nil {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "path":
			current.Path = val
		case "url":
			current.URL = val
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}

	return entries, scanner.Err()
}

// SubmoduleStatus holds the status of one submodule from `git submodule status`.
type SubmoduleStatus struct {
	CommitHash  string
	Initialized bool
	Dirty       bool
}

// gitSubmoduleStatus runs `git -C root submodule status` and parses output.
// Line format: " abc123 path (desc)" or "+abc123 path (desc)" or "-abc123 path".
func gitSubmoduleStatus(root string) (map[string]SubmoduleStatus, error) {
	cmd, cancel := gitCmd("-C", root, "submodule", "status")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	result := make(map[string]SubmoduleStatus)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 {
			continue
		}

		prefix := line[0]
		rest := strings.TrimSpace(line[1:])
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}

		commitHash := fields[0]
		smPath := fields[1]

		status := SubmoduleStatus{
			CommitHash: commitHash,
		}

		switch prefix {
		case ' ':
			status.Initialized = true
		case '+':
			status.Initialized = true
			status.Dirty = true
		case '-':
			status.Initialized = false
		}

		result[smPath] = status
	}

	return result, nil
}

// componentTreeHash gets the tree hash for a component path.
// Uses `git ls-tree HEAD <path>` which returns the recorded commit (submodules)
// or tree object (in-tree directories).
func componentTreeHash(root, path, _ string) (string, error) {
	cmd, cancel := gitCmd("-C", root, "ls-tree", "HEAD", path)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("no tree entry for %s", path)
	}

	// Format: mode type hash\tpath
	tabIdx := strings.IndexByte(line, '\t')
	if tabIdx == -1 {
		return "", fmt.Errorf("unexpected ls-tree format: %s", line)
	}
	fields := strings.Fields(line[:tabIdx])
	if len(fields) < 3 {
		return "", fmt.Errorf("unexpected ls-tree fields: %s", line)
	}

	return fields[2], nil
}

// ────────────────────────────────────────────────────────────────────────────
// Capability Detection
// ────────────────────────────────────────────────────────────────────────────

// detectCapabilities examines a component directory for language and build system.
// Uses os.Stat only — no file reads.
func detectCapabilities(componentPath string) (language, buildSystem string, capabilities []string) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(componentPath, name))
		return err == nil
	}

	// Go
	if exists("go.mod") {
		language = "go"
		buildSystem = "go"
	}

	// Rust
	if exists("Cargo.toml") {
		language = "rust"
		buildSystem = "cargo"
	}

	// Python
	if exists("pyproject.toml") || exists("setup.py") {
		language = "python"
	}

	// TypeScript / JavaScript
	if exists("package.json") {
		if exists("tsconfig.json") {
			language = "typescript"
		} else if language == "" {
			language = "javascript"
		}
		if buildSystem == "" {
			buildSystem = "npm"
		}
	}

	// Makefile (may augment existing build system)
	if exists("Makefile") {
		if buildSystem == "" {
			buildSystem = "make"
		} else {
			buildSystem = buildSystem + "+make"
		}
	}

	// Docker
	if exists("Dockerfile") {
		capabilities = append(capabilities, "docker")
	}

	return
}

// ────────────────────────────────────────────────────────────────────────────
// Registry Loading
// ────────────────────────────────────────────────────────────────────────────

// loadComponentRegistry loads and parses .cog/conf/components.cog.md.
// The registry is a cogdoc: YAML frontmatter + markdown body containing a YAML code block.
func loadComponentRegistry(root string) (*ComponentRegistry, error) {
	regPath := filepath.Join(root, ".cog", "conf", "components.cog.md")
	data, err := os.ReadFile(regPath)
	if err != nil {
		return nil, fmt.Errorf("component registry not found: %w", err)
	}
	content := string(data)

	// Skip frontmatter: find "---\n" at start, then next "\n---\n"
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("component registry missing frontmatter")
	}
	fmEnd := strings.Index(content[4:], "\n---\n")
	if fmEnd == -1 {
		return nil, fmt.Errorf("component registry unclosed frontmatter")
	}
	body := content[4+fmEnd+5:] // skip past closing ---\n

	// Find ```yaml ... ``` code block in body
	yamlStart := strings.Index(body, "```yaml\n")
	if yamlStart == -1 {
		return nil, fmt.Errorf("component registry has no ```yaml code block")
	}
	yamlContent := body[yamlStart+8:]
	yamlEnd := strings.Index(yamlContent, "\n```")
	if yamlEnd == -1 {
		return nil, fmt.Errorf("component registry has unclosed ```yaml block")
	}
	yamlContent = yamlContent[:yamlEnd]

	var reg ComponentRegistry
	if err := yaml.Unmarshal([]byte(yamlContent), &reg); err != nil {
		return nil, fmt.Errorf("parse component registry YAML: %w", err)
	}
	return &reg, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Component Indexer
// ────────────────────────────────────────────────────────────────────────────

// indexComponents is the main indexing function.
//  1. Load registry (declared components)
//  2. Parse .gitmodules (discovered submodules)
//  3. Get submodule status
//  4. Merge: registry declarations + auto-discovered
//  5. For each: compute tree hash, detect capabilities, build blob
//  6. Write blob to .cog/.state/components/<encoded>.json
//  7. Build Merkle index from all blob hashes
//  8. Write index to .cog/.state/components/_index.json
func indexComponents(root string) (*ComponentIndex, error) {
	// Load registry
	reg, regErr := loadComponentRegistry(root)
	if regErr != nil {
		// If no registry, still discover from git
		reg = &ComponentRegistry{Components: make(map[string]ComponentDecl)}
	}

	// Get submodule info
	submodules, _ := parseGitmodules(root)
	statuses, _ := gitSubmoduleStatus(root)

	// Build merged component list
	allPaths := make(map[string]bool)
	for path := range reg.Components {
		allPaths[path] = true
	}
	if reg.Reconciler.AutoDiscover {
		for _, sm := range submodules {
			allPaths[sm.Path] = true
		}
	}

	// Index each component
	blobs := make(map[string]string) // path → blob_hash
	stateDir := filepath.Join(root, ".cog", ".state", "components")
	os.MkdirAll(stateDir, 0755)

	for path := range allPaths {
		blob := buildComponentBlob(root, path, reg, submodules, statuses)
		blob.BlobHash = computeBlobHash(blob)
		blob.IndexedAt = nowISO()

		writeComponentBlob(root, blob)
		blobs[path] = blob.BlobHash
	}

	// Build index
	idx := &ComponentIndex{
		Version:    1,
		RootHash:   computeMerkleRoot(blobs),
		Count:      len(blobs),
		Components: blobs,
		IndexedAt:  nowISO(),
	}

	writeComponentIndex(root, idx)
	return idx, nil
}

// buildComponentBlob assembles a blob for a single component path.
func buildComponentBlob(root, path string, reg *ComponentRegistry, submodules []SubmoduleEntry, statuses map[string]SubmoduleStatus) *ComponentBlob {
	blob := &ComponentBlob{
		Version: 1,
		Path:    path,
		URI:     PathToURI(root, path),
	}

	// Check if declared in registry
	if decl, ok := reg.Components[path]; ok {
		blob.Kind = decl.Kind
		blob.Language = decl.Language
		blob.Declared = &decl
	}

	// Determine kind if not declared
	if blob.Kind == "" {
		for _, sm := range submodules {
			if sm.Path == path {
				blob.Kind = "submodule"
				break
			}
		}
		if blob.Kind == "" {
			blob.Kind = "in-tree"
		}
	}

	// Get tree hash
	treeHash, _ := componentTreeHash(root, path, blob.Kind)
	blob.TreeHash = treeHash

	// Get submodule status if applicable
	if status, ok := statuses[path]; ok {
		blob.CommitHash = status.CommitHash
		blob.Dirty = status.Dirty
	}

	// Detect capabilities from filesystem
	absPath := filepath.Join(root, path)
	if info, err := os.Stat(absPath); err == nil && info.IsDir() {
		lang, buildSys, caps := detectCapabilities(absPath)
		if blob.Language == "" {
			blob.Language = lang
		}
		blob.BuildSystem = buildSys
		blob.Capabilities = caps
	}

	return blob
}

// computeBlobHash creates a deterministic hash of a blob's content.
// Excludes blob_hash (self-referential) and indexed_at (temporal).
func computeBlobHash(blob *ComponentBlob) string {
	tmp := *blob
	tmp.BlobHash = ""
	tmp.IndexedAt = ""
	data, _ := json.Marshal(tmp)
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// computeMerkleRoot sorts paths, concatenates their blob hashes, and hashes the result.
func computeMerkleRoot(blobs map[string]string) string {
	paths := make([]string, 0, len(blobs))
	for p := range blobs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var combined strings.Builder
	for _, p := range paths {
		combined.WriteString(p)
		combined.WriteString(":")
		combined.WriteString(blobs[p])
		combined.WriteString("\n")
	}

	h := sha256.Sum256([]byte(combined.String()))
	return "sha256:" + hex.EncodeToString(h[:])
}

// encodePath converts component paths for use as filenames.
// "apps/cogos" → "apps--cogos"
func encodePath(path string) string {
	return strings.ReplaceAll(path, "/", "--")
}

func writeComponentBlob(root string, blob *ComponentBlob) error {
	dir := filepath.Join(root, ".cog", ".state", "components")
	os.MkdirAll(dir, 0755)
	filename := encodePath(blob.Path) + ".json"
	data, err := json.MarshalIndent(blob, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, filename), data, 0644)
}

func writeComponentIndex(root string, idx *ComponentIndex) error {
	dir := filepath.Join(root, ".cog", ".state", "components")
	os.MkdirAll(dir, 0755)
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "_index.json"), data, 0644)
}

func loadComponentIndex(root string) (*ComponentIndex, error) {
	idxPath := filepath.Join(root, ".cog", ".state", "components", "_index.json")
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return nil, err
	}
	var idx ComponentIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func loadComponentBlob(root, componentPath string) (*ComponentBlob, error) {
	filename := encodePath(componentPath) + ".json"
	blobPath := filepath.Join(root, ".cog", ".state", "components", filename)
	data, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, err
	}
	var blob ComponentBlob
	if err := json.Unmarshal(data, &blob); err != nil {
		return nil, err
	}
	return &blob, nil
}

// ────────────────────────────────────────────────────────────────────────────
// CLI Commands
// ────────────────────────────────────────────────────────────────────────────

func cmdComponents(args []string) int {
	if len(args) == 0 {
		return cmdComponentsList(nil)
	}
	switch args[0] {
	case "list":
		return cmdComponentsList(args[1:])
	case "status":
		return cmdComponentsStatus(args[1:])
	case "index":
		return cmdComponentsIndex(args[1:])
	case "register":
		return cmdComponentsRegister(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", args[0])
		fmt.Fprintf(os.Stderr, "Usage: cog components [list|status|index|register]\n")
		return 1
	}
}

// cmdComponentsList lists all components with status.
func cmdComponentsList(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	jsonOutput := componentHasFlag(args, "--json")

	reg, regErr := loadComponentRegistry(root)
	submodules, _ := parseGitmodules(root)
	statuses, _ := gitSubmoduleStatus(root)

	// Build merged list
	type componentInfo struct {
		Path     string `json:"path"`
		URI      string `json:"uri"`
		Role     string `json:"role"`
		Kind     string `json:"kind"`
		Status   string `json:"status"`
		Hash     string `json:"hash"`
		Declared bool   `json:"declared"`
	}

	var components []componentInfo
	seen := make(map[string]bool)

	// Add registry entries
	if regErr == nil {
		paths := make([]string, 0, len(reg.Components))
		for p := range reg.Components {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		for _, path := range paths {
			decl := reg.Components[path]
			info := componentInfo{
				Path:     path,
				URI:      PathToURI(root, path),
				Role:     decl.Role,
				Kind:     decl.Kind,
				Declared: true,
			}

			if s, ok := statuses[path]; ok {
				info.Hash = s.CommitHash
				if len(info.Hash) > 8 {
					info.Hash = info.Hash[:8]
				}
				if s.Dirty {
					info.Status = "DT"
				} else {
					info.Status = "OK"
				}
			} else {
				// Check if directory exists
				if _, serr := os.Stat(filepath.Join(root, path)); serr == nil {
					info.Status = "OK"
					// Try to get tree hash
					if h, herr := componentTreeHash(root, path, decl.Kind); herr == nil && h != "" {
						if len(h) > 8 {
							info.Hash = h[:8]
						}
					}
				} else {
					info.Status = "--"
				}
			}

			if decl.Notes != "" && strings.Contains(decl.Notes, "stale") {
				info.Status = "--"
				info.Hash = "(stale)"
			}

			components = append(components, info)
			seen[path] = true
		}
	}

	// Add auto-discovered submodules not in registry
	for _, sm := range submodules {
		if seen[sm.Path] {
			continue
		}
		info := componentInfo{
			Path:     sm.Path,
			URI:      PathToURI(root, sm.Path),
			Role:     "unknown",
			Kind:     "submodule",
			Declared: false,
		}
		if s, ok := statuses[sm.Path]; ok {
			info.Hash = s.CommitHash
			if len(info.Hash) > 8 {
				info.Hash = info.Hash[:8]
			}
			if s.Dirty {
				info.Status = "DT"
			} else {
				info.Status = "OK"
			}
		} else {
			info.Status = "--"
		}
		components = append(components, info)
	}

	if jsonOutput {
		data, _ := json.MarshalIndent(components, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	// Count by kind
	subCount, inTreeCount := 0, 0
	for _, c := range components {
		switch c.Kind {
		case "submodule":
			subCount++
		case "in-tree":
			inTreeCount++
		}
	}

	fmt.Printf("Components (%d registered, %d submodules, %d in-tree):\n\n", len(components), subCount, inTreeCount)

	for _, c := range components {
		statusColor := "\033[32m" // green
		switch c.Status {
		case "--":
			statusColor = "\033[33m" // yellow
		case "!!":
			statusColor = "\033[31m" // red
		case "DT":
			statusColor = "\033[36m" // cyan
		}

		hashStr := c.Hash
		if hashStr == "" {
			hashStr = "--------"
		}

		declMarker := " "
		if !c.Declared {
			declMarker = "*"
		}

		fmt.Printf("  %s%-35s %-18s %-12s %s%s\033[0m  %s\n",
			declMarker, c.Path, c.Role, c.Kind, statusColor, c.Status, hashStr)
	}

	return 0
}

// cmdComponentsStatus shows detailed status for one or all components.
func cmdComponentsStatus(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	jsonOutput := componentHasFlag(args, "--json")

	// Filter non-flag args for path
	var targetPath string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			targetPath = a
			break
		}
	}

	reg, _ := loadComponentRegistry(root)
	statuses, _ := gitSubmoduleStatus(root)

	if reg == nil {
		fmt.Fprintf(os.Stderr, "Error: no component registry found\n")
		return 1
	}

	type statusEntry struct {
		Path       string `json:"path"`
		URI        string `json:"uri"`
		Role       string `json:"role"`
		Kind       string `json:"kind"`
		Required   bool   `json:"required"`
		DeclURL    string `json:"declared_url,omitempty"`
		CommitHash string `json:"commit_hash,omitempty"`
		TreeHash   string `json:"tree_hash,omitempty"`
		Dirty      bool   `json:"dirty"`
		Language   string `json:"language,omitempty"`
		BuildSys   string `json:"build_system,omitempty"`
		Exists     bool   `json:"exists"`
	}

	var entries []statusEntry

	paths := make([]string, 0, len(reg.Components))
	for p := range reg.Components {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		if targetPath != "" && path != targetPath {
			continue
		}
		decl := reg.Components[path]
		entry := statusEntry{
			Path:     path,
			URI:      PathToURI(root, path),
			Role:     decl.Role,
			Kind:     decl.Kind,
			Required: decl.Required,
			DeclURL:  decl.URL,
			Language: decl.Language,
		}

		absPath := filepath.Join(root, path)
		if _, serr := os.Stat(absPath); serr == nil {
			entry.Exists = true
		}

		if s, ok := statuses[path]; ok {
			entry.CommitHash = s.CommitHash
			entry.Dirty = s.Dirty
		}

		if treeHash, herr := componentTreeHash(root, path, decl.Kind); herr == nil {
			entry.TreeHash = treeHash
		}

		if entry.Exists {
			lang, buildSys, _ := detectCapabilities(absPath)
			if entry.Language == "" {
				entry.Language = lang
			}
			entry.BuildSys = buildSys
		}

		entries = append(entries, entry)
	}

	if jsonOutput {
		data, _ := json.MarshalIndent(entries, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	for _, e := range entries {
		fmt.Printf("\n\033[1m%s\033[0m:\n", e.URI)
		fmt.Printf("  Registry:  %s | %s", e.Role, e.Kind)
		if e.Required {
			fmt.Printf(" | \033[33mrequired\033[0m")
		}
		fmt.Println()

		if e.Exists {
			dirtyStr := "clean"
			dirtyColor := "\033[32m"
			if e.Dirty {
				dirtyStr = "dirty"
				dirtyColor = "\033[36m"
			}
			fmt.Printf("  Local:     %s (%s%s\033[0m)\n", e.CommitHash, dirtyColor, dirtyStr)
		} else {
			fmt.Printf("  Local:     \033[33mnot present\033[0m\n")
		}

		if e.TreeHash != "" {
			fmt.Printf("  Tree Hash: %s\n", e.TreeHash)
		}
		if e.Language != "" {
			fmt.Printf("  Language:  %s", e.Language)
			if e.BuildSys != "" {
				fmt.Printf(" (%s)", e.BuildSys)
			}
			fmt.Println()
		}
		if e.DeclURL != "" {
			fmt.Printf("  Upstream:  %s\n", e.DeclURL)
		}
	}

	return 0
}

// cmdComponentsIndex rebuilds all component blobs and the Merkle index.
func cmdComponentsIndex(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	jsonOutput := componentHasFlag(args, "--json")

	start := time.Now()
	idx, err := indexComponents(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error indexing components: %v\n", err)
		return 1
	}
	elapsed := time.Since(start)

	if jsonOutput {
		data, _ := json.MarshalIndent(idx, "", "  ")
		fmt.Println(string(data))
		return 0
	}

	fmt.Printf("Indexed %d components in %s\n", idx.Count, elapsed.Round(time.Millisecond))

	// Print each component
	paths := make([]string, 0, len(idx.Components))
	for p := range idx.Components {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		blobHash := idx.Components[path]
		shortHash := strings.TrimPrefix(blobHash, "sha256:")
		if len(shortHash) > 12 {
			shortHash = shortHash[:12]
		}
		fmt.Printf("  %-35s blob:%s\n", path, shortHash)
	}

	rootShort := strings.TrimPrefix(idx.RootHash, "sha256:")
	if len(rootShort) > 16 {
		rootShort = rootShort[:16]
	}
	fmt.Printf("\nMerkle root: %s\n", rootShort)
	fmt.Printf("Written to .cog/.state/components/_index.json\n")

	return 0
}

// cmdComponentsRegister detects and prints a YAML snippet for a new component.
func cmdComponentsRegister(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cog components register <path>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	path := args[0]
	absPath := filepath.Join(root, path)

	if _, err := os.Stat(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s does not exist\n", path)
		return 1
	}

	// Detect kind
	kind := "in-tree"
	var url string
	submodules, _ := parseGitmodules(root)
	for _, sm := range submodules {
		if sm.Path == path {
			kind = "submodule"
			url = sm.URL
			break
		}
	}

	// Detect capabilities
	language, buildSystem, _ := detectCapabilities(absPath)

	fmt.Printf("Detected component at %s:\n", path)
	fmt.Printf("  Kind: %s\n", kind)
	if url != "" {
		fmt.Printf("  URL: %s\n", url)
	}
	if language != "" {
		fmt.Printf("  Language: %s", language)
		if buildSystem != "" {
			fmt.Printf(" (%s)", buildSystem)
		}
		fmt.Println()
	}

	fmt.Printf("\nAdd this to .cog/conf/components.cog.md:\n\n")
	fmt.Printf("  %s:\n", path)
	fmt.Printf("    role: <your-role-here>\n")
	if url != "" {
		fmt.Printf("    url: %s\n", url)
	}
	fmt.Printf("    kind: %s\n", kind)
	if language != "" {
		fmt.Printf("    language: %s\n", language)
	}
	fmt.Printf("    required: false\n")
	fmt.Printf("    sync_policy: manual\n")
	fmt.Printf("    tags: []\n")

	return 0
}

// componentHasFlag checks for a flag in the args slice.
func componentHasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}
