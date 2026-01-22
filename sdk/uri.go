package sdk

import (
	"fmt"
	"net/url"
	"strings"
)

// ParsedURI represents a parsed cog:// URI with its components.
//
// URI format: cog://namespace/path[?query][#fragment]
//
// Examples:
//
//	cog://mem/semantic/insights/eigenform
//	cog://signals/inference?above=0.3
//	cog://context?budget=50000&model=sonnet
//	cog://thread/current#last-10
//	cog://coherence
//	cog://src
type ParsedURI struct {
	// Namespace is the first path component (memory, signals, context, etc.)
	Namespace string

	// Path is everything after the namespace (may be empty).
	Path string

	// Query contains parsed query parameters.
	Query url.Values

	// Fragment is the portion after # (may be empty).
	Fragment string

	// Raw is the original unparsed URI string.
	Raw string
}

// Namespaces defines all valid cog:// namespaces.
// This mirrors the kernel's projection types.
var Namespaces = map[string]bool{
	// Core namespaces
	"mem":       true, // cog://mem/* → Cogdocs
	"signals":   true, // cog://signals/* → Signal field
	"context":   true, // cog://context → 4-tier context
	"thread":    true, // cog://thread/* → Conversation threads
	"coherence": true, // cog://coherence → Coherence state
	"identity":  true, // cog://identity → Workspace identity
	"src":       true, // cog://src → SRC constants
	"adr":       true, // cog://adr/* → Architecture Decision Records
	"ledger":    true, // cog://ledger/* → Event ledger
	"inference": true, // cog://inference → Inference endpoint
	"kernel":    true, // cog://kernel/* → Kernel internal paths
	"hooks":     true, // cog://hooks/* → Hook definitions

	// Extended namespaces (from kernel projections)
	"spec":      true, // cog://spec/* → Specifications
	"specs":     true, // cog://specs/* → Specifications (plural alias)
	"status":    true, // cog://status/* → Status files (JSON)
	"canonical": true, // cog://canonical → Holographic baseline hash
	"handoff":   true, // cog://handoff/* → Handoff documents
	"handoffs":  true, // cog://handoffs/* → Handoffs (plural alias)
	"crystal":   true, // cog://crystal/* → Ledger crystals
	"role":      true, // cog://role/* → Role definitions
	"roles":     true, // cog://roles/* → Roles (plural alias)
	"skill":     true, // cog://skill/* → Skill definitions
	"skills":    true, // cog://skills/* → Skills (plural alias)
	"agent":     true, // cog://agent/* → Agent definitions
	"agents":    true, // cog://agents/* → Agents (plural alias)
}

// ParseURI parses a cog:// URI into its components.
//
// Returns ErrInvalidURI if the URI is malformed or uses an unknown scheme.
// Returns ErrUnknownNamespace if the namespace is not recognized.
//
// Example:
//
//	parsed, err := sdk.ParseURI("cog://mem/semantic/insights?q=topic&limit=10")
//	// parsed.Namespace = "mem"
//	// parsed.Path = "semantic/insights"
//	// parsed.Query = {"q": ["topic"], "limit": ["10"]}
func ParseURI(rawURI string) (*ParsedURI, error) {
	if rawURI == "" {
		return nil, InvalidURIError(rawURI, "empty URI")
	}

	// Must start with cog://
	if !strings.HasPrefix(rawURI, "cog://") {
		return nil, InvalidURIError(rawURI, "must start with cog://")
	}

	// Parse as standard URL (replacing cog:// with http:// for parsing)
	httpURI := "http://" + strings.TrimPrefix(rawURI, "cog://")
	parsed, err := url.Parse(httpURI)
	if err != nil {
		return nil, InvalidURIError(rawURI, err.Error())
	}

	// The "host" in our scheme is the namespace
	namespace := parsed.Host
	if namespace == "" {
		return nil, InvalidURIError(rawURI, "missing namespace")
	}

	// Validate namespace
	if !Namespaces[namespace] {
		return nil, &SDKError{
			Op:    "ParseURI",
			URI:   rawURI,
			Cause: ErrUnknownNamespace,
		}
	}

	// Path is everything after the namespace
	path := strings.TrimPrefix(parsed.Path, "/")

	return &ParsedURI{
		Namespace: namespace,
		Path:      path,
		Query:     parsed.Query(),
		Fragment:  parsed.Fragment,
		Raw:       rawURI,
	}, nil
}

// String returns the canonical string representation of the URI.
func (p *ParsedURI) String() string {
	var sb strings.Builder
	sb.WriteString("cog://")
	sb.WriteString(p.Namespace)
	if p.Path != "" {
		sb.WriteString("/")
		sb.WriteString(p.Path)
	}
	if len(p.Query) > 0 {
		sb.WriteString("?")
		sb.WriteString(p.Query.Encode())
	}
	if p.Fragment != "" {
		sb.WriteString("#")
		sb.WriteString(p.Fragment)
	}
	return sb.String()
}

// WithQuery returns a new ParsedURI with additional query parameters.
func (p *ParsedURI) WithQuery(key, value string) *ParsedURI {
	newURI := *p
	newURI.Query = make(url.Values)
	for k, v := range p.Query {
		newURI.Query[k] = v
	}
	newURI.Query.Set(key, value)
	return &newURI
}

// GetQuery returns a query parameter value, or empty string if not present.
func (p *ParsedURI) GetQuery(key string) string {
	return p.Query.Get(key)
}

// GetQueryInt returns a query parameter as int, or default if not present/invalid.
func (p *ParsedURI) GetQueryInt(key string, defaultVal int) int {
	val := p.Query.Get(key)
	if val == "" {
		return defaultVal
	}
	var result int
	if _, err := fmt.Sscanf(val, "%d", &result); err != nil {
		return defaultVal
	}
	return result
}

// GetQueryFloat returns a query parameter as float64, or default if not present/invalid.
func (p *ParsedURI) GetQueryFloat(key string, defaultVal float64) float64 {
	val := p.Query.Get(key)
	if val == "" {
		return defaultVal
	}
	var result float64
	if _, err := fmt.Sscanf(val, "%f", &result); err != nil {
		return defaultVal
	}
	return result
}

// GetQueryBool returns a query parameter as bool.
// Returns true for "true", "1", "yes"; false otherwise.
func (p *ParsedURI) GetQueryBool(key string) bool {
	val := strings.ToLower(p.Query.Get(key))
	return val == "true" || val == "1" || val == "yes"
}

// HasPath returns true if the URI has a non-empty path.
func (p *ParsedURI) HasPath() bool {
	return p.Path != ""
}

// PathSegments returns the path split by "/".
func (p *ParsedURI) PathSegments() []string {
	if p.Path == "" {
		return nil
	}
	return strings.Split(p.Path, "/")
}

// IsNamespace returns true if this URI refers to just a namespace (no path).
func (p *ParsedURI) IsNamespace() bool {
	return p.Path == ""
}
