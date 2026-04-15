package uri

import (
	"fmt"
	"net/url"
	"strings"
)

// Scheme is the cog URI scheme prefix.
const Scheme = "cog://"

// URI represents a parsed cog:// URI with its components.
//
// Format: cog://namespace/path[?query][#fragment]
type URI struct {
	// Namespace is the first path component (mem, signals, context, etc.).
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

// Parse parses a cog:// URI string into its components.
//
// Returns ErrInvalidURI if the URI is malformed or uses an unknown scheme.
// Returns ErrUnknownNamespace if the namespace is not recognized.
//
// Example:
//
//	u, err := uri.Parse("cog://mem/semantic/insights?q=topic&limit=10")
//	// u.Namespace = "mem"
//	// u.Path = "semantic/insights"
//	// u.Query = {"q": ["topic"], "limit": ["10"]}
func Parse(rawURI string) (*URI, error) {
	if rawURI == "" {
		return nil, &Error{Op: "Parse", URI: rawURI, Err: fmt.Errorf("%w: empty URI", ErrInvalidURI)}
	}

	if !strings.HasPrefix(rawURI, Scheme) {
		return nil, &Error{Op: "Parse", URI: rawURI, Err: fmt.Errorf("%w: must start with %s", ErrInvalidURI, Scheme)}
	}

	// Parse as standard URL (replacing cog:// with http:// for url.Parse).
	httpURI := "http://" + strings.TrimPrefix(rawURI, Scheme)
	parsed, err := url.Parse(httpURI)
	if err != nil {
		return nil, &Error{Op: "Parse", URI: rawURI, Err: fmt.Errorf("%w: %s", ErrInvalidURI, err.Error())}
	}

	namespace := parsed.Host
	if namespace == "" {
		return nil, &Error{Op: "Parse", URI: rawURI, Err: fmt.Errorf("%w: missing namespace", ErrInvalidURI)}
	}

	if !Namespaces[namespace] {
		return nil, &Error{Op: "Parse", URI: rawURI, Err: fmt.Errorf("%w: %q", ErrUnknownNamespace, namespace)}
	}

	path := strings.TrimPrefix(parsed.Path, "/")

	return &URI{
		Namespace: namespace,
		Path:      path,
		Query:     parsed.Query(),
		Fragment:  parsed.Fragment,
		Raw:       rawURI,
	}, nil
}

// String returns the canonical string representation of the URI.
func (u *URI) String() string {
	var sb strings.Builder
	sb.WriteString(Scheme)
	sb.WriteString(u.Namespace)
	if u.Path != "" {
		sb.WriteString("/")
		sb.WriteString(u.Path)
	}
	if len(u.Query) > 0 {
		sb.WriteString("?")
		sb.WriteString(u.Query.Encode())
	}
	if u.Fragment != "" {
		sb.WriteString("#")
		sb.WriteString(u.Fragment)
	}
	return sb.String()
}

// WithQuery returns a new URI with an additional query parameter set.
func (u *URI) WithQuery(key, value string) *URI {
	newURI := *u
	newURI.Query = make(url.Values)
	for k, v := range u.Query {
		newURI.Query[k] = v
	}
	newURI.Query.Set(key, value)
	return &newURI
}

// GetQuery returns a query parameter value, or empty string if not present.
func (u *URI) GetQuery(key string) string {
	return u.Query.Get(key)
}

// GetQueryInt returns a query parameter as int, or defaultVal if not present or invalid.
func (u *URI) GetQueryInt(key string, defaultVal int) int {
	val := u.Query.Get(key)
	if val == "" {
		return defaultVal
	}
	var result int
	if _, err := fmt.Sscanf(val, "%d", &result); err != nil {
		return defaultVal
	}
	return result
}

// GetQueryFloat returns a query parameter as float64, or defaultVal if not present or invalid.
func (u *URI) GetQueryFloat(key string, defaultVal float64) float64 {
	val := u.Query.Get(key)
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
func (u *URI) GetQueryBool(key string) bool {
	val := strings.ToLower(u.Query.Get(key))
	return val == "true" || val == "1" || val == "yes"
}

// HasPath returns true if the URI has a non-empty path component.
func (u *URI) HasPath() bool {
	return u.Path != ""
}

// PathSegments returns the path split by "/".
func (u *URI) PathSegments() []string {
	if u.Path == "" {
		return nil
	}
	return strings.Split(u.Path, "/")
}

// IsNamespace returns true if this URI refers to just a namespace with no path.
func (u *URI) IsNamespace() bool {
	return u.Path == ""
}

// IsCogURI reports whether s begins with the cog:// scheme.
func IsCogURI(s string) bool {
	return strings.HasPrefix(s, Scheme)
}
