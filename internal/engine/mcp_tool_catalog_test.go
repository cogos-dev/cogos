// mcp_tool_catalog_test.go — regression tests for the porcelain/plumbing
// tool-surface split (workflow wkweyu50g): cog_tool_search, cog_tool_invoke,
// and the eager/deferred partition itself.
//
// Test matrix:
//  1. TestToolSearch_FindsKnownPlumbingTool — cog_tool_search returns
//     cog_read_ledger with a non-empty input_schema.
//  2. TestToolInvoke_ExecutesPlumbingToolEndToEnd — cog_tool_invoke runs
//     cog_read_ledger and gets a real result back.
//  3. TestToolInvoke_RejectsUnknownName — an unregistered/unknown name is
//     refused, not silently dispatched.
//  4. TestToolInvoke_EnforcesCapabilityGating — a capability-denied
//     underlying tool is refused through cog_tool_invoke exactly as it
//     would be refused via a direct call.
//  5. TestPorcelainTool_DirectlyCallable — a porcelain tool (cog_get_state)
//     is still directly registered/callable via CallTool.
//  6. TestDeferredTool_NotInEagerToolsList — a deferred tool (cog_read_ledger)
//     is NOT present in the live server's tools/list (ListTools), only in
//     the toolMeta catalog cog_tool_search reads from.
//  7. TestToolInvoke_EagerToolNamesCorrectiveAction — cog_tool_invoke on an
//     eager tool names the fix inline ("call it directly") instead of the
//     generic unknown-name message (flight review 2026-07-03 §5.4).
//  8. TestToolInvoke_UnknownNameSuggestsNearestMatches (+ the two focused
//     unit tests below) — a genuinely unknown/misspelled name keeps the
//     generic message but gains nearest-name-match suggestions.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newPlainMCPServer builds a minimal MCPServer with no gating/session
// backend wiring — enough for the catalog tools, which don't require a
// sessions backend.
func newPlainMCPServer(t *testing.T) *MCPServer {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog"}
	nucleus := &Nucleus{Name: "catalog-test"}
	proc := NewProcess(cfg, nucleus)
	return NewMCPServer(cfg, nucleus, proc)
}

// callToolJSON invokes name via MCPServer.CallTool (in-process transport)
// and decodes the text result as JSON into a map.
func callToolJSON(t *testing.T, m *MCPServer, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	text, isErr, callErr := m.CallTool(context.Background(), name, argsJSON)
	if callErr != nil {
		t.Fatalf("CallTool(%q) transport error: %v", name, callErr)
	}
	var result map[string]any
	if len(text) > 0 {
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			// Not every result is a JSON object (e.g. plain error text) —
			// callers that need structured output should check isErr first.
			return map[string]any{"_raw": text}, isErr
		}
	}
	return result, isErr
}

// ─── 1. cog_tool_search finds a known plumbing tool ──────────────────────────

func TestToolSearch_FindsKnownPlumbingTool(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	result, isErr := callToolJSON(t, m, "cog_tool_search", map[string]any{
		"query": "cog_read_ledger",
		"limit": 5,
	})
	if isErr {
		t.Fatalf("cog_tool_search returned error result: %v", result)
	}

	results, ok := result["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("expected non-empty results array; got %v", result)
	}

	var found map[string]any
	for _, r := range results {
		entry, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == "cog_read_ledger" {
			found = entry
			break
		}
	}
	if found == nil {
		t.Fatalf("cog_read_ledger not found in search results: %v", results)
	}
	if eager, _ := found["eager"].(bool); eager {
		t.Error("cog_read_ledger should be Eager=false (plumbing)")
	}
	schema, ok := found["input_schema"].(map[string]any)
	if !ok || len(schema) == 0 {
		t.Fatalf("cog_read_ledger input_schema missing or empty: %v", found["input_schema"])
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema[\"type\"] = %v; want \"object\"", schema["type"])
	}
}

// ─── 2. cog_tool_invoke executes a plumbing tool end-to-end ──────────────────

func TestToolInvoke_ExecutesPlumbingToolEndToEnd(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	result, isErr := callToolJSON(t, m, "cog_tool_invoke", map[string]any{
		"name": "cog_read_ledger",
		"args": map[string]any{},
	})
	if isErr {
		t.Fatalf("cog_tool_invoke(cog_read_ledger) returned error result: %v", result)
	}
	// cappedMarshal(QueryLedger(...)) result shape — just confirm we got a
	// real structured response back, not a fallback/error envelope.
	if _, hasErrMsg := result["error"]; hasErrMsg {
		t.Errorf("unexpected error field in successful invoke result: %v", result)
	}
}

