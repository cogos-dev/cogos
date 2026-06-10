// z_conversations_wire.go — thin caller that wires the Conversations
// Observatory into the kernel daemon.
//
// Prefixed "z_" so this init() runs after providers_wire.go (p < z),
// ensuring we chain on top of the eval-tools RegisterMCPExtensions set there.
//
// The logic formerly in this file has been extracted to
// internal/providers/all.RegisterConversations so it is importable by test
// helpers outside this package main.  This file keeps daemonConversationsProvider
// as a package-level var because it is referenced directly by the binary-assembly
// regression test in z_conversations_wire_test.go.
//
// PLACEMENT NOTE (preserved from original):
//   This wiring MUST live in cmd/cogos (the kernel daemon binary), not in
//   the repo-root package main.  The root package is the legacy cog CLI
//   monolith mid-decomposition; it never boots engine.Server, so any
//   SetConversationsResolver call placed there never runs in the daemon and
//   /v1/uri/resolve answers "resolver not wired".
package main

import (
	"github.com/myrgic/cogos/internal/conversations"
	"github.com/myrgic/cogos/internal/providers/all"
)

// daemonConversationsProvider is the shared provider singleton for both the
// reconcile registration and the MCP tool handlers.  The existing
// binary-assembly test (z_conversations_wire_test.go) reaches this var
// directly; keep it here rather than in internal/providers/all.
var daemonConversationsProvider = conversations.NewProvider()

func init() {
	all.RegisterConversations(daemonConversationsProvider)
}
