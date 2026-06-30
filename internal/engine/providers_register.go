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

// WireHarnessBackend is called once during Boot after NewServer, to wire the
// RBAC harness-binding layer into the server so cog_register_session can
// create HarnessBindingCRDs for sessions that supply a "subject" field.
// Set by cmd/cogos/providers_wire.go with a concrete HarnessAttacher impl.
// Nil means the session-register path proceeds without identity binding
// (naked-by-default: safe, correct, no functional change for existing callers).
var WireHarnessBackend func(s *Server)

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
