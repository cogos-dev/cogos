// .cog/cog.go
// CogOS Kernel - Minimal nucleus for cog-native workspaces
//
// The eigenform: this source lives in the workspace it validates.
//
// Build: cd .cog && go build -ldflags="-s -w -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o cog .
// Usage: .cog/cog {init|verify|hash|coherence|dispatch|version|help}

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// === VERSION & BUILD INFO ===

const Version = "2.1.0"

var BuildTime = "unknown" // Injected at build time

// === CONFIGURATION ===

// Paths tracked for coherence (relative to .cog/)
var trackedPaths = []string{
	"mem/",
	"schemas/",
	"adr/",
	"roles/",
	"coordination/",
}

// Paths excluded from coherence tracking (ephemeral state)
var excludedPaths = []string{
	"status/",
	"signals/",
	"work/",
	"run/",
	"var/",
}

// === ONTOLOGY CACHE ===

// Global ontology cache - loaded once, used everywhere
var ontologyCache *Ontology
var ontologyCacheMu sync.RWMutex

// === TYPES ===

// Signature represents the section block
type Signature struct {
	Self   string `yaml:"self"`   // Hash of content with section = null
	Parent string `yaml:"parent"` // Tree hash at injection
	Kernel string `yaml:"kernel"` // cog.go hash at injection
}

// TaskGraph represents a directed acyclic graph of task dependencies
type TaskGraph struct {
	Nodes    map[string]*Task    // Task name -> Task
	Edges    map[string][]string // Task name -> dependencies
	InDegree map[string]int      // Task name -> number of incoming edges
}

// TaskResult represents the result of a task execution
type TaskResult struct {
	TaskName string
	Success  bool
	Error    error
	Duration time.Duration
}

// Identity represents .cog/id.cog
type Identity struct {
	Type    string `yaml:"type"`
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	Name    string `yaml:"name"`
	Created string `yaml:"created"`

	// Signature block - the eigenform
	RawSignature interface{} `yaml:"§"`
}

// GetSignature extracts the signature if present
func (id *Identity) GetSignature() *Signature {
	if id.RawSignature == nil {
		return nil
	}
	if m, ok := id.RawSignature.(map[string]interface{}); ok {
		sig := &Signature{}
		if v, ok := m["self"].(string); ok {
			sig.Self = v
		}
		if v, ok := m["parent"].(string); ok {
			sig.Parent = v
		}
		if v, ok := m["kernel"].(string); ok {
			sig.Kernel = v
		}
		if sig.Self != "" {
			return sig
		}
	}
	return nil
}

// Cogdoc represents frontmatter for validation
type Cogdoc struct {
	Type    string      `yaml:"type"`
	ID      string      `yaml:"id"`
	Title   string      `yaml:"title"`
	Created string      `yaml:"created"`
	Status  string      `yaml:"status,omitempty"`   // proposed, active, deprecated, etc.
	Tags    []string    `yaml:"tags,omitempty"`     // Tags for categorization
	Refs    interface{} `yaml:"refs,omitempty"`     // Can be []string or []TypedRef
}

// TypedRef represents a reference with relationship type
type TypedRef struct {
	URI string `yaml:"uri"`
	Rel string `yaml:"rel,omitempty"` // implements, extends, supersedes, describes, requires, suggests
}

// Standard relationship types
var validRefRelations = map[string]bool{
	"refs":             true, // default
	"implements":       true,
	"extends":          true,
	"supersedes":       true,
	"describes":        true,
	"requires":         true,
	"suggests":         true,
	"validates":        true,
	"challenges":       true,
	"prepares":         true,
	"crystallized_from": true,
}

// === ONTOLOGY TYPES ===

// Ontology represents the parsed kernel topology from ontology.cog.md
type Ontology struct {
	// Standard cogdoc frontmatter
	Type       string `yaml:"type"`
	ID         string `yaml:"id"`
	Title      string `yaml:"title"`
	Created    string `yaml:"created"`
	Status     string `yaml:"status"`
	Version    string `yaml:"version"`
	Maintainer string `yaml:"maintainer"`

	// Self-reference
	SelfRef string `yaml:"self_ref"`

	// Bootstrap seed
	Bootstrap struct {
		SeedPath string `yaml:"seed_path"`
		Comment  string `yaml:"comment"`
	} `yaml:"bootstrap"`

	// Workspace topology
	Topology struct {
		Root       string                      `yaml:"root"`
		Primitives map[string]*OntologyPrimitive `yaml:"primitives"`
		Ontology   *OntologyPrimitive            `yaml:"ontology"`
	} `yaml:"topology"`

	// Coherence tracking
	Coherence struct {
		Model        string   `yaml:"model"`
		BaselineFile string   `yaml:"baseline_file"`
		Tracked      []string `yaml:"tracked"`
		Excluded     []string `yaml:"excluded"`
		Thresholds   struct {
			Green  float64 `yaml:"green"`
			Yellow float64 `yaml:"yellow"`
			Red    float64 `yaml:"red"`
		} `yaml:"thresholds"`
	} `yaml:"coherence"`

	// URI scheme
	URIScheme struct {
		Prefix      string                         `yaml:"prefix"`
		Version     string                         `yaml:"version"`
		Projections map[string]*OntologyProjection `yaml:"projections"`
		Resolution  struct {
			DefaultExtension   string   `yaml:"default_extension"`
			FallbackExtensions []string `yaml:"fallback_extensions"`
		} `yaml:"resolution"`
	} `yaml:"uri_scheme"`

	// Cogdoc configuration
	Cogdoc struct {
		Extension string `yaml:"extension"`
		Types     struct {
			Core     []string `yaml:"core"`
			Semantic []string `yaml:"semantic"`
		} `yaml:"types"`
		Relations struct {
			Structural []string `yaml:"structural"`
			Semantic   []string `yaml:"semantic"`
		} `yaml:"relations"`
		Frontmatter struct {
			Required struct {
				All      []string `yaml:"all"`
				NonEvent []string `yaml:"non_event"`
			} `yaml:"required"`
			Validation map[string]struct {
				Pattern     string `yaml:"pattern"`
				Description string `yaml:"description"`
			} `yaml:"validation"`
		} `yaml:"frontmatter"`
	} `yaml:"cogdoc"`

	// Memory sectors (HMD)
	MemorySectors map[string]*OntologyMemorySector `yaml:"memory_sectors"`

	// Hook system
	Hooks struct {
		Events            []string `yaml:"events"`
		DirectoryPattern  string   `yaml:"directory_pattern"`
		PriorityPattern   string   `yaml:"priority_pattern"`
		HandlerExtensions []string `yaml:"handler_extensions"`
		DispatchScript    string   `yaml:"dispatch_script"`
	} `yaml:"hooks"`

	// Identity bootstrap
	OntologyIdentity struct {
		File           string   `yaml:"file"`
		Format         string   `yaml:"format"`
		RequiredFields []string `yaml:"required_fields"`
		SignatureBlock string   `yaml:"signature_block"`
	} `yaml:"identity"`

	// Git tracking policy
	GitPolicy struct {
		Tracked          []string `yaml:"tracked"`
		Excluded         []string `yaml:"excluded"`
		PartiallyTracked []string `yaml:"partially_tracked"`
	} `yaml:"git_policy"`

	// References
	Refs []TypedRef `yaml:"refs"`
}

// OntologyPrimitive represents a workspace topology primitive (bin, lib, conf, etc.)
type OntologyPrimitive struct {
	Path        string      `yaml:"path"`
	Purpose     string      `yaml:"purpose"`
	Description string      `yaml:"description"`
	Tracked     interface{} `yaml:"tracked"` // bool or "partial"
	Immutable   bool        `yaml:"immutable,omitempty"`
	Subdirs     []string    `yaml:"subdirs,omitempty"`
}

// OntologyProjection defines how a cog:// URI type maps to filesystem paths
type OntologyProjection struct {
	Base         string   `yaml:"base,omitempty"`
	ExternalBase string   `yaml:"external_base,omitempty"`
	Pattern      string   `yaml:"pattern"`
	Suffix       string   `yaml:"suffix,omitempty"`
	GlobPattern  string   `yaml:"glob_pattern,omitempty"`
	Aliases      []string `yaml:"aliases,omitempty"`
}

// OntologyMemorySector defines an HMD memory sector
type OntologyMemorySector struct {
	Purpose   string   `yaml:"purpose"`
	Path      string   `yaml:"path"`
	Retention string   `yaml:"retention"`
	Subdirs   []string `yaml:"subdirs,omitempty"`
}

// === ONTOLOGY FUNCTIONS ===

// parseOntology parses YAML frontmatter from a cogdoc into an Ontology struct
func parseOntology(data []byte) (*Ontology, error) {
	content := string(data)

	// Must start with frontmatter delimiter
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("missing frontmatter: ontology.cog.md must start with ---")
	}

	// Find closing delimiter
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return nil, fmt.Errorf("unclosed frontmatter: missing closing ---")
	}

	// Extract frontmatter content
	fmContent := content[4 : 4+end]

	// Parse YAML into Ontology struct
	var ontology Ontology
	if err := yaml.Unmarshal([]byte(fmContent), &ontology); err != nil {
		return nil, fmt.Errorf("failed to parse ontology YAML: %w", err)
	}

	// Validate required fields
	if ontology.Type != "ontology" {
		return nil, fmt.Errorf("invalid ontology: type must be 'ontology', got '%s'", ontology.Type)
	}
	if ontology.ID == "" {
		return nil, fmt.Errorf("invalid ontology: missing id field")
	}

	return &ontology, nil
}

// loadOntology loads and parses the kernel ontology from disk
func loadOntology(cogRoot string) (*Ontology, error) {
	// The ONE hardcoded path - the Planck seed
	const ONTOLOGY_PATH = "ontology/ontology.cog.md"

	path := filepath.Join(cogRoot, ONTOLOGY_PATH)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ontology not found at %s: the kernel bootstrap seed is missing", path)
		}
		return nil, fmt.Errorf("failed to read ontology: %w", err)
	}

	return parseOntology(data)
}

// getOntology returns the cached ontology, loading it if necessary
func getOntology(cogRoot string) (*Ontology, error) {
	// Fast path: check cache with read lock
	ontologyCacheMu.RLock()
	cached := ontologyCache
	ontologyCacheMu.RUnlock()

	if cached != nil {
		return cached, nil
	}

	// Slow path: load and cache with write lock
	ontologyCacheMu.Lock()
	defer ontologyCacheMu.Unlock()

	// Double-check after acquiring write lock
	if ontologyCache != nil {
		return ontologyCache, nil
	}

	ontology, err := loadOntology(cogRoot)
	if err != nil {
		return nil, err
	}

	ontologyCache = ontology
	return ontologyCache, nil
}

// clearOntologyCache clears the cached ontology (useful for testing)
func clearOntologyCache() {
	ontologyCacheMu.Lock()
	ontologyCache = nil
	ontologyCacheMu.Unlock()
}

// === ONTOLOGY HELPER METHODS ===

// GetAllCogdocTypes returns all valid cogdoc types (core + semantic)
func (o *Ontology) GetAllCogdocTypes() map[string]bool {
	types := make(map[string]bool)
	for _, t := range o.Cogdoc.Types.Core {
		types[t] = true
	}
	for _, t := range o.Cogdoc.Types.Semantic {
		types[t] = true
	}
	return types
}

// IsValidCogdocType checks if a type is valid
func (o *Ontology) IsValidCogdocType(t string) bool {
	for _, valid := range o.Cogdoc.Types.Core {
		if valid == t {
			return true
		}
	}
	for _, valid := range o.Cogdoc.Types.Semantic {
		if valid == t {
			return true
		}
	}
	return false
}

// GetAllRelations returns all valid reference relations
func (o *Ontology) GetAllRelations() map[string]bool {
	rels := make(map[string]bool)
	for _, r := range o.Cogdoc.Relations.Structural {
		rels[r] = true
	}
	for _, r := range o.Cogdoc.Relations.Semantic {
		rels[r] = true
	}
	return rels
}

// IsValidRelation checks if a relation is valid
func (o *Ontology) IsValidRelation(rel string) bool {
	for _, valid := range o.Cogdoc.Relations.Structural {
		if valid == rel {
			return true
		}
	}
	for _, valid := range o.Cogdoc.Relations.Semantic {
		if valid == rel {
			return true
		}
	}
	return false
}

// GetTrackedPaths returns paths tracked for coherence
func (o *Ontology) GetTrackedPaths() []string {
	return o.Coherence.Tracked
}

// GetExcludedPaths returns paths excluded from coherence tracking
func (o *Ontology) GetExcludedPaths() []string {
	return o.Coherence.Excluded
}

// === ONTOLOGY-AWARE ACCESSORS ===
// These wrapper functions use the cached ontology if available,
// falling back to hardcoded defaults for bootstrap compatibility.

// getEffectiveTrackedPaths returns tracked paths from ontology or defaults
func getEffectiveTrackedPaths() []string {
	ontologyCacheMu.RLock()
	ont := ontologyCache
	ontologyCacheMu.RUnlock()

	if ont != nil && len(ont.Coherence.Tracked) > 0 {
		return ont.Coherence.Tracked
	}
	return trackedPaths // Fallback to hardcoded defaults
}

// getEffectiveExcludedPaths returns excluded paths from ontology or defaults
func getEffectiveExcludedPaths() []string {
	ontologyCacheMu.RLock()
	ont := ontologyCache
	ontologyCacheMu.RUnlock()

	if ont != nil && len(ont.Coherence.Excluded) > 0 {
		return ont.Coherence.Excluded
	}
	return excludedPaths // Fallback to hardcoded defaults
}

// isValidCogdocTypeFromOntology checks if a type is valid using ontology or defaults
func isValidCogdocTypeFromOntology(t string) bool {
	ontologyCacheMu.RLock()
	ont := ontologyCache
	ontologyCacheMu.RUnlock()

	if ont != nil {
		return ont.IsValidCogdocType(t)
	}
	return validCogdocTypes[t] // Fallback to hardcoded defaults
}

// isValidRelationFromOntology checks if a relation is valid using ontology or defaults
func isValidRelationFromOntology(rel string) bool {
	ontologyCacheMu.RLock()
	ont := ontologyCache
	ontologyCacheMu.RUnlock()

	if ont != nil {
		return ont.IsValidRelation(rel)
	}
	return validRefRelations[rel] // Fallback to hardcoded defaults
}

// === CQL - COGDOC INDEX ===

// IndexedCogdoc represents a cogdoc with full metadata for indexing
type IndexedCogdoc struct {
	URI         string     // cog://mem/episodic/decisions/foo
	Path        string     // .cog/mem/episodic/decisions/foo.cog.md
	Type        string     // decision, session, guide, etc.
	ID          string     // kebab-case identifier
	Title       string     // Human-readable title
	Created     string     // YYYY-MM-DD
	Status      string     // proposed, active, deprecated, etc.
	Tags        []string   // Tags for categorization
	Refs        []TypedRef // Structural references from frontmatter
	InlineRefs  []string   // Navigational references from content
}

// CogdocIndex is an in-memory index of all cogdocs
type CogdocIndex struct {
	ByURI       map[string]*IndexedCogdoc   // URI -> cogdoc
	ByType      map[string][]*IndexedCogdoc // type -> cogdocs
	ByTag       map[string][]*IndexedCogdoc // tag -> cogdocs
	ByStatus    map[string][]*IndexedCogdoc // status -> cogdocs
	RefGraph    map[string][]TypedRef       // URI -> refs (forward)
	InverseRefs map[string][]string         // URI -> referring URIs (backward)
}

