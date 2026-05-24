package conductor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// fakeAgent is a programmable ACP *server* (the "cell") that the conductor
// drives. It is an in-process stand-in for Hermes: it satisfies acp.Agent plus
// the experimental method set (fork, mcp connect/disconnect, set model), and it
// records what it received so tests can assert `_meta` round-trip and per-session
// routing. Its Prompt behavior is configurable per scenario via promptFn.
type fakeAgent struct {
	conn *acp.AgentSideConnection

	mu sync.Mutex
	// captured initialize / new_session / fork inbound for _meta assertions.
	initMeta    map[string]any
	newSessMeta map[string]any
	forkMeta    map[string]any
	// per-session new_session payloads, keyed by minted session id.
	newSessionCwd map[string]string
	// sessions registered for list/load/resume bookkeeping.
	sessions map[string]acp.SessionInfo
	loaded   map[string]bool
	resumed  map[string]bool
	modeSet  map[string]string
	cfgSet   map[string]string
	closed   map[string]bool
	canceled map[string]bool

	// capabilities toggles
	supportLoad bool

	sessionSeq atomic.Int64

	// promptFn drives the session/update stream + permission flow for a turn.
	// If nil, a default single agent_message_chunk + end_turn is sent.
	promptFn func(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error)
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		newSessionCwd: map[string]string{},
		sessions:      map[string]acp.SessionInfo{},
		loaded:        map[string]bool{},
		resumed:       map[string]bool{},
		modeSet:       map[string]string{},
		cfgSet:        map[string]string{},
		closed:        map[string]bool{},
		canceled:      map[string]bool{},
	}
}

func (a *fakeAgent) setConn(c *acp.AgentSideConnection) { a.conn = c }

func (a *fakeAgent) mintSessionID() acp.SessionId {
	n := a.sessionSeq.Add(1)
	return acp.SessionId(fmt.Sprintf("sess_%d", n))
}

// --- core acp.Agent ---

func (a *fakeAgent) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.mu.Lock()
	a.initMeta = p.Meta
	a.mu.Unlock()
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: a.supportLoad,
		},
	}, nil
}

func (a *fakeAgent) Authenticate(ctx context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *fakeAgent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	sid := a.mintSessionID()
	a.mu.Lock()
	a.newSessMeta = p.Meta
	a.newSessionCwd[string(sid)] = p.Cwd
	a.sessions[string(sid)] = acp.SessionInfo{SessionId: sid, Cwd: p.Cwd}
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: sid}, nil
}

func (a *fakeAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	if a.promptFn != nil {
		return a.promptFn(ctx, a, p)
	}
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText("default reply"),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *fakeAgent) Cancel(ctx context.Context, p acp.CancelNotification) error {
	a.mu.Lock()
	a.canceled[string(p.SessionId)] = true
	a.mu.Unlock()
	return nil
}

func (a *fakeAgent) CloseSession(ctx context.Context, p acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	a.mu.Lock()
	a.closed[string(p.SessionId)] = true
	delete(a.sessions, string(p.SessionId))
	a.mu.Unlock()
	return acp.CloseSessionResponse{}, nil
}

func (a *fakeAgent) ListSessions(ctx context.Context, p acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]acp.SessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s)
	}
	return acp.ListSessionsResponse{Sessions: out}, nil
}

func (a *fakeAgent) ResumeSession(ctx context.Context, p acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	a.mu.Lock()
	a.resumed[string(p.SessionId)] = true
	a.mu.Unlock()
	return acp.ResumeSessionResponse{}, nil
}

func (a *fakeAgent) SetSessionMode(ctx context.Context, p acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	a.mu.Lock()
	a.modeSet[string(p.SessionId)] = string(p.ModeId)
	a.mu.Unlock()
	return acp.SetSessionModeResponse{}, nil
}

func (a *fakeAgent) SetSessionConfigOption(ctx context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	a.mu.Lock()
	if p.ValueId != nil {
		a.cfgSet[string(p.ValueId.SessionId)] = string(p.ValueId.Value)
	}
	a.mu.Unlock()
	return acp.SetSessionConfigOptionResponse{}, nil
}

