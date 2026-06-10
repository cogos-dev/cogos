// z_daemon_wiring_test.go — binary-assembly regression guard for the full
// cmd/cogos provider and MCP-extension wiring.
//
// Context (ADR-101 test-gap C):
//
//	PR #370 shipped a dead daemon route: the conversations URI resolver was
//	placed in the repo-root package main (never linked into cmd/cogos), so
//	42 green unit tests passed while GET /v1/uri/resolve answered "not wired"
//	on a live kernel.  z_conversations_wire_test.go guards the resolver seam.
//
//	Gap C is the generalisation: zero tests verified that cmd/cogos as a whole
//	assembles the expected MCP surface.  A future change could drop an import
//	or an init() call and all existing tests would still pass.
//
// This test closes gap C by booting a kernel via internal/testkernel with the
// PRODUCTION registration set (exactly the init() chain that runs in the
// daemon binary) and asserting over the live MCP wire protocol:
//
//  1. The golden tool set (one representative per wired subsystem) is present.
//  2. The conversations URI resolver seam is populated (engine-level check).
//  3. The harness-backend wiring hook is set (engine-level check).
//
// Running inside package main ensures this binary's init() chain — the exact
// wiring the daemon boots with — has executed by the time the test runs.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/testkernel"
)

// goldenTools is the minimal set of MCP tool names that must be present on
// the live daemon surface.  One tool per wired subsystem, matching the e2e
// probe in scripts/e2e-test.sh (Phase 3b section).
//
// Adding a tool here intentionally: the set is deliberately small so that
// normal tool-name churn doesn't require constant test updates.  The goal is
// to detect a dropped registration chain, not to enumerate every tool.
var goldenTools = []string{
	"cog_get_state",           // core kernel MCP (mcp_server.go)
	"cog_search_memory",       // core kernel MCP (mcp_server.go)
	"cog_search_conversations", // conversations layer (z_conversations_wire.go)
	"cog_list_sessions",       // session tools (mcp_sessions.go)
	"cog_read_cogdoc",         // core kernel MCP (mcp_server.go)
}

// TestDaemonWiring boots the kernel daemon with the production registration
// set and asserts the golden MCP surface over the live wire protocol.
//
// This test is the binary-assembly guard for cmd/cogos as a whole.  It
// complements the seam-level checks in z_conversations_wire_test.go by
// exercising the full MCP surface rather than a single engine variable.
func TestDaemonWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("TestDaemonWiring: skipped in -short mode (boots a live kernel)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Boot with the production registration set.  init() in providers_wire.go
	// and z_conversations_wire.go has already fired by the time this line runs;
	// engine.RegisterMCPExtensions holds the full eval+conversations chain.
	k, err := testkernel.Boot(ctx, t)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	// ── 1. Golden tool set ────────────────────────────────────────────────────

	tools, err := k.ListTools(ctx, t)
	if err != nil {
		t.Fatalf("k.ListTools: %v", err)
	}

	// Build a presence map for O(1) lookup.
	toolSet := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		toolSet[name] = struct{}{}
	}

	for _, want := range goldenTools {
		if _, ok := toolSet[want]; !ok {
			t.Errorf("golden tool missing from live MCP surface: %q\n"+
				"  hint: check that the registration chain in cmd/cogos "+
				"(providers_wire.go / z_conversations_wire.go) was not broken\n"+
				"  registered tools: %v", want, tools)
		}
	}

	// ── 2. Conversations resolver seam ────────────────────────────────────────
	// init() in z_conversations_wire.go must have called
	// engine.SetConversationsResolver; if it didn't, GET /v1/uri/resolve will
	// answer "resolver not wired" on a live kernel.

	if r := engine.WiredConversationsResolver(); r == nil {
		t.Error("engine conversations resolver is nil after cmd/cogos init — " +
			"RegisterConversations wiring is missing from the daemon binary " +
			"(it must reach engine.SetConversationsResolver via z_conversations_wire.go)")
	}

	// ── 3. Harness-backend wiring hook ────────────────────────────────────────
	// init() in providers_wire.go must have set engine.WireHarnessBackend so
	// that cog_register_session can create HarnessBindingCRDs for sessions that
	// supply a "subject" field.  A nil hook means the harness layer is dark and
	// RBAC session binding silently does nothing.

	if engine.WireHarnessBackend == nil {
		t.Error("engine.WireHarnessBackend is nil after cmd/cogos init — " +
			"the harness-backend wiring hook is missing from the daemon binary " +
			"(it must be set in providers_wire.go or all.Register)")
	}
}