// BuildCogdocIndex scans .cog/mem/ and builds an in-memory index
func BuildCogdocIndex(cogRoot string) (*CogdocIndex, error) {
	idx := &CogdocIndex{
		ByURI:       make(map[string]*IndexedCogdoc),
		ByType:      make(map[string][]*IndexedCogdoc),
		ByTag:       make(map[string][]*IndexedCogdoc),
		ByStatus:    make(map[string][]*IndexedCogdoc),
		RefGraph:    make(map[string][]TypedRef),
		InverseRefs: make(map[string][]string),
	}

	memoryDir := filepath.Join(cogRoot, "mem")

	// Walk .cog/mem/ and find all .cog.md files
	err := filepath.Walk(memoryDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}
		if info.IsDir() {
			return nil
		}

		// Only index .cog.md files
		if !strings.HasSuffix(path, ".cog.md") {
			return nil
		}

		// Parse cogdoc
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip unreadable files
		}
		content := string(data)

		// Parse frontmatter
		if !strings.HasPrefix(content, "---\n") {
			return nil
		}
		end := strings.Index(content[4:], "\n---")
		if end == -1 {
			return nil
		}
		fmContent := content[4 : 4+end]
		bodyContent := content[4+end+4:]

		var doc Cogdoc
		if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
			return nil // Skip invalid YAML
		}

		// Convert path to URI
		// .cog/mem/episodic/decisions/foo.cog.md -> cog://mem/episodic/decisions/foo
		relPath, err := filepath.Rel(cogRoot, path)
		if err != nil {
			return nil
		}
		// Remove .cog.md extension
		relPath = strings.TrimSuffix(relPath, ".cog.md")
		uri := "cog://" + relPath

		// Parse refs
		refs, _ := parseRefs(doc.Refs)

		// Extract inline refs
		inlineRefs := extractInlineRefs(bodyContent)

		// Create indexed cogdoc
		indexed := &IndexedCogdoc{
			URI:        uri,
			Path:       path,
			Type:       doc.Type,
			ID:         doc.ID,
			Title:      doc.Title,
			Created:    doc.Created,
			Status:     doc.Status,
			Tags:       doc.Tags,
			Refs:       refs,
			InlineRefs: inlineRefs,
		}

		// Add to ByURI index
		idx.ByURI[uri] = indexed

		// Add to ByType index
		if doc.Type != "" {
			idx.ByType[doc.Type] = append(idx.ByType[doc.Type], indexed)
		}

		// Add to ByTag index
		for _, tag := range doc.Tags {
			idx.ByTag[tag] = append(idx.ByTag[tag], indexed)
		}

		// Add to ByStatus index
		if doc.Status != "" {
			idx.ByStatus[doc.Status] = append(idx.ByStatus[doc.Status], indexed)
		}

		// Add to RefGraph (forward refs)
		if len(refs) > 0 {
			idx.RefGraph[uri] = refs
		}

		// Build inverse ref graph
		for _, ref := range refs {
			idx.InverseRefs[ref.URI] = append(idx.InverseRefs[ref.URI], uri)
		}

		return nil
	})

	return idx, err
}

// cogURIPattern matches cog:// URIs in content
var cogURIPattern = regexp.MustCompile(`cog://[a-zA-Z0-9/_-]+`)

// extractInlineRefs finds cog:// URIs in markdown content (navigational refs)
func extractInlineRefs(content string) []string {
	matches := cogURIPattern.FindAllString(content, -1)
	// Deduplicate
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			result = append(result, m)
		}
	}
	return result
}

// parseRefs extracts refs from cogdoc, handling both simple and typed forms
func parseRefs(refs interface{}) ([]TypedRef, error) {
	if refs == nil {
		return nil, nil
	}

	var result []TypedRef

	switch v := refs.(type) {
	case []interface{}:
		for _, item := range v {
			switch ref := item.(type) {
			case string:
				// Simple form: just a URI string
				result = append(result, TypedRef{URI: ref, Rel: "refs"})
			case map[interface{}]interface{}:
				// Typed form: {uri: ..., rel: ...}
				tr := TypedRef{Rel: "refs"}
				if uri, ok := ref["uri"].(string); ok {
					tr.URI = uri
				}
				if rel, ok := ref["rel"].(string); ok {
					tr.Rel = rel
				}
				if tr.URI != "" {
					result = append(result, tr)
				}
			case map[string]interface{}:
				// Typed form (alternate key type)
				tr := TypedRef{Rel: "refs"}
				if uri, ok := ref["uri"].(string); ok {
					tr.URI = uri
				}
				if rel, ok := ref["rel"].(string); ok {
					tr.Rel = rel
				}
				if tr.URI != "" {
					result = append(result, tr)
				}
			}
		}
	}

	return result, nil
}

// Valid cogdoc types (enum)
// Core types (structural)
// Extended types (semantic memory categories)
var validCogdocTypes = map[string]bool{
	// Core structural types
	"identity":  true,
	"ontology":  true,
	"memory":    true,
	"schema":    true,
	"decision":  true,
	"session":   true,
	"handoff":   true,
	"guide":     true,
	"adr":       true,
	"knowledge": true,
	"event":     true, // Declarative event handlers
	"skill":    true, // Skill definitions
	"command":  true, // Command definitions
	// Extended semantic types (common in memory)
	"note":              true, // General notes
	"term":              true, // Terminology definitions
	"spec":              true, // Specifications
	"claim":             true, // Research claims
	"insight":           true, // Crystallized insights
	"architecture":      true, // Architecture docs
	"research_synthesis": true, // Research synthesis
	"observation":       true, // Observations
	"assessment":        true, // Assessments
	"procedural":        true, // Procedural knowledge
	"summary":           true, // Summaries
	"specification":     true, // Full specifications
}

