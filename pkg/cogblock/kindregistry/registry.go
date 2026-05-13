// Package kindregistry provides a concurrent-safe, init()-based registry for
// CogBlock Kind handlers (ADR-090).
//
// Usage pattern:
//
//	// In the owning package's init:
//	func init() {
//	    kindregistry.Register(cogblock.BlockToolResult, handleToolResult)
//	}
//
//	// In the routing layer:
//	if err := kindregistry.Dispatch(block); errors.Is(err, kindregistry.ErrNoHandler) {
//	    // no handler registered for this Kind -- fall through or defer
//	}
//
// Register panics on duplicate registration so that double-init wiring bugs
// surface at process startup rather than silently at runtime.
package kindregistry

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/myrgic/cogos/pkg/cogblock"
)

// ErrNoHandler is returned by Dispatch when no handler has been registered
// for the block's Kind. Callers can distinguish this from handler-internal
// errors via errors.Is.
var ErrNoHandler = errors.New("kindregistry: no handler registered for Kind")

// Handler is the contract a Kind owner registers to handle its Kind.
// The block pointer is never nil when Handler is called.
type Handler func(block *cogblock.CogBlock) error

var (
	mu       sync.RWMutex
	registry = map[cogblock.CogBlockKind]Handler{}
)

// Register associates a handler with a Kind.
// Panics if the Kind is already registered -- this catches double-init wiring
// bugs early (at process startup) rather than silently at runtime.
func Register(k cogblock.CogBlockKind, h Handler) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := registry[k]; exists {
		panic(fmt.Sprintf("kindregistry: duplicate handler registered for %q", k))
	}
	registry[k] = h
}

// Dispatch routes a block to its registered handler.
// Returns ErrNoHandler if no handler is registered for block.Kind.
// The caller may check errors.Is(err, ErrNoHandler) to distinguish
// "no handler" from a handler-internal error.
// Dispatch is safe to call concurrently.
func Dispatch(block *cogblock.CogBlock) error {
	if block == nil {
		return errors.New("kindregistry: Dispatch called with nil block")
	}
	mu.RLock()
	h, ok := registry[block.Kind]
	mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoHandler, block.Kind)
	}
	return h(block)
}

// Registered returns the sorted list of registered Kinds (for diagnostics).
// The returned slice is a snapshot; it does not reflect registrations that
// occur after the call returns.
func Registered() []cogblock.CogBlockKind {
	mu.RLock()
	defer mu.RUnlock()
	ks := make([]cogblock.CogBlockKind, 0, len(registry))
	for k := range registry {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// Reset clears the registry. Intended for use in test packages only.
// Production code must not call Reset.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	registry = map[cogblock.CogBlockKind]Handler{}
}
