// Package contract distills the portable conductor-facing ACP-client interface
// derived from RFC-036 ("The Conductor — CogOS-as-ACP-Client as the Substrate's
// Conducting Seat") and the Hermes/mod3 ACP implementation delta.
//
// This is the structural-isomorphism boundary made executable. The conductor —
// CogOS occupying the ACP *client* role — expects exactly the surface defined
// here. The coder/acp-go-sdk is one backing for it; a native reimplementation is
// another. Tests written against this package therefore validate the *contract*,
// not the SDK, and a SDK swap must keep these tests green.
//
// Design rule (RFC-036 structural-isomorphism vendoring): types in this package
// are SDK-free where the contract can be expressed generically. The conductor
// thinks in terms of sessions, turns, governance decisions, and ledger traces —
// not acp.SessionNotification. Where a value is genuinely an opaque protocol
// payload (an MCP server config, a permission option), the contract carries it
// as an opaque handle rather than re-typing the SDK.
package contract

import "context"

// SessionID is the conductor's handle to a conducted cell session.
type SessionID string

// --- Identity injection (RFC-036 P3 gap 6, delta recommendation 9) ----------

// Identity is the substrate identity the conductor injects into a cell at birth.
// It rides ACP's `_meta` extension point on initialize / new_session so the cell
// is "born substrate-bound." iss/sub mirror the substrate's binding model
// (HarnessBinding URIs per the K8s-RBAC framing): iss = the conducting authority,
// sub = the bound cell identity.
//
// TODO(conductor, RFC-036 P3 gap 6): production derives Iss/Sub from a real
// HarnessBinding identity record, not a literal. See the package doc scaffold
// boundary.
type Identity struct {
	Iss string // issuing authority (the conductor's substrate identity URI)
	Sub string // subject (the cell's substrate identity URI)
}

// Meta renders the identity as the `_meta` map the conductor attaches to ACP
// requests. The cogos.* namespace keeps substrate keys from colliding with the
// protocol's reserved space.
func (i Identity) Meta() map[string]any {
	return map[string]any{
		"cogos.iss": i.Iss,
		"cogos.sub": i.Sub,
	}
}

// --- The ledger sink (RFC-036 P5: stigmergic coordination) -------------------

// LedgerEntryKind enumerates every routed event the conductor must persist. The
// management surface IS the observation surface IS the training corpus, so every
// session/update kind plus governance decisions must land in the ledger.
type LedgerEntryKind string

const (
	// Streamed update kinds (every kind the conductor must consume — RFC-036 P3 gap 2).
	KindAgentMessage LedgerEntryKind = "agent_message_chunk"
	KindAgentThought LedgerEntryKind = "agent_thought_chunk"
	KindUserMessage  LedgerEntryKind = "user_message_chunk"
	KindToolCall     LedgerEntryKind = "tool_call"
	KindToolUpdate   LedgerEntryKind = "tool_call_update"
	KindPlan         LedgerEntryKind = "plan"

	// Governance + lifecycle traces.
	KindPermissionRequest  LedgerEntryKind = "permission_request"
	KindPermissionDecision LedgerEntryKind = "permission_decision"
	KindFsRead             LedgerEntryKind = "fs_read"
	KindFsWrite            LedgerEntryKind = "fs_write"
	KindFsDenied           LedgerEntryKind = "fs_denied"
)

// LedgerEntry is one stigmergic trace. SessionID is load-bearing for the
// multi-session concurrency guarantee: every entry must be attributable to the
// session it came from, with no cross-talk.
type LedgerEntry struct {
	SessionID SessionID
	Kind      LedgerEntryKind
	// Text is the human-readable payload (message text, tool title, decision id).
	Text string
	// Detail holds structured payload where Text is insufficient.
	Detail map[string]any
}

// LedgerSink is the conductor's persistence boundary. In production this is the
// cogos ledger; in tests it is a capture sink. It must be safe for concurrent
// use — N sessions stream updates simultaneously.
//
// TODO(conductor, RFC-036 P5 / ADR-013): the production implementation wires
// this to the real kernel ledger so every update becomes a stigmergic trace.
type LedgerSink interface {
	Record(entry LedgerEntry)
}

// --- Governance (RFC-036 P4: Proposer/Actor consent boundary) ----------------

// PermissionRequest is the conductor's view of a cell asking to do something
// dangerous mid-turn (ACP session/request_permission, agent→client).
type PermissionRequest struct {
	SessionID SessionID
	// ToolTitle is the human-readable action the cell wants to perform.
	ToolTitle string
	// Options are the opaque option identifiers the cell offered. The conductor
	// chooses one to approve, or chooses none to deny.
	Options []PermissionOption
}