// isKebabCase validates that a string is kebab-case
func isKebabCase(s string) bool {
	if len(s) == 0 {
		return false
	}
	// Must start with lowercase letter or digit
	if !((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= '0' && s[0] <= '9')) {
		return false
	}
	// Must contain only lowercase letters, digits, and hyphens
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// isValidDate validates YYYY-MM-DD format
func isValidDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	// Check format: YYYY-MM-DD
	for i, c := range s {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// CoherenceState represents the coherence check result
type CoherenceState struct {
	Coherent      bool     `json:"coherent"`
	CanonicalHash string   `json:"canonical_hash,omitempty"`
	CurrentHash   string   `json:"current_hash,omitempty"`
	Timestamp     string   `json:"timestamp"`
	Drift         []string `json:"drift,omitempty"`
}

// CoherenceRecord represents the stored coherence state with history
type CoherenceRecord struct {
	Current     *CoherenceState   `json:"current"`
	History     []*CoherenceState `json:"history,omitempty"`
	LastUpdated string            `json:"last_updated"`
}

// HookResult represents the result of a hook execution
type HookResult struct {
	Decision          string `json:"decision"`                     // "allow" or "block"
	Reason            string `json:"reason,omitempty"`             // Why blocked
	Message           string `json:"message,omitempty"`            // Human-readable message
	Fallback          bool   `json:"fallback,omitempty"`           // Used default behavior
	AdditionalContext string `json:"additionalContext,omitempty"`  // Context to inject (for PreInference)
}

// Handler represents a hook handler
type Handler struct {
	Event    string
	Matcher  string
	Script   string
	Priority int
	Blocking bool
	Name     string
}

// === TASK ORCHESTRATION & CACHING ===

// Task represents a runnable task with caching
type Task struct {
	Name      string   `yaml:"name"`
	Command   string   `yaml:"command"`
	Cache     bool     `yaml:"cache"`
	DependsOn []string `yaml:"dependsOn,omitempty"`
	Inputs    []string `yaml:"inputs,omitempty"`    // Glob patterns for input files
	Outputs   []string `yaml:"outputs,omitempty"`   // Glob patterns for output files
	Env       []string `yaml:"env,omitempty"`       // Environment variables affecting cache
}

// KernelConfig represents the unified kernel configuration
type KernelConfig struct {
	Version   string            `yaml:"version"`
	GlobalEnv []string          `yaml:"globalEnv,omitempty"`
	Tasks     map[string]Task   `yaml:"tasks,omitempty"`
	Cache     CacheConfig       `yaml:"cache,omitempty"`
}

// CacheConfig controls caching behavior
type CacheConfig struct {
	Dir       string `yaml:"dir"`       // Default: .cog/.cache
	Algorithm string `yaml:"algorithm"` // Default: sha256
	Backend   string `yaml:"backend"`   // Default: local (future: s3, git)
}

// CacheKey components for computing task cache key
type CacheKey struct {
	TaskName    string
	InputsHash  string
	EnvHash     string
	CommandHash string
}

// CacheEntry represents a cached task execution
type CacheEntry struct {
	Key       string            `json:"key"`
	Timestamp time.Time         `json:"timestamp"`
	Stdout    []byte            `json:"stdout,omitempty"`
	Stderr    []byte            `json:"stderr,omitempty"`
	ExitCode  int               `json:"exit_code"`
	Outputs   map[string][]byte `json:"outputs,omitempty"` // Output file contents
}

// CacheStats tracks cache hit/miss statistics
type CacheStats struct {
	Hits      int       `json:"hits"`
	Misses    int       `json:"misses"`
	TotalSize int64     `json:"total_size"`
	Entries   int       `json:"entries"`
	Updated   time.Time `json:"updated"`
}

// === GLOBAL CONFIG (Multi-Workspace Support) ===

// GlobalConfig represents ~/.cog/config for multi-workspace management
type GlobalConfig struct {
	Version          string                     `yaml:"version"`
	CurrentWorkspace string                     `yaml:"current-workspace,omitempty"`
	Workspaces       map[string]*WorkspaceEntry `yaml:"workspaces,omitempty"`
}

// WorkspaceEntry represents a registered workspace
type WorkspaceEntry struct {
	Path        string `yaml:"path"`
	Name        string `yaml:"name,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// globalConfigPath returns the path to ~/.cog/config
func globalConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cog", "config")
}

// loadGlobalConfig loads ~/.cog/config, returning empty config if missing
func loadGlobalConfig() (*GlobalConfig, error) {
	path := globalConfigPath()
	if path == "" {
		return &GlobalConfig{
			Version:    "1.0",
			Workspaces: make(map[string]*WorkspaceEntry),
		}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &GlobalConfig{
				Version:    "1.0",
				Workspaces: make(map[string]*WorkspaceEntry),
			}, nil
		}
		return nil, fmt.Errorf("failed to read global config: %w", err)
	}

	var config GlobalConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse global config: %w", err)
	}

	if config.Workspaces == nil {
		config.Workspaces = make(map[string]*WorkspaceEntry)
	}
	if config.Version == "" {
		config.Version = "1.0"
	}

	return &config, nil
}

// saveGlobalConfig writes ~/.cog/config atomically
func saveGlobalConfig(config *GlobalConfig) error {
	path := globalConfigPath()
	if path == "" {
		return fmt.Errorf("could not determine home directory")
	}

	// Ensure ~/.cog/ exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}

	return writeAtomic(path, data, 0600)
}

// === UTILITY FUNCTIONS ===

func getEnvOr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// camelCaseToKebab converts CamelCase to kebab-case
// e.g., "PreInference" -> "pre-inference", "PostInference" -> "post-inference"
var camelCaseRegex = regexp.MustCompile("([a-z0-9])([A-Z])")

func camelCaseToKebab(s string) string {
	// Insert hyphen before uppercase letters following lowercase
	result := camelCaseRegex.ReplaceAllString(s, "${1}-${2}")
	return strings.ToLower(result)
}

// writeAtomic writes data to a file atomically using rename
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	tmp := path + ".tmp." + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // Clean up on failure
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// === URI RESOLUTION ===

// Projection defines how a URI type resolves to filesystem paths
type Projection struct {
	Base      string // Base directory under .cog/
	Pattern   string // Pattern: "direct", "glob", or "directory"
	Suffix    string // File suffix for direct patterns
	GlobPat   string // Glob pattern for glob-based resolution
	ExtBase   string // External base (e.g., .claude/ for skills)
}

// URI type projections
var projections = map[string]*Projection{
	"mem":       {Base: "mem/", Pattern: "direct", Suffix: ""},
	"adr":       {Base: "adr/", Pattern: "glob", GlobPat: "%s-*.md"},
	"role":      {Base: "roles/", Pattern: "directory", Suffix: "/"},
	"roles":     {Base: "roles/", Pattern: "directory", Suffix: "/"},
	"skill":     {ExtBase: ".claude/skills/", Pattern: "directory", Suffix: "/"},
	"skills":    {ExtBase: ".claude/skills/", Pattern: "directory", Suffix: "/"},
	"agent":     {ExtBase: ".claude/agents/", Pattern: "directory", Suffix: "/"},
	"agents":    {ExtBase: ".claude/agents/", Pattern: "directory", Suffix: "/"},
	"spec":      {Base: "specs/", Pattern: "direct", Suffix: ".cog.md"},
	"specs":     {Base: "specs/", Pattern: "direct", Suffix: ".cog.md"},
	"status":    {Base: "status/", Pattern: "direct", Suffix: ".json"},
	"ledger":    {Base: "ledger/", Pattern: "directory", Suffix: "/"},
	"crystal":   {Base: "ledger/", Pattern: "direct", Suffix: "/crystal.json"},
	"kernel":    {Base: "", Pattern: "direct", Suffix: ""},
	"canonical": {Base: "", Pattern: "direct", Suffix: ""}, // Special: holographic baseline hash
	"handoff":   {ExtBase: "projects/cog_lab_package/handoffs/", Pattern: "glob", GlobPat: "%s*.md"},
	"handoffs":  {ExtBase: "projects/cog_lab_package/handoffs/", Pattern: "directory", Suffix: "/"},
	"artifact":  {Base: "ledger/", Pattern: "glob", GlobPat: "*/artifacts/%s.*"},
	"artifacts": {Base: "ledger/", Pattern: "glob", GlobPat: "*/artifacts/%s.*"},
	"ontology":  {Base: "ontology/", Pattern: "direct", Suffix: ".cog.md"},
	"work":      {Base: "work/", Pattern: "direct", Suffix: ""},
}

// resolveGlob finds first matching file for a glob pattern
func resolveGlob(pattern string) (string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no match for pattern: %s", pattern)
	}
	// Return first match (sorted by filepath.Glob)
	return matches[0], nil
}

// resolveURI converts cog://type/path to filesystem path
func resolveURI(uri string) (string, error) {
	// Parse cog://type/path
	if !strings.HasPrefix(uri, "cog://") {
		return "", fmt.Errorf("invalid URI scheme (expected cog://)")
	}

	path := uri[6:] // Remove cog://
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid URI format")
	}

	uriType := parts[0]
	uriPath := ""
	if len(parts) > 1 {
		uriPath = parts[1]
	}

	// Look up projection
	proj, ok := projections[uriType]
	if !ok {
		return "", fmt.Errorf("unknown URI type: %s", uriType)
	}

	// Determine base directory
	baseDir := ".cog/"
	if proj.ExtBase != "" {
		baseDir = proj.ExtBase
	}

	// Resolve based on pattern type
	switch proj.Pattern {
	case "direct":
		return baseDir + proj.Base + uriPath + proj.Suffix, nil

	case "directory":
		return baseDir + proj.Base + uriPath + proj.Suffix, nil

	case "glob":
		// Build glob pattern and find match
		globPattern := baseDir + proj.Base + fmt.Sprintf(proj.GlobPat, uriPath)
		match, err := resolveGlob(globPattern)
		if err != nil {
			// Return the pattern path for error reporting
			return baseDir + proj.Base + uriPath + ".md", err
		}
		return match, nil

	default:
		return "", fmt.Errorf("unknown pattern type: %s", proj.Pattern)
	}
}

// gitBlobHash computes the git blob hash of a file
func gitBlobHash(path string) (string, error) {
	out, err := exec.Command("git", "hash-object", path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// NOTE: In holographic workspace model (ADR-021), we don't need per-file canonical
// hashes from a separate branch. Coherence is checked at the tree level using
// git write-tree --prefix=.cog/
// These functions are deprecated and kept only for reference.

// gitCanonicalContent gets content from current working tree (holographic projection)
func gitCanonicalContent(path string) ([]byte, error) {
	// In holographic model, just read from current working tree
	return os.ReadFile(path)
}

// gitCanonicalBlobHash gets the blob hash from current working tree (holographic projection)
func gitCanonicalBlobHash(path string) (string, error) {
	// In holographic model, just get current blob hash
	return gitBlobHash(path)
}

// === GIT PRIMITIVES ===

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

// workspaceCache caches the result of workspace resolution.
// ResolveWorkspace() is called 25+ times per process; caching avoids
// redundant config reads and git operations.
var workspaceCache struct {
	once   sync.Once
	root   string
	source string
	err    error
}

// ResolveWorkspace determines the workspace root based on precedence:
// 1. COG_ROOT environment variable (explicit)
// 2. COG_WORKSPACE env var (lookup in global config)
// 3. Local git repo detection (if inside a workspace)
// 4. current-workspace from ~/.cog/config
// Returns (workspaceRoot, source, error) where source describes how it was resolved.
//
// Results are cached for the lifetime of the process since resolution is
// deterministic within a single invocation.
func ResolveWorkspace() (string, string, error) {
	workspaceCache.once.Do(func() {
		workspaceCache.root, workspaceCache.source, workspaceCache.err = resolveWorkspaceUncached()
	})
	return workspaceCache.root, workspaceCache.source, workspaceCache.err
}

// resolveWorkspaceUncached implements the actual workspace resolution logic.
// Called once per process via ResolveWorkspace().
func resolveWorkspaceUncached() (string, string, error) {
	// 1. Explicit COG_ROOT (set by wrapper for --root flag)
	if root := os.Getenv("COG_ROOT"); root != "" {
		cogDir := filepath.Join(root, ".cog")
		if info, err := os.Stat(cogDir); err == nil && info.IsDir() {
			return root, "explicit", nil
		}
		return "", "", fmt.Errorf("COG_ROOT=%s is not a valid workspace (no .cog/ directory)", root)
	}

	// 2. COG_WORKSPACE env var (lookup by name in global config)
	// Graceful degradation: fall through to tier 3 if config fails or workspace not found
	if wsName := os.Getenv("COG_WORKSPACE"); wsName != "" {
		config, err := loadGlobalConfig()
		if err == nil { // Only proceed if config loaded successfully
			if ws, ok := config.Workspaces[wsName]; ok {
				cogDir := filepath.Join(ws.Path, ".cog")
				if _, err := os.Stat(cogDir); err != nil {
					return "", "", fmt.Errorf("workspace '%s' at %s is invalid (no .cog/ directory)", wsName, ws.Path)
				}
				return ws.Path, "env", nil
			}
		}
		// Fall through to tier 3 (local git detection) silently
	}

	// 3. Local git detection (if inside a workspace)
	if root, err := gitRoot(); err == nil {
		cogDir := filepath.Join(root, ".cog")
		if info, err := os.Stat(cogDir); err == nil && info.IsDir() {
			return root, "local", nil
		}
	}

	// 4. Fall back to global current-workspace
	config, err := loadGlobalConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to load global config: %w", err)
	}

	if config.CurrentWorkspace != "" {
		if ws, ok := config.Workspaces[config.CurrentWorkspace]; ok {
			cogDir := filepath.Join(ws.Path, ".cog")
			if _, err := os.Stat(cogDir); err != nil {
				return "", "", fmt.Errorf("workspace '%s' at %s is invalid (no .cog/ directory)", config.CurrentWorkspace, ws.Path)
			}
			return ws.Path, "global", nil
		}
		// Current workspace is set but doesn't exist in config
		return "", "", fmt.Errorf("current workspace '%s' not found in config", config.CurrentWorkspace)
	}

	return "", "", fmt.Errorf("no workspace found (run 'cog workspace add' or cd into a workspace)")
}

func gitTreeHash() (string, error) {
	out, err := exec.Command("git", "write-tree").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCogTreeHash computes the tree hash of .cog/ directory
func gitCogTreeHash(gitRoot string) (string, error) {
	// Stage .cog/ first to include unstaged changes
	stageCmd := exec.Command("git", "-C", gitRoot, "add", "-A", ".cog/")
	if err := stageCmd.Run(); err != nil {
		// Non-fatal: may fail if nothing to stage
	}

	out, err := exec.Command("git", "-C", gitRoot, "write-tree", "--prefix=.cog/").Output()
	if err != nil {
		return "", fmt.Errorf("failed to compute tree hash: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCanonicalHash gets the last validated tree hash from run/coherence/canonical-hash
// This implements the holographic workspace model (ADR-021) where canonical
// state is a stored hash, not a separate git branch.
func gitCanonicalHash(gitRoot string) (string, error) {
	hashFile := filepath.Join(gitRoot, ".cog", "run", "coherence", "canonical-hash")
	data, err := os.ReadFile(hashFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no canonical hash found (run baseline to establish)")
		}
		return "", fmt.Errorf("failed to read canonical hash: %w", err)
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" {
		return "", fmt.Errorf("canonical hash file is empty")
	}
	return hash, nil
}

// setCanonicalHash writes the current tree hash as the new canonical baseline
// This establishes the holographic projection baseline for future coherence checks.
func setCanonicalHash(gitRoot string, hash string) error {
	hashFile := filepath.Join(gitRoot, ".cog", "run", "coherence", "canonical-hash")

	// Ensure run/coherence directory exists
	stateDir := filepath.Join(gitRoot, ".cog", "run", "coherence")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create run/coherence directory: %w", err)
	}

	// Write hash atomically
	if err := writeAtomic(hashFile, []byte(hash+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write canonical hash: %w", err)
	}

	return nil
}

// BuildCognitiveStateTree creates a git tree object representing all files in .cog/
// This includes both tracked and untracked files, unlike git write-tree.
// It walks the directory recursively, creates blob objects for each file,
// and builds tree objects bottom-up.
func BuildCognitiveStateTree(gitRoot string) (string, error) {
	cogPath := filepath.Join(gitRoot, ".cog")

	// Data structure: map of directory path -> list of tree entries
	type treeEntry struct {
		mode string
		typ  string // "blob" or "tree"
		hash string
		name string
	}
	trees := make(map[string][]treeEntry)

	// Walk .cog/ directory and create blob objects
	err := filepath.Walk(cogPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path from .cog/
		relPath, err := filepath.Rel(cogPath, path)
		if err != nil {
			return err
		}

		// Skip .cog itself
		if relPath == "." {
			return nil
		}

		// Skip excluded paths (ephemeral state) - uses ontology if cached
		for _, excluded := range getEffectiveExcludedPaths() {
			if strings.HasPrefix(relPath, excluded) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// Skip symlinks - git hash-object doesn't handle them
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		// Directory or file?
		if info.IsDir() {
			// Initialize directory entry in map
			if _, exists := trees[relPath]; !exists {
				trees[relPath] = []treeEntry{}
			}
			return nil
		}

		// Create blob for file
		cmd := exec.Command("git", "-C", gitRoot, "hash-object", "-w", path)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("failed to hash file %s: %w", path, err)
		}
		blobHash := strings.TrimSpace(string(output))

		// Determine file mode
		mode := "100644" // Regular file
		if info.Mode()&0111 != 0 {
			mode = "100755" // Executable
		}

		// Add to parent directory's tree entries
		dir := filepath.Dir(relPath)
		if dir == "." {
			dir = ""
		}
		trees[dir] = append(trees[dir], treeEntry{
			mode: mode,
			typ:  "blob",
			hash: blobHash,
			name: filepath.Base(relPath),
		})

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to walk .cog directory: %w", err)
	}

	// Build tree objects bottom-up
	// Start with deepest directories and work up to root
	dirsByDepth := make([]string, 0, len(trees))
	for dir := range trees {
		dirsByDepth = append(dirsByDepth, dir)
	}

	// Sort by depth (deepest first)
	sort.Slice(dirsByDepth, func(i, j int) bool {
		depthI := strings.Count(dirsByDepth[i], string(filepath.Separator))
		depthJ := strings.Count(dirsByDepth[j], string(filepath.Separator))
		if depthI != depthJ {
			return depthI > depthJ // Deeper directories first
		}
		return dirsByDepth[i] > dirsByDepth[j] // Alphabetical for same depth
	})

	// Create tree objects
	treeHashes := make(map[string]string) // directory path -> tree hash

	for _, dir := range dirsByDepth {
		entries := trees[dir]

		// Sort entries by name (git requirement)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].name < entries[j].name
		})

		// Build tree input for git mktree
		var treeInput strings.Builder
		for _, entry := range entries {
			// Format: <mode> <type> <hash>\t<name>\n
			treeInput.WriteString(fmt.Sprintf("%s %s %s\t%s\n",
				entry.mode, entry.typ, entry.hash, entry.name))
		}

		// Create tree object
		cmd := exec.Command("git", "-C", gitRoot, "mktree")
		cmd.Stdin = strings.NewReader(treeInput.String())
		output, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("failed to create tree for %s: %w", dir, err)
		}
		treeHash := strings.TrimSpace(string(output))
		treeHashes[dir] = treeHash

		// Add this tree to its parent directory
		if dir != "" {
			parentDir := filepath.Dir(dir)
			if parentDir == "." {
				parentDir = ""
			}
			trees[parentDir] = append(trees[parentDir], treeEntry{
				mode: "40000",
				typ:  "tree",
				hash: treeHash,
				name: filepath.Base(dir),
			})
		}
	}

	// Return root tree hash
	rootHash, ok := treeHashes[""]
	if !ok {
		return "", fmt.Errorf("failed to create root tree")
	}

	return rootHash, nil
}

// WriteCanonicalHash writes a validated tree hash to run/coherence/canonical-hash
// This implements atomic hash writing with pre-validation (Phase 1.2)
func WriteCanonicalHash(gitRoot string, treeHash string) error {
	// Validate hash exists in git object store before writing
	cmd := exec.Command("git", "-C", gitRoot, "cat-file", "-e", treeHash)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("invalid tree hash %s: not found in git object store", treeHash)
	}

	// Verify it's actually a tree object, not a blob or commit
	typeCmd := exec.Command("git", "-C", gitRoot, "cat-file", "-t", treeHash)
	typeOut, err := typeCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to check object type: %w", err)
	}
	objType := strings.TrimSpace(string(typeOut))
	if objType != "tree" {
		return fmt.Errorf("hash %s is a %s, not a tree", treeHash, objType)
	}

	// Write atomically using existing helper
	hashFile := filepath.Join(gitRoot, ".cog", "run", "coherence", "canonical-hash")
	if err := writeAtomic(hashFile, []byte(treeHash+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write canonical hash: %w", err)
	}

	return nil
}

// HealthCheckCanonicalTree verifies canonical tree matches filesystem
// Returns error if divergence is too large (>20% tolerance)
func HealthCheckCanonicalTree(gitRoot string) error {
	// Read canonical hash
	hash, err := gitCanonicalHash(gitRoot)
	if err != nil {
		return fmt.Errorf("no canonical hash: %w", err)
	}

	// Count files in tree using git ls-tree
	lsTreeCmd := exec.Command("git", "-C", gitRoot, "ls-tree", "-r", hash)
	output, err := lsTreeCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to read tree %s: %w", hash, err)
	}

	// Count non-empty lines in ls-tree output
	treeFileCount := 0
	if len(output) > 0 {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				treeFileCount++
			}
		}
	}

	// Count files in filesystem (.cog/ directory)
	fsFileCount := 0
	cogDir := filepath.Join(gitRoot, ".cog")
	err = filepath.Walk(cogDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".json")) {
			// Skip excluded paths (ephemeral state) - uses ontology if cached
			relPath, _ := filepath.Rel(cogDir, path)
			skip := false
			for _, excluded := range getEffectiveExcludedPaths() {
				if strings.HasPrefix(relPath, excluded) {
					skip = true
					break
				}
			}
			if !skip {
				fsFileCount++
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to count filesystem files: %w", err)
	}

	// Compare counts with 20% tolerance
	// Tree should have at least 80% of filesystem files
	minExpected := fsFileCount * 8 / 10
	if treeFileCount < minExpected {
		return fmt.Errorf("canonical tree has %d files, filesystem has %d (divergence >20%%)",
			treeFileCount, fsFileCount)
	}

	fmt.Printf("✓ Canonical tree healthy: %d files (filesystem: %d)\n", treeFileCount, fsFileCount)
	return nil
}

// gitDiffTree computes which files differ between two tree hashes
func gitDiffTree(gitRoot, fromHash, toHash string) ([]string, error) {
	out, err := exec.Command("git", "-C", gitRoot, "diff-tree", "-r", "--name-only",
		fromHash, toHash).Output()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, ".cog/"+line)
		}
	}
	return result, nil
}

func gitHead() string {
	out, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

func gitStagedCogfiles() ([]string, error) {
	out, err := exec.Command("git", "diff", "--cached", "--name-only", "--", "*.cog.md", "*.cog").Output()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// === INCREMENTAL EXECUTION ===

// getChangedFiles returns files changed since a git ref, filtered by glob patterns
func getChangedFiles(gitRoot, since string, patterns []string) ([]string, error) {
	if since == "" {
		since = "HEAD"
	}

	// Build git diff command with patterns
	args := []string{"-C", gitRoot, "diff", "--name-only", since}
	if len(patterns) > 0 {
		args = append(args, "--")
		args = append(args, patterns...)
	}

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// If ref doesn't exist, return all matching files
		if since != "HEAD" {
			return globFiles(gitRoot, patterns)
		}
		return nil, err
	}

	if len(out) == 0 {
		return nil, nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var result []string
	for _, line := range lines {
		if line != "" {
			// Make paths absolute
			if !filepath.IsAbs(line) {
				result = append(result, filepath.Join(gitRoot, line))
			} else {
				result = append(result, line)
			}
		}
	}
	return result, nil
}

// getChangedSinceBaseline returns files changed since the canonical baseline
func getChangedSinceBaseline(gitRoot string, patterns []string) ([]string, error) {
	// Get canonical baseline hash
	baseline, err := gitCanonicalHash(gitRoot)
	if err != nil {
		// No baseline, return all files matching patterns
		return globFiles(gitRoot, patterns)
	}

	// Get current tree hash
	current, err := gitCogTreeHash(gitRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to get current tree hash: %w", err)
	}

	// If hashes match, no changes
	if baseline == current {
		return nil, nil
	}

	// Get diff between baseline and current
	changed, err := gitDiffTree(gitRoot, baseline, current)
	if err != nil {
		return nil, fmt.Errorf("failed to diff trees: %w", err)
	}

	// Filter by patterns if specified
	if len(patterns) == 0 {
		return changed, nil
	}

	var filtered []string
	for _, file := range changed {
		for _, pattern := range patterns {
			matched, _ := filepath.Match(pattern, filepath.Base(file))
			if matched || strings.Contains(file, pattern) {
				filtered = append(filtered, file)
				break
			}
		}
	}
	return filtered, nil
}

// globFiles finds all files matching glob patterns in gitRoot
func globFiles(gitRoot string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	var result []string
	for _, pattern := range patterns {
		// Handle patterns with wildcards
		fullPattern := filepath.Join(gitRoot, pattern)
		matches, err := filepath.Glob(fullPattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err == nil && !info.IsDir() {
				result = append(result, match)
			}
		}
	}
	return result, nil
}

// updateBaseline writes the current tree hash as the baseline for a task
func updateBaseline(gitRoot, taskName string) error {
	// Compute current tree hash
	treeHash, err := gitCogTreeHash(gitRoot)
	if err != nil {
		return fmt.Errorf("failed to compute tree hash: %w", err)
	}

	// Write to .cog/run/coherence/baseline-{taskName}
	stateDir := filepath.Join(gitRoot, ".cog", "run", "coherence")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create coherence directory: %w", err)
	}

	baselineFile := filepath.Join(stateDir, "baseline-"+taskName)
	return writeAtomic(baselineFile, []byte(treeHash+"\n"), 0644)
}

// needsRun checks if a task needs to run based on baseline comparison
func needsRun(gitRoot, taskName string, patterns []string) (bool, error) {
	baselineFile := filepath.Join(gitRoot, ".cog", "run", "coherence", "baseline-"+taskName)
	baselineData, err := os.ReadFile(baselineFile)
	if err != nil {
		// No baseline, needs run
		return true, nil
	}

	baseline := strings.TrimSpace(string(baselineData))
	if baseline == "" {
		return true, nil
	}

	// Get current tree hash
	current, err := gitCogTreeHash(gitRoot)
	if err != nil {
		return true, err
	}

	// If hashes match, check if any matching files changed
	if baseline == current {
		return false, nil
	}

	// Check if any of the specified patterns changed
	if len(patterns) > 0 {
		changed, err := gitDiffTree(gitRoot, baseline, current)
		if err != nil {
			return true, err
		}

		// Filter changed files by patterns
		for _, file := range changed {
			for _, pattern := range patterns {
				matched, _ := filepath.Match(pattern, filepath.Base(file))
				if matched || strings.Contains(file, pattern) {
					return true, nil
				}
			}
		}
		return false, nil
	}

	return baseline != current, nil
}

// === HASHING ===

func hash(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

func hashShort(content []byte) string {
	return hash(content)[:16]
}

// === IDENTITY ===

func loadIdentity(cogRoot string) (*Identity, []byte, error) {
	path := filepath.Join(cogRoot, "id.cog")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var id Identity
	if err := yaml.Unmarshal(data, &id); err != nil {
		return nil, nil, err
	}
	return &id, data, nil
}

func identityHash(data []byte) string {
	// Normalize: set section block to null for consistent hashing
	lines := strings.Split(string(data), "\n")
	var normalized []string
	inSigBlock := false
	for _, line := range lines {
		if strings.HasPrefix(line, "\u00a7:") { // section sign
			normalized = append(normalized, "\u00a7: null")
			inSigBlock = true
			continue
		}
		if inSigBlock && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			continue // Skip indented lines under section
		}
		inSigBlock = false
		normalized = append(normalized, line)
	}
	return hash([]byte(strings.Join(normalized, "\n")))
}

func verifyIdentity(cogRoot string) (bool, *Identity, *Signature, error) {
	id, data, err := loadIdentity(cogRoot)
	if err != nil {
		return false, nil, nil, err
	}
	sig := id.GetSignature()
	if sig == nil {
		return false, id, nil, fmt.Errorf("unsigned (run 'cog init')")
	}
	expected := identityHash(data)
	if sig.Self != expected {
		return false, id, sig, fmt.Errorf("signature mismatch")
	}
	return true, id, sig, nil
}

// === COGDOC VALIDATION ===

// CogdocValidation holds validation results with errors and warnings
type CogdocValidation struct {
	Path           string     `json:"path"`
	Valid          bool       `json:"valid"`
	Errors         []string   `json:"errors,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
	StructuralRefs []TypedRef `json:"structural_refs,omitempty"`
	InlineRefs     []string   `json:"inline_refs,omitempty"`
}

// validateCogdocFull performs two-tier validation with errors and warnings
func validateCogdocFull(path string) *CogdocValidation {
	result := &CogdocValidation{Path: path, Valid: true}

	data, err := os.ReadFile(path)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	content := string(data)

	// Must end in .cog.md
	if !strings.HasSuffix(path, ".cog.md") {
		result.Valid = false
		result.Errors = append(result.Errors, "not a cogdoc (must end in .cog.md)")
		return result
	}

	// Check frontmatter
	if !strings.HasPrefix(content, "---\n") {
		result.Valid = false
		result.Errors = append(result.Errors, "no frontmatter")
		return result
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		result.Valid = false
		result.Errors = append(result.Errors, "unclosed frontmatter")
		return result
	}
	fmContent := content[4 : 4+end]
	bodyContent := content[4+end+4:] // Content after frontmatter

	var doc Cogdoc
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid YAML: %v", err))
		return result
	}

	// Required fields (events have different requirements)
	if doc.Type == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: type")
	}
	if doc.ID == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: id")
	}
	// Events don't require title/created - they use trigger/effects instead
	if doc.Type != "event" {
		if doc.Title == "" {
			result.Valid = false
			result.Errors = append(result.Errors, "missing: title")
		}
		if doc.Created == "" {
			result.Valid = false
			result.Errors = append(result.Errors, "missing: created")
		}
	}

	// Field value validation - uses ontology if cached
	if doc.Type != "" && !isValidCogdocTypeFromOntology(doc.Type) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid type '%s'", doc.Type))
	}
	if doc.ID != "" && !isKebabCase(doc.ID) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid id '%s' (must be kebab-case)", doc.ID))
	}
	if doc.Created != "" && !isValidDate(doc.Created) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid created '%s' (must be YYYY-MM-DD)", doc.Created))
	}

	// STRUCTURAL REFS (errors if broken)
	if doc.Refs != nil {
		refs, err := parseRefs(doc.Refs)
		if err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("invalid refs: %v", err))
		} else {
			result.StructuralRefs = refs
			for _, ref := range refs {
				if !strings.HasPrefix(ref.URI, "cog://") {
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("invalid ref URI '%s' (must start with cog://)", ref.URI))
				} else if ref.Rel != "" && !isValidRelationFromOntology(ref.Rel) {
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("invalid ref relation '%s'", ref.Rel))
				} else if _, err := resolveURI(ref.URI); err != nil {
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("broken structural ref '%s': %v", ref.URI, err))
				}
			}
		}
	}

	// INLINE/NAVIGATIONAL REFS (warnings if broken)
	inlineRefs := extractInlineRefs(bodyContent)
	result.InlineRefs = inlineRefs
	for _, uri := range inlineRefs {
		if _, err := resolveURI(uri); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("broken inline ref '%s': %v", uri, err))
		}
	}

	return result
}

