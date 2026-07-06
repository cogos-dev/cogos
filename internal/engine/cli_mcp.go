// cli_mcp.go — `cogos mcp serve` subcommand: run the engine MCPServer on stdio.
//
// Phase 2 of Track 5 (per Agent I2's revised dead-code plan): this engine
// implementation exists alongside the root-package `cmdMCP` (mcp.go). The
// root-linked `cogos` binary still dispatches to its own path today. Once
// Phase 4 flips the Makefile default build target to cmd/cogos/, `cogos mcp
// serve` will naturally route through here.
//
// Byte-compat and feature diff vs root:
//
//   - Transport: both use stdio with newline-delimited JSON-RPC 2.0.
//     Root implements the JSON-RPC loop by hand (mcp.go:515 Run); engine
//     uses the upstream modelcontextprotocol/go-sdk StdioTransport which
//     speaks the same wire format.
//
//   - Tool catalogue: root's cmdMCP registers the 4 kernel-native tools
//     (memory_search / memory_read / memory_write / coherence_check) plus
//     the bridge-mode external-tool loader. Engine's MCPServer registers
//     the FULL engine tool catalogue (20+ tools including ledger, traces,
//     config, agent-state, kernel-slog, tool-calls, conversation, event-
//     bus) via registerTools in mcp_server.go.
//
//     This is a strict super-set; every root tool has an engine equivalent.
//     When the Phase 4 Makefile switch lands, `cogos mcp serve` users get
//     the richer surface without any additional work.
//
//   - Bridge mode: root's --bridge flag (OpenClaw gateway) is not mirrored
//     in Phase 2. It is scoped for a follow-up if/when needed; bridge mode
//     is not used by the kernel-native workflow.
//
// Lifecycle:
//
//	cogos mcp serve [--workspace PATH]
//
// Workspace resolution mirrors other engine subcommands (auto-detected via
// findWorkspaceRoot when --workspace is absent). The server runs until the
// client closes stdin (EOF) or SIGINT/SIGTERM is received, then returns 0.
package engine

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCPCmd dispatches `cogos mcp <subcommand>` arguments. Today only
// "serve" is supported; other subcommands return an explicit error so
// typos don't silently become no-ops.
//
// Returns an exit code (0 on clean shutdown). The caller in Main() is
// responsible for calling os.Exit.
func runMCPCmd(args []string, defaultWorkspace string) int {
	return runMCPCmdWithIO(args, defaultWorkspace, os.Stderr)
}

// runMCPCmdWithIO is the testable core. stderr defaults to os.Stderr when
// nil is passed. stdin/stdout are always the process streams because the
// SDK's StdioTransport binds to them directly; for unit tests we inject a
// transport via runMCPServeWithTransport below.
func runMCPCmdWithIO(args []string, defaultWorkspace string, stderr io.Writer) int {
	if stderr == nil {
		stderr = os.Stderr
	}

	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: cogos mcp serve [--workspace PATH]")
		return 1
	}

	switch args[0] {
	case "serve":
		return runMCPServeEngine(args[1:], defaultWorkspace, stderr)
	default:
		fmt.Fprintf(stderr, "Unknown mcp subcommand: %s\n", args[0])
		return 1
	}
}

// runMCPServeEngine parses `cogos mcp serve` flags and hands the mcp.Server
// off to StdioTransport. Intentionally does NOT call os.Exit so tests can
// assert on the returned code. On any initialization failure a short error
// goes to stderr and we return 1.
func runMCPServeEngine(args []string, defaultWorkspace string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Resolve workspace. Mirror the emit cmd's behavior — defer to
	// findWorkspaceRoot from cwd when the flag is empty.
	wsRoot := *workspace
	if wsRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "error: getwd: %v\n", err)
			return 1
		}
		ws, err := findWorkspaceRoot(wd)
		if err != nil {
			fmt.Fprintf(stderr, "error: could not detect workspace: %v\n", err)
			return 1
		}
		wsRoot = ws
	}

	cfg, err := LoadConfig(wsRoot, 0)
	if err != nil {
		fmt.Fprintf(stderr, "error: load config: %v\n", err)
		return 1
	}

	nucleus, err := LoadNucleus(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: load nucleus: %v\n", err)
		return 1
	}

	process := NewProcess(cfg, nucleus)
	server := NewMCPServer(cfg, nucleus, process)

	// Wire session-management backends so cog_register_session /
	// cog_list_sessions / cog_offer_handoff / cog_list_handoffs / etc. work
	// over stdio (same registries the HTTP path uses, just no live Server).
	//
	// forkRegistry mirrors serve.go's wiring (SetForkRegistry) so
	// cog_fork_session over stdio also gets a live lineage index — without
	// this, the stdio MCP path silently skipped fork-registry updates
	// entirely (m.forkRegistry stayed nil; see the nil-safe check in
	// mcp_fork_session.go). ReplaySessionRegistry's session.fork case and
	// ReplayForkRegistry both read the durable session.fork bus events so a
	// restart of this process reconstructs prior forks instead of starting
	// blank.
	busSessions := NewBusSessionManager(cfg.WorkspaceRoot)
	sessionRegistry := NewSessionRegistry()
	handoffRegistry := NewHandoffRegistry()
	forkRegistry := NewForkRegistry()
	_ = ReplaySessionRegistry(busSessions, sessionRegistry)
	_ = ReplayHandoffRegistry(busSessions, handoffRegistry)
	_ = ReplayForkRegistry(busSessions, forkRegistry)
	server.SetSessionsBackend(busSessions, sessionRegistry, handoffRegistry)
	server.SetForkRegistry(forkRegistry)

	// Wire a signal-aware context so shells (or hosts like Claude Desktop)
	// that send SIGINT/SIGTERM on shutdown get a clean exit.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := server.RunStdio(ctx); err != nil {
		// EOF-style termination from the client returns nil from Run; any
		// other error reaches here.
		fmt.Fprintf(stderr, "mcp serve: %v\n", err)
		return 1
	}
	return 0
}

// RunStdio runs the MCP server on the upstream SDK's StdioTransport, which
// reads/writes newline-delimited JSON-RPC 2.0 on os.Stdin / os.Stdout. This
// is the transport Claude Desktop, Cursor, Windsurf, etc. all speak.
//
// Blocks until ctx is cancelled or the client closes the stream. Returns
// nil on clean EOF; otherwise the underlying transport error.
func (m *MCPServer) RunStdio(ctx context.Context) error {
	return m.server.Run(ctx, &mcp.StdioTransport{})
}

// runServerOnTransport is a test hook: runs an already-built MCPServer on an
// arbitrary SDK transport. Not part of the public Engine API; exists so the
// unit tests can use NewInMemoryTransports to verify flag parsing and
// dispatch without touching real stdin/stdout.
func runServerOnTransport(ctx context.Context, m *MCPServer, t mcp.Transport) error {
	return m.server.Run(ctx, t)
}
