// z_conversations_mcp.go — wires Conversations Observatory MCP tools.
//
// Prefixed with "z_" so this file's init() is guaranteed to run after
// eval_wiring.go (e < z alphabetically). This ensures that when we capture
// engine.RegisterMCPExtensions, it already holds the eval-tools registration
// set by eval_wiring.go, and we chain on top of it rather than overwriting it.
//
// Tools registered:
//   cog_search_conversations  — full-text search over indexed operator turns
//   cog_get_conversation_turn — fetch one turn by session_id + turn_index
//   cog_list_conversations    — list indexed sessions with metadata

package main

import (
	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/internal/engine"
)

func init() {
	// Capture the previously registered extension chain (set by eval_wiring.go).
	// eval_wiring.go registers eval tools; we chain conversations tools after.
	prev := engine.RegisterMCPExtensions
	engine.RegisterMCPExtensions = func(srv *engine.MCPServer) {
		if prev != nil {
			prev(srv)
		}
		conversations.RegisterConversationTools(srv.Server(), conversationsProviderInstance)
	}
}
