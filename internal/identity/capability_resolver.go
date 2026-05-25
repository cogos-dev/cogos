// capability_resolver.go
// Resolves agent URIs using the capability cache. Provides a thin wrapper
// around CapabilityCache that makes it queryable in a URI-resolution-friendly
// way: given an agent ID (as extracted from a cog://agent/<id> URI), the
// resolver checks whether that agent is known via its advertised capabilities
// and returns its endpoint and tool availability.
//
// Canonical home: internal/identity (per ADR-100 P0 extraction).
// Root aliases in capability_resolver.go (package main) keep existing call
// sites unchanged.

package identity

import (
	"github.com/myrgic/cogos/pkg/substrate/capability"
)

// CapabilityResolver resolves agent URIs using the capability cache.
// It provides a lookup function that checks if an agent is known via
// its advertised capabilities before falling back to file-based resolution.
type CapabilityResolver struct {
	cache *CapabilityCache
}

// NewCapabilityResolver creates a resolver backed by the given cache.
func NewCapabilityResolver(cache *CapabilityCache) *CapabilityResolver {
	return &CapabilityResolver{cache: cache}
}

// ResolveAgent checks if an agent is registered in the capability cache
// and returns its capabilities payload and a boolean indicating presence.
// The caller can inspect the payload for endpoint, tools, MCP servers, etc.
//
// Usage with URI resolution:
//
//	parsed, _ := sdk.ParseURI("cog://agent/sandy")
//	if caps, ok := resolver.ResolveAgent(parsed.Path); ok {
//	    // Agent is live — use caps.Endpoint, caps.Tools, etc.
//	}
func (r *CapabilityResolver) ResolveAgent(agentID string) (*capability.Payload, bool) {
	caps := r.cache.Get(agentID)
	if caps == nil {
		return nil, false
	}
	return caps, true
}

// CanInvokeTool checks if a specific agent can handle a tool invocation
// based on its cached capabilities. Returns false if the agent is not
// registered or the tool is denied/not in the allow list.
func (r *CapabilityResolver) CanInvokeTool(agentID, tool string) bool {
	return r.cache.HasTool(agentID, tool)
}

// CanInvoke satisfies the engine.capabilityGater interface.
//
// Permit-by-default semantics: when the subject has no capability envelope
// in the cache (e.g. the agent never advertised its capabilities), the call
// is permitted. Restriction requires an explicit deny entry or a non-empty
// allow-list in the advertised Payload.
//
// This differs from CanInvokeTool (which returns false for unknown agents)
// so that the engine gating contract — "no envelope → no restriction" — is
// upheld without callers needing a separate nil-check.
func (r *CapabilityResolver) CanInvoke(subject, toolName string) bool {
	payload := r.cache.Get(subject)
	if payload == nil {
		// No envelope declared — open by default.
		return true
	}
	// Envelope present: apply allow/deny policy from cache.HasTool.
	return r.cache.HasTool(subject, toolName)
}

// ListAvailableAgents returns all agents currently registered in the cache
// (non-expired). The map is keyed by agent ID.
func (r *CapabilityResolver) ListAvailableAgents() map[string]capability.Payload {
	return r.cache.List()
}
