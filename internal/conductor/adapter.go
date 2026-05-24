// adapter.go backs the portable conductor contract with coder/acp-go-sdk.
//
// It is intentionally thin: it translates between the SDK's wire types and the
// contract's substrate-shaped types, and it wires the SDK's client-side Client
// handlers (RequestPermission, SessionUpdate, ReadTextFile/WriteTextFile,
// terminal ops) to the conductor's Governor / LedgerSink / FsPolicy. This is the
// only file in the package that imports the SDK; everything else speaks contract.

package conductor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	"github.com/myrgic/cogos/internal/conductor/contract"
)

// clientHandlers implements acp.Client. The SDK invokes these when the agent
// (cell) calls back to the client (conductor). It routes everything to the
// substrate-shaped collaborators.
type clientHandlers struct {
	ledger contract.LedgerSink
	gov    contract.Governor
	fs     contract.FsPolicy
}

var _ acp.Client = (*clientHandlers)(nil)

// SessionUpdate consumes the streamed update and routes EVERY kind to the
// ledger (RFC-036 P3 gap 2 + gap 4: consume all kinds, ledger-write-on-update).
func (h *clientHandlers) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	sid := contract.SessionID(n.SessionId)
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindAgentMessage, Text: textOf(u.AgentMessageChunk.Content)})
	case u.AgentThoughtChunk != nil:
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindAgentThought, Text: textOf(u.AgentThoughtChunk.Content)})
	case u.UserMessageChunk != nil:
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindUserMessage, Text: textOf(u.UserMessageChunk.Content)})
	case u.ToolCall != nil:
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindToolCall, Text: u.ToolCall.Title,
			Detail: map[string]any{"id": string(u.ToolCall.ToolCallId), "status": string(u.ToolCall.Status)}})
	case u.ToolCallUpdate != nil:
		status := ""
		if u.ToolCallUpdate.Status != nil {
			status = string(*u.ToolCallUpdate.Status)
		}
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindToolUpdate, Text: string(u.ToolCallUpdate.ToolCallId),
			Detail: map[string]any{"status": status}})
	case u.Plan != nil:
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindPlan, Text: fmt.Sprintf("%d entries", len(u.Plan.Entries)),
			Detail: map[string]any{"entries": len(u.Plan.Entries)}})
	default:
		// Other kinds (available_commands_update, current_mode_update,
		// config_option_update, session_info_update, usage_update) are not in
		// the RFC-036 required set; ignored here.
	}
	return nil
}

// RequestPermission is the governance limb (RFC-036 P4). The cell asks; the
// conductor's Governor makes the Proposer/Actor decision; the outcome propagates
// back to the cell as the ACP outcome.
func (h *clientHandlers) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	sid := contract.SessionID(p.SessionId)
	title := ""
	if p.ToolCall.Title != nil {
		title = *p.ToolCall.Title
	}
	opts := make([]contract.PermissionOption, 0, len(p.Options))
	for _, o := range p.Options {
		opts = append(opts, contract.PermissionOption{ID: string(o.OptionId), Name: o.Name, Kind: kindOf(o.Kind)})
	}
	req := contract.PermissionRequest{SessionID: sid, ToolTitle: title, Options: opts}
	h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindPermissionRequest, Text: title})

	dec := h.gov.Decide(ctx, req)
	if dec.Denied || dec.OptionID == "" {
		h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindPermissionDecision, Text: "denied"})
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	h.ledger.Record(contract.LedgerEntry{SessionID: sid, Kind: contract.KindPermissionDecision, Text: "approved:" + dec.OptionID})
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(dec.OptionID)},
	}}, nil
}

// ReadTextFile gates filesystem reads through FsPolicy (policy gate; can deny).
func (h *clientHandlers) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if h.fs == nil || !h.fs.AllowRead(ctx, p.Path) {
		h.ledger.Record(contract.LedgerEntry{Kind: contract.KindFsDenied, Text: "read:" + p.Path})
		return acp.ReadTextFileResponse{}, fmt.Errorf("fs read denied by policy: %s", p.Path)
	}
	h.ledger.Record(contract.LedgerEntry{Kind: contract.KindFsRead, Text: p.Path})
	// In the scaffold the conductor returns synthetic content; production would
	// resolve through the substrate URI layer.
	// TODO(conductor, RFC-036 P5): resolve reads through the substrate URI layer.
	return acp.ReadTextFileResponse{Content: "policy-allowed:" + p.Path}, nil
}