// --- AgentLoader (optional) ---

func (a *fakeAgent) LoadSession(ctx context.Context, p acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	// Hermes-shape: replay history synchronously via session/update BEFORE the
	// load response returns (delta divergence 4).
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateUserMessageText("(replayed) prior user turn"),
	})
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText("(replayed) prior agent turn"),
	})
	a.mu.Lock()
	a.loaded[string(p.SessionId)] = true
	a.mu.Unlock()
	return acp.LoadSessionResponse{}, nil
}

// --- AgentExperimental subset we exercise ---

func (a *fakeAgent) UnstableForkSession(ctx context.Context, p acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	sid := a.mintSessionID()
	a.mu.Lock()
	a.forkMeta = p.Meta
	a.sessions[string(sid)] = acp.SessionInfo{SessionId: sid, Cwd: p.Cwd}
	a.mu.Unlock()
	return acp.UnstableForkSessionResponse{SessionId: sid}, nil
}

// MCP-over-ACP: the agent (cell) is the CALLER of mcp/connect; the conductor's
// client handles it. So the fakeAgent issues these toward the client. We expose
// helper methods rather than implementing them as Agent handlers.

// --- helpers for assertions ---

func (a *fakeAgent) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"initMeta":    a.initMeta,
		"newSessMeta": a.newSessMeta,
		"forkMeta":    a.forkMeta,
	}
}

// promptStreamAllKinds streams one of every required update kind, then ends.
func promptStreamAllKinds(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := p.SessionId
	send := func(u acp.SessionUpdate) {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
	}
	send(acp.UpdateUserMessageText("echo: user said hi"))
	send(acp.UpdateAgentThoughtText("thinking about it"))
	send(acp.UpdateAgentMessageText("here is my answer"))
	send(acp.StartToolCall(acp.ToolCallId("call_1"), "run analysis",
		acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusPending)))
	send(acp.UpdateToolCall(acp.ToolCallId("call_1"), acp.WithUpdateStatus(acp.ToolCallStatusCompleted)))
	send(acp.UpdatePlan(
		acp.PlanEntry{Content: "step one", Priority: acp.PlanEntryPriorityHigh, Status: acp.PlanEntryStatusPending},
	))
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// promptRequestPermission asks the client for permission mid-turn, then acts on
// the decision: on approval streams a completed tool call; on deny streams a
// failed tool call. The outcome is observable to the test via the ledger and via
// the returned stop reason.
func promptRequestPermission(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := p.SessionId
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.StartToolCall(acp.ToolCallId("danger_1"), "delete production data",
			acp.WithStartKind(acp.ToolKindDelete), acp.WithStartStatus(acp.ToolCallStatusPending)),
	})
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sid,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId("danger_1"),
			Title:      acp.Ptr("delete production data"),
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")},
		},
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if resp.Outcome.Selected != nil && string(resp.Outcome.Selected.OptionId) == "allow" {
		_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateToolCall(acp.ToolCallId("danger_1"), acp.WithUpdateStatus(acp.ToolCallStatusCompleted)),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	// Denied / cancelled outcome.
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateToolCall(acp.ToolCallId("danger_1"), acp.WithUpdateStatus(acp.ToolCallStatusFailed)),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
}

// promptBlockUntilCancel blocks the turn until the context is cancelled, then
// emits a trailing frame and returns the cancelled stop reason. Exercises clean
// mid-turn cancellation.
func promptBlockUntilCancel(ctx context.Context, a *fakeAgent, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := p.SessionId
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateAgentMessageText("starting long work"),
	})
	select {
	case <-ctx.Done():
		// Trailing frame after cancellation: the SDK keeps inbound ctx alive long
		// enough to deliver this; the conductor must tolerate it.
		_ = a.conn.SessionUpdate(context.Background(), acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateAgentMessageText("(cancelled, cleaning up)"),
		})
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	case <-time.After(5 * time.Second):
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
}
