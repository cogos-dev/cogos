// serve_manifest.go — self-describing kernel manifest endpoint.
//
//	GET /v1/manifest → JSON describing this kernel's full surface
//
// The manifest enumerates every HTTP route and MCP tool at runtime by walking
// registries populated during startup — no hardcoded lists. If a future wave
// adds a new route or tool, the manifest reflects it without touching this
// file.
//
// Design:
//   - HTTP routes are captured at mux-registration time via Server.route and
//     Server.routeH, which forward to http.ServeMux and also append to
//     s.httpRoutes.
//   - MCP tools are captured via MCPServer.trackTool, which records the
//     *mcp.Tool metadata into m.toolMeta and then returns the pointer so the
//     caller threads it through to mcp.AddTool unchanged.
//
// Why not extend /v1/card? The existing /v1/card endpoint is an OpenClaw v2
// compatibility stub with a specific contract (models list, capabilities
// booleans, `endpoints` string map). Reshaping it would break that
// contract. /v1/manifest lives alongside as the CogOS-native surface for
// kernel introspection.
package engine

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// routeMeta records one HTTP route registration.
type routeMeta struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Family string `json:"family"`
}

// mcpToolMeta records one MCP tool registration.
type mcpToolMeta struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Family      string `json:"family"`
	// Eager reports whether this tool was registered on the live mcp.AddTool
	// path (porcelain — appears in tools/list) versus the deferred catalog
	// reached via cog_tool_search/cog_tool_invoke (plumbing — invoke-only).
	// See trackToolDeferred and the tier-then-trim mechanism doc.
	Eager bool `json:"eager"`
	// InputSchema is the tool's JSON Schema, captured off the *mcp.Tool
	// pointer in trackTool/trackToolDeferred BEFORE the pointer is handed to
	// mcp.AddTool (which internally copies via `tt := *t`, so reading the
	// schema off the original after that point would still work for the
	// pointer itself, but capturing here keeps deferred tools — which never
	// reach mcp.AddTool at all — populated the same way as eager ones).
	InputSchema any `json:"input_schema,omitempty"`
}

// route registers a handler on mux and records the (method, path) tuple onto
// s.httpRoutes. The pattern follows http.ServeMux's method-prefixed form,
// e.g. "GET /v1/health" or "POST /v1/sessions/{id}/heartbeat".
//
// Every handler is automatically wrapped by withSpan so that each invocation
// emits a KernelHandlerSpan to the bus_traces bus. The handler name is derived
// from the pattern (method + path) rather than reflection; it is stable and
// readable in log output.
func (s *Server) route(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	method, path := splitRoutePattern(pattern)
	handlerName := method + " " + path
	mux.HandleFunc(pattern, withSpan(handlerName, handler, s.spanEmitter))
	s.recordRoute(pattern)
}

// Route is the exported variant of route for use by RegisterHTTPExtensions
// callbacks (which live outside the engine package).
func (s *Server) Route(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	s.route(mux, pattern, handler)
}

// routeH is like route but takes an http.Handler (used for /mcp which is
// backed by the streamable HTTP handler from the MCP library).
func (s *Server) routeH(mux *http.ServeMux, pattern string, handler http.Handler) {
	mux.Handle(pattern, handler)
	s.recordRoute(pattern)
}

// recordRoute parses "METHOD /path" and appends a routeMeta entry.
func (s *Server) recordRoute(pattern string) {
	method, path := splitRoutePattern(pattern)
	s.httpRoutes = append(s.httpRoutes, routeMeta{
		Method: method,
		Path:   path,
		Family: classifyHTTPFamily(path),
	})
}

// splitRoutePattern parses "GET /foo" into ("GET", "/foo"). Patterns without
// a method prefix ("/foo") degrade to method="" path="/foo" — the http.ServeMux
// accepts that form too but the kernel uses method-prefixed form everywhere.
func splitRoutePattern(pattern string) (method, path string) {
	p := strings.TrimSpace(pattern)
	if i := strings.IndexByte(p, ' '); i > 0 {
		return p[:i], strings.TrimSpace(p[i+1:])
	}
	return "", p
}

