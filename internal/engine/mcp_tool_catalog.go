// mcp_tool_catalog.go — cog_tool_search + cog_tool_invoke: the porcelain
// front door onto the deferred plumbing catalog (workflow wkweyu50g,
// tier-then-trim mechanism doc cog:mem/semantic/architecture/
// mcp-tool-surface-tier-then-trim.cog.md #Mechanism, RESOLVED).
//
// Why these two tools exist: the kernel cannot ask an MCP client (Claude
// Code or any other harness) to defer-load specific tools — defer_loading /
// tool_search_tool_* are Anthropic-Messages-API client-side attributes with
// no MCP wire equivalent. So instead of shrinking the *set* of tools the
// kernel implements, the kernel shrinks the set it *advertises* via
// mcp.AddTool (the porcelain, see registerTools) and moves everything else
// (the plumbing, registered via trackToolDeferred into m.deferredHandlers)
// behind these two tools:
//
//   - cog_tool_search(query, limit) — ranks the full catalog (both eager and
//     deferred entries in m.toolMeta) by substring/token match and returns
//     {name, description, input_schema, family} so a caller can discover a
//     plumbing tool without it ever appearing in tools/list.
//   - cog_tool_invoke(name, args) — looks up name in m.deferredHandlers and
//     dispatches. Unknown names are rejected outright (no silent widening,
//     mirroring KernelToolRegistry.Scoped in tool_loop.go).
//
// SECURITY-CRITICAL (ADR G2 PART C): cog_tool_invoke MUST NOT become a
// confused-deputy bypass of per-tool capability gating. The normal
// enforcement path (tool_observer.go's withToolObserver) keys off
// req.Session.ID() — the REAL MCP transport session ID — resolved via
// m.resolveTransportSession. If cog_tool_invoke simply called the deferred
// handler in-process (or via a nested in-memory MCP transport a la
// snapshotToolDefinitions/CallTool), the underlying handler's own
// withToolObserver wrapping would see a *different* req (either the same
// req.Session as the outer cog_tool_invoke call, which is fine, or — if
// dispatched over a fresh in-memory transport pair — an empty/unrelated
// session ID, which would silently skip enforcement entirely: the classic
// confused-deputy shape).
//
// This file avoids that trap by checking capability BEFORE dispatch, using
// the OUTER call's own req (the real transport session), against the name of
// the UNDERLYING tool being invoked — then calling the deferred handler
// in-process with that same req threaded through, so any capability check
// inside the underlying handler (should one exist independently) still sees
// the correct session. This is the same check withToolObserver performs,
// applied one level up, for the tool that's actually about to run rather
// than for "cog_tool_invoke" itself.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerToolCatalogTools installs cog_tool_search and cog_tool_invoke —
// the two porcelain tools that front the deferred plumbing catalog. Called
// last from registerTools() so the full m.toolMeta / m.deferredHandlers
// population (including these two tools' own entries) is available for
// cog_tool_search to rank over.
func (m *MCPServer) registerToolCatalogTools() {
	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_tool_search",
		Description: "Search the full CogOS MCP tool catalog — both the eager " +
			"porcelain tools (already directly callable) and the deferred " +
			"plumbing tools (invoke-only via cog_tool_invoke, not present in " +
			"tools/list). Ranks by substring/token match over name, " +
			"description, and family. Required: query. Optional: limit " +
			"(default 10, max 50). Returns [{name, description, input_schema, " +
			"family, eager}]. Use this to discover a plumbing tool's schema " +
			"before calling cog_tool_invoke.",
	}), withToolObserver(m, "cog_tool_search", m.toolToolSearch))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_tool_invoke",
		Description: "Invoke a deferred (plumbing) MCP tool by name. Rejects " +
			"unknown names — this is NOT a general-purpose dispatcher, only " +
			"the closed set of tools registered as plumbing (see " +
			"cog_tool_search). Required: name, args (object; pass {} for " +
			"tools with no required input). Subject to the same per-tool " +
			"capability-envelope gating (ADR G2 PART C) as calling the " +
			"underlying tool directly — cog_tool_invoke is not a bypass. " +
			"Returns the underlying tool's result verbatim.",
	}), withToolObserver(m, "cog_tool_invoke", m.toolToolInvoke))
}

// ── cog_tool_search ──────────────────────────────────────────────────────