// ─── 3. cog_tool_invoke rejects an unknown name ───────────────────────────────

func TestToolInvoke_RejectsUnknownName(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	text, isErr, callErr := m.CallTool(context.Background(), "cog_tool_invoke", []byte(`{"name":"cog_does_not_exist","args":{}}`))
	if callErr != nil {
		t.Fatalf("unexpected transport error: %v", callErr)
	}
	if !isErr {
		t.Fatalf("expected IsError=true for unknown tool name; got text: %s", text)
	}
	if !g2ContainsAny(text, "unknown tool", "not in the deferred plumbing catalog") {
		t.Errorf("expected an unknown-tool message; got: %s", text)
	}

	// Also verify the porcelain tools themselves are not reachable through
	// cog_tool_invoke — they were never added to m.deferredHandlers, so this
	// is really the same "unknown name" path, but worth pinning explicitly:
	// invoke is not a second way to reach the eager set.
	text2, isErr2, callErr2 := m.CallTool(context.Background(), "cog_tool_invoke", []byte(`{"name":"cog_get_state","args":{}}`))
	if callErr2 != nil {
		t.Fatalf("unexpected transport error: %v", callErr2)
	}
	if !isErr2 {
		t.Errorf("expected cog_get_state (porcelain, not in deferredHandlers) to be rejected via invoke; got text: %s", text2)
	}
}

// ─── 4. cog_tool_invoke enforces capResolver ──────────────────────────────────

// TestToolInvoke_EnforcesCapabilityGating verifies that a capability-denied
// underlying tool is refused through cog_tool_invoke exactly as it would be
// refused via a direct call — the security-critical property from ADR G2
// PART C. Uses the real Streamable HTTP transport so req.Session.ID() is
// non-empty and resolves through the correlation store, same pattern as
// TestG2_CapabilityGating_FlagOn_Denied in mcp_sessions_g2_test.go.
//
// Note: this test does NOT reuse callToolViaStreamableWithCorrelation from
// mcp_sessions_g2_test.go — that helper mints a harness session_id of
// "gating-pre-<subject>-<toolName>", and sessionIDPattern (sessions.go) only
// allows [a-z0-9-]; "cog_tool_invoke" contains underscores and would fail
// session_id validation, silently skipping the correlation record. A local
// variant here mints an ASCII-lowercase-hyphen-only session_id instead.
func TestToolInvoke_EnforcesCapabilityGating(t *testing.T) {
	t.Parallel()
	m, gater := newMCPServerWithGating(t, true /* flagOn */)

	// Baseline: allowed subject can invoke the deferred tool through
	// cog_tool_invoke.
	okAllowed, textAllowed := invokeViaStreamableWithCorrelation(t, m, "galadriel",
		"cog_read_ledger", map[string]any{})
	if !okAllowed {
		t.Fatalf("expected allowed subject to succeed via cog_tool_invoke; got: %s", textAllowed)
	}

	// Deny gimli from calling cog_read_ledger directly (the UNDERLYING tool
	// name, not cog_tool_invoke).
	gater.deny["gimli/cog_read_ledger"] = struct{}{}

	okDenied, textDenied := invokeViaStreamableWithCorrelation(t, m, "gimli",
		"cog_read_ledger", map[string]any{})
	if okDenied {
		t.Fatalf("expected capability-denied subject to be refused via cog_tool_invoke; got success: %s", textDenied)
	}
	if !g2ContainsAny(textDenied, "capability envelope denied", "not permitted") {
		t.Errorf("expected denial message; got: %s", textDenied)
	}

	// Confirm cog_tool_invoke itself was NOT denied (only the underlying
	// tool is gated) — i.e. the deny entry is keyed to cog_read_ledger, not
	// cog_tool_invoke, proving the check targets the right name.
	if _, stillDenied := gater.deny["gimli/cog_tool_invoke"]; stillDenied {
		t.Fatal("test setup error: deny map should not contain cog_tool_invoke")
	}
}

