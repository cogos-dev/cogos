// z_conversations_wire_test.go — binary-assembly regression guard for the
// conversations URI resolver wiring.
//
// PR #370's first cut placed the engine.SetConversationsResolver call in the
// repo-root package main (the legacy cog CLI monolith), which is never linked
// into this daemon binary. Every unit test passed while GET /v1/uri/resolve
// answered "conversations resolver not wired" on a live kernel. These tests
// close that gap: they run inside the cmd/cogos package, so the package's
// init() chain — the exact wiring the daemon boots with — has executed by the
// time they run.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/myrgic/cogos/internal/engine"
)

// TestConversationsResolverWiredIntoEngine asserts that after this binary's
// standard init() sequence, the engine's conversations resolver seam is
// populated. If someone moves or deletes the SetConversationsResolver call,
// this fails at `go test ./cmd/cogos` instead of at runtime on a live kernel.
func TestConversationsResolverWiredIntoEngine(t *testing.T) {
	r := engine.WiredConversationsResolver()
	if r == nil {
		t.Fatal("engine conversations resolver is nil after cmd/cogos init — " +
			"SetConversationsResolver wiring is missing from the daemon binary " +
			"(it must live in cmd/cogos, not the repo-root package main)")
	}
}

// TestConversationsResolverDelegatesToProvider proves the wired resolver is
// actually connected to the daemon's conversations provider singleton (not a
// stub): URI validation errors from the resolver must surface through the
// engine seam once the provider's index is initialised.
func TestConversationsResolverDelegatesToProvider(t *testing.T) {
	r := engine.WiredConversationsResolver()
	if r == nil {
		t.Fatal("resolver not wired (see TestConversationsResolverWiredIntoEngine)")
	}

	// Initialise the shared provider's index against a scratch workspace so
	// resolution gets past the "index not initialised" guard. This mirrors
	// what engine.SetProvidersWorkspace does at daemon boot.
	if _, err := daemonConversationsProvider.LoadConfig(t.TempDir()); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ctx := context.Background()

	// A valid URI over the (empty) index resolves cleanly.
	if _, err := r.ResolveURI(ctx, "cog:conversations?limit=1"); err != nil {
		t.Fatalf("valid URI should resolve against an initialised empty index, got: %v", err)
	}

	// Unknown-param rejection must propagate through the engine seam.
	_, err := r.ResolveURI(ctx, "cog:conversations?frobnicate=1")
	if err == nil || !strings.Contains(err.Error(), "unknown query parameter") {
		t.Fatalf("want unknown-param error through the wired resolver, got: %v", err)
	}

	// component= is now an active param (v0.2 ontology-as-class enforcement);
	// it should resolve without a "reserved" error (the empty index returns
	// an empty slice, not an error).
	_, err = r.ResolveURI(ctx, "cog:conversations?component=session.turn")
	if err != nil {
		t.Fatalf("component= param should be accepted (v0.2 active), got: %v", err)
	}
}