func validateCogdoc(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)

	// Must end in .cog.md
	if !strings.HasSuffix(path, ".cog.md") {
		return fmt.Errorf("not a cogdoc (must end in .cog.md)")
	}

	// Check frontmatter
	if !strings.HasPrefix(content, "---\n") {
		return fmt.Errorf("no frontmatter")
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return fmt.Errorf("unclosed frontmatter")
	}
	fmContent := content[4 : 4+end]

	var doc Cogdoc
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}

	// Check required fields are present (events have different requirements)
	if doc.Type == "" {
		return fmt.Errorf("missing: type")
	}
	if doc.ID == "" {
		return fmt.Errorf("missing: id")
	}
	// Events don't require title/created - they use trigger/effects instead
	if doc.Type != "event" {
		if doc.Title == "" {
			return fmt.Errorf("missing: title")
		}
		if doc.Created == "" {
			return fmt.Errorf("missing: created")
		}
	}

	// Validate field VALUES (not just presence)

	// type: must be in allowed enum - uses ontology if cached
	if !isValidCogdocTypeFromOntology(doc.Type) {
		return fmt.Errorf("invalid type '%s' (see cog ontology types for allowed types)", doc.Type)
	}

	// id: must be kebab-case
	if !isKebabCase(doc.ID) {
		return fmt.Errorf("invalid id '%s' (must be kebab-case: lowercase, numbers, dashes only)", doc.ID)
	}

	// created: must be YYYY-MM-DD format (only checked if present)
	if doc.Created != "" && !isValidDate(doc.Created) {
		return fmt.Errorf("invalid created '%s' (must be YYYY-MM-DD)", doc.Created)
	}

	// Validate refs (structural references from frontmatter)
	if doc.Refs != nil {
		refs, err := parseRefs(doc.Refs)
		if err != nil {
			return fmt.Errorf("invalid refs: %w", err)
		}

		for _, ref := range refs {
			// Validate URI format
			if !strings.HasPrefix(ref.URI, "cog://") {
				return fmt.Errorf("invalid ref URI '%s' (must start with cog://)", ref.URI)
			}

			// Validate relationship type - uses ontology if cached
			if ref.Rel != "" && !isValidRelationFromOntology(ref.Rel) {
				return fmt.Errorf("invalid ref relation '%s' (see cog ontology relations for allowed values)", ref.Rel)
			}

			// Check if ref resolves (structural refs must exist)
			_, err := resolveURI(ref.URI)
			if err != nil {
				return fmt.Errorf("broken ref '%s': %w", ref.URI, err)
			}
		}
	}

	return nil
}

// === COHERENCE MODULE ===

// isPathTracked checks if a path is tracked for coherence validation
func isPathTracked(filePath string) bool {
	if !strings.HasPrefix(filePath, ".cog/") {
		return false
	}
	relPath := filePath[5:] // Remove .cog/ prefix

	// Check exclusions first - uses ontology if cached
	for _, excluded := range getEffectiveExcludedPaths() {
		if strings.HasPrefix(relPath, excluded) {
			return false
		}
	}

	// Check if in tracked paths - uses ontology if cached
	for _, tracked := range getEffectiveTrackedPaths() {
		if strings.HasPrefix(relPath, tracked) {
			return true
		}
	}

	return false
}

// checkCoherence checks overall coherence of the workspace
func checkCoherence(root string) (*CoherenceState, error) {
	canonical, canonicalErr := gitCanonicalHash(root)
	current, currentErr := gitCogTreeHash(root)

	state := &CoherenceState{
		Timestamp: nowISO(),
	}

	// Handle missing canonical hash file gracefully
	if canonicalErr != nil {
		state.Coherent = true // No baseline = coherent by default
		state.CurrentHash = current
		return state, nil
	}

	state.CanonicalHash = canonical
	state.CurrentHash = current
	state.Coherent = canonical == current

	// If incoherent, compute what drifted
	if !state.Coherent && canonical != "" && current != "" {
		drift, err := gitDiffTree(root, canonical, current)
		if err == nil {
			state.Drift = drift
		}
	}

	return state, currentErr
}

// recordCoherenceState saves the current coherence state to disk
func recordCoherenceState(root string) (*CoherenceState, error) {
	state, err := checkCoherence(root)
	if err != nil {
		return nil, err
	}

	stateFile := filepath.Join(root, ".cog", "run", "coherence", "coherence.json")

	// Load existing history
	var record CoherenceRecord
	if data, err := os.ReadFile(stateFile); err == nil {
		json.Unmarshal(data, &record)
	}

	// Keep last 100 history entries
	if len(record.History) >= 100 {
		record.History = record.History[1:]
	}
	if record.Current != nil {
		record.History = append(record.History, record.Current)
	}

	record.Current = state
	record.LastUpdated = nowISO()

	// Write atomically
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal coherence state: %w", err)
	}

	if err := writeAtomic(stateFile, data, 0644); err != nil {
		return nil, err
	}

	return state, nil
}

// getLastCoherenceState reads the last recorded coherence state
func getLastCoherenceState(root string) (*CoherenceState, error) {
	stateFile := filepath.Join(root, ".cog", "run", "coherence", "coherence.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}

	var record CoherenceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}

	return record.Current, nil
}

// === HOOK DISPATCH MODULE ===

// extractPriority extracts numeric priority from filename
// e.g., 10-signature-block.py -> 10
func extractPriority(filename string) int {
	re := regexp.MustCompile(`^(\d+)-`)
	if match := re.FindStringSubmatch(filename); len(match) > 1 {
		if n, err := strconv.Atoi(match[1]); err == nil {
			return n
		}
	}
	return 50 // Default priority
}

// parseHooksFile parses the main hooks control file
func parseHooksFile(hooksDir string) ([]Handler, error) {
	var handlers []Handler
	hooksFile := filepath.Join(hooksDir, "hooks")

	file, err := os.Open(hooksFile)
	if err != nil {
		return handlers, nil // No hooks file is OK
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		event, matcher, handlerPath := parts[0], parts[1], parts[2]
		fullPath := filepath.Join(hooksDir, handlerPath)

		// Check if it's a directory or file
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}

		if info.IsDir() {
			// Discover handlers in directory
			dirHandlers := discoverHandlersInDir(fullPath, event, matcher)
			handlers = append(handlers, dirHandlers...)
		} else {
			// Single script
			handlers = append(handlers, Handler{
				Event:    event,
				Matcher:  matcher,
				Script:   fullPath,
				Priority: extractPriority(info.Name()),
				Blocking: true,
				Name:     strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())),
			})
		}
	}

	return handlers, scanner.Err()
}

// discoverHandlersInDir finds all executable scripts in a directory
func discoverHandlersInDir(dir, event, matcher string) []Handler {
	var handlers []Handler

	entries, err := os.ReadDir(dir)
	if err != nil {
		return handlers
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Skip hidden files and disabled scripts
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".disabled") {
			continue
		}

		// Skip .builtin files (they're just markers, not executable)
		ext := filepath.Ext(name)
		if ext == ".builtin" {
			continue
		}

		// Must be executable or .py/.sh
		info, _ := entry.Info()
		isScript := ext == ".py" || ext == ".sh"
		isExec := info != nil && info.Mode()&0111 != 0

		if !isScript && !isExec {
			continue
		}

		handlers = append(handlers, Handler{
			Event:    event,
			Matcher:  matcher,
			Script:   filepath.Join(dir, name),
			Priority: extractPriority(name),
			Blocking: true,
			Name:     strings.TrimSuffix(name, ext),
		})
	}

	return handlers
}


// runHandler executes a single handler script
func runHandler(handler Handler, inputData map[string]interface{}) *HookResult {
	// Try built-in handler first (FAST PATH)
	if result := tryBuiltinHandler(handler, inputData); result != nil {
		return result
	}

	// Fall back to external script (SLOW PATH)
	var cmd *exec.Cmd
	ext := filepath.Ext(handler.Script)

	switch ext {
	case ".py":
		cmd = exec.Command("python3", handler.Script)
	case ".sh":
		cmd = exec.Command("bash", handler.Script)
	default:
		cmd = exec.Command(handler.Script)
	}

	// Prepare input
	inputJSON, _ := json.Marshal(inputData)
	cmd.Stdin = strings.NewReader(string(inputJSON))

	// Set working directory to project root
	if root, _, err := ResolveWorkspace(); err == nil {
		cmd.Dir = root
	}

	// Run with timeout (handled by exec, we'll add explicit timeout later)
	output, err := cmd.Output()
	if err != nil {
		// On error, allow by default (graceful degradation)
		return &HookResult{Decision: "allow", Fallback: true}
	}

	// Parse output
	if len(output) > 0 {
		var result HookResult
		if err := json.Unmarshal(output, &result); err == nil {
			return &result
		}
	}

	// No output or non-JSON = allow
	return &HookResult{Decision: "allow"}
}

// dispatch routes an event to matching handlers
func dispatch(event, toolName string, inputData map[string]interface{}) *HookResult {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return &HookResult{Decision: "allow", Fallback: true}
	}

	hooksDir := filepath.Join(root, ".cog", "hooks")

	// Collect all handlers
	handlers, _ := parseHooksFile(hooksDir)

	// Also check event-specific directory
	// Convert event name to kebab-case (e.g., PreInference -> pre-inference)
	eventDir := filepath.Join(hooksDir, camelCaseToKebab(event)+".d")
	if info, err := os.Stat(eventDir); err == nil && info.IsDir() {
		dirHandlers := discoverHandlersInDir(eventDir, event, "*")
		handlers = append(handlers, dirHandlers...)
	}

	// Filter to matching handlers
	var matching []Handler
	for _, h := range handlers {
		if h.Event != event {
			continue
		}
		// Check matcher
		if h.Matcher == "*" || h.Matcher == toolName || h.Matcher == "tool:"+toolName {
			matching = append(matching, h)
		}
	}

	// Sort by priority
	sort.Slice(matching, func(i, j int) bool {
		return matching[i].Priority < matching[j].Priority
	})

	// Run handlers
	for _, handler := range matching {
		result := runHandler(handler, inputData)

		// Blocking handler returned block -> stop
		if handler.Blocking && result.Decision == "block" {
			return result
		}
	}

	return &HookResult{Decision: "allow"}
}

// === EVENT SYSTEM ===
// Declarative event handlers loaded from .cog/events/*.cog.md
// Terraform-like: cogdocs define behavior, kernel executes effects

// EventHandler represents a declarative event handler cogdoc
type EventHandler struct {
	ID       string                   `yaml:"id"`
	Trigger  string                   `yaml:"trigger"`
	Enabled  bool                     `yaml:"enabled"`
	Priority int                      `yaml:"priority"`
	Effects  []map[string]interface{} `yaml:"effects"`
	Path     string                   // Source file path
}

// EventIndex holds all loaded event handlers
type EventIndex struct {
	ByTrigger map[string][]*EventHandler
	All       []*EventHandler
}

// BuildEventIndex loads all event handlers from .cog/events/
func BuildEventIndex(cogRoot string) (*EventIndex, error) {
	idx := &EventIndex{
		ByTrigger: make(map[string][]*EventHandler),
		All:       []*EventHandler{},
	}

	eventsDir := filepath.Join(cogRoot, "events")
	if _, err := os.Stat(eventsDir); os.IsNotExist(err) {
		// No events directory yet, return empty index
		return idx, nil
	}

	// Glob all .cog.md files in events/
	pattern := filepath.Join(eventsDir, "*.cog.md")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	for _, path := range files {
		handler, err := loadEventHandler(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load event handler %s: %v\n", path, err)
			continue
		}
		if handler == nil || !handler.Enabled {
			continue
		}

		idx.All = append(idx.All, handler)
		idx.ByTrigger[handler.Trigger] = append(idx.ByTrigger[handler.Trigger], handler)
	}

	// Sort handlers by priority within each trigger
	for trigger := range idx.ByTrigger {
		handlers := idx.ByTrigger[trigger]
		sort.Slice(handlers, func(i, j int) bool {
			return handlers[i].Priority < handlers[j].Priority
		})
	}

	return idx, nil
}

// loadEventHandler parses a single event handler cogdoc
func loadEventHandler(path string) (*EventHandler, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)

	// Parse frontmatter
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("missing frontmatter")
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return nil, fmt.Errorf("unclosed frontmatter")
	}
	fmContent := content[4 : 4+end]

	// Parse into a map first to check type
	var raw map[string]interface{}
	if err := yaml.Unmarshal([]byte(fmContent), &raw); err != nil {
		return nil, err
	}

	// Must be type: event
	docType, _ := raw["type"].(string)
	if docType != "event" {
		return nil, nil // Not an event handler, skip silently
	}

	// Parse handler fields
	handler := &EventHandler{
		Path:     path,
		Enabled:  true, // Default enabled
		Priority: 100,  // Default priority
	}

	if v, ok := raw["id"].(string); ok {
		handler.ID = v
	}
	if v, ok := raw["trigger"].(string); ok {
		handler.Trigger = v
	}
	if v, ok := raw["enabled"].(bool); ok {
		handler.Enabled = v
	}
	if v, ok := raw["priority"].(int); ok {
		handler.Priority = v
	}

	// Parse effects array
	if effects, ok := raw["effects"].([]interface{}); ok {
		for _, e := range effects {
			if em, ok := e.(map[string]interface{}); ok {
				handler.Effects = append(handler.Effects, em)
			}
		}
	}

	if handler.Trigger == "" {
		return nil, fmt.Errorf("missing trigger field")
	}
	if len(handler.Effects) == 0 {
		return nil, fmt.Errorf("no effects defined")
	}

	return handler, nil
}

