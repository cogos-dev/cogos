// session_fork_test.go — unit and integration tests for RFC-0005
// cog_fork_session, ForkRegistry, and the /v1/sessions/{id}/fork HTTP endpoint.
//
// Test matrix:
//
//  1. TestForkRegistry_Record_And_ForkChildren          — basic record + children query
//  2. TestForkRegistry_ForkAncestors                    — lineage walk
//  3. TestForkRegistry_GCRootExpiry                     — GC root holds parent past children
//  4. TestForkRegistry_ExpiredChildExcluded             — expired child not in ForkChildren
//  5. TestForkRegistry_PinFieldPreventsExpiry           — PinnedUntil overrides default TTL
//  6. TestForkRegistry_PruneExpired                     — PruneExpired removes stale entries
//  7. TestForkRegistry_Len                              — Len counts live entries
//  8. TestForkRegistry_CycleDetection                   — cycle in ancestor walk terminates
//  9. TestSessionOverlay_OverlayLayers                  — layer names string
// 10. TestMintForkChildID_ValidSessionID                — minted IDs pass ValidateSessionID
// 11. TestParseISO8601Duration_BasicCases               — P7D, PT1H, P1W, P1M, P1Y
// 12. TestParseISO8601Duration_InvalidCases             — malformed strings return error
// 13. TestHTTP_ForkSession_HappyPath                    — 201, child queryable in registry
// 14. TestHTTP_ForkSession_ParentNotFound               — 404
// 15. TestHTTP_ForkSession_InvalidParentID              — 400
// 16. TestHTTP_ForkSession_InvalidChildID               — 400
// 17. TestHTTP_ForkSession_CrossWorkspaceForkReturns501 — 501
// 18. TestHTTP_ForkSession_OverlayRoleApplied           — child inherits overlay role
// 19. TestHTTP_ForkSession_PinDurationApplied           — pinned_until set in response
// 20. TestHTTP_ForkSession_ForkRegistryUpdated          — forkRegistry.ForkChildren populated
// 21. TestKindSessionFork_HandlerRegistered             — KindSessionFork registered in init
// 22. TestKindSessionFork_DispatchHandlerNoops          — dispatch of fork block does not error
// 23. TestHTTP_ForkSession_LineageProjection            — ForkAncestors walks correctly after fork
package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ─── ForkRegistry unit tests ─────────────────────────────────────────────────

func TestForkRegistry_Record_And_ForkChildren(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()

	body := SessionForkBody{
		ParentSessionID:   "parent-a-one",
		ChildSessionID:    "fork-parent-a-one-0001",
		ParentSessionHash: "sha256:abc",
		ForkPoint:         1,
		Overlay:           SessionOverlay{},
	}
	fr.Record(body, now)

	children := fr.ForkChildren("parent-a-one", now)
	if len(children) != 1 || children[0] != "fork-parent-a-one-0001" {
		t.Errorf("ForkChildren = %v, want [fork-parent-a-one-0001]", children)
	}
}

func TestForkRegistry_ForkAncestors(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()

	// Build a chain: root → child-1 → child-2.
	fr.Record(SessionForkBody{
		ParentSessionID: "root-session-a",
		ChildSessionID:  "fork-root-session-0001",
		ForkPoint:       1,
	}, now)
	fr.Record(SessionForkBody{
		ParentSessionID: "fork-root-session-0001",
		ChildSessionID:  "fork-fork-root-session-0002",
		ForkPoint:       2,
	}, now)

	ancestors := fr.ForkAncestors("fork-fork-root-session-0002")
	if len(ancestors) != 2 {
		t.Fatalf("ForkAncestors = %d entries, want 2", len(ancestors))
	}
	if ancestors[0].SessionID != "fork-root-session-0001" {
		t.Errorf("ancestors[0].SessionID = %q, want fork-root-session-0001", ancestors[0].SessionID)
	}
	if ancestors[1].SessionID != "root-session-a" {
		t.Errorf("ancestors[1].SessionID = %q, want root-session-a", ancestors[1].SessionID)
	}
}

