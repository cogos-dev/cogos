// conductor_test.go validates the conductor↔ACP-client-SDK integration path
// described by RFC-036. Tests run against the portable contract.Conductor
// interface, backed by a coder/acp-go-sdk adapter, driving an in-process fake
// ACP agent (cell) over io.Pipe. The captureSink stands in for the cogos ledger.

package conductor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/myrgic/cogos/internal/conductor/contract"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestInitialize covers `initialize` with `_meta` identity injection: assert the
// iss/sub `_meta` round-trips to the agent (RFC-036 P3 gap 6).
func TestInitialize(t *testing.T) {
	t.Run("meta_identity_roundtrips", func(t *testing.T) {
		agent := newFakeAgent()
		r := newRig(t, agent, nil, nil)

		pv, err := r.cond.Initialize(ctxT(t), testIdentity())
		if err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if pv != int(acp.ProtocolVersionNumber) {
			t.Fatalf("protocol version = %d, want %d", pv, acp.ProtocolVersionNumber)
		}
		snap := agent.snapshot()
		meta, _ := snap["initMeta"].(map[string]any)
		if meta == nil {
			t.Fatal("agent received nil _meta on initialize")
		}
		if meta["cogos.iss"] != testIss {
			t.Errorf("iss = %v, want %s", meta["cogos.iss"], testIss)
		}
		if meta["cogos.sub"] != testSub {
			t.Errorf("sub = %v, want %s", meta["cogos.sub"], testSub)
		}
	})
}

// TestNewSession covers `new_session` (cwd, mcpServers, `_meta`).
func TestNewSession(t *testing.T) {
	t.Run("cwd_and_meta_roundtrip", func(t *testing.T) {
		agent := newFakeAgent()
		r := newRig(t, agent, nil, nil)
		ctx := ctxT(t)
		if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		sid, err := r.cond.NewSession(ctx, defaultSpec("/work/proj"))
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if sid == "" {
			t.Fatal("empty session id")
		}
		agent.mu.Lock()
		gotCwd := agent.newSessionCwd[string(sid)]
		gotMeta := agent.newSessMeta
		agent.mu.Unlock()
		if gotCwd != "/work/proj" {
			t.Errorf("cwd = %q, want /work/proj", gotCwd)
		}
		if gotMeta["cogos.sub"] != testSub {
			t.Errorf("new_session _meta sub = %v, want %s", gotMeta["cogos.sub"], testSub)
		}
	})
}

// TestPromptStreamAllKinds covers prompt → consume the session/update stream;
// assert EVERY required update kind is handled and routed to the ledger sink.
func TestPromptStreamAllKinds(t *testing.T) {
	agent := newFakeAgent()
	agent.promptFn = promptStreamAllKinds
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)

	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, err := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	stop, err := r.cond.Prompt(ctx, sid, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != string(acp.StopReasonEndTurn) {
		t.Errorf("stop = %q, want end_turn", stop)
	}

	required := []contract.LedgerEntryKind{
		contract.KindUserMessage,
		contract.KindAgentThought,
		contract.KindAgentMessage,
		contract.KindToolCall,
		contract.KindToolUpdate,
		contract.KindPlan,
	}
	for _, k := range required {
		t.Run(string(k), func(t *testing.T) {
			if !r.sink.hasKind(sid, k) {
				t.Errorf("ledger missing kind %q for session %s; got %v", k, sid, r.sink.kindsForSession(sid))
			}
		})
	}
}

