// capability_cache.go — Zero-churn root alias shim. Canonical implementation
// lives in internal/identity per ADR-100 P0 extraction.
//
// Type alias and forwarding constructor let existing call sites
// (capability_test.go, capability_advertiser.go, capability_resolver.go,
// bus_tool_router.go) compile unchanged. New code should prefer the
// internal/identity import path.

package main

import (
	"github.com/myrgic/cogos/internal/identity"
)

// CapabilityCache stores agent capability advertisements with TTL-based expiry.
// Canonical home: internal/identity.CapabilityCache.
type CapabilityCache = identity.CapabilityCache

// defaultCapabilityTTL is the package-level default TTL for capability cache
// entries. Canonical home: internal/identity.DefaultCapabilityTTL.
const defaultCapabilityTTL = identity.DefaultCapabilityTTL

// NewCapabilityCache creates a new empty CapabilityCache.
// Canonical home: internal/identity.NewCapabilityCache.
func NewCapabilityCache() *CapabilityCache {
	return identity.NewCapabilityCache()
}