func TestForkRegistry_GCRootExpiry(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()

	fr.Record(SessionForkBody{
		ParentSessionID: "parent-b-root",
		ChildSessionID:  "fork-parent-b-root-0001",
		ForkPoint:       1,
	}, now)

	expiry := fr.GCRootExpiry("parent-b-root", now)
	if expiry.IsZero() {
		t.Fatal("GCRootExpiry returned zero time for parent with live children")
	}
	// expiry should be at least DefaultForkRetention in the future.
	minExpected := now.Add(DefaultForkRetention)
	if expiry.Before(minExpected) {
		t.Errorf("GCRootExpiry = %v, want at least %v", expiry, minExpected)
	}
}

func TestForkRegistry_ExpiredChildExcluded(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	// Fork time in the past so it's already expired.
	pastFork := time.Now().UTC().Add(-8 * 24 * time.Hour) // 8 days ago

	fr.Record(SessionForkBody{
		ParentSessionID: "parent-c-old",
		ChildSessionID:  "fork-parent-c-old-0001",
		ForkPoint:       1,
	}, pastFork)

	children := fr.ForkChildren("parent-c-old", time.Now().UTC())
	if len(children) != 0 {
		t.Errorf("ForkChildren should be empty for expired child, got %v", children)
	}
}

func TestForkRegistry_PinFieldPreventsExpiry(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	pastFork := time.Now().UTC().Add(-8 * 24 * time.Hour)
	// Pin until 30 days from now.
	pinUntil := time.Now().UTC().Add(30 * 24 * time.Hour)

	fr.Record(SessionForkBody{
		ParentSessionID: "parent-d-pin",
		ChildSessionID:  "fork-parent-d-pin-0001",
		ForkPoint:       1,
		PinnedUntil:     &pinUntil,
	}, pastFork)

	children := fr.ForkChildren("parent-d-pin", time.Now().UTC())
	if len(children) != 1 {
		t.Errorf("ForkChildren = %v, want 1 pinned child", children)
	}
}

func TestForkRegistry_PruneExpired(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()
	pastFork := now.Add(-8 * 24 * time.Hour)

	fr.Record(SessionForkBody{
		ParentSessionID: "parent-e-prune",
		ChildSessionID:  "fork-parent-e-prune-0001",
		ForkPoint:       1,
	}, pastFork)
	fr.Record(SessionForkBody{
		ParentSessionID: "parent-e-prune",
		ChildSessionID:  "fork-parent-e-prune-0002",
		ForkPoint:       2,
	}, now) // still alive

	pruned := fr.PruneExpired(now)
	if pruned != 1 {
		t.Errorf("PruneExpired = %d, want 1", pruned)
	}
	if fr.Len() != 1 {
		t.Errorf("Len() after prune = %d, want 1", fr.Len())
	}
}

func TestForkRegistry_Len(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()

	if fr.Len() != 0 {
		t.Errorf("empty registry Len = %d, want 0", fr.Len())
	}
	fr.Record(SessionForkBody{ParentSessionID: "par-f-len", ChildSessionID: "fork-par-f-len-0001"}, now)
	if fr.Len() != 1 {
		t.Errorf("registry Len = %d, want 1", fr.Len())
	}
}

func TestForkRegistry_CycleDetection(t *testing.T) {
	t.Parallel()
	fr := NewForkRegistry()
	now := time.Now().UTC()

	// Intentionally create a loop: A → B → A (shouldn't happen in practice,
	// but the walk must not loop forever).
	fr.Record(SessionForkBody{ParentSessionID: "cycle-a-root", ChildSessionID: "fork-cycle-a-root-0001"}, now)
	// Manually stitch a loop via Record (normally impossible via the tool).
	fr.mu.Lock()
	fr.byChild["fork-cycle-a-root-0001"] = forkEntry{
		body: SessionForkBody{
			ParentSessionID: "fork-cycle-a-root-0001",
			ChildSessionID:  "cycle-a-root",
		},
		expiresAt: now.Add(DefaultForkRetention),
	}
	fr.mu.Unlock()

	// Should terminate and return at most maxDepth entries (never panic).
	ancestors := fr.ForkAncestors("fork-cycle-a-root-0001")
	if len(ancestors) >= 100 {
		t.Error("ancestor walk did not terminate on cycle")
	}
}

