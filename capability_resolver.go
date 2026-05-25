// capability_resolver.go — Zero-churn root alias shim. Canonical implementation
// lives in internal/identity per ADR-100 P0 extraction.
//
// Type alias and forwarding constructor let existing call sites
// (bus_tool_router.go, capability_test.go) compile unchanged. New code
// should prefer the internal/identity import path.

package main

import (
	"github.com/myrgic/cogos/internal/identity"
)

// CapabilityResolver resolves agent URIs using the capability cache.
// Canonical home: internal/identity.CapabilityResolver.
type CapabilityResolver = identity.CapabilityResolver

// NewCapabilityResolver creates a resolver backed by the given cache.
// Canonical home: internal/identity.NewCapabilityResolver.
func NewCapabilityResolver(cache *CapabilityCache) *CapabilityResolver {
	return identity.NewCapabilityResolver(cache)
}
