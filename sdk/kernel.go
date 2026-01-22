package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Kernel is the holographic projector - the single entry point for all workspace operations.
//
// The kernel resolves cog:// URIs into Resources. It maintains:
//   - A registry of projectors (one per namespace)
//   - The workspace root path
//   - Event sequence for ordering
//
// Create a Kernel using Connect(). Close it when done with Close().
type Kernel struct {
	root       string
	projectors map[string]Projector
	eventSeq   int64
	closed     atomic.Bool
	closeMu    sync.Mutex
	closeOnce  sync.Once
}

// Projector is the interface that all namespace handlers must implement.
type Projector interface {
	// Namespace returns the namespace this projector handles.
	Namespace() string

	// Resolve reads and projects the resource at the given URI.
	Resolve(ctx context.Context, uri *ParsedURI) (*Resource, error)

	// CanMutate returns true if this projector supports mutations.
	CanMutate() bool

	// Mutate writes or updates the resource at the given URI.
	Mutate(ctx context.Context, uri *ParsedURI, mutation *Mutation) error
}

// BaseProjector provides a default implementation for read-only projectors.
type BaseProjector struct {
	ns string
}

// NewBaseProjector creates a new BaseProjector with the given namespace.
func NewBaseProjector(namespace string) BaseProjector {
	return BaseProjector{ns: namespace}
}

// Namespace returns the namespace.
func (b BaseProjector) Namespace() string {
	return b.ns
}

// CanMutate returns false for base projectors.
func (b BaseProjector) CanMutate() bool {
	return false
}

// Mutate returns ErrReadOnly for base projectors.
func (b BaseProjector) Mutate(ctx context.Context, uri *ParsedURI, mutation *Mutation) error {
	return NewURIError("Mutate", uri.Raw, ErrReadOnly)
}

// Root returns the workspace root directory.
func (k *Kernel) Root() string {
	return k.root
}

// CogDir returns the .cog directory path.
func (k *Kernel) CogDir() string {
	return filepath.Join(k.root, ".cog")
}

// StateDir returns the .cog/.state directory path.
func (k *Kernel) StateDir() string {
	return filepath.Join(k.root, ".cog", ".state")
}

// MemoryDir returns the .cog/memory directory path.
func (k *Kernel) MemoryDir() string {
	return filepath.Join(k.root, ".cog", "memory")
}

// RegisterProjector adds a projector to the kernel.
func (k *Kernel) RegisterProjector(p Projector) {
	if k.projectors == nil {
		k.projectors = make(map[string]Projector)
	}
	k.projectors[p.Namespace()] = p
}

// GetProjector returns the projector for the given namespace.
func (k *Kernel) GetProjector(namespace string) Projector {
	return k.projectors[namespace]
}

// NextEventSeq returns the next event sequence number.
func (k *Kernel) NextEventSeq() int64 {
	return atomic.AddInt64(&k.eventSeq, 1)
}

// Resolve reads and projects the resource at the given URI.
//
// This is THE core operation of the SDK. Everything goes through Resolve.
//
// Examples:
//
//	// Read a single cogdoc
//	resource, err := kernel.Resolve("cog://mem/semantic/insights/eigenform")
//
//	// Search memory with query
//	resource, err := kernel.Resolve("cog://mem/semantic?q=topic&limit=10")
//
//	// Get active signals
//	resource, err := kernel.Resolve("cog://signals/inference?above=0.3")
//
//	// Get coherence state
//	resource, err := kernel.Resolve("cog://coherence")
func (k *Kernel) Resolve(uri string) (*Resource, error) {
	return k.ResolveContext(context.Background(), uri)
}

// ResolveContext is like Resolve but accepts a context for cancellation.
func (k *Kernel) ResolveContext(ctx context.Context, uri string) (*Resource, error) {
	if k.closed.Load() {
		return nil, NewError("Resolve", ErrNotConnected)
	}

	// Parse URI
	parsed, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}

	// Get projector for namespace
	proj := k.projectors[parsed.Namespace]
	if proj == nil {
		return nil, NewURIError("Resolve", uri, ErrUnknownNamespace)
	}

	// Delegate to projector
	return proj.Resolve(ctx, parsed)
}

