// kindregistry.go — CogBlock Kind dispatch registry (ADR-090).
//
// Provides a concurrent-safe, init()-based registry for CogBlock Kind
// handlers. Packages that own a Kind register a handler in their init();
// the membrane (and any future routing layer) calls Dispatch rather than
// writing a Kind switch.
//
// Usage pattern:
//
//	// In the owning package's init:
//	func init() {
//	    RegisterKindHandler(BlockToolResult, handleToolResult)
//	}
//
//	// In the routing layer:
//	if err := DispatchKind(block); errors.Is(err, ErrNoKindHandler) {
//	    // no handler registered for this Kind -- fall through or defer
//	}
//
// RegisterKindHandler panics on duplicate registration so that double-init
// wiring bugs surface at process startup rather than silently at runtime.
package engine

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNoKindHandler is returned by DispatchKind when no handler has been
// registered for the block's Kind. Callers can distinguish this from
// handler-internal errors via errors.Is.
var ErrNoKindHandler = errors.New("kindregistry: no handler registered for Kind")

// KindHandler is the contract a Kind owner registers.
// The block pointer is never nil when KindHandler is called.
type KindHandler func(block *CogBlock) error

var (
	kindMu       sync.RWMutex
	kindHandlers = map[CogBlockKind]KindHandler{}
)

// RegisterKindHandler associates a handler with a Kind.
// Panics if the Kind is already registered -- this catches double-init wiring
// bugs at process startup rather than silently at runtime.
func RegisterKindHandler(k CogBlockKind, h KindHandler) {
	kindMu.Lock()
	defer kindMu.Unlock()
	if _, exists := kindHandlers[k]; exists {
		panic(fmt.Sprintf("kindregistry: duplicate handler registered for %q", k))
	}
	kindHandlers[k] = h
}

// DispatchKind routes a block to its registered handler.
// Returns ErrNoKindHandler if no handler is registered for block.Kind.
// The caller may use errors.Is(err, ErrNoKindHandler) to distinguish
// "no handler" from a handler-internal error.
// DispatchKind is safe to call concurrently.
func DispatchKind(block *CogBlock) error {
	if block == nil {
		return errors.New("kindregistry: DispatchKind called with nil block")
	}
	kindMu.RLock()
	h, ok := kindHandlers[block.Kind]
	kindMu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoKindHandler, block.Kind)
	}
	return h(block)
}

// RegisteredKinds returns the sorted list of registered Kinds (for diagnostics).
// The returned slice is a snapshot; it does not reflect registrations that
// occur after the call returns.
func RegisteredKinds() []CogBlockKind {
	kindMu.RLock()
	defer kindMu.RUnlock()
	ks := make([]CogBlockKind, 0, len(kindHandlers))
	for k := range kindHandlers {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// resetKindRegistry clears the registry. For use in test files only.
// Production code must not call this function.
func resetKindRegistry() {
	kindMu.Lock()
	defer kindMu.Unlock()
	kindHandlers = map[CogBlockKind]KindHandler{}
}
