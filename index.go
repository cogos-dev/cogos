// index.go — CogDoc index for CogOS v3
//
// BuildIndex walks .cog/mem/ and constructs an in-memory lookup table for all
// CogDoc files (Markdown files with YAML frontmatter).  The index provides
// O(1) lookups by URI, type, tag, and status, plus forward and inverse
// reference graphs for coherence validation.
//
// Index lifecycle:
//  1. Built on startup (best-effort; errors are non-fatal).
//  2. Rebuilt by Process.runConsolidation() on each consolidation tick.
//  3. Served via /v1/resolve for URI resolution queries.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── Document model ────────────────────────────────────────────────────────────

// DocRef is an explicit typed reference declared in a CogDoc's frontmatter.
type DocRef struct {
	// URI is the target cog:// URI.
	URI string `yaml:"uri"`
	// Rel is the relationship label (e.g. "related", "supersedes", "depends-on").
	Rel string `yaml:"rel"`
}

// IndexedCogdoc is a lightweight representation of a single CogDoc file,
// containing only the metadata needed for index lookups and coherence checks.
type IndexedCogdoc struct {
	// URI is the canonical cog:// address of this document.
	URI string
	// Path is the absolute filesystem path.
	Path string
	// ID is the value of the `id:` frontmatter field (may be empty).
	ID string
	// Title is the value of the `title:` frontmatter field.
	Title string
	// Type is the value of the `type:` frontmatter field (e.g. "insight").
	Type string
	// Tags is the value of the `tags:` frontmatter field.
	Tags []string
	// Status is the value of the `status:` frontmatter field (e.g. "active").
	Status string
	// Created is the value of the `created:` frontmatter field (string, any format).
	Created string
	// Refs are the explicit `refs:` entries in the frontmatter.
	Refs []DocRef
	// InlineRefs are cog:// URIs found in the document body (extracted by regex).
	InlineRefs []string
}

// ── Index ─────────────────────────────────────────────────────────────────────

// CogDocIndex is the complete in-memory catalogue of the memory corpus.
// All fields are populated by BuildIndex; nil maps indicate an empty corpus.
type CogDocIndex struct {
	// ByURI maps canonical cog:// URI → document.
	ByURI map[string]*IndexedCogdoc
	// ByType maps type string → all documents of that type.
	ByType map[string][]*IndexedCogdoc
	// ByTag maps tag string → all documents carrying that tag.
	ByTag map[string][]*IndexedCogdoc
	// ByStatus maps status string → all documents with that status.
	ByStatus map[string][]*IndexedCogdoc
	// RefGraph maps source URI → its explicit DocRef targets.
	RefGraph map[string][]DocRef
	// InverseRefs maps target URI → list of source URIs that reference it.
	InverseRefs map[string][]string
}

// ── Builder ───────────────────────────────────────────────────────────────────

// BuildIndex walks .cog/mem/ under workspaceRoot, parses CogDoc frontmatter,
// and returns a fully populated CogDocIndex.
//
// Files with unparseable frontmatter are included with empty metadata (best-effort).
// If .cog/mem/ does not exist, an empty index is returned without error.
func BuildIndex(workspaceRoot string) (*CogDocIndex, error) {
	idx := &CogDocIndex{
		ByURI:       make(map[string]*IndexedCogdoc),
		ByType:      make(map[string][]*IndexedCogdoc),
		ByTag:       make(map[string][]*IndexedCogdoc),
		ByStatus:    make(map[string][]*IndexedCogdoc),
		RefGraph:    make(map[string][]DocRef),
		InverseRefs: make(map[string][]string),
	}

	memDir := filepath.Join(workspaceRoot, ".cog", "mem")
	if _, err := os.Stat(memDir); os.IsNotExist(err) {
		return idx, nil
	}

	err := filepath.Walk(memDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // skip unreadable dirs silently
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		doc := indexFile(workspaceRoot, path)
		if doc == nil {
			return nil
		}

		idx.ByURI[doc.URI] = doc
		if doc.Type != "" {
			idx.ByType[doc.Type] = append(idx.ByType[doc.Type], doc)
		}
		for _, tag := range doc.Tags {
			idx.ByTag[tag] = append(idx.ByTag[tag], doc)
		}
		if doc.Status != "" {
			idx.ByStatus[doc.Status] = append(idx.ByStatus[doc.Status], doc)
		}
		if len(doc.Refs) > 0 {
			idx.RefGraph[doc.URI] = doc.Refs
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	buildInverseRefs(idx)
	return idx, nil
}

// indexFile reads a single file and constructs its IndexedCogdoc.
// Returns nil if the file cannot be read or the URI cannot be resolved.
func indexFile(workspaceRoot, path string) *IndexedCogdoc {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)

	uri, err := PathToURI(workspaceRoot, path)
	if err != nil {
		// File is in .cog/mem/ but PathToURI failed — use a best-effort URI.
		rel, _ := filepath.Rel(workspaceRoot, path)
		uri = "cog://mem/" + filepath.ToSlash(strings.TrimPrefix(rel, ".cog/mem/"))
	}

	fm, body := parseCogdocFrontmatter(content)

	return &IndexedCogdoc{
		URI:        uri,
		Path:       path,
		ID:         fm.ID,
		Title:      fm.Title,
		Type:       fm.Type,
		Tags:       fm.Tags,
		Status:     fm.Status,
		Created:    fm.Created,
		Refs:       fm.Refs,
		InlineRefs: ExtractInlineRefs(body),
	}
}

// buildInverseRefs populates idx.InverseRefs from the explicit RefGraph.
func buildInverseRefs(idx *CogDocIndex) {
	for sourceURI, refs := range idx.RefGraph {
		for _, ref := range refs {
			idx.InverseRefs[ref.URI] = append(idx.InverseRefs[ref.URI], sourceURI)
		}
	}
}

// ── Frontmatter parser ────────────────────────────────────────────────────────

// cogdocFrontmatter holds the YAML frontmatter fields recognised by the index.
type cogdocFrontmatter struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Type        string   `yaml:"type"`
	Tags        []string `yaml:"tags"`
	Status      string   `yaml:"status"`
	Created     string   `yaml:"created"`
	Refs        []DocRef `yaml:"refs"`
}

// parseCogdocFrontmatter splits a Markdown document into its YAML frontmatter
// and body.  If no valid frontmatter block is found the returned struct is
// zero-valued and body equals the full content.
//
// A frontmatter block begins with "---\n" on the first line and ends at the
// next "---\n" line.
func parseCogdocFrontmatter(content string) (cogdocFrontmatter, string) {
	var fm cogdocFrontmatter

	skipBytes := 0
	switch {
	case strings.HasPrefix(content, "---\n"):
		skipBytes = 4
	case strings.HasPrefix(content, "---\r\n"):
		skipBytes = 5
	default:
		return fm, content
	}

	rest := content[skipBytes:]
	yamlBlock, tail, found := strings.Cut(rest, "\n---")
	if !found {
		return fm, content
	}
	// Strip the line ending + blank lines after "---" so body starts at
	// the first real content line.
	body := strings.TrimLeft(tail, "\r\n")

	_ = yaml.Unmarshal([]byte(yamlBlock), &fm)
	return fm, body
}