// Project resolves a URI and unmarshals the result into the given value.
//
// This is a convenience method that combines Resolve with type-safe unmarshaling.
//
// Example:
//
//	var coherence types.CoherenceState
//	err := kernel.Project("cog://coherence", &coherence)
func (k *Kernel) Project(uri string, into any) error {
	return k.ProjectContext(context.Background(), uri, into)
}

// ProjectContext is like Project but accepts a context.
func (k *Kernel) ProjectContext(ctx context.Context, uri string, into any) error {
	resource, err := k.ResolveContext(ctx, uri)
	if err != nil {
		return err
	}

	// Unmarshal based on content type
	switch resource.ContentType {
	case ContentTypeJSON:
		return json.Unmarshal(resource.Content, into)
	case ContentTypeCogdoc, ContentTypeMarkdown:
		// For cogdocs, the metadata contains structured fields
		metaJSON, err := json.Marshal(resource.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
		return json.Unmarshal(metaJSON, into)
	default:
		// Try JSON unmarshal as fallback
		return json.Unmarshal(resource.Content, into)
	}
}

// Mutate writes or updates the resource at the given URI.
//
// Example:
//
//	mutation := sdk.NewSetMutation([]byte("# New Content\n..."))
//	err := kernel.Mutate("cog://mem/semantic/insights/new-insight", mutation)
func (k *Kernel) Mutate(uri string, mutation *Mutation) error {
	return k.MutateContext(context.Background(), uri, mutation)
}

// MutateContext is like Mutate but accepts a context.
func (k *Kernel) MutateContext(ctx context.Context, uri string, mutation *Mutation) error {
	if k.closed.Load() {
		return NewError("Mutate", ErrNotConnected)
	}

	// Parse URI
	parsed, err := ParseURI(uri)
	if err != nil {
		return err
	}

	// Get projector
	proj := k.projectors[parsed.Namespace]
	if proj == nil {
		return NewURIError("Mutate", uri, ErrUnknownNamespace)
	}

	// Check if mutable
	if !proj.CanMutate() {
		return NewURIError("Mutate", uri, ErrReadOnly)
	}

	// Delegate to projector
	return proj.Mutate(ctx, parsed, mutation)
}

// Watch returns a channel that emits resources when the URI's data changes.
// The channel is closed when the context is cancelled.
//
// Not implemented in Phase 1 - returns error.
func (k *Kernel) Watch(ctx context.Context, uri string) (<-chan *Resource, error) {
	return nil, NewError("Watch", fmt.Errorf("not implemented"))
}

// Close releases kernel resources.
func (k *Kernel) Close() error {
	k.closeMu.Lock()
	defer k.closeMu.Unlock()

	var closeErr error
	k.closeOnce.Do(func() {
		k.closed.Store(true)
	})
	return closeErr
}

// IsClosed returns true if the kernel has been closed.
func (k *Kernel) IsClosed() bool {
	return k.closed.Load()
}

// newKernel creates a new kernel for the given workspace root.
func newKernel(root string) *Kernel {
	return &Kernel{
		root:       root,
		projectors: make(map[string]Projector),
		eventSeq:   time.Now().UnixNano() / 1000,
	}
}

// ReadFile is a convenience method to read a file relative to workspace root.
func (k *Kernel) ReadFile(relativePath string) ([]byte, error) {
	fullPath := filepath.Join(k.root, relativePath)
	return os.ReadFile(fullPath)
}

// WriteFile is a convenience method to write a file relative to workspace root.
// Uses atomic write pattern (temp file + rename).
func (k *Kernel) WriteFile(relativePath string, data []byte, perm os.FileMode) error {
	fullPath := filepath.Join(k.root, relativePath)
	dir := filepath.Dir(fullPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpPath := fullPath + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}

	return os.Rename(tmpPath, fullPath)
}

// FileExists checks if a file exists relative to workspace root.
func (k *Kernel) FileExists(relativePath string) bool {
	fullPath := filepath.Join(k.root, relativePath)
	_, err := os.Stat(fullPath)
	return err == nil
}
