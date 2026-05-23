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
	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

var daemonConversationsProvider = conversations.NewProvider()

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
		conversations.RegisterConversationTools(srv.Server(), daemonConversationsProvider)
	}
}
