package conductor

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/myrgic/cogos/internal/conductor/contract"
)

// quietLogger discards SDK connection diagnostics (e.g. "connection closed"
// INFO lines on pipe teardown) to keep test output focused on assertions.
var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// captureSink is the test stand-in for the cogos ledger. Concurrency-safe so it
// can absorb N sessions streaming simultaneously.
type captureSink struct {
	mu      sync.Mutex
	entries []contract.LedgerEntry
}

func (s *captureSink) Record(e contract.LedgerEntry) {
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
}

func (s *captureSink) all() []contract.LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contract.LedgerEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *captureSink) kindsForSession(sid contract.SessionID) []contract.LedgerEntryKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contract.LedgerEntryKind
	for _, e := range s.entries {
		if e.SessionID == sid {
			out = append(out, e.Kind)
		}
	}
	return out
}

func (s *captureSink) hasKind(sid contract.SessionID, k contract.LedgerEntryKind) bool {
	for _, got := range s.kindsForSession(sid) {
		if got == k {
			return true
		}
	}
	return false
}

func (s *captureSink) countKind(k contract.LedgerEntryKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// fnGovernor adapts a func to contract.Governor.
type fnGovernor struct {
	fn func(ctx context.Context, req contract.PermissionRequest) contract.PermissionDecision
}

func (g fnGovernor) Decide(ctx context.Context, req contract.PermissionRequest) contract.PermissionDecision {
	return g.fn(ctx, req)
}

// approveFirstAllow picks the first option classified as allow; denies if none.
func approveFirstAllow(ctx context.Context, req contract.PermissionRequest) contract.PermissionDecision {
	for _, o := range req.Options {
		if o.Kind == contract.PermitAllow {
			return contract.PermissionDecision{OptionID: o.ID}
		}
	}
	return contract.PermissionDecision{Denied: true}
}

// denyAll always denies.
func denyAll(ctx context.Context, req contract.PermissionRequest) contract.PermissionDecision {
	return contract.PermissionDecision{Denied: true}
}

// fnFsPolicy adapts funcs to contract.FsPolicy.
type fnFsPolicy struct {
	read  func(ctx context.Context, path string) bool
	write func(ctx context.Context, path string) bool
}

func (p fnFsPolicy) AllowRead(ctx context.Context, path string) bool {
	if p.read == nil {
		return false
	}
	return p.read(ctx, path)
}
func (p fnFsPolicy) AllowWrite(ctx context.Context, path string) bool {
	if p.write == nil {
		return false
	}
	return p.write(ctx, path)
}

func allowAllFs() fnFsPolicy {
	return fnFsPolicy{
		read:  func(ctx context.Context, path string) bool { return true },
		write: func(ctx context.Context, path string) bool { return true },
	}
}

// rig is a fully wired in-process conductor↔cell pair over io.Pipe.
type rig struct {
	cond   *SDKConductor
	agent  *fakeAgent
	sink   *captureSink
	asc    *acp.AgentSideConnection
	closeF func()
}

// newRig wires a conductor (ACP client) to a fakeAgent (ACP server) over two
// io.Pipe duplexes. gov / fs default to approve-allow / allow-all if nil.
func newRig(t *testing.T, agent *fakeAgent, gov contract.Governor, fs contract.FsPolicy) *rig {
	t.Helper()

	// Two pipes: clientToAgent carries client→agent bytes; agentToClient carries
	// agent→client bytes. The client writes to clientToAgentW and reads from
	// agentToClientR; the agent writes to agentToClientW and reads clientToAgentR.
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	sink := &captureSink{}
	if gov == nil {
		gov = fnGovernor{fn: approveFirstAllow}
	}
	if fs == nil {
		fs = allowAllFs()
	}

	cond := New(c2aW, a2cR, sink, gov, fs)
	cond.SetLogger(quietLogger)
	asc := acp.NewAgentSideConnection(agent, a2cW, c2aR)
	asc.SetLogger(quietLogger)
	agent.setConn(asc)

	closeF := func() {
		_ = c2aW.Close()
		_ = a2cW.Close()
		_ = c2aR.Close()
		_ = a2cR.Close()
	}
	t.Cleanup(closeF)

	return &rig{cond: cond, agent: agent, sink: sink, asc: asc, closeF: closeF}
}

// newConductorOverStreams wires a conductor to arbitrary peer streams (e.g. a
// subprocess' stdin/stdout). Proves transport pluggability: the same adapter
// works over any io.Writer/io.Reader pair.
func newConductorOverStreams(peerInput io.Writer, peerOutput io.Reader, sink *captureSink) *SDKConductor {
	c := New(peerInput, peerOutput, sink,
		fnGovernor{fn: approveFirstAllow}, allowAllFs())
	c.SetLogger(quietLogger)
	return c
}

const testIss = "cog://identity/conductor/darkstar"
const testSub = "cog://identity/cell/hermes-1"

func testIdentity() contract.Identity {
	return contract.Identity{Iss: testIss, Sub: testSub}
}

func defaultSpec(cwd string) contract.SessionSpec {
	return contract.SessionSpec{
		Cwd:        cwd,
		Identity:   testIdentity(),
		McpServers: nil,
	}
}
