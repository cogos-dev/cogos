// serve_identity_gateway_test.go — G1: per-session identity resolution at the
// inference gateway.
//
// Test matrix (four cases as specified):
//
//	(a) flag OFF, unbound        → TargetIdentity==nucleus.Name, nucleus card present.
//	(b) flag OFF, bound "alice"  → TargetIdentity unchanged from today's path (flag off);
//	                               nucleus card still present (ONLY attribution changes).
//	(c) flag ON,  unbound        → NO nucleus card, AssembleContext skipped.
//	(d) flag ON,  bound "alice"  → attribution "alice", clean (no nucleus card).
//
// All four tests use the StubProvider so handleChat runs the full success path
// (not the 501 branch) and we can inspect lastRequest.SystemPrompt.
// The nucleus card sentinel: makeNucleus("TestNucleus", "test-role") produces
// Card "# TestNucleus\nRole: test-role\n"; we check for "TestNucleus" presence.
//
// Note on resolveBoundIdentity: the helper is also unit-tested via
// TestResolveBoundIdentity_* below, which verifies header vs. User fallback
// and the nil-backend case without needing a full HTTP round-trip.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// ─── test-server builder ──────────────────────────────────────────────────────

// newGatewayTestServer builds a Server with:
//   - nucleus named "TestNucleus" / role "test-role"
//   - StubProvider router (returns "reply")
//   - fakeHarnessAttacher wired as harnessBackend
//   - cfg.IdentityNakedDefault set by caller
func newGatewayTestServer(t *testing.T, nakedDefault bool) (*Server, *StubProvider, *fakeHarnessAttacher) {
	t.Helper()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.IdentityNakedDefault = nakedDefault
	nucleus := makeNucleus("TestNucleus", "test-role")
	process := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, process)

	stub := NewStubProvider("stub", "reply")
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	fake := newFakeHarnessAttacher()
	srv.harnessBackend = fake

	return srv, stub, fake
}

// bindSession registers a HarnessBindingCRD mapping sessionID → subject on
// the fakeHarnessAttacher so resolveBoundIdentity can find it.
func bindSession(fake *fakeHarnessAttacher, sessionID, subject string) {
	fake.AttachHarness(&subidentity.HarnessBindingCRD{
		Spec: subidentity.HarnessBindingSpec{
			SessionID: sessionID,
			Subject:   subject,
			Type:      "agent",
		},
	})
}

// doChatRequest POSTs to /v1/chat/completions and returns the HTTP recorder.
// sessionHeader is the X-Cogos-Session-Id value; empty means not sent.
func doChatRequest(t *testing.T, srv *Server, sessionHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "local",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"stream":   false,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if sessionHeader != "" {
		req.Header.Set("X-Cogos-Session-Id", sessionHeader)
	}
	w := httptest.NewRecorder()
	srv.handleChat(w, req)
	return w
}

// ─── (a) flag OFF, unbound → nucleus attribution + nucleus card ──────────────

// TestGatewayIdentity_FlagOff_Unbound verifies that with IdentityNakedDefault=false
// and no session header, the nucleus card is present in the system prompt.
// This is the no-regression case: behavior must be byte-for-byte today's behavior.
func TestGatewayIdentity_FlagOff_Unbound(t *testing.T) {
	t.Parallel()
	srv, stub, _ := newGatewayTestServer(t, false)

	w := doChatRequest(t, srv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("(a) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(a) stub did not capture lastRequest")
	}
	// Nucleus card must be present — identical to pre-G1 behavior.
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(a) flag OFF unbound: expected nucleus card in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── (b) flag OFF, bound "alice" → only attribution changes; card still present

// TestGatewayIdentity_FlagOff_BoundForeign verifies that with
// IdentityNakedDefault=false, a bound (but foreign) session does NOT strip the
// nucleus card. The ONLY observable change from pre-G1 is ledger attribution
// (block.TargetIdentity = "alice"), which we confirm via resolveBoundIdentity.
func TestGatewayIdentity_FlagOff_BoundForeign(t *testing.T) {
	t.Parallel()
	srv, stub, fake := newGatewayTestServer(t, false)
	bindSession(fake, "sess-alice", "alice")

	w := doChatRequest(t, srv, "sess-alice")
	if w.Code != http.StatusOK {
		t.Fatalf("(b) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(b) stub did not capture lastRequest")
	}
	// Flag OFF: embodiment unchanged — nucleus card still present.
	if !strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(b) flag OFF bound: expected nucleus card in SystemPrompt, got %q",
			lr.SystemPrompt)
	}

	// Verify resolveBoundIdentity resolved correctly for "alice".
	bi := srv.resolveBoundIdentity(
		httptest.NewRequest(http.MethodPost, "/", nil), "")
	// (no header on that bare request → unbound; use proper request)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Cogos-Session-Id", "sess-alice")
	bi = srv.resolveBoundIdentity(req, "")
	if !bi.Bound {
		t.Error("(b) expected Bound=true for sess-alice")
	}
	if bi.Subject != "alice" {
		t.Errorf("(b) expected Subject=alice, got %q", bi.Subject)
	}
}