type toolSearchInput struct {
	Query string `json:"query" jsonschema:"Search query — matched as substring/token over tool name, description, and family"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10, max 50)"`
}

type toolSearchResultEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Family      string `json:"family"`
	Eager       bool   `json:"eager"`
	InputSchema any    `json:"input_schema,omitempty"`
}

// toolToolSearch implements cog_tool_search. In-memory ranking over
// m.toolMeta is fine for the current catalog size (~57 rows); FTS5 over
// constellation.db is the escalation path only if the catalog grows enough
// to matter (see mcp-tool-surface-tier-then-trim.cog.md #Mechanism).
func (m *MCPServer) toolToolSearch(ctx context.Context, req *mcp.CallToolRequest, input toolSearchInput) (*mcp.CallToolResult, any, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return fallbackResult("cog_tool_search: query is required", "")
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	terms := strings.Fields(strings.ToLower(query))

	type scored struct {
		entry mcpToolMeta
		score int
	}
	var candidates []scored
	for _, t := range m.toolMeta {
		hay := strings.ToLower(t.Name + " " + t.Description + " " + t.Family)
		score := 0
		for _, term := range terms {
			if term == "" {
				continue
			}
			if strings.Contains(strings.ToLower(t.Name), term) {
				score += 5
			}
			if strings.Contains(strings.ToLower(t.Family), term) {
				score += 2
			}
			if strings.Contains(hay, term) {
				score++
			}
		}
		if score > 0 {
			candidates = append(candidates, scored{entry: t, score: score})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].entry.Name < candidates[j].entry.Name
	})

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	results := make([]toolSearchResultEntry, 0, len(candidates))
	for _, c := range candidates {
		results = append(results, toolSearchResultEntry{
			Name:        c.entry.Name,
			Description: c.entry.Description,
			Family:      c.entry.Family,
			Eager:       c.entry.Eager,
			InputSchema: c.entry.InputSchema,
		})
	}

	return marshalResult(map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}

// ── cog_tool_invoke ───────────────────────────────────────────────────────

type toolInvokeInput struct {
	Name string          `json:"name" jsonschema:"The plumbing tool's name, as returned by cog_tool_search"`
	Args json.RawMessage `json:"args,omitempty" jsonschema:"Arguments object for the underlying tool; omit or pass {} for tools with no required input"`
}

// toolToolInvoke implements cog_tool_invoke. See the file-level doc comment
// for the confused-deputy hazard this guards against and why the capability
// check happens here, before dispatch, against req.Session.ID() (the real
// transport session) rather than inside a nested/re-dispatched call.
func (m *MCPServer) toolToolInvoke(ctx context.Context, req *mcp.CallToolRequest, input toolInvokeInput) (*mcp.CallToolResult, any, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return fallbackResult("cog_tool_invoke: name is required", "")
	}

	handler, ok := m.deferredHandlers[name]
	if !ok {
		// No silent widening: reject unknown names outright, mirroring
		// KernelToolRegistry.Scoped (tool_loop.go) rather than falling
		// through to some generic dispatch.
		return fallbackResult(
			fmt.Sprintf("cog_tool_invoke: unknown tool %q (not in the deferred plumbing catalog; use cog_tool_search to discover valid names)", name),
			"",
		)
	}

	// SECURITY-CRITICAL (ADR G2 PART C): gate the UNDERLYING tool (name),
	// not "cog_tool_invoke" itself, using the OUTER call's real transport
	// session — exactly the check withToolObserver performs for a direct
	// call, applied here so a deferred tool gets the same enforcement it
	// would have gotten had it been registered eagerly.
	if m.cfg != nil && m.cfg.IdentityNakedDefault && m.capResolver != nil {
		transportID := ""
		if req != nil && req.Session != nil {
			transportID = req.Session.ID()
		}
		if entry, ok := m.resolveTransportSession(transportID); ok && entry.Subject != "" {
			if !m.capResolver.CanInvoke(entry.Subject, name) {
				return fallbackResult(
					"capability envelope denied: subject "+entry.Subject+" is not permitted to invoke "+name,
					"",
				)
			}
		}
	}

	result, err := handler(ctx, req, input.Args)
	if err != nil {
		return fallbackResult(fmt.Sprintf("cog_tool_invoke: %s: %v", name, err), "")
	}
	return result, nil, nil
}
