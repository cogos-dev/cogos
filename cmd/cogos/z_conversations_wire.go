// z_conversations_wire.go — wires the Conversations Observatory into the
// kernel daemon.
//
// Prefixed "z_" so this init() runs after providers_wire.go (p < z), ensuring
// we chain on top of the eval-tools RegisterMCPExtensions set there.
//
// The daemon does not run plan/apply for the conversations provider
// (that is the cog CLI's job via the workspace-root reconcile loop).
// However the daemon DOES need the MCP tools registered so agents can
// call cog_search_conversations from within a daemon-mediated session.
//
// The conversations provider singleton is shared between the reconcile
// registration (wired here) and the MCP tool handlers. The provider's
// LoadConfig is called lazily the first time the MCP tools are invoked
// (or when engine.SetProvidersWorkspace fires), so the daemon starts
// cleanly even before a workspace is configured.
package main

import (
	"context"

	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

var daemonConversationsProvider = conversations.NewProvider()

// daemonURIResolver adapts the conversations provider to the
// engine.ConversationsResolver interface backing GET /v1/uri/resolve.
//
// Placement matters: this wiring MUST live in cmd/cogos (the kernel daemon
// binary), not in the repo-root package main. The root package is the legacy
// cog CLI monolith mid-decomposition; it never boots engine.Server, so any
// SetConversationsResolver call placed there never runs in the daemon and
// /v1/uri/resolve answers "resolver not wired" — the exact bug this comment
// guards against (caught by real-data e2e review of PR #370).
type daemonURIResolver struct {
	p *conversations.Provider
}

func (r *daemonURIResolver) ResolveURI(_ context.Context, uri string) (any, error) {
	return r.p.ResolveURI(uri)
}

func init() {
	// Register the conversations provider with the reconcile registry so the
	// daemon's proprioception Health() block includes it.
	reconcile.RegisterProvider("conversations", daemonConversationsProvider)

	// Wire workspace root injection — called by SetProvidersWorkspace after
	// LoadConfig resolves cfg.WorkspaceRoot. Triggers index initialisation.
	prevSetWorkspace := engine.SetProvidersWorkspace
	engine.SetProvidersWorkspace = func(workspaceRoot string) {
		if prevSetWorkspace != nil {
			prevSetWorkspace(workspaceRoot)
		}
		if workspaceRoot != "" {
			// Initialise the index against the resolved workspace root.
			// Errors are non-fatal — the provider degrades gracefully.
			_, _ = daemonConversationsProvider.LoadConfig(workspaceRoot)
		}
	}

	// Chain conversations MCP tools after the eval tools registered by
	// providers_wire.go (p < z ensures providers_wire.go init() ran first).
	prev := engine.RegisterMCPExtensions
	engine.RegisterMCPExtensions = func(srv *engine.MCPServer) {
		if prev != nil {
			prev(srv)
		}
		conversations.RegisterConversationTools(srv.Server(), srv.TrackTool, daemonConversationsProvider, srv.MaxToolOutputBytes())
	}

	// Wire the cog:conversations URI resolver behind GET /v1/uri/resolve.
	// Shares the same provider singleton as the MCP tools so both surfaces
	// see the same live index.
	engine.SetConversationsResolver(&daemonURIResolver{p: daemonConversationsProvider})
}