// === EFFECT EXECUTORS ===

// execEffect runs a single effect
func execEffect(effect map[string]interface{}, dryRun bool) error {
	// Effects are maps with one key (the effect name) and value (params)
	// e.g., {"log.info": "message"} or {"wave.title": "My Title"}

	for name, params := range effect {
		if dryRun {
			fmt.Fprintf(os.Stderr, "[dry-run] Would execute: %s = %v\n", name, params)
			continue
		}

		var err error
		switch name {
		case "log.info":
			err = execLogInfo(params)
		case "log.error":
			err = execLogError(params)
		case "wave.title":
			err = execWaveTitle(params)
		case "wave.state":
			err = execWaveState(params)
		case "signal.emit":
			err = execSignalEmit(params)
		default:
			fmt.Fprintf(os.Stderr, "Warning: unknown effect %s\n", name)
		}

		if err != nil {
			return fmt.Errorf("effect %s failed: %w", name, err)
		}
	}
	return nil
}

func execLogInfo(params interface{}) error {
	msg, _ := params.(string)
	fmt.Fprintf(os.Stderr, "[event] %s\n", msg)
	return nil
}

func execLogError(params interface{}) error {
	msg, _ := params.(string)
	fmt.Fprintf(os.Stderr, "[event:error] %s\n", msg)
	return nil
}

func execWaveTitle(params interface{}) error {
	title, _ := params.(string)
	if title == "" {
		return nil
	}

	// Check if wsh is available
	wshPath, err := exec.LookPath("wsh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[event] wave.title skipped: wsh not found\n")
		return nil // Graceful degradation
	}

	cmd := exec.Command(wshPath, "setmeta", "-b", "this", fmt.Sprintf("title=%s", title))
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[event] wave.title warning: %v\n", err)
	}
	return nil
}

func execWaveState(params interface{}) error {
	// params should be map with "key" and "value"
	pm, ok := params.(map[string]interface{})
	if !ok {
		return fmt.Errorf("wave.state requires {key, value}")
	}

	key, _ := pm["key"].(string)
	value, _ := pm["value"].(string)
	if key == "" {
		return nil
	}

	// Check if wsh is available
	wshPath, err := exec.LookPath("wsh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[event] wave.state skipped: wsh not found\n")
		return nil // Graceful degradation
	}

	cmd := exec.Command(wshPath, "setvar", fmt.Sprintf("%s=%s", key, value))
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[event] wave.state warning: %v\n", err)
	}
	return nil
}

func execSignalEmit(params interface{}) error {
	// params should be map with "name" and optionally "data", "ttl"
	pm, ok := params.(map[string]interface{})
	if !ok {
		// Allow simple string for signal name
		if name, ok := params.(string); ok {
			pm = map[string]interface{}{"name": name}
		} else {
			return fmt.Errorf("signal.emit requires {name} or string")
		}
	}

	name, _ := pm["name"].(string)
	if name == "" {
		return fmt.Errorf("signal.emit requires name")
	}

	// Get workspace root
	root, _, err := ResolveWorkspace()
	if err != nil {
		return err
	}

	signalsDir := filepath.Join(root, ".cog", "signals")
	if err := os.MkdirAll(signalsDir, 0755); err != nil {
		return err
	}

	signal := map[string]interface{}{
		"name":      name,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"source":    "event",
	}
	if data, ok := pm["data"]; ok {
		signal["data"] = data
	}

	signalPath := filepath.Join(signalsDir, name+".json")
	signalData, _ := json.MarshalIndent(signal, "", "  ")
	return os.WriteFile(signalPath, signalData, 0644)
}

// === EMIT COMMAND ===

func cmdEmit(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cog emit <event> [--dry-run]\n")
		fmt.Fprintf(os.Stderr, "\nEvents: cog.session.start, cog.session.end, etc.\n")
		return 1
	}

	eventName := args[0]
	dryRun := false

	for _, arg := range args[1:] {
		if arg == "--dry-run" {
			dryRun = true
		}
	}

	// Get workspace root
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding workspace: %v\n", err)
		return 1
	}
	cogRoot := filepath.Join(root, ".cog")

	// Build event index
	idx, err := BuildEventIndex(cogRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading event handlers: %v\n", err)
		return 1
	}

	// Find matching handlers
	handlers := idx.ByTrigger[eventName]
	if len(handlers) == 0 {
		if dryRun {
			fmt.Fprintf(os.Stderr, "[dry-run] No handlers for event: %s\n", eventName)
		}
		return 0 // No handlers is not an error
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "[dry-run] Event: %s, %d handler(s)\n", eventName, len(handlers))
	}

	// Execute handlers in priority order
	for _, handler := range handlers {
		if dryRun {
			fmt.Fprintf(os.Stderr, "[dry-run] Handler: %s (priority %d)\n", handler.ID, handler.Priority)
		}

		for _, effect := range handler.Effects {
			if err := execEffect(effect, dryRun); err != nil {
				fmt.Fprintf(os.Stderr, "Error in handler %s: %v\n", handler.ID, err)
				return 1 // Stop on first failure
			}
		}
	}

	return 0
}

// === CQL COMMANDS ===

func cmdReadURI(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cog read <uri>\n")
		return 1
	}

	uri := args[0]

	// Resolve URI to path
	path, err := resolveURI(uri)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving URI: %v\n", err)
		return 1
	}

	// Try with .cog.md extension first
	fullPath := path
	if !strings.HasSuffix(path, ".cog.md") && !strings.HasSuffix(path, ".md") {
		fullPath = path + ".cog.md"
	}

	// Read content
	data, err := os.ReadFile(fullPath)
	if err != nil {
		// Try without extension
		data, err = os.ReadFile(path)
		if err != nil {
			// Try with .md extension
			data, err = os.ReadFile(path + ".md")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
				return 1
			}
		}
	}

	// Output content
	fmt.Print(string(data))
	return 0
}

func cmdListURI(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cog list <uri-namespace>\n")
		return 1
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	cogRoot := filepath.Join(root, ".cog")

	// Build index
	idx, err := BuildCogdocIndex(cogRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building index: %v\n", err)
		return 1
	}

	namespace := args[0]

	// Remove trailing slash
	namespace = strings.TrimSuffix(namespace, "/")

	// List all URIs that start with namespace
	var matching []string
	for uri := range idx.ByURI {
		if strings.HasPrefix(uri, namespace) {
			matching = append(matching, uri)
		}
	}

	// Sort for consistent output
	sort.Strings(matching)

	// Output
	for _, uri := range matching {
		fmt.Println(uri)
	}

	return 0
}

func cmdQueryURI(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	cogRoot := filepath.Join(root, ".cog")

	// Build index
	idx, err := BuildCogdocIndex(cogRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building index: %v\n", err)
		return 1
	}

	// Parse arguments
	pattern := ""
	var filterTags []string
	filterType := ""
	filterStatus := ""

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--tags" {
			if i+1 < len(args) {
				filterTags = strings.Split(args[i+1], ",")
				i += 2
			} else {
				fmt.Fprintf(os.Stderr, "Error: --tags requires an argument\n")
				return 1
			}
		} else if arg == "--type" {
			if i+1 < len(args) {
				filterType = args[i+1]
				i += 2
			} else {
				fmt.Fprintf(os.Stderr, "Error: --type requires an argument\n")
				return 1
			}
		} else if arg == "--status" {
			if i+1 < len(args) {
				filterStatus = args[i+1]
				i += 2
			} else {
				fmt.Fprintf(os.Stderr, "Error: --status requires an argument\n")
				return 1
			}
		} else {
			pattern = arg
			i++
		}
	}

	// Start with all URIs or filtered by pattern
	var candidates []*IndexedCogdoc
	if pattern != "" {
		// Filter by URI prefix
		pattern = strings.TrimSuffix(pattern, "/")
		for uri, doc := range idx.ByURI {
			if strings.HasPrefix(uri, pattern) {
				candidates = append(candidates, doc)
			}
		}
	} else {
		// All cogdocs
		for _, doc := range idx.ByURI {
			candidates = append(candidates, doc)
		}
	}

	// Apply filters
	var results []*IndexedCogdoc

	for _, doc := range candidates {
		// Filter by type
		if filterType != "" && doc.Type != filterType {
			continue
		}

		// Filter by status
		if filterStatus != "" && doc.Status != filterStatus {
			continue
		}

		// Filter by tags (all tags must match)
		if len(filterTags) > 0 {
			hasAllTags := true
			for _, filterTag := range filterTags {
				found := false
				for _, docTag := range doc.Tags {
					if docTag == filterTag {
						found = true
						break
					}
				}
				if !found {
					hasAllTags = false
					break
				}
			}
			if !hasAllTags {
				continue
			}
		}

		results = append(results, doc)
	}

	// Sort results by URI
	sort.Slice(results, func(i, j int) bool {
		return results[i].URI < results[j].URI
	})

	// Output
	for _, doc := range results {
		fmt.Println(doc.URI)
	}

	return 0
}

// === COMMANDS ===

func cmdInit() int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	cogRoot := filepath.Join(root, ".cog")

	// Load identity
	id, data, err := loadIdentity(cogRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Check if already signed
	sig := id.GetSignature()
	if sig != nil && sig.Self != "" {
		valid, _, _, err := verifyIdentity(cogRoot)
		if valid {
			fmt.Printf("Already signed: %s\n", sig.Self[:16])
			return 0
		}
		fmt.Printf("Invalid signature: %v\nRe-signing...\n", err)
	}

	// Compute hashes
	selfHash := identityHash(data)
	parentHash, _ := gitTreeHash()
	kernelData, _ := os.ReadFile(filepath.Join(cogRoot, "cog.go"))
	kernelHash := hash(kernelData)

	// Write signed identity
	newContent := fmt.Sprintf(`type: %s
id: %s
version: %s
name: %s
created: %s

§:
  self: %s
  parent: %s
  kernel: %s
`, id.Type, id.ID, id.Version, id.Name, id.Created, selfHash, parentHash, kernelHash)

	if err := writeAtomic(filepath.Join(cogRoot, "id.cog"), []byte(newContent), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing: %v\n", err)
		return 1
	}

	fmt.Printf("Identity signed.\n")
	fmt.Printf("  self:   %s\n", selfHash[:16])
	fmt.Printf("  parent: %s\n", parentHash[:16])
	fmt.Printf("  kernel: %s\n", kernelHash[:16])
	return 0
}

func cmdVerify(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	cogRoot := filepath.Join(root, ".cog")

	// Parse flags
	var changed bool
	var since string
	var force bool
	var fileArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--changed":
			changed = true
		case "--since":
			if i+1 < len(args) {
				since = args[i+1]
				i++
			} else {
				fmt.Fprintf(os.Stderr, "Error: --since requires an argument\n")
				return 1
			}
		case "--force":
			force = true
		default:
			fileArgs = append(fileArgs, arg)
		}
	}

	// Verify identity
	valid, _, sig, err := verifyIdentity(cogRoot)
	idStatus := "\033[31mINVALID\033[0m"
	if err != nil {
		idStatus = fmt.Sprintf("\033[31m%v\033[0m", err)
	} else if valid && sig != nil {
		idStatus = fmt.Sprintf("\033[32m%s\033[0m", sig.Self[:16])
	}

	// Get tree hash
	treeHash, _ := gitTreeHash()
	if treeHash == "" {
		treeHash = "(dirty)"
	}

	fmt.Printf("Identity: %s\n", idStatus)
	if len(treeHash) >= 16 {
		fmt.Printf("Tree:     %s\n", treeHash[:16])
	}

	// Check if we can skip verification
	if !force && !changed && len(fileArgs) == 0 {
		needs, err := needsRun(root, "verify", []string{".cog/**/*.cog.md"})
		if err == nil && !needs {
			fmt.Printf("Cogdocs:  \033[32m✓ No changes, skipping validation\033[0m\n")
			if valid {
				return 0
			}
			return 1
		}
	}

	// Determine files to validate
	var files []string
	if changed {
		// Get files changed since ref or baseline
		patterns := []string{".cog/**/*.cog.md"}
		if since != "" {
			files, err = getChangedFiles(root, since, patterns)
		} else {
			files, err = getChangedSinceBaseline(root, patterns)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting changed files: %v\n", err)
			return 1
		}
	} else if len(fileArgs) > 0 {
		files = fileArgs
	} else {
		files, _ = gitStagedCogfiles()
	}

	if len(files) == 0 {
		if changed {
			fmt.Printf("Cogdocs:  \033[32m✓ No changed files\033[0m\n")
		} else {
			fmt.Printf("Cogdocs:  (none staged)\n")
		}
		if valid {
			// Update baseline on successful validation
			if changed || force {
				updateBaseline(root, "verify")
			}
			return 0
		}
		return 1
	}

	// Validate files
	fmt.Printf("Cogdocs:  %d to verify\n", len(files))
	allValid := true
	for _, f := range files {
		// Use path as-is if absolute, otherwise join with root
		path := f
		if !filepath.IsAbs(f) {
			path = filepath.Join(root, f)
		}
		if err := validateCogdoc(path); err != nil {
			fmt.Printf("  \033[31m[X]\033[0m %s: %v\n", f, err)
			allValid = false
		} else {
			fmt.Printf("  \033[32m[OK]\033[0m %s\n", f)
		}
	}

	// Update baseline on successful validation
	if allValid && (changed || force) {
		if err := updateBaseline(root, "verify"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update baseline: %v\n", err)
		}
	}

	if !valid || !allValid {
		return 1
	}
	return 0
}

func cmdHash() int {
	h, err := gitTreeHash()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Println(h)
	return 0
}

