package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/types"
)

// MemoryClient provides ergonomic access to cog://mem/*
//
// The Memory namespace contains the Holographic Memory Domain (HMD):
//   - semantic/ - Knowledge, architecture, insights
//   - episodic/ - Sessions, decisions, implementations
//   - procedural/ - Guides, workflows, how-tos
//   - reflective/ - Retrospectives, consolidations
//
// All methods are goroutine-safe.
type MemoryClient struct {
	kernel *sdk.Kernel
}

// NewMemoryClient creates a new MemoryClient.
func NewMemoryClient(k *sdk.Kernel) *MemoryClient {
	return &MemoryClient{kernel: k}
}

// SearchOption configures memory search behavior.
type SearchOption func(*searchOptions)

type searchOptions struct {
	limit   int
	offset  int
	docType types.CogdocType
	tags    []string
	sortBy  string
	desc    bool
}

// WithLimit sets the maximum number of results.
func WithLimit(n int) SearchOption {
	return func(o *searchOptions) {
		o.limit = n
	}
}

// WithOffset sets the pagination offset.
func WithOffset(n int) SearchOption {
	return func(o *searchOptions) {
		o.offset = n
	}
}

// WithType filters by cogdoc type.
func WithType(t types.CogdocType) SearchOption {
	return func(o *searchOptions) {
		o.docType = t
	}
}

// WithTags filters to documents containing all specified tags.
func WithTags(tags ...string) SearchOption {
	return func(o *searchOptions) {
		o.tags = tags
	}
}

// WithSort sets the sort field and direction.
func WithSort(field string, desc bool) SearchOption {
	return func(o *searchOptions) {
		o.sortBy = field
		o.desc = desc
	}
}

// Get retrieves a resource from memory by path.
// The path is relative to .cog/mem/ (e.g., "semantic/insights/eigenform").
//
// Example:
//
//	resource, err := c.Memory.Get("semantic/insights/eigenform")
func (c *MemoryClient) Get(path string) (*sdk.Resource, error) {
	return c.GetContext(context.Background(), path)
}

// GetContext is like Get but accepts a context.
func (c *MemoryClient) GetContext(ctx context.Context, path string) (*sdk.Resource, error) {
	uri := fmt.Sprintf("cog://mem/%s", strings.TrimPrefix(path, "/"))
	return c.kernel.ResolveContext(ctx, uri)
}

// GetCogdoc retrieves a cogdoc from memory and parses it into a typed struct.
// The path should not include the .cog.md extension.
//
// Example:
//
//	doc, err := c.Memory.GetCogdoc("semantic/insights/eigenform")
//	fmt.Println(doc.Meta.Title)
func (c *MemoryClient) GetCogdoc(path string) (*types.Cogdoc, error) {
	return c.GetCogdocContext(context.Background(), path)
}

// GetCogdocContext is like GetCogdoc but accepts a context.
func (c *MemoryClient) GetCogdocContext(ctx context.Context, path string) (*types.Cogdoc, error) {
	resource, err := c.GetContext(ctx, path)
	if err != nil {
		return nil, err
	}

	var doc types.Cogdoc
	if err := c.resourceToCogdoc(resource, &doc); err != nil {
		return nil, fmt.Errorf("parse cogdoc: %w", err)
	}

	return &doc, nil
}

// List returns all resources in a memory sector.
// The sector should be one of: "semantic", "episodic", "procedural", "reflective".
//
// Example:
//
//	resources, err := c.Memory.List("semantic")
func (c *MemoryClient) List(sector string) ([]*sdk.Resource, error) {
	return c.ListContext(context.Background(), sector)
}

// ListContext is like List but accepts a context.
func (c *MemoryClient) ListContext(ctx context.Context, sector string) ([]*sdk.Resource, error) {
	uri := fmt.Sprintf("cog://mem/%s", sector)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	if resource.IsCollection() {
		return resource.Children, nil
	}

	// Single resource - wrap in slice
	return []*sdk.Resource{resource}, nil
}

// Search searches memory with a query and optional filters.
//
// Example:
//
//	results, err := c.Memory.Search("eigenform", WithLimit(10), WithType(types.CogdocTypeInsight))
func (c *MemoryClient) Search(query string, opts ...SearchOption) ([]*sdk.Resource, error) {
	return c.SearchContext(context.Background(), query, opts...)
}