// WriteTextFile gates filesystem writes through FsPolicy.
func (h *clientHandlers) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if h.fs == nil || !h.fs.AllowWrite(ctx, p.Path) {
		h.ledger.Record(contract.LedgerEntry{Kind: contract.KindFsDenied, Text: "write:" + p.Path})
		return acp.WriteTextFileResponse{}, fmt.Errorf("fs write denied by policy: %s", p.Path)
	}
	h.ledger.Record(contract.LedgerEntry{Kind: contract.KindFsWrite, Text: p.Path})
	return acp.WriteTextFileResponse{}, nil
}

// Terminal handlers: deny by default in the scaffold (policy gate). The conductor
// is not a shell; terminal access is a governed capability.
func (h *clientHandlers) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("terminal denied by conductor policy")
}
func (h *clientHandlers) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, fmt.Errorf("terminal denied by conductor policy")
}
func (h *clientHandlers) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, fmt.Errorf("terminal denied by conductor policy")
}
func (h *clientHandlers) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, fmt.Errorf("terminal denied by conductor policy")
}
func (h *clientHandlers) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, fmt.Errorf("terminal denied by conductor policy")
}

// SDKConductor backs contract.Conductor with a ClientSideConnection.
//
// TODO(conductor, RFC-036 P3+P2): the fleet manager driving N concurrent
// sessions is not built here. SDKConductor tracks session ids for one ACP
// connection; the orchestration loop that owns fleet lifecycle is application
// logic left for the real conductor build. See the package doc scaffold boundary.
type SDKConductor struct {
	conn *acp.ClientSideConnection
	h    *clientHandlers

	mu       sync.Mutex
	sessions map[contract.SessionID]struct{}
}

var _ contract.Conductor = (*SDKConductor)(nil)

// New wires a conductor over the given peer streams (an io.Pipe pair or a
// subprocess' stdin/stdout). gov / ledger / fs are the substrate collaborators.
func New(peerInput io.Writer, peerOutput io.Reader,
	ledger contract.LedgerSink, gov contract.Governor, fs contract.FsPolicy) *SDKConductor {
	h := &clientHandlers{ledger: ledger, gov: gov, fs: fs}
	conn := acp.NewClientSideConnection(h, peerInput, peerOutput)
	return &SDKConductor{conn: conn, h: h, sessions: map[contract.SessionID]struct{}{}}
}

// Done exposes the underlying connection's done channel.
func (c *SDKConductor) Done() <-chan struct{} { return c.conn.Done() }

// SetLogger directs the client connection's diagnostics to l.
func (c *SDKConductor) SetLogger(l *slog.Logger) { c.conn.SetLogger(l) }

func (c *SDKConductor) Initialize(ctx context.Context, id contract.Identity) (int, error) {
	resp, err := c.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		Meta:            id.Meta(),
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	})
	if err != nil {
		return 0, err
	}
	return int(resp.ProtocolVersion), nil
}

func (c *SDKConductor) NewSession(ctx context.Context, spec contract.SessionSpec) (contract.SessionID, error) {
	resp, err := c.conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        spec.Cwd,
		Meta:       spec.Identity.Meta(),
		McpServers: toMcpServers(spec.McpServers),
	})
	if err != nil {
		return "", err
	}
	sid := contract.SessionID(resp.SessionId)
	c.track(sid)
	return sid, nil
}

func (c *SDKConductor) Prompt(ctx context.Context, sid contract.SessionID, text string) (string, error) {
	resp, err := c.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acp.SessionId(sid),
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	if err != nil {
		return "", err
	}
	return string(resp.StopReason), nil
}

func (c *SDKConductor) Cancel(ctx context.Context, sid contract.SessionID) error {
	return c.conn.Cancel(ctx, acp.CancelNotification{SessionId: acp.SessionId(sid)})
}

func (c *SDKConductor) Fork(ctx context.Context, sid contract.SessionID, spec contract.SessionSpec) (contract.SessionID, error) {
	resp, err := c.conn.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{
		SessionId: acp.SessionId(sid),
		Cwd:       spec.Cwd,
		Meta:      spec.Identity.Meta(),
	})
	if err != nil {
		return "", err
	}
	nsid := contract.SessionID(resp.SessionId)
	c.track(nsid)
	return nsid, nil
}

