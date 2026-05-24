// Command fakeagent is a minimal stdio ACP *server* (cell) used by the stdio
// transport-pluggability test. It mirrors the in-process fakeagent's behavior:
// echoes `_meta` back on initialize as an agent_message_chunk so the parent can
// assert identity round-trip over a real OS pipe, streams one agent message, and
// ends the turn.
//
// It is its own package main so `go run ./cmd/fakeagent` spawns a real process
// the conductor talks to over stdin/stdout — proving the same client works over
// a stdio-spawned subprocess, not just io.Pipe. This is a test helper for the
// conductor scaffold, not a production command.
package main

import (
	"context"
	"fmt"
	"os"

	acp "github.com/coder/acp-go-sdk"
)

type stdioAgent struct {
	conn    *acp.AgentSideConnection
	lastSub string
}

func (a *stdioAgent) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	if p.Meta != nil {
		if v, ok := p.Meta["cogos.sub"].(string); ok {
			a.lastSub = v
		}
	}
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (a *stdioAgent) Authenticate(ctx context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *stdioAgent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId("stdio_sess_1")}, nil
}

func (a *stdioAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	// Echo the captured substrate identity so the parent can assert _meta made it
	// across the OS pipe.
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText(fmt.Sprintf("sub=%s", a.lastSub)),
	})
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *stdioAgent) Cancel(ctx context.Context, _ acp.CancelNotification) error { return nil }
func (a *stdioAgent) CloseSession(ctx context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *stdioAgent) ListSessions(ctx context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}
func (a *stdioAgent) ResumeSession(ctx context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *stdioAgent) SetSessionMode(ctx context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *stdioAgent) SetSessionConfigOption(ctx context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func main() {
	ag := &stdioAgent{}
	// peerInput = where we write to the peer = os.Stdout; peerOutput = what the
	// peer sends us = os.Stdin.
	asc := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	ag.conn = asc
	<-asc.Done()
}