// SearchContext is like Search but accepts a context.
func (c *MemoryClient) SearchContext(ctx context.Context, query string, opts ...SearchOption) ([]*sdk.Resource, error) {
	// Apply options
	o := &searchOptions{
		limit: 20, // Default limit
	}
	for _, opt := range opts {
		opt(o)
	}

	// Build query string
	params := url.Values{}
	params.Set("q", query)
	if o.limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", o.limit))
	}
	if o.offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", o.offset))
	}
	if o.docType != "" {
		params.Set("type", string(o.docType))
	}
	if len(o.tags) > 0 {
		params.Set("tags", strings.Join(o.tags, ","))
	}
	if o.sortBy != "" {
		params.Set("sort", o.sortBy)
		if o.desc {
			params.Set("desc", "true")
		}
	}

	uri := fmt.Sprintf("cog://memory?%s", params.Encode())
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	if resource.IsCollection() {
		return resource.Children, nil
	}

	if resource.IsEmpty() {
		return []*sdk.Resource{}, nil
	}

	return []*sdk.Resource{resource}, nil
}

// Write writes content to a memory path.
// Creates the file if it doesn't exist, replaces if it does.
//
// Example:
//
//	err := c.Memory.Write("semantic/insights/new-insight.cog.md", content)
func (c *MemoryClient) Write(path string, content []byte) error {
	return c.WriteContext(context.Background(), path, content)
}