func (c *SDKConductor) List(ctx context.Context) ([]contract.SessionID, error) {
	resp, err := c.conn.ListSessions(ctx, acp.ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]contract.SessionID, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		out = append(out, contract.SessionID(s.SessionId))
	}
	return out, nil
}

func (c *SDKConductor) Resume(ctx context.Context, sid contract.SessionID, spec contract.SessionSpec) error {
	_, err := c.conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId: acp.SessionId(sid),
		Cwd:       spec.Cwd,
		Meta:      spec.Identity.Meta(),
	})
	return err
}

func (c *SDKConductor) Load(ctx context.Context, sid contract.SessionID, spec contract.SessionSpec) error {
	_, err := c.conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  acp.SessionId(sid),
		Cwd:        spec.Cwd,
		Meta:       spec.Identity.Meta(),
		McpServers: toMcpServers(spec.McpServers),
	})
	return err
}

func (c *SDKConductor) SetMode(ctx context.Context, sid contract.SessionID, modeID string) error {
	_, err := c.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
		SessionId: acp.SessionId(sid),
		ModeId:    acp.SessionModeId(modeID),
	})
	return err
}

func (c *SDKConductor) SetConfigOption(ctx context.Context, sid contract.SessionID, configID, value string) error {
	req := acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sid),
			ConfigId:  acp.SessionConfigId(configID),
			Value:     acp.SessionConfigValueId(value),
		},
	}
	_, err := c.conn.SetSessionConfigOption(ctx, req)
	return err
}

func (c *SDKConductor) CloseSession(ctx context.Context, sid contract.SessionID) error {
	_, err := c.conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: acp.SessionId(sid)})
	if err == nil {
		c.untrack(sid)
	}
	return err
}

// ErrMcpDirectionUnsupported documents a real SDK-surface gap: MCP-over-ACP in
// coder/acp-go-sdk is modeled agent-as-caller / client-as-handler only. The
// mcp/connect + mcp/disconnect methods live on *AgentSideConnection*, and the
// matching handlers live on the Client (ClientExperimental). There is NO
// ClientSideConnection.UnstableConnectMcp — so a conductor (ACP client) cannot
// DRIVE mcp/connect toward a cell over this SDK. The conductor attaches MCP
// servers to a cell via new_session's McpServers (transport config) instead.
//
// TODO(conductor, RFC-036 delta divergence-6): upstream-contribution candidate —
// ClientSideConnection.UnstableConnectMcp (or a spec clarification).
var ErrMcpDirectionUnsupported = fmt.Errorf(
	"MCP-over-ACP mcp/connect is agent→client in coder/acp-go-sdk; " +
		"a conductor cannot drive it toward a cell (use new_session McpServers config instead)")

func (c *SDKConductor) ConnectMcp(ctx context.Context, acpID string) (string, error) {
	return "", ErrMcpDirectionUnsupported
}

func (c *SDKConductor) DisconnectMcp(ctx context.Context, connectionID string) error {
	return ErrMcpDirectionUnsupported
}

// --- helpers -----------------------------------------------------------------

func (c *SDKConductor) track(sid contract.SessionID) {
	c.mu.Lock()
	c.sessions[sid] = struct{}{}
	c.mu.Unlock()
}
func (c *SDKConductor) untrack(sid contract.SessionID) {
	c.mu.Lock()
	delete(c.sessions, sid)
	c.mu.Unlock()
}

func textOf(b acp.ContentBlock) string {
	if b.Text != nil {
		return b.Text.Text
	}
	return ""
}

func kindOf(k acp.PermissionOptionKind) contract.PermissionKind {
	switch k {
	case acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways:
		return contract.PermitAllow
	default:
		return contract.PermitReject
	}
}

// toMcpServers passes opaque []acp.McpServer through if the caller supplied SDK
// types; otherwise yields an empty slice (new_session requires non-nil).
func toMcpServers(in []any) []acp.McpServer {
	out := make([]acp.McpServer, 0, len(in))
	for _, v := range in {
		if s, ok := v.(acp.McpServer); ok {
			out = append(out, s)
		}
	}
	return out
}
