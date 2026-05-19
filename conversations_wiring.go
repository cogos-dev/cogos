// conversations_wiring.go — registers ConversationsProvider with the reconcile
// engine.
//
// This file handles the reconcile.RegisterProvider call for the conversations
// resource type. MCP tool wiring is handled in conversations_mcp_wire.go,
// which runs after eval_wiring.go (alphabetically) so it can chain the existing
// RegisterMCPExtensions rather than overwriting it.
//
// Provider singleton is shared between reconcile and MCP tool handlers so that
// cog_search_conversations queries see the freshest indexed state without an
// extra round-trip.

package main

import (
	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/pkg/reconcile"
)

// conversationsProviderInstance is the singleton registered with the reconcile
// registry. Shared with MCP tools in conversations_mcp_wire.go.
var conversationsProviderInstance = conversations.NewProvider()

func init() {
	// Register the provider with the reconcile registry.
	reconcile.RegisterProvider("conversations", conversationsProviderInstance)
}
