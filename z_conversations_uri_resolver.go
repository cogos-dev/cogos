// z_conversations_uri_resolver.go — wires the cog:conversations URI resolver
// into the kernel HTTP server (GET /v1/uri/resolve).
//
// The conversations package cannot be imported by internal/engine (circular
// import). This file, which is in package main (root), bridges the gap:
// it wraps conversations.Provider.ResolveURI in a ConversationsResolver
// adapter and registers it via engine.SetConversationsResolver.
//
// Prefixed "z_" so this init() runs after z_conversations_mcp.go, which sets
// conversationsProviderInstance.
package main

import (
	"context"
	"fmt"

	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/internal/engine"
)

// providerURIResolver implements engine.ConversationsResolver by delegating to
// Provider.ResolveURI, which accesses the live index safely under a lock.
type providerURIResolver struct {
	p *conversations.Provider
}

func (r *providerURIResolver) ResolveURI(_ context.Context, uri string) (any, error) {
	slice, err := r.p.ResolveURI(uri)
	if err != nil {
		return nil, err
	}
	if slice == nil {
		return nil, fmt.Errorf("conversations: nil slice from resolver")
	}
	return slice, nil
}

func init() {
	engine.SetConversationsResolver(&providerURIResolver{
		p: conversationsProviderInstance,
	})
}