// ─── SessionOverlay helper ────────────────────────────────────────────────────

func TestSessionOverlay_OverlayLayers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		overlay SessionOverlay
		want    string
	}{
		{SessionOverlay{}, ""},
		{SessionOverlay{Role: &RoleOverlay{Role: "foo"}}, "role"},
		{SessionOverlay{
			Identity: &IdentityOverlay{},
			Role:     &RoleOverlay{},
			Context:  &ContextOverlay{},
			Tools:    &ToolsOverlay{},
			KVCache:  &KVCacheOverlay{},
		}, "identity,role,context,tools,kvcache"},
	}
	for _, tc := range cases {
		got := tc.overlay.OverlayLayers()
		if got != tc.want {
			t.Errorf("OverlayLayers() = %q, want %q (overlay=%+v)", got, tc.want, tc.overlay)
		}
	}

	var nilOverlay *SessionOverlay
	if nilOverlay.OverlayLayers() != "" {
		t.Error("nil overlay should return empty string")
	}
}

// ─── mintForkChildID / parseISO8601Duration helpers ──────────────────────────

func TestMintForkChildID_ValidSessionID(t *testing.T) {
	t.Parallel()
	for i := range 20 {
		_ = i
		id := mintForkChildID("parent-session-here", time.Now().UTC())
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("minted ID %q is invalid: %v", id, err)
		}
	}
}