// PermissionOption is an offered choice. Kind distinguishes allow/reject so the
// Proposer/Actor decision can be made without re-typing the SDK enum.
type PermissionOption struct {
	ID   string
	Name string
	Kind PermissionKind
}

// PermissionKind is the coarse allow/reject classification of an option.
type PermissionKind string

const (
	PermitAllow  PermissionKind = "allow"
	PermitReject PermissionKind = "reject"
)

// PermissionDecision is the conductor's governance verdict.
type PermissionDecision struct {
	// Approved selects an option by ID. Empty means deny (Cancelled outcome).
	OptionID string
	Denied   bool
}

// Governor makes the Proposer/Actor decision. The conductor (Eigen-as-Proposer)
// proposes; the policy/operator (Actor) authorizes. This is the substrate's
// consent boundary realized as the ACP permission flow.
//
// TODO(conductor, RFC-036 P4 / RFC-009): the production implementation wires
// this to the RFC-009 Proposer/Actor consent boundary.
type Governor interface {
	Decide(ctx context.Context, req PermissionRequest) PermissionDecision
}

// --- Filesystem / terminal policy gate (delta: fs/* client callbacks) --------

// FsPolicy gates the cell's filesystem requests. Returning false denies the
// request — the conductor's Client handler must be able to refuse, not just
// proxy to the OS.
type FsPolicy interface {
	AllowRead(ctx context.Context, path string) bool
	AllowWrite(ctx context.Context, path string) bool
}

// --- Session setup payloads --------------------------------------------------

// SessionSpec is the conductor's request to spin up a cell session. Cwd and
// McpServers map directly onto ACP new_session; Identity rides `_meta`.
type SessionSpec struct {
	Cwd      string
	Identity Identity
	// McpServers are opaque protocol payloads — the conductor passes them
	// through without re-typing the SDK's discriminated union.
	McpServers []any
}

// --- The conductor's ACP-client contract -------------------------------------

// Conductor is the portable surface the CogOS conductor expects of any ACP
// client. Every method corresponds to a RFC-036 requirement or one of the six
// generalization-delta gaps. A backing (SDK adapter, native impl) satisfies
// this interface; the test suite validates the contract through it.
type Conductor interface {
	// Initialize negotiates the protocol and injects substrate identity via
	// `_meta` (RFC-036 P3 gap 6). Returns the negotiated protocol version.
	Initialize(ctx context.Context, id Identity) (protocolVersion int, err error)

	// NewSession opens a cell session with cwd, mcpServers, and `_meta` identity.
	NewSession(ctx context.Context, spec SessionSpec) (SessionID, error)

	// Prompt sends a user message and drives the resulting session/update stream
	// to completion, routing every update kind to the ledger sink (gap 2 + gap 4).
	// Returns the stop reason.
	Prompt(ctx context.Context, sid SessionID, text string) (stopReason string, err error)

	// Cancel requests clean cancellation of an in-flight turn.
	Cancel(ctx context.Context, sid SessionID) error

	// --- Fleet verbs (RFC-036 P3 gap 5 + P2) ---

	// Fork branches a session (ACP session/fork). Returns the new session id.
	Fork(ctx context.Context, sid SessionID, spec SessionSpec) (SessionID, error)
	// List enumerates known sessions (ACP session/list).
	List(ctx context.Context) ([]SessionID, error)
	// Resume re-attaches to a prior session without history replay.
	Resume(ctx context.Context, sid SessionID, spec SessionSpec) error
	// Load re-attaches with synchronous history replay (delta divergence 4).
	Load(ctx context.Context, sid SessionID, spec SessionSpec) error

	// --- Backend / embodiment selection (delta recommendation 7) ---

	// SetMode selects a session mode (ACP session/set_mode).
	SetMode(ctx context.Context, sid SessionID, modeID string) error
	// SetConfigOption selects a backend/embodiment via SessionConfigOption,
	// the on-spec hook for model/backend selection (closes Finding 3).
	SetConfigOption(ctx context.Context, sid SessionID, configID, value string) error

	// CloseSession frees a session's resources (ACP session/close).
	CloseSession(ctx context.Context, sid SessionID) error

	// --- MCP-over-ACP (experimental, delta) ---

	// ConnectMcp opens an MCP-over-ACP connection by ACP id, returns a connection id.
	ConnectMcp(ctx context.Context, acpID string) (connectionID string, err error)
	// DisconnectMcp closes an MCP-over-ACP connection.
	DisconnectMcp(ctx context.Context, connectionID string) error
}