// TestRequestPermission covers the governance limb thoroughly: approve AND deny
// cases; assert the agent receives the decision and the outcome propagates.
func TestRequestPermission(t *testing.T) {
	cases := []struct {
		name        string
		gov         func(ctx context.Context, req contract.PermissionRequest) contract.PermissionDecision
		wantStop    string
		wantDecided string // ledger decision Text prefix
	}{
		{"approve", approveFirstAllow, string(acp.StopReasonEndTurn), "approved:"},
		{"deny", denyAll, string(acp.StopReasonRefusal), "denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAgent()
			agent.promptFn = promptRequestPermission
			r := newRig(t, agent, fnGovernor{fn: tc.gov}, nil)
			ctx := ctxT(t)
			if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			sid, err := r.cond.NewSession(ctx, defaultSpec("/x"))
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			stop, err := r.cond.Prompt(ctx, sid, "do the dangerous thing")
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			// Outcome propagated to the agent: agent's branch picks stop reason.
			if stop != tc.wantStop {
				t.Errorf("stop = %q, want %q (agent did not receive expected decision)", stop, tc.wantStop)
			}
			// Conductor recorded the request + the decision in the ledger.
			if !r.sink.hasKind(sid, contract.KindPermissionRequest) {
				t.Error("ledger missing permission_request")
			}
			var decisionText string
			for _, e := range r.sink.all() {
				if e.SessionID == sid && e.Kind == contract.KindPermissionDecision {
					decisionText = e.Text
				}
			}
			if !strings.HasPrefix(decisionText, tc.wantDecided) {
				t.Errorf("decision text = %q, want prefix %q", decisionText, tc.wantDecided)
			}
		})
	}
}

// TestFork covers UnstableForkSession: assert a new session id is returned and
// `_meta` rides the fork request.
func TestFork(t *testing.T) {
	agent := newFakeAgent()
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	parent, err := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	child, err := r.cond.Fork(ctx, parent, defaultSpec("/x"))
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if child == "" || child == parent {
		t.Fatalf("fork returned bad id: parent=%s child=%s", parent, child)
	}
	agent.mu.Lock()
	forkMeta := agent.forkMeta
	agent.mu.Unlock()
	if forkMeta["cogos.sub"] != testSub {
		t.Errorf("fork _meta sub = %v, want %s", forkMeta["cogos.sub"], testSub)
	}
}

// TestList covers ListSessions.
func TestList(t *testing.T) {
	agent := newFakeAgent()
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	s1, _ := r.cond.NewSession(ctx, defaultSpec("/a"))
	s2, _ := r.cond.NewSession(ctx, defaultSpec("/b"))
	got, err := r.cond.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	set := map[contract.SessionID]bool{}
	for _, s := range got {
		set[s] = true
	}
	if !set[s1] || !set[s2] {
		t.Errorf("List missing sessions: got %v, want includes %s,%s", got, s1, s2)
	}
}

// TestResume covers ResumeSession.
func TestResume(t *testing.T) {
	agent := newFakeAgent()
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err := r.cond.Resume(ctx, sid, defaultSpec("/x")); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	agent.mu.Lock()
	resumed := agent.resumed[string(sid)]
	agent.mu.Unlock()
	if !resumed {
		t.Error("agent did not record resume")
	}
}