// classifyHTTPFamily maps an HTTP path prefix to a family tag. The map is
// kept compact; ordering matters because longer prefixes win. New routes that
// don't match fall into "misc".
func classifyHTTPFamily(path string) string {
	// Prefix table, scanned in order. Longer prefixes first so /v1/bus beats
	// any hypothetical /v1 fallback.
	prefixes := []struct {
		prefix, family string
	}{
		{"/mcp", "mcp"},
		{"/v1/chat/completions", "openai"},
		{"/v1/models", "openai"},
		{"/v1/messages", "anthropic"},
		{"/v1/bus", "bus"},
		{"/v1/events", "bus"},
		{"/v1/blocks", "bus"},
		{"/v1/channel-sessions", "sessions"},
		{"/v1/sessions", "sessions"},
		{"/v1/handoffs", "sessions"},
		{"/v1/cogdoc", "memory"},
		{"/v1/context", "memory"},
		{"/v1/resolve", "memory"},
		{"/memory", "memory"},
		{"/v1/ledger", "observability"},
		{"/v1/traces", "observability"},
		{"/v1/tool-calls", "observability"},
		{"/v1/kernel-log", "observability"},
		{"/v1/conversation", "observability"},
		{"/v1/proprioceptive", "observability"},
		{"/v1/debug", "observability"},
		{"/v1/attention", "attention"},
		{"/v1/constellation", "attention"},
		{"/v1/observer", "attention"},
		{"/v1/lightcone", "attention"},
		{"/v1/services", "services"},
		{"/v1/card", "compat"},
		{"/v1/providers", "compat"},
		{"/v1/taa", "compat"},
		{"/coherence", "kernel"},
		{"/v1/config", "config"},
		{"/v1/manifest", "kernel"},
		{"/health", "kernel"},
		{"/canvas", "kernel"},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p.prefix) {
			return p.family
		}
	}
	if path == "/" {
		return "kernel"
	}
	return "misc"
}

// classifyMCPFamily takes the prefix before the first underscore as the
// family. Tools without an underscore return "misc".
func classifyMCPFamily(name string) string {
	if i := strings.IndexByte(name, '_'); i > 0 {
		return name[:i]
	}
	return "misc"
}

// trackTool records the tool metadata into m.toolMeta and returns the input
// pointer unchanged. Typical usage:
//
//	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{Name: "cog_search_memory", ...}), handler)
//
// Returning the pointer means each existing registration changes by one
// wrapping call — no separate metadata list, no risk of drift.
//
// InputSchema capture: t.InputSchema is read here, BEFORE the pointer is
// handed to mcp.AddTool, per the design note in mcpToolMeta. In practice this
// is almost always nil at this point — every call site in this codebase
// relies on mcp.AddTool's automatic schema inference from the handler's `In`
// type parameter (see AddTool's doc comment: "If the tool's input schema is
// nil, it is set to the schema inferred from the In type parameter"), and
// that inference writes the result onto an internal copy (`tt := *t`) the SDK
// makes inside toolForErr — never back onto the original *t this function
// holds. So for eager tools registered via mcp.AddTool with inference, the
// InputSchema field captured here stays nil; it is backfilled after
// registration completes by backfillEagerSchemas (constructor, after
// registerTools/registerResources), which queries the live server the same
// way snapshotToolDefinitions does. This function still captures whatever is
// present on t at call time so explicitly-schema'd tools (t.InputSchema set
// on the literal) are captured immediately and correctly.
func (m *MCPServer) trackTool(t *mcp.Tool) *mcp.Tool {
	m.toolMeta = append(m.toolMeta, mcpToolMeta{
		Name:        t.Name,
		Description: t.Description,
		Family:      classifyMCPFamily(t.Name),
		Eager:       true,
		InputSchema: t.InputSchema,
	})
	return t
}