// cmdOntology handles ontology operations
func cmdOntology(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	cogRoot := filepath.Join(root, ".cog")

	subCmd := "show"
	if len(args) > 0 {
		subCmd = args[0]
	}

	switch subCmd {
	case "show", "":
		// Load and display ontology
		ont, err := getOntology(cogRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading ontology: %v\n", err)
			return 1
		}

		fmt.Printf("Ontology: %s\n", ont.Title)
		fmt.Printf("  ID:       %s\n", ont.ID)
		fmt.Printf("  Version:  %s\n", ont.Version)
		fmt.Printf("  Status:   %s\n", ont.Status)
		fmt.Printf("\nTopology Primitives:\n")
		for name, prim := range ont.Topology.Primitives {
			fmt.Printf("  %s: %s (%s)\n", name, prim.Path, prim.Purpose)
		}
		fmt.Printf("\nCoherence:\n")
		fmt.Printf("  Model:    %s\n", ont.Coherence.Model)
		fmt.Printf("  Tracked:  %v\n", ont.Coherence.Tracked)
		fmt.Printf("  Excluded: %v\n", ont.Coherence.Excluded)
		fmt.Printf("\nCogdoc Types: %d core + %d semantic\n",
			len(ont.Cogdoc.Types.Core), len(ont.Cogdoc.Types.Semantic))
		fmt.Printf("Relations:    %d structural + %d semantic\n",
			len(ont.Cogdoc.Relations.Structural), len(ont.Cogdoc.Relations.Semantic))

		return 0

	case "verify":
		// Verify ontology is valid and parseable
		ont, err := loadOntology(cogRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			return 1
		}

		// Check basic validity
		if ont.Type != "ontology" {
			fmt.Fprintf(os.Stderr, "FAIL: type must be 'ontology', got '%s'\n", ont.Type)
			return 1
		}
		if ont.ID == "" {
			fmt.Fprintf(os.Stderr, "FAIL: missing id field\n")
			return 1
		}
		if len(ont.Topology.Primitives) < 7 {
			fmt.Fprintf(os.Stderr, "FAIL: expected 7 primitives, got %d\n", len(ont.Topology.Primitives))
			return 1
		}

		fmt.Printf("OK: Ontology valid (%s v%s)\n", ont.ID, ont.Version)
		fmt.Printf("  Primitives:     %d\n", len(ont.Topology.Primitives))
		fmt.Printf("  URI Projections: %d\n", len(ont.URIScheme.Projections))
		fmt.Printf("  Cogdoc Types:   %d\n", len(ont.Cogdoc.Types.Core)+len(ont.Cogdoc.Types.Semantic))
		fmt.Printf("  Relations:      %d\n", len(ont.Cogdoc.Relations.Structural)+len(ont.Cogdoc.Relations.Semantic))
		return 0

	case "types":
		// List all valid cogdoc types
		ont, err := getOntology(cogRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		fmt.Println("Core types:")
		for _, t := range ont.Cogdoc.Types.Core {
			fmt.Printf("  %s\n", t)
		}
		fmt.Println("\nSemantic types:")
		for _, t := range ont.Cogdoc.Types.Semantic {
			fmt.Printf("  %s\n", t)
		}
		return 0

	case "relations":
		// List all valid relations
		ont, err := getOntology(cogRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		fmt.Println("Structural relations:")
		for _, r := range ont.Cogdoc.Relations.Structural {
			fmt.Printf("  %s\n", r)
		}
		fmt.Println("\nSemantic relations:")
		for _, r := range ont.Cogdoc.Relations.Semantic {
			fmt.Printf("  %s\n", r)
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown ontology subcommand: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog ontology [show|verify|types|relations]\n")
		return 1
	}
}

func cmdCoherence(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Parse flags and subcommand
	subCmd := "check"
	var changed bool
	var since string

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--changed" {
			changed = true
			i++
		} else if arg == "--since" {
			if i+1 < len(args) {
				since = args[i+1]
				i += 2
			} else {
				fmt.Fprintf(os.Stderr, "Error: --since requires an argument\n")
				return 1
			}
		} else {
			subCmd = arg
			i++
			break
		}
	}

	switch subCmd {
	case "check":
		// Check if we need to run full coherence check
		if changed {
			// Get changed files
			var changedFiles []string
			if since != "" {
				changedFiles, err = getChangedFiles(root, since, []string{".cog/**/*"})
			} else {
				changedFiles, err = getChangedSinceBaseline(root, []string{".cog/**/*"})
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error getting changed files: %v\n", err)
				return 1
			}

			if len(changedFiles) == 0 {
				fmt.Println(`{"coherent": true, "message": "No changes detected"}`)
				return 0
			}

			fmt.Fprintf(os.Stderr, "Checking coherence for %d changed files...\n", len(changedFiles))
		}

		state, err := checkCoherence(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		output, _ := json.MarshalIndent(state, "", "  ")
		fmt.Println(string(output))
		if !state.Coherent {
			return 1
		}
		return 0

	case "baseline", "record":
		// Compute current tree hash
		currentHash, err := gitCogTreeHash(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error computing tree hash: %v\n", err)
			return 1
		}

		// Set as canonical (holographic baseline)
		if err := setCanonicalHash(root, currentHash); err != nil {
			fmt.Fprintf(os.Stderr, "Error setting canonical hash: %v\n", err)
			return 1
		}

		// Record coherence state
		state, err := recordCoherenceState(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		output, _ := json.MarshalIndent(state, "", "  ")
		fmt.Println(string(output))
		fmt.Fprintf(os.Stderr, "Holographic baseline established: %s\n", currentHash[:12])
		fmt.Fprintf(os.Stderr, "Canonical hash: .cog/run/coherence/canonical-hash\n")
		fmt.Fprintf(os.Stderr, "Coherence state: .cog/run/coherence/coherence.json\n")

		// Also update coherence baseline
		updateBaseline(root, "coherence")
		return 0

	case "drift":
		// Check if we need to show drift
		if changed {
			needs, err := needsRun(root, "coherence", []string{".cog/**/*"})
			if err == nil && !needs {
				fmt.Println("No changes since last check - coherent")
				return 0
			}
		}

		state, err := checkCoherence(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if state.Coherent {
			fmt.Println("No drift - coherent with canonical")
			return 0
		}
		fmt.Printf("Drift detected (%d files):\n", len(state.Drift))
		for _, f := range state.Drift {
			fmt.Printf("  - %s\n", f)
		}
		return 1

	case "status":
		state, err := getLastCoherenceState(root)
		if err != nil {
			fmt.Println("No recorded coherence state")
			return 0
		}
		output, _ := json.MarshalIndent(state, "", "  ")
		fmt.Println(string(output))
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown coherence command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog coherence [--changed] [--since REF] {check|baseline|drift|status}\n")
		return 1
	}
}

func cmdTree(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	subCmd := "build"
	if len(args) > 0 {
		subCmd = args[0]
	}

	switch subCmd {
	case "build":
		// Build complete cognitive state tree (all files, tracked + untracked)
		fmt.Fprintf(os.Stderr, "Building cognitive state tree from .cog/...\n")

		treeHash, err := BuildCognitiveStateTree(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Count files in tree
		cmd := exec.Command("git", "-C", root, "ls-tree", "-r", treeHash)
		output, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error counting files: %v\n", err)
			return 1
		}
		fileCount := 0
		if len(output) > 0 {
			fileCount = len(strings.Split(strings.TrimSpace(string(output)), "\n"))
		}

		// Write to canonical hash file
		if err := setCanonicalHash(root, treeHash); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing canonical hash: %v\n", err)
			return 1
		}

		fmt.Printf("%s\n", treeHash)
		fmt.Fprintf(os.Stderr, "Tree built successfully: %d files\n", fileCount)
		fmt.Fprintf(os.Stderr, "Canonical hash written to: .cog/run/coherence/canonical-hash\n")
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown tree command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog tree {build}\n")
		return 1
	}
}

func cmdDispatch(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cog dispatch <event> [tool]\n")
		fmt.Fprintf(os.Stderr, "Events: PreToolUse, SessionStart, SessionEnd, Stop, PreCompact\n")
		return 1
	}

	event := args[0]
	toolName := "*"
	if len(args) > 1 {
		toolName = args[1]
	}

	// Read input from stdin
	inputData := make(map[string]interface{})
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		// Stdin has data
		data, err := io.ReadAll(os.Stdin)
		if err == nil && len(data) > 0 {
			json.Unmarshal(data, &inputData)
			// Extract tool name from input if not provided
			if toolName == "*" {
				if tn, ok := inputData["tool_name"].(string); ok {
					toolName = tn
				}
			}
		}
	}

	result := dispatch(event, toolName, inputData)

	// Only output if blocking
	if result.Decision == "block" {
		output, _ := json.Marshal(result)
		fmt.Println(string(output))
	}

	return 0
}

func cmdVersion() int {
	fmt.Printf("cog %s\n", Version)
	if BuildTime != "unknown" {
		fmt.Printf("built: %s\n", BuildTime)
	}
	root, _, err := ResolveWorkspace()
	if err == nil {
		data, _ := os.ReadFile(filepath.Join(root, ".cog", "cog.go"))
		if len(data) > 0 {
			fmt.Printf("kernel: %s\n", hashShort(data))
		}
	}
	return 0
}

func cmdHealth(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Printf("git: \033[31mNO\033[0m (%v)\n", err)
		return 1
	}

	// Handle subcommands
	if len(args) > 0 {
		switch args[0] {
		case "canonical-tree":
			// Run canonical tree health check
			if err := HealthCheckCanonicalTree(root); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			return 0
		default:
			fmt.Fprintf(os.Stderr, "Unknown health subcommand: %s\n", args[0])
			fmt.Fprintf(os.Stderr, "Available: canonical-tree\n")
			return 1
		}
	}

	// Default health check (no subcommand)
	fmt.Printf("git: \033[32mOK\033[0m\n")

	cogRoot := filepath.Join(root, ".cog")

	// Check identity
	valid, _, sig, err := verifyIdentity(cogRoot)
	if valid && sig != nil {
		fmt.Printf("identity: \033[32m%s\033[0m\n", sig.Self[:16])
	} else {
		fmt.Printf("identity: \033[31m%v\033[0m\n", err)
	}

	// Check coherence
	state, err := checkCoherence(root)
	if err != nil {
		fmt.Printf("coherence: \033[31mERROR\033[0m (%v)\n", err)
	} else if state.Coherent {
		fmt.Printf("coherence: \033[32mOK\033[0m\n")
	} else {
		fmt.Printf("coherence: \033[33mDRIFT\033[0m (%d files)\n", len(state.Drift))
	}

	// Check HFS directories (run/ for runtime state, var/ for variable data)
	runDir := filepath.Join(cogRoot, "run")
	varDir := filepath.Join(cogRoot, "var")
	runOk := false
	varOk := false
	if info, err := os.Stat(runDir); err == nil && info.IsDir() {
		runOk = true
	}
	if info, err := os.Stat(varDir); err == nil && info.IsDir() {
		varOk = true
	}
	if runOk && varOk {
		fmt.Printf("hfs_dirs: \033[32mOK\033[0m (run/, var/)\n")
	} else if runOk || varOk {
		missing := "run/"
		if runOk {
			missing = "var/"
		}
		fmt.Printf("hfs_dirs: \033[33mPARTIAL\033[0m (missing %s)\n", missing)
	} else {
		fmt.Printf("hfs_dirs: \033[31mMISSING\033[0m (run/, var/)\n")
	}

	// Check hooks
	hooksDir := filepath.Join(cogRoot, "hooks")
	hooksFile := filepath.Join(hooksDir, "hooks")
	if _, err := os.Stat(hooksFile); err == nil {
		handlers, _ := parseHooksFile(hooksDir)
		fmt.Printf("hooks: \033[32m%d handlers\033[0m\n", len(handlers))
	} else {
		fmt.Printf("hooks: \033[33mNO CONTROL FILE\033[0m\n")
	}

	return 0
}

func cmdProjection(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	subCmd := "project"
	if len(args) > 0 {
		subCmd = args[0]
	}

	switch subCmd {
	case "project", "run":
		result := runFullProjection(root)
		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))
		if !result.Success {
			return 1
		}
		return 0

	case "validate":
		result := validateProjection(root)
		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))
		if valid, ok := result["valid"].(bool); ok && !valid {
			return 1
		}
		return 0

	case "drift":
		result := detectProjectionDrift(root)
		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))
		if hasDrift, ok := result["has_drift"].(bool); ok && hasDrift {
			return 1
		}
		return 0

	case "clean":
		orphans := cleanOrphanProjections(root)
		result := map[string]interface{}{
			"cleaned": orphans,
			"count":   len(orphans),
		}
		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))
		return 0

	case "state":
		state, err := loadProjectionState(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		output, _ := json.MarshalIndent(state, "", "  ")
		fmt.Println(string(output))
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown projection command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog projection {project|validate|drift|clean|state}\n")
		return 1
	}
}

// cmdArtifact handles artifact management commands
func cmdArtifact(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	subCmd := "list"
	if len(args) > 0 {
		subCmd = args[0]
	}

	ledgerDir := filepath.Join(root, ".cog", "ledger")

	switch subCmd {
	case "list":
		// List artifacts from current or specified session
		session := ""
		if len(args) > 1 {
			session = args[1]
		}
		return artifactList(ledgerDir, session)

	case "get":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog artifact get <hash> [session]\n")
			return 1
		}
		hash := args[1]
		session := ""
		if len(args) > 2 {
			session = args[2]
		}
		return artifactGet(ledgerDir, hash, session)

	case "sessions":
		return artifactSessions(ledgerDir)

	case "resolve":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog artifact resolve <hash>\n")
			return 1
		}
		hash := args[1]
		return artifactResolve(ledgerDir, hash)

	default:
		fmt.Fprintf(os.Stderr, "Unknown artifact command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog artifact {list|get|sessions|resolve}\n")
		return 1
	}
}

// artifactList lists artifacts from a session
func artifactList(ledgerDir, session string) int {
	// If no session specified, find most recent
	if session == "" {
		entries, err := os.ReadDir(ledgerDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading ledger: %v\n", err)
			return 1
		}
		var latest string
		var latestTime int64
		for _, e := range entries {
			if e.IsDir() {
				info, err := e.Info()
				if err == nil && info.ModTime().Unix() > latestTime {
					latestTime = info.ModTime().Unix()
					latest = e.Name()
				}
			}
		}
		if latest == "" {
			fmt.Println("No sessions found")
			return 0
		}
		session = latest
	}

	manifestPath := filepath.Join(ledgerDir, session, "artifacts", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "No artifacts for session %s\n", session)
		return 0
	}

	var manifest map[string]interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing manifest: %v\n", err)
		return 1
	}

	// Output as JSON
	output, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(output))
	return 0
}

// artifactGet retrieves a specific artifact by hash
func artifactGet(ledgerDir, hash, session string) int {
	var searchDirs []string

	if session != "" {
		searchDirs = append(searchDirs, filepath.Join(ledgerDir, session, "artifacts"))
	} else {
		// Search all sessions
		entries, _ := os.ReadDir(ledgerDir)
		for _, e := range entries {
			if e.IsDir() {
				searchDirs = append(searchDirs, filepath.Join(ledgerDir, e.Name(), "artifacts"))
			}
		}
	}

	for _, dir := range searchDirs {
		pattern := filepath.Join(dir, hash+".*")
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			content, err := os.ReadFile(matches[0])
			if err == nil {
				fmt.Print(string(content))
				return 0
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Artifact not found: %s\n", hash)
	return 1
}

// artifactSessions lists all sessions with artifacts
func artifactSessions(ledgerDir string) int {
	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading ledger: %v\n", err)
		return 1
	}

	sessions := []map[string]interface{}{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(ledgerDir, e.Name(), "artifacts", "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest map[string]interface{}
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		sessions = append(sessions, map[string]interface{}{
			"session_id":     e.Name(),
			"artifact_count": manifest["artifact_count"],
			"extracted_at":   manifest["extracted_at"],
		})
	}

	output, _ := json.MarshalIndent(sessions, "", "  ")
	fmt.Println(string(output))
	return 0
}

// artifactResolve finds an artifact by hash across all sessions
func artifactResolve(ledgerDir, hash string) int {
	pattern := filepath.Join(ledgerDir, "*", "artifacts", hash+".*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "Artifact not found: %s\n", hash)
		return 1
	}

	// Return the path
	fmt.Println(matches[0])
	return 0
}

// === HFS COMMANDS ===

// cmdHFS handles holographic filesystem structure commands
func cmdHFS(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	subCmd := "check"
	if len(args) > 0 {
		subCmd = args[0]
	}

	validator := filepath.Join(root, ".cog", "lib", "shell", "hfs-validate.sh")

	switch subCmd {
	case "check":
		// Run validator
		cmd := exec.Command(validator, "check")
		cmd.Dir = filepath.Join(root, ".cog")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			return 1
		}
		return 0

	case "json":
		// JSON output for programmatic use
		cmd := exec.Command(validator, "json")
		cmd.Dir = filepath.Join(root, ".cog")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
		return 0

	case "suggest":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog hfs suggest <filename>\n")
			return 1
		}
		cmd := exec.Command(validator, "suggest", args[1])
		cmd.Dir = filepath.Join(root, ".cog")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
		return 0

	case "governance":
		// Show the governance document
		govDoc := filepath.Join(root, ".cog", "ontology", "filesystem.cog.md")
		if _, err := os.Stat(govDoc); err != nil {
			fmt.Fprintf(os.Stderr, "Governance document not found: %s\n", govDoc)
			return 1
		}
		content, err := os.ReadFile(govDoc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading governance: %v\n", err)
			return 1
		}
		fmt.Print(string(content))
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown hfs command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog hfs {check|json|suggest|governance}\n")
		return 1
	}
}

// === LEDGER COMMANDS ===

// cmdLedger handles ledger subcommands
func cmdLedger(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	subCmd := "list"
	if len(args) > 0 {
		subCmd = args[0]
	}

	ledgerDir := filepath.Join(root, ".cog", "ledger")

	switch subCmd {
	case "list":
		// List all sessions with events
		return ledgerList(ledgerDir)

	case "get":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ledger get <session-id>\n")
			return 1
		}
		sessionID := args[1]
		return ledgerGet(ledgerDir, sessionID)

	case "append":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ledger append <session-id>\n")
			fmt.Fprintf(os.Stderr, "Reads event JSON from stdin\n")
			return 1
		}
		sessionID := args[1]
		return ledgerAppend(root, ledgerDir, sessionID)

	case "manifest":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ledger manifest <session-id> [--update key=value]\n")
			return 1
		}
		sessionID := args[1]
		return ledgerManifest(ledgerDir, sessionID, args[2:])

	default:
		fmt.Fprintf(os.Stderr, "Unknown ledger command: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Usage: cog ledger {list|get|append|manifest}\n")
		return 1
	}
}

