// Package project contains the projector interface and registry.
//
// This package is designed to have minimal dependencies to avoid import cycles.
// The actual projector implementations live in the main sdk package.
package project

import (
	"context"
)

// Resource is an interface for projected resources.
// The concrete implementation is in the sdk package.
type Resource interface {
	URI() string
	Content() []byte
}

// Mutation is an interface for resource mutations.
// The concrete implementation is in the sdk package.
type Mutation interface {
	Op() string
	Content() []byte
}

// ParsedURI is an interface for parsed URIs.
type ParsedURI interface {
	Namespace() string
	Path() string
	Raw() string
	GetQuery(key string) string
	GetQueryInt(key string, defaultVal int) int
	GetQueryFloat(key string, defaultVal float64) float64
	GetQueryBool(key string) bool
}

// Projector is the interface that all namespace handlers must implement.
//
// Projectors are the workhorses of the SDK. They:
//   - Read underlying data (files, computed state, etc.)
//   - Apply query filters and projections
//   - Return shaped Resources
//
// Each namespace has exactly one Projector registered in the Registry.
type Projector interface {
	// Namespace returns the namespace this projector handles.
	Namespace() string

	// Resolve reads and projects the resource at the given URI.
	Resolve(ctx context.Context, uri ParsedURI) (Resource, error)

	// CanMutate returns true if this projector supports mutations.
	CanMutate() bool

	// Mutate writes or updates the resource at the given URI.
	Mutate(ctx context.Context, uri ParsedURI, mutation Mutation) error
}

// BaseProjector provides a default implementation for read-only projectors.
type BaseProjector struct {
	ns string
}

// NewBaseProjector creates a new BaseProjector with the given namespace.
func NewBaseProjector(namespace string) BaseProjector {
	return BaseProjector{ns: namespace}
}

// Namespace returns the namespace this projector handles.
func (b BaseProjector) Namespace() string {
	return b.ns
}

// CanMutate returns false for base projectors.
func (b BaseProjector) CanMutate() bool {
	return false
}
