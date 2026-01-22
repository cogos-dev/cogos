package project

import (
	"sync"
)

// Registry holds all registered projectors indexed by namespace.
// It is safe for concurrent access.
type Registry struct {
	mu         sync.RWMutex
	projectors map[string]Projector
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		projectors: make(map[string]Projector),
	}
}

// Register adds a projector to the registry.
// Panics if a projector for the namespace is already registered.
func (r *Registry) Register(p Projector) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ns := p.Namespace()
	if _, exists := r.projectors[ns]; exists {
		panic("projector already registered for namespace: " + ns)
	}
	r.projectors[ns] = p
}

// Get returns the projector for the given namespace.
// Returns nil if no projector is registered.
func (r *Registry) Get(namespace string) Projector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projectors[namespace]
}

// Has returns true if a projector is registered for the namespace.
func (r *Registry) Has(namespace string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.projectors[namespace]
	return exists
}

// Namespaces returns all registered namespace names.
func (r *Registry) Namespaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ns := make([]string, 0, len(r.projectors))
	for k := range r.projectors {
		ns = append(ns, k)
	}
	return ns
}

// Count returns the number of registered projectors.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.projectors)
}