// ledgerList lists all sessions with event counts
func ledgerList(ledgerDir string) int {
	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading ledger: %v\n", err)
		return 1
	}

	sessions := []map[string]interface{}{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessionID := e.Name()

		// Try to read events.jsonl (could be a symlink)
		eventsPath := filepath.Join(ledgerDir, sessionID, "events.jsonl")
		eventCount := 0
		var firstEvent, lastEvent time.Time

		// Count events and get timestamps
		if file, err := os.Open(eventsPath); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				eventCount++
				if eventCount == 1 {
					// Parse first event timestamp
					var evt map[string]interface{}
					if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
						if ts, ok := evt["ts"].(string); ok {
							firstEvent, _ = time.Parse(time.RFC3339, ts)
						}
					}
				}
				// Parse last event timestamp (keep updating)
				var evt map[string]interface{}
				if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
					if ts, ok := evt["ts"].(string); ok {
						lastEvent, _ = time.Parse(time.RFC3339, ts)
					}
				}
			}
			file.Close()
		}

		if eventCount > 0 {
			sessionInfo := map[string]interface{}{
				"session_id":  sessionID,
				"event_count": eventCount,
			}
			if !firstEvent.IsZero() {
				sessionInfo["first_event"] = firstEvent.Format(time.RFC3339)
			}
			if !lastEvent.IsZero() {
				sessionInfo["last_event"] = lastEvent.Format(time.RFC3339)
			}
			sessions = append(sessions, sessionInfo)
		}
	}

	// Sort by last event time (most recent first)
	sort.Slice(sessions, func(i, j int) bool {
		ti, _ := sessions[i]["last_event"].(string)
		tj, _ := sessions[j]["last_event"].(string)
		return ti > tj
	})

	output, _ := json.MarshalIndent(sessions, "", "  ")
	fmt.Println(string(output))
	return 0
}

// ledgerGet retrieves events for a specific session
func ledgerGet(ledgerDir, sessionID string) int {
	eventsPath := filepath.Join(ledgerDir, sessionID, "events.jsonl")
	manifestPath := filepath.Join(ledgerDir, sessionID, "manifest.json")

	// Read events
	events := []map[string]interface{}{}
	file, err := os.Open(eventsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading events: %v\n", err)
		return 1
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var evt map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &evt); err == nil {
			events = append(events, evt)
		}
	}

	// Read manifest (optional)
	manifest := map[string]interface{}{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		json.Unmarshal(data, &manifest)
	}

	// Combine into output
	result := map[string]interface{}{
		"session_id":  sessionID,
		"event_count": len(events),
		"events":      events,
	}
	if len(manifest) > 0 {
		result["manifest"] = manifest
	}

	output, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(output))
	return 0
}

// ledgerAppend appends an event to a session's events.jsonl
func ledgerAppend(root, ledgerDir, sessionID string) int {
	// Read event JSON from stdin
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintf(os.Stderr, "Error: no input on stdin\n")
		return 1
	}
	eventJSON := scanner.Bytes()

	// Validate JSON
	var evt map[string]interface{}
	if err := json.Unmarshal(eventJSON, &evt); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON: %v\n", err)
		return 1
	}

	// Ensure session directory exists
	sessionDir := filepath.Join(ledgerDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating session directory: %v\n", err)
		return 1
	}

	// Determine events path
	// First check if events.jsonl is a symlink
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	if info, err := os.Lstat(eventsPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			// It's a symlink - resolve it
			target, err := os.Readlink(eventsPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving symlink: %v\n", err)
				return 1
			}
			// If relative, make it absolute
			if !filepath.IsAbs(target) {
				eventsPath = filepath.Join(sessionDir, target)
			} else {
				eventsPath = target
			}
		}
	} else {
		// File doesn't exist - use direct path
		eventsPath = filepath.Join(sessionDir, "events.jsonl")
	}

	// Append event (with newline)
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening events file: %v\n", err)
		return 1
	}
	defer file.Close()

	if _, err := file.Write(append(eventJSON, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing event: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "Event appended to session %s\n", sessionID)
	return 0
}

// ledgerManifest gets or updates a session's manifest.json
func ledgerManifest(ledgerDir, sessionID string, args []string) int {
	manifestPath := filepath.Join(ledgerDir, sessionID, "manifest.json")

	// If no args, just read and display
	if len(args) == 0 {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("{}")
				return 0
			}
			fmt.Fprintf(os.Stderr, "Error reading manifest: %v\n", err)
			return 1
		}
		fmt.Print(string(data))
		return 0
	}

	// Parse update args (key=value pairs)
	manifest := map[string]interface{}{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		json.Unmarshal(data, &manifest)
	}

	// Update manifest with key=value pairs
	for _, arg := range args {
		if arg == "--update" {
			continue
		}
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Error: invalid update format: %s (expected key=value)\n", arg)
			return 1
		}
		key := parts[0]
		value := parts[1]

		// Try to parse value as JSON (for structured data)
		var jsonValue interface{}
		if err := json.Unmarshal([]byte(value), &jsonValue); err == nil {
			manifest[key] = jsonValue
		} else {
			// Store as string
			manifest[key] = value
		}
	}

	// Add updated timestamp
	manifest["updated_at"] = time.Now().UTC().Format(time.RFC3339)

	// Write manifest
	output, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeAtomic(manifestPath, append(output, '\n'), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "Manifest updated for session %s\n", sessionID)
	fmt.Println(string(output))
	return 0
}

func cmdRef(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cog ref <subcommand> [args...]\n")
		fmt.Fprintf(os.Stderr, "Subcommands: resolve, hash, verify, canonical, check, validate\n")
		return 1
	}

	subCmd := args[0]

	switch subCmd {
	case "resolve":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref resolve <uri>\n")
			return 1
		}
		uri := args[1]
		path, err := resolveURI(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Println(path)
		return 0

	case "hash":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref hash <uri>\n")
			return 1
		}
		uri := args[1]
		path, err := resolveURI(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Check if file/directory exists
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: path does not exist: %s\n", path)
			return 1
		}

		// Get blob hash
		blobHash, err := gitBlobHash(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Println(blobHash)
		return 0

	case "verify":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref verify <uri> <expected-hash>\n")
			return 1
		}
		uri := args[1]
		expectedHash := args[2]

		path, err := resolveURI(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Check if file/directory exists
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: path does not exist: %s\n", path)
			return 1
		}

		// Get blob hash
		blobHash, err := gitBlobHash(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		if blobHash == expectedHash {
			return 0
		}
		return 1

	case "canonical":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref canonical <uri>\n")
			return 1
		}
		uri := args[1]
		path, err := resolveURI(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Get canonical blob hash
		canonicalHash, err := gitCanonicalBlobHash(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Println(canonicalHash)
		return 0

	case "check":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref check <uri>\n")
			return 1
		}
		uri := args[1]
		path, err := resolveURI(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		// Check if file/directory exists
		exists := true
		if _, err := os.Stat(path); os.IsNotExist(err) {
			exists = false
		}

		result := map[string]interface{}{
			"uri":    uri,
			"path":   path,
			"exists": exists,
		}

		if exists {
			// Get current hash
			currentHash, err := gitBlobHash(path)
			if err == nil {
				result["current_hash"] = currentHash
			}

			// Get canonical hash
			canonicalHash, err := gitCanonicalBlobHash(path)
			if err == nil {
				result["canonical_hash"] = canonicalHash
				result["coherent"] = currentHash == canonicalHash
			} else {
				result["coherent"] = false
			}
		} else {
			result["current_hash"] = nil
			result["canonical_hash"] = nil
			result["coherent"] = false
		}

		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))

		if !exists {
			return 1
		}
		if coherent, ok := result["coherent"].(bool); ok && !coherent {
			return 1
		}
		return 0

	case "validate":
		// Full cogdoc validation with structural errors and navigational warnings
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: cog ref validate <cogdoc-path>\n")
			return 1
		}
		cogdocPath := args[1]

		result := validateCogdocFull(cogdocPath)
		output, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(output))

		if !result.Valid {
			return 1
		}
		if len(result.Warnings) > 0 {
			return 2 // Success but with warnings
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown ref subcommand: %s\n", subCmd)
		fmt.Fprintf(os.Stderr, "Subcommands: resolve, hash, verify, canonical, check, validate\n")
		return 1
	}
}

func cmdHelp() {
	fmt.Printf(`CogOS Kernel %s - cog-native workspace nucleus

Usage: cog <command> [args...]

Identity & Verification:
  init             Sign workspace identity
  verify [files]   Verify identity + validate cogdocs
  hash             Print git tree hash
  health           Check workspace health

Task Orchestration:
  run <task-name>           Execute task with dependency resolution
  tasks list                List all available tasks
  tasks graph <task-name>   Show dependency graph for task
  tasks show <task-name>    Show task definition
  cache stats               Show cache statistics
  cache list                List cached tasks
  cache clean               Clear task cache

Coherence:
  coherence check     Check current state against canonical
  coherence baseline  Record current state as baseline
  coherence drift     Show drift from canonical
  coherence status    Show last recorded state

Tree Building:
  tree build          Build complete cognitive state tree (all .cog/ files)

URI Resolution:
  ref resolve <uri>              Resolve URI to filesystem path
  ref hash <uri>                 Get git blob hash of URI target
  ref verify <uri> <hash>        Verify URI target matches hash
  ref canonical <uri>            Get canonical hash from holographic baseline
  ref check <uri>                Full coherence check with JSON output
  ref validate <cogdoc>          Validate cogdoc refs (errors + warnings)

CQL - Cognitive Query Language:
  read <uri>                     Read content from URI
  list <uri-namespace>           List all URIs in namespace
  query <pattern> [--flags]      Query cogdocs with filters
    --tags <tag1,tag2>           Filter by tags (all must match)
    --type <type>                Filter by type
    --status <status>            Filter by status

Artifact Management:
  artifact list [session]     List artifacts (default: most recent session)
  artifact get <hash>         Get artifact content by hash
  artifact sessions           List all sessions with artifacts
  artifact resolve <hash>     Find artifact path by hash

Timeline & Narrative (Explainability):
  events list [--session=X] [--type=Y]   List events with filters
  events show <event_id>                 Show detailed event information
  events explain <event_id>              Human-readable event explanation
  events query <query>                   Search events (type:X, artifact:Y)
  events narrative <session_id>          Generate narrative with event refs

Projection:
  projection project    Run full projection (.cog → .claude)
  projection validate   Validate existing projections
  projection drift      Detect drift in projections
  projection clean      Remove orphan projections
  projection state      Show projection state

Hook Dispatch:
  dispatch <event> [tool]   Dispatch event to handlers

Fleet (Agent Orchestration):
  fleet spawn <config> --task "..."   Spawn agent fleet
  fleet status [fleet_id]             Show fleet status
  fleet reap <fleet_id>               Collect results
  fleet logs <fleet_id>               View fleet logs
  fleet configs                       List available configs

Inference:
  infer [options] <prompt>  Run inference using shared engine
    --schema, -s <path>     JSON schema file for structured output
    --system <prompt>       System prompt
    --model, -m <model>     Model to use (default: claude)
    --json                  Output as JSON (for programmatic use)
    --origin <origin>       Tag request origin (default: "cli")
  serve [command] [--port]  OpenAI-compatible HTTP server (default port: 5100)
    (no command)            Run in foreground
    start                   Start as background daemon
    stop                    Stop the background daemon
    status                  Show server status and stats
    enable                  Register with launchd for auto-start
    disable                 Remove from launchd

Workspace (Multi-Workspace Management):
  workspace                  Show current workspace
  workspace list             List registered workspaces
  workspace current          Show current workspace with source
  workspace use <name>       Switch to named workspace
  workspace add <name> <path>   Register a workspace
  workspace remove <name>    Unregister a workspace
  ws                         Alias for workspace

Observability:
  tui, dashboard   Interactive TUI dashboard (TAA, coherence, events)
  ontology show    Display parsed ontology structure
  ontology types   List valid cogdoc types
  ontology relations  List valid reference relations

Info:
  version          Show kernel version
  help             Show this help

Examples:
  cog read cog://mem/episodic/decisions/cql-uri-first-interface
  cog list cog://mem/episodic/decisions/
  cog query cog://mem/ --tags raia

If no files given to verify, validates staged cogdocs.

Architecture:
  .cog/cog.go              - kernel source
  .cog/cog                 - kernel binary
  .cog/id.cog              - workspace identity
  .cog/run/                - ephemeral runtime state
  .cog/var/                - persistent runtime data
  .cog/hooks/hooks         - hook control file

The eigenform: source, binary, identity.
`, Version)
}

// === MAIN ===

// fixAndroidArgs handles a quirk where Android/Termux inserts the full binary
// path as os.Args[1], shifting actual arguments to higher indices.
// Detects this by checking if Args[1] looks like an absolute path to a cog binary.
func fixAndroidArgs() {
	if len(os.Args) >= 2 {
		arg1 := os.Args[1]
		// Check if arg1 looks like the binary path (absolute path containing "cog")
		if len(arg1) > 0 && arg1[0] == '/' && strings.Contains(arg1, "/cog") {
			// Shift args: remove the spurious path at index 1
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
}

// cmdSalience dispatches salience subcommands
func cmdSalience(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog salience {file|metrics|rank|hot|cold|stale|health}")
	}

	subcommand := args[0]
	subargs := args[1:]

	switch subcommand {
	case "file":
		return cmdSalienceFile(subargs)
	case "metrics":
		return cmdSalienceMetrics(subargs)
	case "rank":
		return cmdSalienceRank(subargs)
	case "hot":
		return cmdSalienceHot(subargs)
	case "cold":
		return cmdSalienceCold(subargs)
	case "stale":
		return cmdSalienceStale(subargs)
	case "health":
		return cmdSalienceHealth(subargs)
	default:
		return fmt.Errorf("unknown salience subcommand: %s", subcommand)
	}
}

// =============================================================================
// COORDINATION COMMANDS
// =============================================================================

func cmdCoordination(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord {claim|release|claimed|owner|claims|checkpoint|wait|handoff|handoffs|accept|broadcast|broadcasts}")
	}

	workspaceRoot := getWorkspaceRoot()

	subcommand := args[0]
	subargs := args[1:]

	switch subcommand {
	case "claim":
		return cmdCoordClaim(workspaceRoot, subargs)
	case "release":
		return cmdCoordRelease(workspaceRoot, subargs)
	case "claimed":
		return cmdCoordClaimed(workspaceRoot, subargs)
	case "owner":
		return cmdCoordOwner(workspaceRoot, subargs)
	case "claims":
		return cmdCoordClaims(workspaceRoot, subargs)
	case "checkpoint":
		return cmdCoordCheckpoint(workspaceRoot, subargs)
	case "wait":
		return cmdCoordWait(workspaceRoot, subargs)
	case "handoff":
		return cmdCoordHandoff(workspaceRoot, subargs)
	case "handoffs":
		return cmdCoordHandoffs(workspaceRoot, subargs)
	case "accept":
		return cmdCoordAccept(workspaceRoot, subargs)
	case "broadcast":
		return cmdCoordBroadcast(workspaceRoot, subargs)
	case "broadcasts":
		return cmdCoordBroadcasts(workspaceRoot, subargs)
	default:
		return fmt.Errorf("unknown coordination command: %s", subcommand)
	}
}

func cmdCoordClaim(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord claim <path> [reason]")
	}
	path := args[0]
	reason := "working"
	if len(args) > 1 {
		reason = strings.Join(args[1:], " ")
	}
	return CreateClaim(workspaceRoot, path, reason)
}

func cmdCoordRelease(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord release <path>")
	}
	return ReleaseClaim(workspaceRoot, args[0])
}

func cmdCoordClaimed(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord claimed <path>")
	}
	if IsClaimed(workspaceRoot, args[0]) {
		fmt.Println("claimed")
		return nil
	}
	fmt.Println("not claimed")
	return nil
}

func cmdCoordOwner(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord owner <path>")
	}
	owner, err := ClaimOwner(workspaceRoot, args[0])
	if err != nil {
		return err
	}
	fmt.Println(owner)
	return nil
}

func cmdCoordClaims(workspaceRoot string, args []string) error {
	claims, err := ListClaims(workspaceRoot)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		fmt.Printf("%s: %s (claimed by %s at %s)\n",
			claim.Agent, claim.Path, claim.Agent, claim.ClaimedAt.Format(time.RFC3339))
	}
	return nil
}

func cmdCoordCheckpoint(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord checkpoint <name>")
	}
	return CreateCheckpoint(workspaceRoot, args[0])
}

func cmdCoordWait(workspaceRoot string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: cog coord wait <checkpoint_name> <agent1> [agent2] ...")
	}
	name := args[0]
	agents := args[1:]
	timeout := 300 * time.Second
	if timeoutEnv := os.Getenv("COG_CHECKPOINT_TIMEOUT"); timeoutEnv != "" {
		if t, err := time.ParseDuration(timeoutEnv + "s"); err == nil {
			timeout = t
		}
	}
	return WaitCheckpoint(workspaceRoot, name, agents, timeout)
}

