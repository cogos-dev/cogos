// Package sdk provides cogdoc indexing for Cognitive Query Language (CQL).
//
// The index provides fast lookups by URI, type, tag, status, and reference graph.
// This is ported from the kernel's BuildCogdocIndex functionality.
package sdk

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cogos-dev/cogos/sdk/types"
	"gopkg.in/yaml.v3"
)

// TypedRef represents a reference with relationship type.
// Mirrors the kernel's TypedRef structure.
type TypedRef struct {
	URI string `yaml:"uri" json:"uri"`
	Rel string `yaml:"rel,omitempty" json:"rel,omitempty"` // implements, extends, supersedes, describes, requires, suggests
}

// ValidRefRelations defines the allowed relationship types for typed refs.
var ValidRefRelations = map[string]bool{
	"refs":       true, // default
	"implements": true,
	"extends":    true,
	"supersedes": true,
	"describes":  true,
	"requires":   true,
	"suggests":   true,
}

// IndexedCogdoc represents a cogdoc with full metadata for indexing.
type IndexedCogdoc struct {
	URI        string            `json:"uri"`         // cog://mem/episodic/decisions/foo
	Path       string            `json:"path"`        // .cog/mem/episodic/decisions/foo.cog.md
	Type       types.CogdocType  `json:"type"`        // decision, session, guide, etc.
	ID         string            `json:"id"`          // kebab-case identifier
	Title      string            `json:"title"`       // Human-readable title
	Created    string            `json:"created"`     // YYYY-MM-DD
	Status     string            `json:"status"`      // proposed, active, deprecated, etc.
	Tags       []string          `json:"tags"`        // Tags for categorization
	Refs       []TypedRef        `json:"refs"`        // Structural references from frontmatter
	InlineRefs []string          `json:"inline_refs"` // Navigational references from content
}

// CogdocIndex is an in-memory index of all cogdocs.
// This mirrors the kernel's CogdocIndex structure.
type CogdocIndex struct {
	// ByURI maps URI to cogdoc
	ByURI map[string]*IndexedCogdoc

	// ByType maps type to cogdocs of that type
	ByType map[types.CogdocType][]*IndexedCogdoc

	// ByTag maps tag to cogdocs with that tag
	ByTag map[string][]*IndexedCogdoc

	// ByStatus maps status to cogdocs with that status
	ByStatus map[string][]*IndexedCogdoc

	// RefGraph maps URI to its forward refs
	RefGraph map[string][]TypedRef

	// InverseRefs maps URI to URIs that reference it (backward refs)
	InverseRefs map[string][]string
}

// cogURIPattern matches cog:// URIs in content
var cogURIPattern = regexp.MustCompile(`cog://[a-zA-Z0-9/_-]+`)

// BuildIndex scans .cog/mem/ and builds an in-memory index of all cogdocs.
// This is the SDK equivalent of the kernel's BuildCogdocIndex.
func (k *Kernel) BuildIndex() (*CogdocIndex, error) {
	idx := &CogdocIndex{
		ByURI:       make(map[string]*IndexedCogdoc),
		ByType:      make(map[types.CogdocType][]*IndexedCogdoc),
		ByTag:       make(map[string][]*IndexedCogdoc),
		ByStatus:    make(map[string][]*IndexedCogdoc),
		RefGraph:    make(map[string][]TypedRef),
		InverseRefs: make(map[string][]string),
	}

	memoryDir := k.MemoryDir()

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
		indexed, err := parseCogdocForIndex(path, k.CogDir())
		if err != nil {
			return nil // Skip unparseable files
		}

		// Add to ByURI index
		idx.ByURI[indexed.URI] = indexed

		// Add to ByType index
		if indexed.Type != "" {
			idx.ByType[indexed.Type] = append(idx.ByType[indexed.Type], indexed)
		}

		// Add to ByTag index
		for _, tag := range indexed.Tags {
			idx.ByTag[tag] = append(idx.ByTag[tag], indexed)
		}

		// Add to ByStatus index
		if indexed.Status != "" {
			idx.ByStatus[indexed.Status] = append(idx.ByStatus[indexed.Status], indexed)
		}

		// Add to RefGraph (forward refs)
		if len(indexed.Refs) > 0 {
			idx.RefGraph[indexed.URI] = indexed.Refs
		}

		// Build inverse ref graph
		for _, ref := range indexed.Refs {
			idx.InverseRefs[ref.URI] = append(idx.InverseRefs[ref.URI], indexed.URI)
		}

		return nil
	})

	return idx, err
}