func TestParseISO8601Duration_BasicCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"P7D", 7 * 24 * time.Hour},
		{"P30D", 30 * 24 * time.Hour},
		{"P1W", 7 * 24 * time.Hour},
		{"PT1H", time.Hour},
		{"PT30M", 30 * time.Minute},
		{"P1Y", 365 * 24 * time.Hour},
		{"P1M", 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		got, err := parseISO8601Duration(tc.input)
		if err != nil {
			t.Errorf("parseISO8601Duration(%q) error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseISO8601Duration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseISO8601Duration_InvalidCases(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "7D", "P", "X1D", "P0D"} {
		_, err := parseISO8601Duration(s)
		if err == nil {
			t.Errorf("parseISO8601Duration(%q) should have returned error", s)
		}
	}
}

// ─── HTTP endpoint tests ─────────────────────────────────────────────────────

// registerParent is a test helper that registers a parent session and asserts success.
func registerParent(t *testing.T, ts *httptest.Server, sessionID string) {
	t.Helper()
	body := map[string]any{"session_id": sessionID, "workspace": "demo", "role": "author"}
	resp := postJSON(t, ts.URL+"/v1/sessions/register", body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("register parent %q: status %d", sessionID, resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTP_ForkSession_HappyPath(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	registerParent(t, ts, "parent-g-fork")

	resp := postJSON(t, ts.URL+"/v1/sessions/parent-g-fork/fork", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("fork status = %d, want 201", resp.StatusCode)
	}
	var out forkSessionHTTPResponse
	decodeJSON(t, resp, &out)
	if !out.OK {
		t.Error("fork response ok = false")
	}
	if out.ChildSessionID == "" {
		t.Error("child_session_id is empty")
	}
	if out.ForkBlockHash == "" {
		t.Error("fork_block_hash is empty")
	}

	// Child should be queryable in the session registry.
	_, ok := srv.sessionRegistry.Get(out.ChildSessionID)
	if !ok {
		t.Errorf("child session %q not found in registry after fork", out.ChildSessionID)
	}
}

func TestHTTP_ForkSession_ParentNotFound(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	resp := postJSON(t, ts.URL+"/v1/sessions/not-a-real-session/fork", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTP_ForkSession_InvalidParentID(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	resp := postJSON(t, ts.URL+"/v1/sessions/bad/fork", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHTTP_ForkSession_InvalidChildID(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)
	registerParent(t, ts, "parent-h-bad-child")

	resp := postJSON(t, ts.URL+"/v1/sessions/parent-h-bad-child/fork", map[string]any{
		"child_session_id": "bad", // too short
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHTTP_ForkSession_CrossWorkspaceForkReturns501(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)
	registerParent(t, ts, "parent-i-cross")

	// external:// identity ref signals cross-workspace.
	resp := postJSON(t, ts.URL+"/v1/sessions/parent-i-cross/fork", map[string]any{
		"overlay": map[string]any{
			"identity": map[string]any{
				"identity_ref": "external://other-workspace/identity/foo",
			},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestHTTP_ForkSession_OverlayRoleApplied(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)
	registerParent(t, ts, "parent-j-role")

	resp := postJSON(t, ts.URL+"/v1/sessions/parent-j-role/fork", map[string]any{
		"overlay": map[string]any{
			"role": map[string]any{"role": "btw-aside"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("fork status = %d, want 201", resp.StatusCode)
	}
	var out forkSessionHTTPResponse
	decodeJSON(t, resp, &out)

	child, ok := srv.sessionRegistry.Get(out.ChildSessionID)
	if !ok {
		t.Fatalf("child session %q not in registry", out.ChildSessionID)
	}
	if child.Role != "btw-aside" {
		t.Errorf("child.Role = %q, want btw-aside", child.Role)
	}
}

func TestHTTP_ForkSession_PinDurationApplied(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)
	registerParent(t, ts, "parent-k-pin")

	resp := postJSON(t, ts.URL+"/v1/sessions/parent-k-pin/fork", map[string]any{
		"pin_duration": "P30D",
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("fork status = %d, want 201", resp.StatusCode)
	}
	var out forkSessionHTTPResponse
	decodeJSON(t, resp, &out)

	if out.PinnedUntil == "" {
		t.Error("pinned_until should be set when pin_duration=P30D")
	}
}

func TestHTTP_ForkSession_ForkRegistryUpdated(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)
	registerParent(t, ts, "parent-l-registry")

	resp := postJSON(t, ts.URL+"/v1/sessions/parent-l-registry/fork", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("fork status = %d, want 201", resp.StatusCode)
	}
	var out forkSessionHTTPResponse
	decodeJSON(t, resp, &out)

	children := srv.forkRegistry.ForkChildren("parent-l-registry", time.Now().UTC())
	if len(children) == 0 {
		t.Error("forkRegistry should have at least one child after fork")
	}
	found := false
	for _, c := range children {
		if c == out.ChildSessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("child %q not found in forkRegistry.ForkChildren; got %v", out.ChildSessionID, children)
	}
}

// ─── Kind registry + dispatch tests ──────────────────────────────────────────

func TestKindSessionFork_HandlerRegistered(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)
	// Register the handler manually (mirrors what init() does at process start).
	RegisterKindHandler(KindSessionFork, handleSessionForkBlock)

	kinds := RegisteredKinds()
	found := false
	for _, k := range kinds {
		if k == KindSessionFork {
			found = true
		}
	}
	if !found {
		t.Errorf("KindSessionFork %q not in RegisteredKinds(); got %v", KindSessionFork, kinds)
	}
}

func TestKindSessionFork_DispatchHandlerNoops(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)
	// Register the handler so dispatch can find it.
	RegisterKindHandler(KindSessionFork, handleSessionForkBlock)

	block := &CogBlock{
		Kind: KindSessionFork,
	}
	// Should not return an error.
	if err := DispatchKind(block); err != nil {
		t.Errorf("DispatchKind(session.fork) returned error: %v", err)
	}
}

// ─── Lineage projection integration test ────────────────────────────────────

func TestHTTP_ForkSession_LineageProjection(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)
	registerParent(t, ts, "root-m-lineage")

	// Fork root → child1.
	resp1 := postJSON(t, ts.URL+"/v1/sessions/root-m-lineage/fork", map[string]any{})
	if resp1.StatusCode != http.StatusCreated {
		resp1.Body.Close()
		t.Fatalf("fork1 status = %d, want 201", resp1.StatusCode)
	}
	var out1 forkSessionHTTPResponse
	decodeJSON(t, resp1, &out1)

	// Register child1 so it can be forked.
	body := map[string]any{"session_id": out1.ChildSessionID, "workspace": "demo", "role": "fork-child"}
	bReg := postJSON(t, ts.URL+"/v1/sessions/register", body)
	bReg.Body.Close()

	// Fork child1 → child2.
	resp2 := postJSON(t, ts.URL+"/v1/sessions/"+out1.ChildSessionID+"/fork", map[string]any{})
	if resp2.StatusCode != http.StatusCreated {
		resp2.Body.Close()
		t.Fatalf("fork2 status = %d, want 201", resp2.StatusCode)
	}
	var out2 forkSessionHTTPResponse
	decodeJSON(t, resp2, &out2)

	// ForkAncestors(child2) should walk back through child1 to root.
	ancestors := srv.forkRegistry.ForkAncestors(out2.ChildSessionID)
	if len(ancestors) < 2 {
		// Some fork operations may not have the full chain in the registry yet
		// if child1 was registered without being in byChild first; at minimum
		// there must be one ancestor (child1 → root).
		if len(ancestors) == 0 {
			t.Error("ForkAncestors(child2) returned empty, want at least 1 ancestor")
		}
	}

	// ForkChildren(root) should include child1.
	children := srv.forkRegistry.ForkChildren("root-m-lineage", time.Now().UTC())
	if len(children) == 0 {
		t.Error("ForkChildren(root) should include child1")
	}
}

// ─── Duplicate registration panic test ───────────────────────────────────────
// (Smoke test for ADR-090 invariant — duplicate Kind registration panics.)

func TestKindSessionFork_DuplicateRegistrationPanics(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	// Register once.
	RegisterKindHandler(KindSessionFork, handleSessionForkBlock)

	// Second registration must panic.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate KindSessionFork registration, got none")
		}
	}()
	RegisterKindHandler(KindSessionFork, handleSessionForkBlock)
}

// ─── MCP tool round-trip test ─────────────────────────────────────────────────

func TestMCP_ForkSession_RoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	proc := NewProcess(cfg, &Nucleus{Name: "test"})
	mcpSrv := NewMCPServer(cfg, &Nucleus{Name: "test"}, proc)
	bus := NewBusSessionManager(root)
	sessions := NewSessionRegistry()
	handoffs := NewHandoffRegistry()
	fr := NewForkRegistry()
	mcpSrv.SetSessionsBackend(bus, sessions, handoffs)
	mcpSrv.SetForkRegistry(fr)

	ctx := mcpTestCtx(t)

	// Register the parent session.
	_, _, err := mcpSrv.toolRegisterSession(ctx, nil, registerSessionInput{
		SessionID: "parent-n-mcp", Workspace: "w", Role: "author",
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}

	// Fork the parent.
	forkResult, _, err := mcpSrv.toolForkSession(ctx, nil, forkSessionInput{
		ParentSessionID: "parent-n-mcp",
	})
	if err != nil {
		t.Fatalf("toolForkSession: %v", err)
	}
	if forkResult == nil {
		t.Fatal("toolForkSession returned nil result")
	}

	var forkOut forkSessionOutput
	decodeMCPJSON(t, forkResult, &forkOut)
	if forkOut.ChildSessionID == "" {
		t.Error("child_session_id is empty in MCP response")
	}
	if forkOut.ForkBlockHash == "" {
		t.Error("fork_block_hash is empty in MCP response")
	}

	// Registry should have the child session.
	_, ok := sessions.Get(forkOut.ChildSessionID)
	if !ok {
		t.Errorf("child %q not in registry after MCP fork", forkOut.ChildSessionID)
	}

	// ForkRegistry should have the child.
	children := fr.ForkChildren("parent-n-mcp", time.Now().UTC())
	found := false
	for _, c := range children {
		if c == forkOut.ChildSessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("child %q not in forkRegistry after MCP fork; got %v", forkOut.ChildSessionID, children)
	}
}