func cmdCoordHandoff(workspaceRoot string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: cog coord handoff <to_agent> <artifact> [message]")
	}
	toAgent := args[0]
	artifact := args[1]
	message := "handoff"
	if len(args) > 2 {
		message = strings.Join(args[2:], " ")
	}
	return CreateHandoff(workspaceRoot, toAgent, artifact, message)
}

func cmdCoordHandoffs(workspaceRoot string, args []string) error {
	agent := ""
	if len(args) > 0 {
		agent = args[0]
	}
	handoffs, err := ListHandoffs(workspaceRoot, agent)
	if err != nil {
		return err
	}
	for _, h := range handoffs {
		data, _ := json.MarshalIndent(h, "", "  ")
		fmt.Println(string(data))
	}
	return nil
}

func cmdCoordAccept(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord accept <handoff_file>")
	}
	return AcceptHandoff(workspaceRoot, args[0])
}

func cmdCoordBroadcast(workspaceRoot string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: cog coord broadcast <channel> <message>")
	}
	channel := args[0]
	message := strings.Join(args[1:], " ")
	return CreateBroadcast(workspaceRoot, channel, message)
}

func cmdCoordBroadcasts(workspaceRoot string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog coord broadcasts <channel> [seconds_ago]")
	}
	channel := args[0]
	since := 3600 * time.Second // default 1 hour
	if len(args) > 1 {
		if seconds, err := time.ParseDuration(args[1] + "s"); err == nil {
			since = seconds
		}
	}
	broadcasts, err := ListBroadcasts(workspaceRoot, channel, since)
	if err != nil {
		return err
	}
	for _, b := range broadcasts {
		data, _ := json.MarshalIndent(b, "", "  ")
		fmt.Println(string(data))
		fmt.Println("---")
	}
	return nil
}

func cmdFrontmatter(args []string) error {
	if len(args) == 0 {
		fmt.Println("Usage: cog frontmatter {generate|apply|fix|cogify|migrate} [args]")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  generate <path> <title>     Generate frontmatter for path and title")
		fmt.Println("  apply <file> [--force]      Add or fix frontmatter on a file")
		fmt.Println("  fix <file>                  Fix existing frontmatter (infer missing fields)")
		fmt.Println("  cogify <file>               Rename .md to .cog.md if valid cogdoc")
		fmt.Println("  migrate [--dry-run] [sector]  Full migration (fix + cogify)")
		return nil
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		return err
	}

	switch args[0] {
	case "generate":
		if len(args) < 3 {
			return fmt.Errorf("usage: cog frontmatter generate <path> <title>")
		}
		path := args[1]
		title := strings.Join(args[2:], " ")
		fmt.Print(GenerateFrontmatter(path, title))
		return nil

	case "apply":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog frontmatter apply <file> [--force]")
		}
		file := args[1]
		force := len(args) > 2 && args[2] == "--force"
		return ApplyFrontmatter(file, force)

	case "fix":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog frontmatter fix <file>")
		}
		return FixExistingFrontmatter(args[1])

	case "cogify":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog frontmatter cogify <file>")
		}
		newPath, err := CogifyFile(args[1])
		if err != nil {
			return err
		}
		if newPath != args[1] {
			fmt.Printf("Renamed: %s -> %s\n", args[1], newPath)
		} else {
			fmt.Printf("Unchanged: %s\n", args[1])
		}
		return nil

	case "migrate":
		baseDir := filepath.Join(root, ".cog", "mem")
		var sector *MemorySector
		dryRun := false

		for i := 1; i < len(args); i++ {
			if args[i] == "--dry-run" {
				dryRun = true
			} else if args[i] == "semantic" || args[i] == "episodic" || args[i] == "procedural" || args[i] == "reflective" {
				s := MemorySector(args[i])
				sector = &s
			}
		}

		return MigrateFrontmatter(baseDir, sector, dryRun)

	default:
		return fmt.Errorf("unknown frontmatter subcommand: %s", args[0])
	}
}

func cmdMemory(args []string) error {
	if len(args) == 0 {
		fmt.Println("Usage: cog memory {search|list|read|write|append|stats} [args]")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  search <query> [--deep [depth]] [--raw]  Search memory with optional deep waypoint traversal")
		fmt.Println("  list <sector> [subdir]                    List documents in a sector")
		fmt.Println("  read <path>                               Read a memory document")
		fmt.Println("  write <path> <title> [content]            Create a new memory document")
		fmt.Println("  append <path> <content>                   Append to existing document")
		fmt.Println("  stats                                     Show memory statistics")
		return nil
	}

	root, _, err := ResolveWorkspace()
	if err != nil {
		return err
	}

	switch args[0] {
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog memory search <query> [--deep [depth]] [--raw]")
		}

		query := args[1]
		deepMode := false
		deepDepth := 2
		decayFactor := 0.7
		rawMode := false

		// Parse flags
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--deep":
				deepMode = true
				// Check if next arg is a number
				if i+1 < len(args) {
					if depth, err := fmt.Sscanf(args[i+1], "%d", &deepDepth); err == nil && depth == 1 {
						i++ // Consume depth arg
					}
				}
			case "--raw":
				rawMode = true
			}
		}

		results, err := MemorySearch(root, query, deepMode, deepDepth, decayFactor, rawMode)
		if err != nil {
			return err
		}

		// Display results
		if len(results) == 0 {
			fmt.Printf("No matches found for: %s\n", query)
			return nil
		}

		directMatches := 0
		waypointMatches := 0
		for _, r := range results {
			if r.SourceType == "direct" {
				directMatches++
			} else {
				waypointMatches++
			}
		}

		if deepMode {
			fmt.Printf("Query: %s (deep search, depth=%d)\n", query, deepDepth)
			fmt.Printf("Found: %d documents (%d direct + %d via waypoints)\n\n", len(results), directMatches, waypointMatches)
		} else {
			showing := len(results)
			if showing > 20 {
				showing = 20
			}
			fmt.Printf("Query: %s\n", query)
			fmt.Printf("Found: %d documents (showing top %d, ranked by relevance)\n\n", len(results), showing)
		}

		// Group by depth if deep mode
		currentDepth := -1
		count := 0
		maxDisplay := 20

		for _, result := range results {
			if count >= maxDisplay {
				break
			}

			// Print depth header if changed (deep mode only)
			if deepMode && result.Depth != currentDepth {
				if currentDepth != -1 {
					fmt.Println()
				}
				currentDepth = result.Depth
				if result.Depth == 0 {
					fmt.Println("## Direct Matches")
				} else {
					fmt.Printf("## Via Waypoints (depth %d)\n", result.Depth)
				}
				fmt.Println()
			}

			// Print result
			if result.SourceType == "waypoint" {
				fmt.Printf("  %.2f  %s  [activation: %.2f]\n", result.Score, result.Path, result.Score)
			} else {
				fmt.Printf("  %.2f  %s\n", result.Score, result.Path)
			}

			fmt.Printf("        Title: %s | Type: %s\n", result.Title, result.Type)

			// Show score breakdown for top results
			if count < 3 {
				if result.SourceType == "waypoint" {
					fmt.Printf("        [depth: %d, activation via waypoints]\n", result.Depth)
				} else {
					fmt.Printf("        [mem_strength: %.2f, salience: %.2f]\n", result.MemoryStrength, result.Salience)
				}
			}

			fmt.Println()
			count++
		}

		return nil

	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog memory list <sector> [subdir]")
		}
		sector := args[1]
		subdir := ""
		if len(args) > 2 {
			subdir = args[2]
		}

		results, err := MemoryList(root, sector, subdir)
		if err != nil {
			return err
		}

		fmt.Printf("Sector: %s", sector)
		if subdir != "" {
			fmt.Printf("/%s", subdir)
		}
		fmt.Printf("\nFound: %d documents\n\n", len(results))

		for _, result := range results {
			relPath, _ := filepath.Rel(root, result.Path)
			fmt.Printf("  %s\n", relPath)
			fmt.Printf("    Title: %s\n", result.Title)
			fmt.Printf("    Type: %s\n\n", result.Type)
		}

		return nil

	case "read":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog memory read <path>")
		}
		content, err := MemoryRead(root, args[1])
		if err != nil {
			return err
		}
		fmt.Print(content)
		return nil

	case "write":
		if len(args) < 3 {
			return fmt.Errorf("usage: cog memory write <path> <title> [content]")
		}
		path := args[1]
		title := args[2]
		content := ""
		if len(args) > 3 {
			content = strings.Join(args[3:], " ")
		}
		return MemoryWrite(root, path, title, content)

	case "append":
		if len(args) < 3 {
			return fmt.Errorf("usage: cog memory append <path> <content>")
		}
		path := args[1]
		content := strings.Join(args[2:], " ")
		return MemoryAppend(root, path, content)

	case "stats":
		return MemoryStats(root)

	default:
		return fmt.Errorf("unknown memory subcommand: %s", args[0])
	}
}

// === WORKSPACE COMMAND ===

// cmdWorkspace handles the workspace subcommand for multi-workspace management
func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		// Default: show current workspace
		return cmdWorkspaceCurrent()
	}

	switch args[0] {
	case "list", "ls":
		return cmdWorkspaceList()
	case "current":
		return cmdWorkspaceCurrent()
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog workspace use <name>")
		}
		return cmdWorkspaceUse(args[1])
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: cog workspace add <name> <path>")
		}
		return cmdWorkspaceAdd(args[1], args[2])
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: cog workspace remove <name>")
		}
		return cmdWorkspaceRemove(args[1])
	default:
		return fmt.Errorf("unknown workspace subcommand: %s\nUsage: cog workspace [list|current|use|add|remove]", args[0])
	}
}

// cmdWorkspaceList shows all registered workspaces
func cmdWorkspaceList() error {
	config, err := loadGlobalConfig()
	if err != nil {
		return err
	}

	if len(config.Workspaces) == 0 {
		fmt.Println("No workspaces registered.")
		fmt.Println("Run 'cog workspace add <name> <path>' to register a workspace.")
		return nil
	}

	// Print header
	fmt.Printf("%-2s %-16s %s\n", "", "NAME", "PATH")

	// Get workspace names and sort them
	names := make([]string, 0, len(config.Workspaces))
	for name := range config.Workspaces {
		names = append(names, name)
	}
	sort.Strings(names)

	// Print workspaces
	for _, name := range names {
		ws := config.Workspaces[name]
		marker := "  "
		if name == config.CurrentWorkspace {
			marker = "* "
		}
		fmt.Printf("%s%-16s %s\n", marker, name, ws.Path)
	}

	return nil
}

// cmdWorkspaceCurrent shows the current workspace and how it was resolved
func cmdWorkspaceCurrent() error {
	root, source, err := ResolveWorkspace()
	if err != nil {
		return err
	}

	config, _ := loadGlobalConfig()
	wsName := "(local)"
	if source == "global" && config != nil {
		wsName = config.CurrentWorkspace
	} else if source == "env" {
		wsName = os.Getenv("COG_WORKSPACE")
	} else if source == "explicit" {
		wsName = "(explicit)"
	}

	fmt.Printf("Workspace: %s\n", wsName)
	fmt.Printf("Path:      %s\n", root)
	fmt.Printf("Source:    %s\n", source)

	return nil
}

// cmdWorkspaceUse switches to a named workspace
func cmdWorkspaceUse(name string) error {
	config, err := loadGlobalConfig()
	if err != nil {
		return err
	}

	ws, ok := config.Workspaces[name]
	if !ok {
		return fmt.Errorf("unknown workspace: %s\nRun 'cog workspace list' to see available workspaces.", name)
	}

	// Validate workspace path still exists and is valid
	cogDir := filepath.Join(ws.Path, ".cog")
	if _, err := os.Stat(cogDir); err != nil {
		return fmt.Errorf("workspace '%s' path invalid: %s\n.cog/ directory not found. Run 'cog workspace remove %s' to unregister.", name, ws.Path, name)
	}

	config.CurrentWorkspace = name
	if err := saveGlobalConfig(config); err != nil {
		return err
	}

	fmt.Printf("Switched to workspace: %s\n", name)
	fmt.Printf("Path: %s\n", ws.Path)

	return nil
}

// cmdWorkspaceAdd registers a new workspace
func cmdWorkspaceAdd(name, path string) error {
	// Resolve to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	// Verify it's a valid workspace
	cogDir := filepath.Join(absPath, ".cog")
	if info, err := os.Stat(cogDir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a valid cog workspace (no .cog/ directory)", absPath)
	}

	config, err := loadGlobalConfig()
	if err != nil {
		return err
	}

	// Try to load workspace name from .cog/config.yaml
	wsName := name
	wsConfigPath := filepath.Join(cogDir, "config.yaml")
	if data, err := os.ReadFile(wsConfigPath); err == nil {
		var wsConfig struct {
			Workspace struct {
				Name string `yaml:"name"`
			} `yaml:"workspace"`
		}
		if yaml.Unmarshal(data, &wsConfig) == nil && wsConfig.Workspace.Name != "" {
			wsName = wsConfig.Workspace.Name
		}
	}

	config.Workspaces[name] = &WorkspaceEntry{
		Path: absPath,
		Name: wsName,
	}

	// If this is the first workspace, make it current
	if config.CurrentWorkspace == "" {
		config.CurrentWorkspace = name
	}

	if err := saveGlobalConfig(config); err != nil {
		return err
	}

	fmt.Printf("Added workspace: %s\n", name)
	fmt.Printf("Path: %s\n", absPath)
	if config.CurrentWorkspace == name {
		fmt.Println("(set as current workspace)")
	}

	return nil
}

// cmdWorkspaceRemove unregisters a workspace
func cmdWorkspaceRemove(name string) error {
	config, err := loadGlobalConfig()
	if err != nil {
		return err
	}

	if _, ok := config.Workspaces[name]; !ok {
		return fmt.Errorf("workspace not found: %s", name)
	}

	delete(config.Workspaces, name)

	// Clear current if we just removed it
	if config.CurrentWorkspace == name {
		config.CurrentWorkspace = ""
		fmt.Println("Note: removed current workspace; no workspace is now active.")
	}

	if err := saveGlobalConfig(config); err != nil {
		return err
	}

	fmt.Printf("Removed workspace: %s\n", name)

	return nil
}

func main() {
	// Handle Android/Termux argument quirk
	fixAndroidArgs()

	if len(os.Args) < 2 {
		cmdHelp()
		os.Exit(0)
	}

	var code int
	switch os.Args[1] {
	case "init":
		code = cmdInit()
	case "verify":
		code = cmdVerify(os.Args[2:])
	case "hash":
		code = cmdHash()
	case "coherence":
		code = cmdCoherence(os.Args[2:])
	case "ontology":
		code = cmdOntology(os.Args[2:])
	case "tui", "dashboard":
		code = cmdTUI(os.Args[2:])
	case "tree":
		code = cmdTree(os.Args[2:])
	case "ref":
		code = cmdRef(os.Args[2:])
	case "projection":
		code = cmdProjection(os.Args[2:])
	case "dispatch":
		code = cmdDispatch(os.Args[2:])
	case "emit":
		code = cmdEmit(os.Args[2:])
	case "run":
		code = cmdRun(os.Args[2:])
	case "tasks":
		code = cmdTasks(os.Args[2:])
	case "cache":
		code = cmdCache(os.Args[2:])
	case "fleet":
		code = cmdFleet(os.Args[2:])
	case "infer":
		code = cmdInfer(os.Args[2:])
	case "serve":
		code = cmdServe(os.Args[2:])
	case "health":
		code = cmdHealth(os.Args[2:])
	case "read":
		code = cmdReadURI(os.Args[2:])
	case "list":
		code = cmdListURI(os.Args[2:])
	case "query":
		code = cmdQueryURI(os.Args[2:])
	case "artifact":
		code = cmdArtifact(os.Args[2:])
	case "ledger":
		code = cmdLedger(os.Args[2:])
	case "hfs":
		code = cmdHFS(os.Args[2:])
	case "events":
		if err := cmdEvents(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "session":
		if err := cmdSession(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "salience":
		if err := cmdSalience(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "coord", "coordination":
		if err := cmdCoordination(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "frontmatter", "fm":
		if err := cmdFrontmatter(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "memory":
		if err := cmdMemory(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "constellation":
		if err := cmdConstellation(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "loop":
		if err := cmdLoop(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "workspace", "ws":
		if err := cmdWorkspace(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			code = 1
		}
	case "version", "-v", "--version":
		code = cmdVersion()
	case "help", "-h", "--help":
		cmdHelp()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		fmt.Fprintf(os.Stderr, "Run 'cog help' for usage\n")
		code = 1
	}
	os.Exit(code)
}