// TestToolInvoke_FailsClosedOnUnresolvedSession verifies the fail-closed nit:
// when IdentityNakedDefault gating is enabled and a capResolver is wired, but
// the transport session cannot be resolved to a bound subject (no
// cog_register_session with a subject was performed), cog_tool_invoke must DENY
// rather than dispatch. cog_tool_invoke is the porcelain gateway to the entire
// deferred-plumbing catalog; permitting an unattributable caller to reach
// arbitrary plumbing tools would make it a confused-deputy bypass of per-tool
// capability gating.
func TestToolInvoke_FailsClosedOnUnresolvedSession(t *testing.T) {
	t.Parallel()
	m, _ := newMCPServerWithGating(t, true /* flagOn */)

	// Invoke WITHOUT registering a session first → correlation unresolved.
	ok, text := invokeViaStreamableNoCorrelation(t, m, "cog_read_ledger", map[string]any{})
	if ok {
		t.Fatalf("expected fail-closed denial for unresolved session with gating on; got success: %s", text)
	}
	if !g2ContainsAny(text, "requires a resolved session identity", "capability envelope") {
		t.Errorf("expected an unresolved-identity denial message; got: %s", text)
	}
}

// invokeViaStreamableNoCorrelation calls cog_tool_invoke over the real
// Streamable HTTP transport WITHOUT first calling cog_register_session, so the
// transport session ID does not resolve to any subject in the correlation
// store. Returns (!IsError, text).
func invokeViaStreamableNoCorrelation(
	t *testing.T, m *MCPServer, underlyingTool string, underlyingArgs map[string]any,
) (bool, string) {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return m.server
	}, nil)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "invoke-nocorr-test", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/"}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "cog_tool_invoke",
		Arguments: map[string]any{
			"name": underlyingTool,
			"args": underlyingArgs,
		},
	})
	if callErr != nil {
		return false, callErr.Error()
	}
	if result == nil || len(result.Content) == 0 {
		return true, ""
	}
	text := ""
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return !result.IsError, text
}

// invokeViaStreamableWithCorrelation is a cog_tool_invoke-specific variant of
// callToolViaStreamableWithCorrelation (mcp_sessions_g2_test.go) that mints a
// session_id compatible with sessionIDPattern (ASCII-lowercase, hyphens only
// — no underscores), then calls cog_tool_invoke with the given underlying
// tool name + args on that correlated transport session.
func invokeViaStreamableWithCorrelation(
	t *testing.T, m *MCPServer, subject, underlyingTool string, underlyingArgs map[string]any,
) (bool, string) {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return m.server
	}, nil)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "invoke-gating-test", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/"}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	_, regErr := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "cog_register_session",
		Arguments: map[string]any{
			"session_id": "invoke-gate-" + subject,
			"workspace":  "/tmp/ws",
			"role":       "agent",
			"subject":    subject,
		},
	})
	if regErr != nil {
		t.Fatalf("pre-register failed: %v", regErr)
	}

	result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "cog_tool_invoke",
		Arguments: map[string]any{
			"name": underlyingTool,
			"args": underlyingArgs,
		},
	})
	if callErr != nil {
		return false, callErr.Error()
	}
	if result == nil || len(result.Content) == 0 {
		return true, ""
	}
	text := ""
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return !result.IsError, text
}

// ─── 5. a porcelain tool is still directly registered/callable ───────────────

func TestPorcelainTool_DirectlyCallable(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	result, isErr := callToolJSON(t, m, "cog_get_state", map[string]any{})
	if isErr {
		t.Fatalf("cog_get_state returned error result: %v", result)
	}
	if _, ok := result["state"]; !ok {
		t.Errorf("expected \"state\" key in cog_get_state result; got %v", result)
	}
}

// ─── 6. a deferred tool is NOT in the mcp.AddTool/tools-list set ─────────────

func TestDeferredTool_NotInEagerToolsList(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := m.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "catalog-test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		if tool != nil {
			names[tool.Name] = true
		}
	}

	if names["cog_read_ledger"] {
		t.Error("cog_read_ledger (plumbing) should NOT appear in tools/list")
	}
	if names["mod3_speak"] {
		t.Error("mod3_speak (plumbing) should NOT appear in tools/list")
	}
	if !names["cog_get_state"] {
		t.Error("cog_get_state (porcelain) should appear in tools/list")
	}
	if !names["cog_tool_search"] {
		t.Error("cog_tool_search (porcelain) should appear in tools/list")
	}
	if !names["cog_tool_invoke"] {
		t.Error("cog_tool_invoke (porcelain) should appear in tools/list")
	}

	// Cross-check against toolMeta: cog_read_ledger IS present there
	// (Eager=false), just not on the live server.
	var deferredMetaFound bool
	for _, tm := range m.toolMeta {
		if tm.Name == "cog_read_ledger" {
			deferredMetaFound = true
			if tm.Eager {
				t.Error("cog_read_ledger toolMeta entry should have Eager=false")
			}
		}
	}
	if !deferredMetaFound {
		t.Error("cog_read_ledger should still be present in m.toolMeta (deferred catalog)")
	}
}

