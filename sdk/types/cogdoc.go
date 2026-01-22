// Package types contains the projection target types for SDK resources.
//
// These types are what URIs resolve into. They can be imported
// independently for use in libraries that need to accept SDK types
// without depending on the full SDK.
package types

import "time"

// CogdocType is the type enum for cogdocs.
type CogdocType string

// Core structural types
const (
	CogdocTypeIdentity  CogdocType = "identity"
	CogdocTypeOntology  CogdocType = "ontology"
	CogdocTypeMemory    CogdocType = "memory"
	CogdocTypeSchema    CogdocType = "schema"
	CogdocTypeDecision  CogdocType = "decision"
	CogdocTypeSession   CogdocType = "session"
	CogdocTypeHandoff   CogdocType = "handoff"
	CogdocTypeGuide     CogdocType = "guide"
	CogdocTypeADR       CogdocType = "adr"
	CogdocTypeKnowledge CogdocType = "knowledge"
)

// Extended semantic types (common in memory)
const (
	CogdocTypeNote              CogdocType = "note"
	CogdocTypeTerm              CogdocType = "term"
	CogdocTypeSpec              CogdocType = "spec"
	CogdocTypeClaim             CogdocType = "claim"
	CogdocTypeInsight           CogdocType = "insight"
	CogdocTypeArchitecture      CogdocType = "architecture"
	CogdocTypeResearchSynthesis CogdocType = "research_synthesis"
	CogdocTypeObservation       CogdocType = "observation"
	CogdocTypeAssessment        CogdocType = "assessment"
	CogdocTypeProcedural        CogdocType = "procedural"
	CogdocTypeSummary           CogdocType = "summary"
	CogdocTypeSpecification     CogdocType = "specification"
)

// ValidCogdocTypes is the set of all valid cogdoc types.
// This mirrors the kernel's validCogdocTypes map.
var ValidCogdocTypes = map[CogdocType]bool{
	// Core structural types
	CogdocTypeIdentity:  true,
	CogdocTypeOntology:  true,
	CogdocTypeMemory:    true,
	CogdocTypeSchema:    true,
	CogdocTypeDecision:  true,
	CogdocTypeSession:   true,
	CogdocTypeHandoff:   true,
	CogdocTypeGuide:     true,
	CogdocTypeADR:       true,
	CogdocTypeKnowledge: true,
	// Extended semantic types
	CogdocTypeNote:              true,
	CogdocTypeTerm:              true,
	CogdocTypeSpec:              true,
	CogdocTypeClaim:             true,
	CogdocTypeInsight:           true,
	CogdocTypeArchitecture:      true,
	CogdocTypeResearchSynthesis: true,
	CogdocTypeObservation:       true,
	CogdocTypeAssessment:        true,
	CogdocTypeProcedural:        true,
	CogdocTypeSummary:           true,
	CogdocTypeSpecification:     true,
}

// IsValid returns true if this is a known cogdoc type.
func (t CogdocType) IsValid() bool {
	return ValidCogdocTypes[t]
}

// String returns the string representation of the type.
func (t CogdocType) String() string {
	return string(t)
}

// CogdocMeta is the YAML frontmatter structure for cogdocs.
type CogdocMeta struct {
	// ID is the unique identifier for the cogdoc.
	// Format: <domain>.<type>.<slug> (e.g., "semantic.insight.eigenform")
	ID string `yaml:"id" json:"id"`

	// Type is the cogdoc type.
	Type CogdocType `yaml:"type" json:"type"`

	// Title is the human-readable title.
	Title string `yaml:"title" json:"title"`

	// Created is when the cogdoc was first created.
	Created time.Time `yaml:"created" json:"created"`

	// Updated is when the cogdoc was last modified.
	Updated time.Time `yaml:"updated" json:"updated"`

	// Author is the author identifier.
	Author string `yaml:"author,omitempty" json:"author,omitempty"`

	// Tags are classification labels.
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

	// Refs are cog:// URI references to related resources.
	Refs []string `yaml:"refs,omitempty" json:"refs,omitempty"`

	// Status is the document status (draft, active, archived).
	Status string `yaml:"status,omitempty" json:"status,omitempty"`

	// Confidence is a confidence score [0, 1] for knowledge/insights.
	Confidence float64 `yaml:"confidence,omitempty" json:"confidence,omitempty"`
}

// Cogdoc represents a full cogdoc with metadata and content.
type Cogdoc struct {
	// Meta is the YAML frontmatter.
	Meta CogdocMeta `json:"meta"`

	// Content is the markdown body (after frontmatter).
	Content string `json:"content"`

	// Path is the filesystem path relative to .cog/.
	Path string `json:"path"`

	// Hash is the content-addressable hash.
	Hash string `json:"hash,omitempty"`
}

// CogdocList is a collection of cogdocs.
type CogdocList struct {
	// Docs is the list of cogdocs.
	Docs []*Cogdoc `json:"docs"`

	// Total is the total count before pagination.
	Total int `json:"total"`

	// Query contains the search parameters used.
	Query *CogdocQuery `json:"query,omitempty"`
}

// CogdocQuery represents search/filter parameters.
type CogdocQuery struct {
	// Q is the full-text search query.
	Q string `json:"q,omitempty"`

	// Type filters by cogdoc type.
	Type CogdocType `json:"type,omitempty"`

	// Sector filters by memory sector (semantic, episodic, etc.).
	Sector string `json:"sector,omitempty"`

	// Tags filters to docs containing all specified tags.
	Tags []string `json:"tags,omitempty"`

	// Limit is the maximum number of results.
	Limit int `json:"limit,omitempty"`

	// Offset is the pagination offset.
	Offset int `json:"offset,omitempty"`

	// SortBy is the sort field (created, updated, title).
	SortBy string `json:"sort_by,omitempty"`

	// SortDesc is true for descending sort.
	SortDesc bool `json:"sort_desc,omitempty"`
}