// backfillEagerSchemas fills in m.toolMeta[i].InputSchema for every eager
// entry whose schema wasn't captured at registration time (the common case —
// see trackTool's doc comment on why inferred schemas aren't visible on the
// original *mcp.Tool pointer). It queries the now-fully-registered live
// server via the same in-process ListTools mechanism snapshotToolDefinitions
// uses, so this must run after registerTools()/registerResources() return.
// Best-effort: a query failure leaves InputSchema nil on affected entries
// rather than failing construction.
func (m *MCPServer) backfillEagerSchemas() {
	if m.server == nil {
		return
	}
	defs := snapshotToolDefinitions(m.server)
	if len(defs) == 0 {
		return
	}
	schemaByName := make(map[string]map[string]interface{}, len(defs))
	for _, d := range defs {
		schemaByName[d.Name] = d.InputSchema
	}
	for i := range m.toolMeta {
		if !m.toolMeta[i].Eager || m.toolMeta[i].InputSchema != nil {
			continue
		}
		if s, ok := schemaByName[m.toolMeta[i].Name]; ok {
			m.toolMeta[i].InputSchema = s
		}
	}
}

// TrackTool is the exported equivalent of trackTool for use by extension
// packages (e.g. conversations, eval) that register tools via
// RegisterMCPExtensions. It records the tool in the manifest registry and
// returns the pointer unchanged, so callers can write:
//
//	mcp.AddTool(srv.Server(), srv.TrackTool(&mcp.Tool{...}), handler)
func (m *MCPServer) TrackTool(t *mcp.Tool) *mcp.Tool {
	return m.trackTool(t)
}

// handleManifest returns the kernel self-describing manifest as JSON. Read-
// only, side-effect-free. Enumeration is cheap (a couple of slice copies plus
// a sort) so we recompute per request rather than caching — this keeps the
// endpoint honest if tools or routes are added dynamically after startup.
//
//	GET /v1/manifest
//	200 → {service, version, build_time, node_id, transports, http_routes,
//	       mcp_tools}
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	// Copy + sort HTTP routes for stable output. Sort key: family then path
	// then method, which groups related endpoints visually in jq output.
	routes := make([]routeMeta, len(s.httpRoutes))
	copy(routes, s.httpRoutes)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Family != routes[j].Family {
			return routes[i].Family < routes[j].Family
		}
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	// MCP tool metadata comes from the MCPServer instance wired up in
	// registerMCPRoutes. Sorted by name for stable output.
	var tools []mcpToolMeta
	if s.mcpServer != nil {
		tools = make([]mcpToolMeta, len(s.mcpServer.toolMeta))
		copy(tools, s.mcpServer.toolMeta)
		sort.Slice(tools, func(i, j int) bool {
			return tools[i].Name < tools[j].Name
		})
	}

	// Transports section — each protocol entry enumerates the routes that
	// carry it, counted off s.httpRoutes so we don't repeat the source of
	// truth. The MCP entry also reports tool_count.
	openaiEndpoints := pathsForFamily(s.httpRoutes, "openai")
	anthropicEndpoints := pathsForFamily(s.httpRoutes, "anthropic")
	mcpEndpoints := pathsForFamily(s.httpRoutes, "mcp")

	nodeID := ""
	if s.process != nil {
		nodeID = s.process.NodeID
	}

	resp := map[string]any{
		"service":    "cogos-kernel",
		"version":    Version,
		"build_time": BuildTime,
		"node_id":    nodeID,
		"transports": map[string]any{
			"mcp": map[string]any{
				"protocol":        "mcp",
				"version":         "2025-03-26", // Streamable HTTP spec version
				"endpoints":       mcpEndpoints,
				"streamable_http": true,
				"tool_count":      len(tools),
			},
			"openai": map[string]any{
				"protocol":  "openai-compat",
				"endpoints": openaiEndpoints,
			},
			"anthropic": map[string]any{
				"protocol":  "anthropic-compat",
				"endpoints": anthropicEndpoints,
			},
		},
		"http_routes": routes,
		"mcp_tools":   tools,
		"generated":   time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// pathsForFamily returns a sorted, deduplicated list of paths for routes in
// the given family. Used to populate the transports.{proto}.endpoints slices
// in the manifest.
func pathsForFamily(routes []routeMeta, family string) []string {
	seen := map[string]struct{}{}
	for _, r := range routes {
		if r.Family == family {
			seen[r.Path] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