// ─── 7. cog_tool_invoke on an EAGER tool names the corrective action ─────────

// TestToolInvoke_EagerToolNamesCorrectiveAction is the regression for flight
// review 2026-07-03 §5.4: cog_tool_invoke("cog_get_state", ...) — a porcelain
// (eager) tool, never registered into m.deferredHandlers — previously
// returned the same generic "not in the deferred plumbing catalog" message
// as a genuinely unknown name. The flight model (gemma-4-26b) burned two
// attempts on this before self-correcting. The eager case must instead name
// the fix inline: call the tool directly.
func TestToolInvoke_EagerToolNamesCorrectiveAction(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	text, isErr, callErr := m.CallTool(context.Background(), "cog_tool_invoke", []byte(`{"name":"cog_get_state","args":{}}`))
	if callErr != nil {
		t.Fatalf("unexpected transport error: %v", callErr)
	}
	if !isErr {
		t.Fatalf("expected cog_get_state (eager, not in deferredHandlers) to be rejected via invoke; got text: %s", text)
	}
	if !g2ContainsAny(text, "is eager", "call it directly") {
		t.Errorf("expected an eager-tool corrective message naming direct call; got: %s", text)
	}
	// Must NOT fall back to the generic unknown-name message — that's the
	// whole point of the distinction.
	if g2ContainsAny(text, "not in the deferred plumbing catalog") {
		t.Errorf("eager-tool rejection should not reuse the generic unknown-name message; got: %s", text)
	}
}

// ─── 8. cog_tool_invoke on an unknown/misspelled name suggests near matches ──

// TestToolInvoke_UnknownNameSuggestsNearestMatches verifies the cheap-win
// typo-correction addition: a name that is neither in m.deferredHandlers nor
// a known eager tool keeps the original generic message, but gains up to 3
// nearest-name suggestions from the full catalog so an operator/model with a
// slightly-wrong name gets a corrective nudge instead of a dead end.
func TestToolInvoke_UnknownNameSuggestsNearestMatches(t *testing.T) {
	t.Parallel()
	m := newPlainMCPServer(t)

	// A near-miss on a real deferred tool name.
	text, isErr, callErr := m.CallTool(context.Background(), "cog_tool_invoke", []byte(`{"name":"cog_read_ledgr","args":{}}`))
	if callErr != nil {
		t.Fatalf("unexpected transport error: %v", callErr)
	}
	if !isErr {
		t.Fatalf("expected cog_read_ledgr (misspelled, unknown) to be rejected; got text: %s", text)
	}
	if !g2ContainsAny(text, "not in the deferred plumbing catalog") {
		t.Errorf("expected the generic unknown-name message to still be present; got: %s", text)
	}
	if !g2ContainsAny(text, "did you mean", "cog_read_ledger") {
		t.Errorf("expected a nearest-match suggestion including cog_read_ledger; got: %s", text)
	}
}

// TestNearestToolNames_RanksClosestFirst is a focused unit test on the
// suggestion-ranking helper itself, independent of the MCP transport.
func TestNearestToolNames_RanksClosestFirst(t *testing.T) {
	t.Parallel()
	toolMeta := []mcpToolMeta{
		{Name: "cog_read_ledger", Eager: false},
		{Name: "cog_read_cogdoc", Eager: true},
		{Name: "mod3_speak", Eager: false},
	}

	got := nearestToolNames(toolMeta, "cog_read_ledgr", 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 suggestions; got %v", got)
	}
	if got[0] != "cog_read_ledger" {
		t.Errorf("closest match = %q; want cog_read_ledger", got[0])
	}
}

// TestEagerToolExists_DistinguishesEagerFromDeferred pins the lookup helper
// cog_tool_invoke's rejection path uses to decide which message to return.
func TestEagerToolExists_DistinguishesEagerFromDeferred(t *testing.T) {
	t.Parallel()
	toolMeta := []mcpToolMeta{
		{Name: "cog_get_state", Eager: true},
		{Name: "cog_read_ledger", Eager: false},
	}

	if !eagerToolExists(toolMeta, "cog_get_state") {
		t.Error("cog_get_state should be reported as an eager tool")
	}
	if eagerToolExists(toolMeta, "cog_read_ledger") {
		t.Error("cog_read_ledger is deferred (Eager=false), should not be reported eager")
	}
	if eagerToolExists(toolMeta, "cog_does_not_exist") {
		t.Error("a name absent from toolMeta entirely should not be reported eager")
	}
}