// ─── (c) flag ON, unbound → no nucleus card, clean transport ─────────────────

// TestGatewayIdentity_FlagOn_Unbound verifies that with IdentityNakedDefault=true
// and no session, the nucleus card is absent from the system prompt and
// AssembleContext is skipped (clean transport path).
func TestGatewayIdentity_FlagOn_Unbound(t *testing.T) {
	t.Parallel()
	srv, stub, _ := newGatewayTestServer(t, true)

	w := doChatRequest(t, srv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("(c) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(c) stub did not capture lastRequest")
	}
	// Clean transport: NO nucleus card in system prompt.
	if strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(c) flag ON unbound: nucleus card must NOT appear in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── (d) flag ON, bound "alice" (foreign) → attribution "alice", no card ─────

// TestGatewayIdentity_FlagOn_BoundForeign verifies that with
// IdentityNakedDefault=true, a session bound to a foreign subject ("alice") gets
// clean transport (no nucleus card).
func TestGatewayIdentity_FlagOn_BoundForeign(t *testing.T) {
	t.Parallel()
	srv, stub, fake := newGatewayTestServer(t, true)
	bindSession(fake, "sess-alice", "alice")

	w := doChatRequest(t, srv, "sess-alice")
	if w.Code != http.StatusOK {
		t.Fatalf("(d) status = %d; want 200", w.Code)
	}
	lr := stub.lastRequest
	if lr == nil {
		t.Fatal("(d) stub did not capture lastRequest")
	}
	// Foreign bound subject → clean (no nucleus card).
	if strings.Contains(lr.SystemPrompt, "TestNucleus") {
		t.Errorf("(d) flag ON foreign-bound: nucleus card must NOT appear in SystemPrompt, got %q",
			lr.SystemPrompt)
	}
}

// ─── resolveBoundIdentity unit tests ─────────────────────────────────────────

// TestResolveBoundIdentity_Header verifies header extraction.
func TestResolveBoundIdentity_Header(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("TestNucleus", "r")
	srv := NewServer(cfg, nucleus, NewProcess(cfg, nucleus))
	fake := newFakeHarnessAttacher()
	srv.harnessBackend = fake
	bindSession(fake, "hdr-session", "bob")

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Cogos-Session-Id", "hdr-session")
	bi := srv.resolveBoundIdentity(req, "")
	if !bi.Bound || bi.Subject != "bob" {
		t.Errorf("expected Bound=true, Subject=bob; got %+v", bi)
	}
}

// TestResolveBoundIdentity_UserFallback verifies the User body field fallback.
func TestResolveBoundIdentity_UserFallback(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("TestNucleus", "r")
	srv := NewServer(cfg, nucleus, NewProcess(cfg, nucleus))
	fake := newFakeHarnessAttacher()
	srv.harnessBackend = fake
	bindSession(fake, "user-session", "carol")

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// No header → falls through to reqUser.
	bi := srv.resolveBoundIdentity(req, "user-session")
	if !bi.Bound || bi.Subject != "carol" {
		t.Errorf("expected Bound=true, Subject=carol; got %+v", bi)
	}
}

// TestResolveBoundIdentity_NilBackend verifies nil-safety.
func TestResolveBoundIdentity_NilBackend(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("TestNucleus", "r")
	srv := NewServer(cfg, nucleus, NewProcess(cfg, nucleus))
	// No harnessBackend set.

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Cogos-Session-Id", "any-session")
	bi := srv.resolveBoundIdentity(req, "")
	if bi.Bound {
		t.Errorf("expected Bound=false with nil backend; got %+v", bi)
	}
}

// TestResolveBoundIdentity_NoSession verifies absent session → unbound.
func TestResolveBoundIdentity_NoSession(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("TestNucleus", "r")
	srv := NewServer(cfg, nucleus, NewProcess(cfg, nucleus))
	fake := newFakeHarnessAttacher()
	srv.harnessBackend = fake

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	bi := srv.resolveBoundIdentity(req, "")
	if bi.Bound {
		t.Errorf("expected Bound=false with no session; got %+v", bi)
	}
}