// WriteContext is like Write but accepts a context.
func (c *MemoryClient) WriteContext(ctx context.Context, path string, content []byte) error {
	uri := fmt.Sprintf("cog://mem/%s", strings.TrimPrefix(path, "/"))
	mutation := sdk.NewSetMutation(content)
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// WriteCogdoc writes a cogdoc to memory.
// The path should not include the .cog.md extension (it will be added).
//
// Example:
//
//	doc := &types.Cogdoc{
//	    Meta: types.CogdocMeta{ID: "semantic.insight.new", Type: types.CogdocTypeInsight, Title: "New Insight"},
//	    Content: "# New Insight\n\nContent here...",
//	}
//	err := c.Memory.WriteCogdoc("semantic/insights/new-insight", doc)
func (c *MemoryClient) WriteCogdoc(path string, doc *types.Cogdoc) error {
	return c.WriteCogdocContext(context.Background(), path, doc)
}

// WriteCogdocContext is like WriteCogdoc but accepts a context.
func (c *MemoryClient) WriteCogdocContext(ctx context.Context, path string, doc *types.Cogdoc) error {
	content, err := c.cogdocToBytes(doc)
	if err != nil {
		return fmt.Errorf("marshal cogdoc: %w", err)
	}

	// Ensure path ends with .cog.md
	if !strings.HasSuffix(path, ".cog.md") {
		path = path + ".cog.md"
	}

	return c.WriteContext(ctx, path, content)
}

// Delete removes a resource from memory.
//
// Example:
//
//	err := c.Memory.Delete("semantic/insights/old-insight.cog.md")
func (c *MemoryClient) Delete(path string) error {
	return c.DeleteContext(context.Background(), path)
}

// DeleteContext is like Delete but accepts a context.
func (c *MemoryClient) DeleteContext(ctx context.Context, path string) error {
	uri := fmt.Sprintf("cog://mem/%s", strings.TrimPrefix(path, "/"))
	mutation := sdk.NewDeleteMutation()
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// Semantic returns a SectorClient for the semantic memory sector.
func (c *MemoryClient) Semantic() *SectorClient {
	return &SectorClient{memory: c, sector: "semantic"}
}

// Episodic returns a SectorClient for the episodic memory sector.
func (c *MemoryClient) Episodic() *SectorClient {
	return &SectorClient{memory: c, sector: "episodic"}
}

// Procedural returns a SectorClient for the procedural memory sector.
func (c *MemoryClient) Procedural() *SectorClient {
	return &SectorClient{memory: c, sector: "procedural"}
}

// Reflective returns a SectorClient for the reflective memory sector.
func (c *MemoryClient) Reflective() *SectorClient {
	return &SectorClient{memory: c, sector: "reflective"}
}

// SectorClient provides operations scoped to a specific memory sector.
type SectorClient struct {
	memory *MemoryClient
	sector string
}

// Get retrieves a resource from this sector.
func (s *SectorClient) Get(path string) (*sdk.Resource, error) {
	return s.memory.Get(fmt.Sprintf("%s/%s", s.sector, path))
}

// GetCogdoc retrieves a cogdoc from this sector.
func (s *SectorClient) GetCogdoc(path string) (*types.Cogdoc, error) {
	return s.memory.GetCogdoc(fmt.Sprintf("%s/%s", s.sector, path))
}

// List returns all resources in this sector.
func (s *SectorClient) List() ([]*sdk.Resource, error) {
	return s.memory.List(s.sector)
}

// Search searches this sector with a query.
func (s *SectorClient) Search(query string, opts ...SearchOption) ([]*sdk.Resource, error) {
	// Add sector filter to search
	// The memory projector will handle sector filtering
	uri := fmt.Sprintf("%s?q=%s", s.sector, url.QueryEscape(query))
	return s.memory.SearchContext(context.Background(), uri, opts...)
}

// Write writes content to this sector.
func (s *SectorClient) Write(path string, content []byte) error {
	return s.memory.Write(fmt.Sprintf("%s/%s", s.sector, path), content)
}

// WriteCogdoc writes a cogdoc to this sector.
func (s *SectorClient) WriteCogdoc(path string, doc *types.Cogdoc) error {
	return s.memory.WriteCogdoc(fmt.Sprintf("%s/%s", s.sector, path), doc)
}

// Delete removes a resource from this sector.
func (s *SectorClient) Delete(path string) error {
	return s.memory.Delete(fmt.Sprintf("%s/%s", s.sector, path))
}

// resourceToCogdoc converts a Resource to a Cogdoc.
func (c *MemoryClient) resourceToCogdoc(r *sdk.Resource, doc *types.Cogdoc) error {
	// The resource metadata contains the parsed frontmatter
	if r.Metadata != nil {
		metaJSON, err := json.Marshal(r.Metadata)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(metaJSON, &doc.Meta); err != nil {
			return err
		}
	}

	// Content is the markdown body
	doc.Content = string(r.Content)
	doc.Hash = r.Hash

	// Extract path from URI
	if strings.HasPrefix(r.URI, "cog://mem/") {
		doc.Path = strings.TrimPrefix(r.URI, "cog://mem/")
	}

	return nil
}

// cogdocToBytes serializes a Cogdoc to YAML frontmatter + markdown content.
func (c *MemoryClient) cogdocToBytes(doc *types.Cogdoc) ([]byte, error) {
	// Build YAML frontmatter
	var sb strings.Builder
	sb.WriteString("---\n")

	// Use JSON roundtrip to get map for YAML (simple approach)
	metaJSON, err := json.Marshal(doc.Meta)
	if err != nil {
		return nil, err
	}

	var metaMap map[string]interface{}
	if err := json.Unmarshal(metaJSON, &metaMap); err != nil {
		return nil, err
	}

	// Write key-value pairs in a reasonable order
	writeField := func(key string, val interface{}) {
		if val == nil {
			return
		}
		switch v := val.(type) {
		case string:
			if v != "" {
				sb.WriteString(fmt.Sprintf("%s: %s\n", key, v))
			}
		case float64:
			if v != 0 {
				sb.WriteString(fmt.Sprintf("%s: %v\n", key, v))
			}
		case []interface{}:
			if len(v) > 0 {
				sb.WriteString(fmt.Sprintf("%s:\n", key))
				for _, item := range v {
					sb.WriteString(fmt.Sprintf("  - %v\n", item))
				}
			}
		default:
			sb.WriteString(fmt.Sprintf("%s: %v\n", key, v))
		}
	}

	// Write fields in preferred order
	orderedKeys := []string{"id", "type", "title", "created", "updated", "author", "status", "confidence", "tags", "refs"}
	for _, key := range orderedKeys {
		if val, ok := metaMap[key]; ok {
			writeField(key, val)
			delete(metaMap, key)
		}
	}
	// Write any remaining fields
	for key, val := range metaMap {
		writeField(key, val)
	}

	sb.WriteString("---\n")

	// Add content
	if doc.Content != "" {
		sb.WriteString("\n")
		sb.WriteString(doc.Content)
	}

	return []byte(sb.String()), nil
}
