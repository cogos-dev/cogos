package sdk

import (
	"encoding/json"
	"time"
)

// ContentType indicates the type of content in a Resource.
type ContentType string

const (
	// ContentTypeRaw is unprocessed bytes (default).
	ContentTypeRaw ContentType = "raw"

	// ContentTypeJSON is JSON-encoded structured data.
	ContentTypeJSON ContentType = "json"

	// ContentTypeYAML is YAML-encoded structured data.
	ContentTypeYAML ContentType = "yaml"

	// ContentTypeMarkdown is Markdown text (possibly with YAML frontmatter).
	ContentTypeMarkdown ContentType = "markdown"

	// ContentTypeCogdoc is a validated cogdoc (Markdown + YAML frontmatter).
	ContentTypeCogdoc ContentType = "cogdoc"
)

// Resource is the universal return type for Resolve operations.
// It wraps content with metadata about the resolution.
//
// Resources are the "atoms" of the holographic projection - they are
// what the kernel renders from URIs.
type Resource struct {
	// URI is the original URI that was resolved.
	URI string `json:"uri"`

	// Content is the raw content bytes.
	Content []byte `json:"content,omitempty"`

	// ContentType indicates how to interpret Content.
	ContentType ContentType `json:"content_type"`

	// Metadata contains type-specific structured data.
	// For cogdocs, this includes frontmatter fields.
	// For signals, this includes decay parameters.
	Metadata map[string]any `json:"metadata,omitempty"`

	// Hash is the content-addressable hash (SHA-256) of Content.
	// Empty if not computed.
	Hash string `json:"hash,omitempty"`

	// ModTime is when the underlying data was last modified.
	ModTime time.Time `json:"mod_time,omitempty"`

	// Relevance is the computed relevance score [0, 1].
	// For signals, this is based on decay formula.
	// For memory, this may be based on search ranking.
	Relevance float64 `json:"relevance,omitempty"`

	// Children contains nested resources for collection responses.
	// Used when resolving a namespace or directory.
	Children []*Resource `json:"children,omitempty"`
}

// NewResource creates a Resource with raw content.
func NewResource(uri string, content []byte) *Resource {
	return &Resource{
		URI:         uri,
		Content:     content,
		ContentType: ContentTypeRaw,
		Metadata:    make(map[string]any),
	}
}

// NewJSONResource creates a Resource with JSON content.
func NewJSONResource(uri string, data any) (*Resource, error) {
	content, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &Resource{
		URI:         uri,
		Content:     content,
		ContentType: ContentTypeJSON,
		Metadata:    make(map[string]any),
	}, nil
}

// String returns the Content as a string.
func (r *Resource) String() string {
	return string(r.Content)
}

// JSON unmarshals the Content into the given value.
// Returns error if ContentType is not JSON or unmarshaling fails.
func (r *Resource) JSON(v any) error {
	return json.Unmarshal(r.Content, v)
}

// SetMetadata sets a metadata key-value pair.
func (r *Resource) SetMetadata(key string, value any) *Resource {
	if r.Metadata == nil {
		r.Metadata = make(map[string]any)
	}
	r.Metadata[key] = value
	return r
}

// GetMetadata retrieves a metadata value by key.
func (r *Resource) GetMetadata(key string) (any, bool) {
	if r.Metadata == nil {
		return nil, false
	}
	v, ok := r.Metadata[key]
	return v, ok
}

// GetMetadataString retrieves a metadata value as string.
func (r *Resource) GetMetadataString(key string) string {
	v, ok := r.Metadata[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// IsEmpty returns true if the resource has no content.
func (r *Resource) IsEmpty() bool {
	return len(r.Content) == 0 && len(r.Children) == 0
}

// IsCollection returns true if this resource contains child resources.
func (r *Resource) IsCollection() bool {
	return len(r.Children) > 0
}

// Count returns the number of items (1 for single resource, len for collection).
func (r *Resource) Count() int {
	if len(r.Children) > 0 {
		return len(r.Children)
	}
	if len(r.Content) > 0 {
		return 1
	}
	return 0
}

// WithRelevance sets the relevance score and returns the resource.
func (r *Resource) WithRelevance(rel float64) *Resource {
	r.Relevance = rel
	return r
}

// WithHash sets the content hash and returns the resource.
func (r *Resource) WithHash(hash string) *Resource {
	r.Hash = hash
	return r
}

// WithModTime sets the modification time and returns the resource.
func (r *Resource) WithModTime(t time.Time) *Resource {
	r.ModTime = t
	return r
}
