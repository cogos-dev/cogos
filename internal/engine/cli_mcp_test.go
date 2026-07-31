package engine

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRunMCPCmd_UsageAndSubcommandDispatch pins the arg-parsing contract:
//   - empty args → usage text on stderr, exit 1
//   - unknown subcommand → error on stderr, exit 1
//   - "serve" is the only accepted subcommand today
//
// We don't actually start the server here — the serve path is exercised via
// an in-memory transport in the round-trip test below.
func TestRunMCPCmd_UsageAndSubcommandDispatch(t *testing.T) {
	t.Parallel()

	t.Run("no args → usage on stderr, exit 1", func(t *testing.T) {
		t.Parallel()
		var stderr bytes.Buffer
		code := runMCPCmdWithIO(nil, "", &stderr)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
		if !strings.Contains(stderr.String(), "Usage: cogos mcp serve") {
			t.Errorf("stderr missing usage line; got %q", stderr.String())
		}
	})

	t.Run("unknown subcommand → error on stderr, exit 1", func(t *testing.T) {
		t.Parallel()
		var stderr bytes.Buffer
		code := runMCPCmdWithIO([]string{"bogus"}, "", &stderr)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
		if !strings.Contains(stderr.String(), "Unknown mcp subcommand: bogus") {
			t.Errorf("stderr missing unknown-subcommand line; got %q", stderr.String())
		}
	})

	t.Run("serve with bad flag → exit 1", func(t *testing.T) {
		t.Parallel()
		var stderr bytes.Buffer
		code := runMCPCmdWithIO([]string{"serve", "--not-a-real-flag"}, "", &stderr)
		if code != 1 {
			t.Errorf("exit = %d; want 1", code)
		}
	})
}

// TestRunMCPCmd_ServeWithoutWorkspace verifies that when --workspace is
// empty and no .cog/ exists in the cwd or ancestors, the command fails
// cleanly (exit 1) with a workspace-detection error on stderr instead of
// panicking or hanging.
func TestRunMCPCmd_ServeWithoutWorkspace(t *testing.T) {
	// Not parallel: t.Chdir is incompatible with t.Parallel and the cwd is
	// process-global anyway.
	t.Chdir(t.TempDir())

	var stderr bytes.Buffer
	code := runMCPCmdWithIO([]string{"serve"}, "", &stderr)
	if code != 1 {
		t.Errorf("exit = %d; want 1 (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not detect workspace") &&
		!strings.Contains(stderr.String(), "load config") {
		t.Errorf("stderr should mention workspace detection; got %q", stderr.String())
	}
}

// TestMCPServer_RunStdio_RoundTrip exercises the real `server.Run(ctx, t)`
// path via the SDK's in-memory transport pair. This is the byte-compat
// check that matters: after an MCP client connects + initializes, the
// server advertises the engine tool catalogue the caller expects.
//
// We don't use StdioTransport directly because it binds to os.Stdin/Stdout;
// InMemoryTransport speaks the same newline-delimited JSON-RPC framing.
func TestMCPServer_RunStdio_RoundTrip(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Cog", "tester")
	process := NewProcess(cfg, nucleus)
	server := NewMCPServer(cfg, nucleus, process)

	// Set up paired in-memory transports.
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run the server in a goroutine. runServerOnTransport is the test hook
	// exposed in cli_mcp.go.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runServerOnTransport(ctx, server, serverTransport)
	}()

	// Connect a client. Client.Connect performs the initialize handshake,
	// so if the server mis-speaks the protocol this call returns an error.
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer func() {
		_ = session.Close()
		cancel()
		// Give the server goroutine a short grace period to unwind so it
		// doesn't race with test cleanup.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server goroutine did not exit within 5s after Close+cancel")
		}
	}()

	// Ping works.
	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("session.Ping: %v", err)
	}

	// Tools list must include the kernel-native set. This is the byte-compat
	// guarantee against root's cogos mcp serve: the four tools that root
	// registered (memory_search / memory_read / memory_write / coherence)
	// are present here under their engine names.
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("session.ListTools: %v", err)
	}
	wantAny := []string{
		"cog_search_memory",
		"cog_read_cogdoc",
		"cog_write_cogdoc",
		"cog_check_coherence",
	}
	got := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	for _, name := range wantAny {
		if !got[name] {
			t.Errorf("ListTools missing %q; have %d tools, got names: %v",
				name, len(tools.Tools), toolNames(tools.Tools))
		}
	}
	// Phase 2 goal: engine exposes the FULL catalogue, not just the four.
	// We assert >= 10 to catch accidental registerTools regressions without
	// pinning the exact count (which grows as new tools land).
	if len(tools.Tools) < 10 {
		t.Errorf("ListTools returned %d tools; want at least 10 (engine catalogue)",
			len(tools.Tools))
	}
}

// toolNames extracts the Name field from a []*mcp.Tool into a sorted-ish
// slice for readable error messages. Order is preserved as returned by the
// server.
func toolNames(ts []*mcp.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// TestNewStdioMCPServer_AppliesExtensions is the regression test for
// myrgic/cogos#422: the stdio entrypoint (newStdioMCPServer, used by
// runMCPServeEngine) must call ApplyExtensions so any tool registered via the
// RegisterMCPExtensions hook — the mechanism cmd/cogos/providers_wire.go uses
// to chain in the conversations/eval tool families — appears in stdio's
// tools/list, exactly as it already does for the HTTP transport
// (registerMCPRoutes in serve_mcp.go). Before the fix, newStdioMCPServer's
// predecessor code never called RegisterMCPExtensions at all, so a fake
// extension registered here would be invisible over stdio while still
// showing up if the same MCPServer were wired the HTTP way.
func TestNewStdioMCPServer_AppliesExtensions(t *testing.T) {
	// Not parallel: mutates the package-level RegisterMCPExtensions hook.
	prev := RegisterMCPExtensions
	t.Cleanup(func() { RegisterMCPExtensions = prev })

	const markerTool = "test_extension_marker_tool"
	RegisterMCPExtensions = func(srv *MCPServer) {
		mcp.AddTool(srv.Server(), &mcp.Tool{
			Name:        markerTool,
			Description: "marker tool standing in for a real extension (conversations/eval) for #422 regression coverage",
		}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	}

	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Cog", "tester")
	process := NewProcess(cfg, nucleus)

	// This is the exact call runMCPServeEngine makes; testing it directly
	// (rather than round-tripping through flag parsing + real stdio) lets us
	// use the in-memory transport pair like the round-trip test above does.
	server := newStdioMCPServer(cfg, nucleus, process)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runServerOnTransport(ctx, server, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer func() {
		_ = session.Close()
		cancel()
		wg.Wait()
	}()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("session.ListTools: %v", err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == markerTool {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stdio tools/list missing extension tool %q registered via RegisterMCPExtensions; "+
			"got %d tools: %v — ApplyExtensions was not run", markerTool, len(tools.Tools), toolNames(tools.Tools))
	}

	// ToolDefinitions() (the snapshot IsInternalTool/CallTool and the
	// kernel-agent chat auto-advertise path key off) must also see it —
	// ApplyExtensions refreshes this snapshot, not just tools/list.
	defs := server.ToolDefinitions()
	defFound := false
	for _, d := range defs {
		if d.Name == markerTool {
			defFound = true
			break
		}
	}
	if !defFound {
		t.Errorf("ToolDefinitions() missing extension tool %q; toolDefs snapshot was not refreshed by ApplyExtensions", markerTool)
	}
}