// parseCogdocForIndex parses a cogdoc file and extracts metadata for indexing.
func parseCogdocForIndex(path, cogRoot string) (*IndexedCogdoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)

	// Parse frontmatter
	if !strings.HasPrefix(content, "---\n") {
		return nil, ErrValidation
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return nil, ErrValidation
	}
	fmContent := content[4 : 4+end]
	bodyContent := content[4+end+4:]

	// Parse YAML frontmatter
	var doc struct {
		Type    string      `yaml:"type"`
		ID      string      `yaml:"id"`
		Title   string      `yaml:"title"`
		Created string      `yaml:"created"`
		Status  string      `yaml:"status,omitempty"`
		Tags    []string    `yaml:"tags,omitempty"`
		Refs    interface{} `yaml:"refs,omitempty"`
	}
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		return nil, err
	}

	// Convert path to URI
	// .cog/mem/episodic/decisions/foo.cog.md -> cog://mem/episodic/decisions/foo
	relPath, err := filepath.Rel(cogRoot, path)
	if err != nil {
		return nil, err
	}
	// Remove .cog.md extension
	relPath = strings.TrimSuffix(relPath, ".cog.md")
	uri := "cog://" + relPath

	// Parse refs
	refs := parseRefs(doc.Refs)

	// Extract inline refs
	inlineRefs := extractInlineRefs(bodyContent)

	return &IndexedCogdoc{
		URI:        uri,
		Path:       path,
		Type:       types.CogdocType(doc.Type),
		ID:         doc.ID,
		Title:      doc.Title,
		Created:    doc.Created,
		Status:     doc.Status,
		Tags:       doc.Tags,
		Refs:       refs,
		InlineRefs: inlineRefs,
	}, nil
}

// parseRefs extracts refs from cogdoc, handling both simple and typed forms.
func parseRefs(refs interface{}) []TypedRef {
	if refs == nil {
		return nil
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

	return result
}

// extractInlineRefs finds cog:// URIs in markdown content (navigational refs).
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

// Query methods on CogdocIndex

// GetByURI returns a cogdoc by its URI.
func (idx *CogdocIndex) GetByURI(uri string) *IndexedCogdoc {
	return idx.ByURI[uri]
}

// GetByType returns all cogdocs of a given type.
func (idx *CogdocIndex) GetByType(t types.CogdocType) []*IndexedCogdoc {
	return idx.ByType[t]
}

// GetByTag returns all cogdocs with a given tag.
func (idx *CogdocIndex) GetByTag(tag string) []*IndexedCogdoc {
	return idx.ByTag[tag]
}

// GetByStatus returns all cogdocs with a given status.
func (idx *CogdocIndex) GetByStatus(status string) []*IndexedCogdoc {
	return idx.ByStatus[status]
}

// GetRefs returns the forward refs for a URI.
func (idx *CogdocIndex) GetRefs(uri string) []TypedRef {
	return idx.RefGraph[uri]
}

// GetBackrefs returns URIs that reference the given URI.
func (idx *CogdocIndex) GetBackrefs(uri string) []string {
	return idx.InverseRefs[uri]
}

// Query performs a filtered query on the index.
type IndexQuery struct {
	// URIPrefix filters to URIs starting with this prefix
	URIPrefix string

	// Type filters to cogdocs of this type
	Type types.CogdocType

	// Status filters to cogdocs with this status
	Status string

	// Tags filters to cogdocs containing ALL these tags
	Tags []string

	// Limit caps the number of results (0 = unlimited)
	Limit int
}

// Query executes a query against the index.
func (idx *CogdocIndex) Query(q *IndexQuery) []*IndexedCogdoc {
	var candidates []*IndexedCogdoc

	// Start with all cogdocs or filter by URI prefix
	if q.URIPrefix != "" {
		prefix := strings.TrimSuffix(q.URIPrefix, "/")
		for uri, doc := range idx.ByURI {
			if strings.HasPrefix(uri, prefix) {
				candidates = append(candidates, doc)
			}
		}
	} else {
		for _, doc := range idx.ByURI {
			candidates = append(candidates, doc)
		}
	}

	// Apply filters
	var results []*IndexedCogdoc

	for _, doc := range candidates {
		// Filter by type
		if q.Type != "" && doc.Type != q.Type {
			continue
		}

		// Filter by status
		if q.Status != "" && doc.Status != q.Status {
			continue
		}

		// Filter by tags (all tags must match)
		if len(q.Tags) > 0 {
			hasAllTags := true
			for _, filterTag := range q.Tags {
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

	// Sort results by URI for consistent output
	sort.Slice(results, func(i, j int) bool {
		return results[i].URI < results[j].URI
	})

	// Apply limit
	if q.Limit > 0 && len(results) > q.Limit {
		results = results[:q.Limit]
	}

	return results
}

// URIs returns all indexed URIs sorted.
func (idx *CogdocIndex) URIs() []string {
	uris := make([]string, 0, len(idx.ByURI))
	for uri := range idx.ByURI {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// Types returns all types present in the index.
func (idx *CogdocIndex) Types() []types.CogdocType {
	typeList := make([]types.CogdocType, 0, len(idx.ByType))
	for t := range idx.ByType {
		typeList = append(typeList, t)
	}
	sort.Slice(typeList, func(i, j int) bool {
		return string(typeList[i]) < string(typeList[j])
	})
	return typeList
}

// Tags returns all tags present in the index.
func (idx *CogdocIndex) Tags() []string {
	tags := make([]string, 0, len(idx.ByTag))
	for tag := range idx.ByTag {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// Statuses returns all statuses present in the index.
func (idx *CogdocIndex) Statuses() []string {
	statuses := make([]string, 0, len(idx.ByStatus))
	for status := range idx.ByStatus {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	return statuses
}

// Count returns the total number of indexed cogdocs.
func (idx *CogdocIndex) Count() int {
	return len(idx.ByURI)
}
