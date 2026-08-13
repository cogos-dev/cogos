// providers_register.go — registration hooks for Reconcilable providers
// and MCP tool extensions.
//
// RegisterProviders is a function variable that, when non-nil, is called once
// at daemon boot (inside runServe) before the HTTP server starts. The daemon
// binary (cmd/cogos) sets this variable from its own init() via an import
// of internal/providers/daemon, which triggers that package's init() and
// registers all 9 production providers with pkg/reconcile.
//
// SetProvidersWorkspace is called after LoadConfig resolves the workspace root
// so daemon-side providers can resolve their filesystem checks without
// depending on workspace.ResolveWorkspace()'s DI seams (which are not wired
// in the cmd/cogos binary).
//
// RegisterMCPExtensions is a function variable that, when non-nil, is called
// once when the MCP server is built inside registerMCPRoutes. It receives the
// live *MCPServer so that extension packages (e.g. internal/eval) can register
// additional MCP tools without importing internal/engine directly. Set by
// workspace-root wiring (e.g. eval_wiring.go).
//
// All hooks are intentionally nil in the engine package itself so that test
// binaries (which register stub providers directly) are not affected.
package engine

import "net/http"

// RegisterProviders is called once at daemon boot to populate the
// pkg/reconcile provider registry. Set by cmd/cogos/providers_wire.go.
// Nil means "no additional providers to register" (e.g. in tests).
var RegisterProviders func()

// SetProvidersWorkspace is called once after LoadConfig resolves
// cfg.WorkspaceRoot so daemon-side provider Health() implementations can
// perform real filesystem checks. Set by cmd/cogos/providers_wire.go.
// Nil means "workspace injection not requested" (e.g. in tests).
var SetProvidersWorkspace func(workspaceRoot string)

// RegisterMCPExtensions is called once when the MCP server is created during
// registerMCPRoutes. Extensions receive the live *MCPServer so they can call
// mcp.AddTool on its internal server. Set by workspace-root wiring.
// Nil means no extensions are registered.
var RegisterMCPExtensions func(srv *MCPServer)

// ApplyExtensions is the single shared post-construction step every MCPServer
// entrypoint MUST run before serving traffic, regardless of transport. It:
//
//  1. Calls RegisterMCPExtensions (if set) so the conversations/eval tool
//     families register on this server instance.
//  2. Re-runs backfillEagerSchemas, because extension hooks add EAGER tools
//     via TrackTool *after* the constructor's initial backfill ran, leaving
//     their inferred InputSchemas nil in toolMeta.
//  3. Re-snapshots toolDefs, because it too is first populated at
//     construction time — before extensions exist — and both
//     IsInternalTool/CallTool (in-process tool execution) and the
//     kernel-agent chat auto-advertise path key off it.
//
// This exists because myrgic/cogos#422 found the stdio entrypoint
// (runMCPServeEngine in cli_mcp.go) constructed an MCPServer and skipped this
// step entirely, silently exposing 14 of 21 tools over stdio while the HTTP
// entrypoint (registerMCPRoutes in serve_mcp.go) exposed all 21. Routing both
// transports through one method — instead of each re-implementing the same
// two-call sequence — is the fix: a future third entrypoint gets this for
// free, and there is no second list to fall out of sync.
func (m *MCPServer) ApplyExtensions() {
	if RegisterMCPExtensions != nil {
		RegisterMCPExtensions(m)
	}
	m.backfillEagerSchemas()
	m.toolDefs = snapshotToolDefinitions(m.server)
}

// WireHarnessBackend is called once during Boot after NewServer, to wire the
// RBAC harness-binding layer into the server so cog_register_session can
// create HarnessBindingCRDs for sessions that supply a "subject" field.
// Set by cmd/cogos/providers_wire.go with a concrete HarnessAttacher impl.
// Nil means the session-register path proceeds without identity binding
// (naked-by-default: safe, correct, no functional change for existing callers).
var WireHarnessBackend func(s *Server)

// WireProviderRuntime is called once during Boot after NewServer (and after
// WireHarnessBackend), so provider packages that need post-boot kernel
// handles — the running *Process for ledger events, the *BusSessionManager /
// *BusEventBroker for the SSE bus — can be wired without internal/engine
// importing them directly. Set by internal/providers/all.Register, which
// looks the provider up via pkg/substrate/reconcile.GetProvider and calls its
// exported setter (e.g. marginbridge.Provider.SetEventSink) with an adapter
// built from Server.Process()/BusSessions()/BusBroker(). Nil means no
// provider needs post-boot runtime wiring (e.g. in tests).
var WireProviderRuntime func(s *Server)

// RegisterHTTPExtensions is called once during NewServer after the built-in
// routes are registered. Extensions receive the *Server and *http.ServeMux so
// they can call s.route() to register additional HTTP endpoints (e.g. the
// observatory coverage route). Set by cmd/cogos extension init() functions.
// Nil means no additional routes are registered.
var RegisterHTTPExtensions func(s *Server, mux *http.ServeMux)

// WireConstellationIndexer is called once during Boot after NewServer, to wire
// a live ConstellationIndexer (the *constellation.Constellation handle from the
// root package) into the server so that CogDocService.WriteAndSync /
// PatchAndSync perform an eager per-file FTS upsert, and so that the lazy
// drift-repair path in searchMemoryFTSDriftRepair can call IndexFile without
// importing sdk/constellation (package-boundary guard).
// Set by cmd/cogos/providers_wire.go with the concrete *constellation.Constellation.
// Nil means eager upsert and drift repair are disabled (degraded mode; safe).
var WireConstellationIndexer func(s *Server)

// ExtraCogdocRootsFunc resolves the workspace-root cogdoc directories declared
// via .cog/config/cogdocs.yaml requiredPaths that fall OUTSIDE .cog/ (the same
// extra roots sdk/constellation's IndexWorkspace walk covers). Both Boot's
// live mem_watcher wiring and the lazy drift-repair sampling in
// mcp_stubs.go use this so incremental/live indexing stays consistent with
// what a full reindex covers — without internal/engine importing
// sdk/constellation directly (package-boundary guard: only cli_*.go files in
// this package may import sdk/constellation; the daemon path Boot→MCPServer
// must not).
// Set by cmd/cogos/providers_wire.go to sdk/constellation.ExtraCogdocRoots.
// Nil means no extra roots are considered (degraded mode: only .cog/mem is
// live-watched/repaired; a full `cogos reindex` still covers widened roots).
var ExtraCogdocRootsFunc func(workspaceRoot string) []string