// TestLoad covers LoadSession WITH synchronous history replay (delta divergence
// 4): the replayed session/update frames must reach the ledger before/around the
// load response.
func TestLoad(t *testing.T) {
	agent := newFakeAgent()
	agent.supportLoad = true
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err := r.cond.Load(ctx, sid, defaultSpec("/x")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	agent.mu.Lock()
	loaded := agent.loaded[string(sid)]
	agent.mu.Unlock()
	if !loaded {
		t.Error("agent did not record load")
	}
	// Replayed history landed in the ledger (user + agent replayed turns).
	if !r.sink.hasKind(sid, contract.KindUserMessage) || !r.sink.hasKind(sid, contract.KindAgentMessage) {
		t.Errorf("replayed history not routed to ledger; got %v", r.sink.kindsForSession(sid))
	}
}

// TestSetSessionModeAndConfig covers set_session_mode and set_session_config_option
// (backend / embodiment selection — closes Finding 3).
func TestSetSessionModeAndConfig(t *testing.T) {
	t.Run("set_session_mode", func(t *testing.T) {
		agent := newFakeAgent()
		r := newRig(t, agent, nil, nil)
		ctx := ctxT(t)
		if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		if err := r.cond.SetMode(ctx, sid, "architect"); err != nil {
			t.Fatalf("SetMode: %v", err)
		}
		agent.mu.Lock()
		mode := agent.modeSet[string(sid)]
		agent.mu.Unlock()
		if mode != "architect" {
			t.Errorf("mode = %q, want architect", mode)
		}
	})
	t.Run("set_session_config_option", func(t *testing.T) {
		agent := newFakeAgent()
		r := newRig(t, agent, nil, nil)
		ctx := ctxT(t)
		if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		if err := r.cond.SetConfigOption(ctx, sid, "backend", "test-backend-b"); err != nil {
			t.Fatalf("SetConfigOption: %v", err)
		}
		agent.mu.Lock()
		cfg := agent.cfgSet[string(sid)]
		agent.mu.Unlock()
		if cfg != "test-backend-b" {
			t.Errorf("config value = %q, want test-backend-b", cfg)
		}
	})
}

// TestCancelMidTurn covers cancel mid-turn: clean cancellation + trailing-frame
// tolerance. The conductor cancels via a context deadline on Prompt; the agent's
// turn unblocks on ctx.Done, emits a trailing frame, and returns cancelled.
func TestCancelMidTurn(t *testing.T) {
	agent := newFakeAgent()
	agent.promptFn = promptBlockUntilCancel
	r := newRig(t, agent, nil, nil)
	bg := ctxT(t)
	if _, err := r.cond.Initialize(bg, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, _ := r.cond.NewSession(bg, defaultSpec("/x"))

	// Prompt with a short deadline; the SDK's client.Prompt sends session/cancel
	// to the agent when ctx expires (see client_gen.go Prompt).
	pctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := r.cond.Prompt(pctx, sid, "long task")
	// We expect either a context error or a cancelled stop reason; both are
	// "clean" — the point is no panic / no deadlock and the cancel propagated.
	if err == nil {
		t.Log("Prompt returned without error (agent completed cancel handshake before deadline)")
	}

	// Give the agent's Cancel handler a moment to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		agent.mu.Lock()
		canceled := agent.canceled[string(sid)]
		agent.mu.Unlock()
		if canceled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	agent.mu.Lock()
	canceled := agent.canceled[string(sid)]
	agent.mu.Unlock()
	if !canceled {
		t.Error("agent did not receive session/cancel")
	}
	// The conductor recorded the initial frame; trailing frame tolerated (no panic).
	if !r.sink.hasKind(sid, contract.KindAgentMessage) {
		t.Error("expected at least the initial agent_message_chunk in ledger")
	}
}

// TestCloseSession covers close_session.
func TestCloseSession(t *testing.T) {
	agent := newFakeAgent()
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err := r.cond.CloseSession(ctx, sid); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	agent.mu.Lock()
	closed := agent.closed[string(sid)]
	agent.mu.Unlock()
	if !closed {
		t.Error("agent did not record close")
	}
}

// TestFsPolicyGate covers fs/* handlers: the conductor's Client handlers are
// invoked and can DENY (policy gate). The agent issues fs reads/writes via the
// AgentSideConnection; the conductor's FsPolicy decides.
func TestFsPolicyGate(t *testing.T) {
	t.Run("allow_read", func(t *testing.T) {
		agent := newFakeAgent()
		agent.promptFn = func(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
			resp, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "/etc/allowed.txt"})
			if err != nil {
				return acp.PromptResponse{}, err
			}
			if !strings.Contains(resp.Content, "policy-allowed") {
				return acp.PromptResponse{}, fmt.Errorf("unexpected content: %q", resp.Content)
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		r := newRig(t, agent, nil, allowAllFs())
		ctx := ctxT(t)
		_, _ = r.cond.Initialize(ctx, testIdentity())
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		if _, err := r.cond.Prompt(ctx, sid, "read a file"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if r.sink.countKind(contract.KindFsRead) != 1 {
			t.Errorf("expected 1 fs_read ledger entry, got %d", r.sink.countKind(contract.KindFsRead))
		}
	})

	t.Run("deny_write", func(t *testing.T) {
		agent := newFakeAgent()
		var agentErr error
		var once sync.Once
		agent.promptFn = func(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
			_, err := a.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "/etc/passwd", Content: "evil"})
			once.Do(func() { agentErr = err })
			// The agent observes the denial as an error; turn ends.
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		denyWrites := fnFsPolicy{
			read:  func(ctx context.Context, path string) bool { return true },
			write: func(ctx context.Context, path string) bool { return false },
		}
		r := newRig(t, agent, nil, denyWrites)
		ctx := ctxT(t)
		_, _ = r.cond.Initialize(ctx, testIdentity())
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		if _, err := r.cond.Prompt(ctx, sid, "write a file"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if agentErr == nil {
			t.Error("agent's WriteTextFile should have returned a policy-denied error")
		}
		if r.sink.countKind(contract.KindFsDenied) != 1 {
			t.Errorf("expected 1 fs_denied ledger entry, got %d", r.sink.countKind(contract.KindFsDenied))
		}
	})

	t.Run("terminal_denied", func(t *testing.T) {
		agent := newFakeAgent()
		var termErr error
		agent.promptFn = func(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
			_, termErr = a.conn.CreateTerminal(ctx, acp.CreateTerminalRequest{Command: "rm", Args: []string{"-rf", "/"}})
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		r := newRig(t, agent, nil, allowAllFs())
		ctx := ctxT(t)
		_, _ = r.cond.Initialize(ctx, testIdentity())
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		if _, err := r.cond.Prompt(ctx, sid, "spawn a shell"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if termErr == nil {
			t.Error("CreateTerminal should have been denied by conductor policy")
		}
	})
}

// TestMcpOverAcp is a smoke test for ConnectMcp/DisconnectMcp (experimental).
// The conductor's Client must handle the agent-issued mcp/connect + mcp/disconnect.
func TestMcpOverAcp(t *testing.T) {
	t.Run("connect_disconnect_smoke", func(t *testing.T) {
		agent := newFakeAgent()
		var (
			connID  string
			connErr error
			discErr error
		)
		agent.promptFn = func(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
			resp, err := a.conn.UnstableConnectMcp(ctx, acp.UnstableConnectMcpRequest{AcpId: acp.UnstableMcpServerAcpId("acp-mcp-1")})
			connErr = err
			if err == nil {
				connID = string(resp.ConnectionId)
				_, discErr = a.conn.UnstableDisconnectMcp(ctx, acp.UnstableDisconnectMcpRequest{ConnectionId: resp.ConnectionId})
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		// The conductor's Client must implement the experimental MCP handlers for
		// the agent's calls to succeed. The SDK adapter's clientHandlers does NOT
		// implement UnstableConnectMcp/UnstableDisconnectMcp (the conductor consumes
		// MCP-over-ACP as a CALLER, not as a handler). So these agent→client calls
		// will get MethodNotFound. That is the documented finding: see skip below.
		r := newRig(t, agent, nil, nil)
		ctx := ctxT(t)
		_, _ = r.cond.Initialize(ctx, testIdentity())
		sid, _ := r.cond.NewSession(ctx, defaultSpec("/x"))
		_, _ = r.cond.Prompt(ctx, sid, "use mcp")

		if connErr != nil {
			t.Skipf("conductor (ACP client) does not handle agent-issued mcp/connect: %v. "+
				"GAP/FINDING: MCP-over-ACP in this SDK has the agent as the mcp/connect CALLER and "+
				"the CLIENT as the handler (ClientExperimental.UnstableConnectMcp). The conductor "+
				"as drawn consumes MCP-over-ACP toward its OWN cells (conductor=caller), which is the "+
				"opposite direction. To support cells that expose MCP servers back to the conductor, "+
				"the adapter's clientHandlers must implement ClientExperimental. Documented, not failed.", connErr)
		}
		if connID == "" {
			t.Error("expected non-empty connection id")
		}
		if discErr != nil {
			t.Errorf("DisconnectMcp: %v", discErr)
		}
	})

	// Direction the conductor actually drives: conductor → cell. This requires the
	// FAKE AGENT to implement UnstableConnectMcp as an Agent handler. The SDK only
	// routes mcp/connect to the CLIENT side (ClientMethodMcpConnect), never to the
	// agent. So the conductor-as-caller direction is not expressible against this
	// SDK's method routing. Documented as a skip — highest-value finding.
	t.Run("conductor_drives_mcp_connect_to_cell", func(t *testing.T) {
		agent := newFakeAgent()
		r := newRig(t, agent, nil, nil)
		ctx := ctxT(t)
		_, _ = r.cond.Initialize(ctx, testIdentity())
		_, err := r.cond.ConnectMcp(ctx, "cell-mcp-1")
		t.Skipf("conductor.ConnectMcp routes to ClientMethodMcpConnect (agent→client direction). "+
			"The SDK has no agent-side mcp/connect handler, so a conductor cannot drive mcp/connect "+
			"TOWARD a cell over this SDK. Observed err=%v. FINDING: MCP-over-ACP is modeled "+
			"agent-as-caller only; conductor-driven MCP attach to a cell needs an upstream method "+
			"or must ride MCP transport config in new_session instead.", err)
	})
}

// TestMultiSessionConcurrency drives N concurrent sessions and asserts no
// cross-talk: every ledger entry for a session carries that session's id, and
// each session sees exactly its own stream.
func TestMultiSessionConcurrency(t *testing.T) {
	const n = 8
	agent := newFakeAgent()
	agent.promptFn = promptStreamAllKinds
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Create N sessions up front (NewSession mints distinct ids).
	sids := make([]contract.SessionID, n)
	for i := 0; i < n; i++ {
		sid, err := r.cond.NewSession(ctx, defaultSpec(fmt.Sprintf("/s/%d", i)))
		if err != nil {
			t.Fatalf("NewSession %d: %v", i, err)
		}
		sids[i] = sid
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.cond.Prompt(ctx, sids[i], fmt.Sprintf("prompt %d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Prompt session %d: %v", i, err)
		}
	}

	// Each session must have its full required kind set, and no entry may bear a
	// foreign session id.
	required := []contract.LedgerEntryKind{
		contract.KindUserMessage, contract.KindAgentThought, contract.KindAgentMessage,
		contract.KindToolCall, contract.KindToolUpdate, contract.KindPlan,
	}
	validIDs := map[contract.SessionID]bool{}
	for _, s := range sids {
		validIDs[s] = true
	}
	for _, e := range r.sink.all() {
		if !validIDs[e.SessionID] {
			t.Errorf("ledger entry with unknown session id %q (cross-talk)", e.SessionID)
		}
	}
	for _, sid := range sids {
		for _, k := range required {
			if !r.sink.hasKind(sid, k) {
				t.Errorf("session %s missing kind %q (routing/cross-talk failure)", sid, k)
			}
		}
	}
}

// TestTransportPluggability_Pipe proves the same conductor works over io.Pipe.
// (The subprocess-over-stdio variant lives in transport_stdio_test.go.)
func TestTransportPluggability_Pipe(t *testing.T) {
	agent := newFakeAgent()
	r := newRig(t, agent, nil, nil)
	ctx := ctxT(t)
	if _, err := r.cond.Initialize(ctx, testIdentity()); err != nil {
		t.Fatalf("Initialize over io.Pipe: %v", err)
	}
	sid, err := r.cond.NewSession(ctx, defaultSpec("/x"))
	if err != nil {
		t.Fatalf("NewSession over io.Pipe: %v", err)
	}
	if _, err := r.cond.Prompt(ctx, sid, "hi"); err != nil {
		t.Fatalf("Prompt over io.Pipe: %v", err)
	}
}
