// mcp_fork_session.go — cog_fork_session MCP tool (RFC-0005).
//
// Registers the cog_fork_session tool in registerForkSessionTool() which is
// called from mcp_sessions.go's registerSessionTools(). The tool creates a
// child session rooted at the parent's current ledger state (or a specific
// parent_state_hash if provided), applying the given SessionOverlay.
//
// The fork registry (ForkRegistry) is wired via SetForkRegistry after the
// sessions backend is set. If not wired, the tool functions but the in-memory
// derived view is not updated (registry is nil-safe).
//
// Structured logs per RFC-0005: every fork emits fields
// operation, parent_session_id, child_session_id, overlay_layers, ts.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── wiring ───────────────────────────────────────────────────────────────────

// SetForkRegistry wires the ForkRegistry into the MCPServer so fork operations
// update the in-memory derived view. Safe to call post-construction.
func (m *MCPServer) SetForkRegistry(fr *ForkRegistry) {
	m.forkRegistry = fr
}

// ─── tool registration ────────────────────────────────────────────────────────

// registerForkSessionTool installs the cog_fork_session MCP tool.
// Called from registerSessionTools() in mcp_sessions.go.
func (m *MCPServer) registerForkSessionTool() {
	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "cog_fork_session",
		Description: "Fork an existing session at a specific ledger state, " +
			"producing a child session with an optional layer overlay. " +
			"Required: parent_session_id. Optional: parent_state_hash " +
			"(defaults to current HEAD), overlay (JSON object per SessionOverlay " +
			"schema with identity/role/context/tools/kv_cache layers), " +
			"pin_duration (ISO 8601 duration, default P7D), child_session_id " +
			"(caller-supplied or auto-minted as fork-<parent>-<hex>). " +
			"Returns: child_session_id, fork_block_hash, fork_point, pinned_until. " +
			"Cross-workspace forks return 501 Not Implemented (reserved for v1).",
	}), withToolObserver(m, "cog_fork_session", m.toolForkSession))
}

// ─── input / output types ────────────────────────────────────────────────────

type forkSessionInput struct {
	// ParentSessionID is required: the session being forked.
	ParentSessionID string `json:"parent_session_id"`
	// ParentStateHash is the content-addressed hash of the parent session state
	// at the desired fork point. If empty, defaults to the parent's current HEAD
	// (latest ledger entry).
	ParentStateHash string `json:"parent_state_hash,omitempty"`
	// Overlay is the layer-by-layer config override for the child session.
	Overlay *SessionOverlay `json:"overlay,omitempty"`
	// PinDuration is an ISO 8601 duration string (e.g. "P30D") overriding the
	// default 7-day GC retention. If empty, DefaultForkRetention applies.
	PinDuration string `json:"pin_duration,omitempty"`
	// ChildSessionID is an optional caller-supplied child session ID. If empty,
	// the kernel mints one as "fork-<parent>-<hex>".
	ChildSessionID string `json:"child_session_id,omitempty"`
}

type forkSessionOutput struct {
	ChildSessionID string `json:"child_session_id"`
	ForkBlockHash  string `json:"fork_block_hash"`
	ForkPoint      int64  `json:"fork_point"`
	PinnedUntil    string `json:"pinned_until,omitempty"` // RFC3339
}

// ─── tool handler ─────────────────────────────────────────────────────────────

func (m *MCPServer) toolForkSession(ctx context.Context, _ *mcp.CallToolRequest, in forkSessionInput) (*mcp.CallToolResult, any, error) {
	if !m.sessionsBackendReady() {
		return fallbackResult("sessions backend not configured",
			"curl -X POST http://localhost:6931/v1/sessions/<id>/fork -d '{...}'")
	}

	if in.ParentSessionID == "" {
		return textResult("parent_session_id is required")
	}
	if err := ValidateSessionID(in.ParentSessionID); err != nil {
		return textResult(fmt.Sprintf("invalid parent_session_id: %v", err))
	}

	// Verify parent exists.
	_, ok := m.sessionRegistry.Get(in.ParentSessionID)
	if !ok {
		return textResult(fmt.Sprintf("parent session %q not found (404)", in.ParentSessionID))
	}

	now := time.Now().UTC()

	// Mint or validate child session ID.
	childID := in.ChildSessionID
	if childID == "" {
		childID = mintForkChildID(in.ParentSessionID, now)
	}
	if err := ValidateSessionID(childID); err != nil {
		return textResult(fmt.Sprintf("invalid child_session_id %q: %v", childID, err))
	}

	// Parse pin duration.
	var pinnedUntil *time.Time
	if in.PinDuration != "" {
		d, err := parseISO8601Duration(in.PinDuration)
		if err != nil {
			return textResult(fmt.Sprintf("invalid pin_duration %q: %v", in.PinDuration, err))
		}
		t := now.Add(d)
		pinnedUntil = &t
	}

	// Resolve parent state hash (use provided or get latest from bus).
	parentHash := in.ParentStateHash
	var forkPoint int64
	if parentHash == "" {
		hash, seq, err := m.busSessions.LatestEventHash(BusSessions)
		if err == nil {
			parentHash = hash
			forkPoint = seq
		}
		// If error (no events yet), parentHash stays "" — still a valid fork.
	}

	// Resolve KV block hash if kvcache overlay requests it.
	overlay := SessionOverlay{}
	if in.Overlay != nil {
		overlay = *in.Overlay
	}
	if overlay.KVCache != nil && overlay.KVCache.InheritParentKV && overlay.KVCache.ParentKVBlockHash == "" {
		// Query the KVBlockHashProvider if one is registered. Degrade
		// gracefully if none is available (RFC-0005 §Cross-RFC integration).
		if m.kvBlockHashProvider != nil {
			hash, err := m.kvBlockHashProvider.ParentKVBlockHash(ctx, in.ParentSessionID, "")
			if err != nil {
				slog.Warn("session.fork: KVBlockHashProvider error (degrading to cold start)",
					"operation", "fork",
					"parent_session_id", in.ParentSessionID,
					"err", err,
				)
			} else {
				overlay.KVCache.ParentKVBlockHash = string(hash)
			}
		}
	}

	// Build the fork body.
	body := SessionForkBody{
		ParentSessionHash: parentHash,
		ParentSessionID:   in.ParentSessionID,
		ChildSessionID:    childID,
		ForkPoint:         forkPoint,
		Overlay:           overlay,
		PinnedUntil:       pinnedUntil,
	}

	// Serialize the body for the bus event payload.
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return fallbackResult(fmt.Sprintf("marshal fork body: %v", err), "")
	}
	var payloadMap map[string]interface{}
	if err := json.Unmarshal(bodyJSON, &payloadMap); err != nil {
		return fallbackResult(fmt.Sprintf("unmarshal fork payload: %v", err), "")
	}

	// Append fork event to bus_sessions.
	evt, err := m.busSessions.AppendEvent(BusSessions, string(KindSessionFork), in.ParentSessionID, payloadMap)
	if err != nil {
		return fallbackResult(fmt.Sprintf("bus append failed: %v", err), "")
	}

	// Register the child session derived from the parent's state.
	parentState, _ := m.sessionRegistry.Get(in.ParentSessionID)
	childState := deriveChildState(parentState, childID, overlay, now)
	_, _, regErr := m.sessionRegistry.ApplyRegister(
		childState,
		time.Duration(defaultActiveWithinSeconds)*time.Second,
		now,
		nil, // child registration event goes on bus via the fork block above
	)
	if regErr != nil {
		// Non-fatal: the bus event was already written. Log and continue.
		slog.Warn("session.fork: child session register failed",
			"operation", "fork",
			"parent_session_id", in.ParentSessionID,
			"child_session_id", childID,
			"err", regErr,
		)
	}

	// Update the fork registry (derived view).
	if m.forkRegistry != nil {
		m.forkRegistry.Record(body, now)
	}

	// Structured log per RFC-0005 §Structured logs.
	slog.Info("session.fork: forked",
		"operation", "fork",
		"parent_session_id", in.ParentSessionID,
		"child_session_id", childID,
		"overlay_layers", overlay.OverlayLayers(),
		"ts", now.Format(time.RFC3339),
	)

	out := forkSessionOutput{
		ChildSessionID: childID,
		ForkBlockHash:  evt.Hash,
		ForkPoint:      forkPoint,
	}
	if pinnedUntil != nil {
		out.PinnedUntil = pinnedUntil.Format(time.RFC3339)
	}
	return marshalResult(out)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// mintForkChildID creates a child session ID in the format
// "fork-<parent_trunc>-<hex>" so it's always a valid 3-component hyphenated ID.
func mintForkChildID(parentID string, now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	// Truncate parent to first component to keep ID reasonable length.
	first := parentID
	if idx := len(first); idx > 12 {
		first = first[:12]
	}
	return fmt.Sprintf("fork-%s-%s%d", first, hex.EncodeToString(b[:]), now.UnixMilli()%10000)
}

// deriveChildState builds a SessionState for the child session by inheriting
// from the parent and applying the overlay's role layer (other layers are
// metadata; they don't have a direct SessionState field mapping in v0.5.0).
func deriveChildState(parent *SessionState, childID string, overlay SessionOverlay, now time.Time) SessionState {
	child := SessionState{
		SessionID:    childID,
		Workspace:    "",
		Role:         "fork-child",
		RegisteredAt: now,
		LastSeen:     now,
	}
	if parent != nil {
		child.Workspace = parent.Workspace
		child.Role = parent.Role
		child.Model = parent.Model
		child.Hostname = parent.Hostname
	}
	if overlay.Role != nil && overlay.Role.Role != "" {
		child.Role = overlay.Role.Role
	}
	return child
}

// parseISO8601Duration parses a subset of ISO 8601 duration strings into
// a time.Duration. Supports P<n>D, PT<n>H, PT<n>M, P<n>W, and combinations
// of weeks and days. Sufficient for the pin_duration use case.
func parseISO8601Duration(s string) (time.Duration, error) {
	if len(s) < 2 || s[0] != 'P' {
		return 0, fmt.Errorf("must start with P")
	}
	var total time.Duration
	cur := s[1:]
	inTime := false
	for len(cur) > 0 {
		if cur[0] == 'T' {
			inTime = true
			cur = cur[1:]
			continue
		}
		var n int64
		i := 0
		for i < len(cur) && cur[i] >= '0' && cur[i] <= '9' {
			n = n*10 + int64(cur[i]-'0')
			i++
		}
		if i == 0 || i >= len(cur) {
			return 0, fmt.Errorf("unexpected format near %q", cur)
		}
		unit := cur[i]
		cur = cur[i+1:]
		switch {
		case !inTime && unit == 'Y':
			total += time.Duration(n) * 365 * 24 * time.Hour
		case !inTime && unit == 'M':
			total += time.Duration(n) * 30 * 24 * time.Hour
		case !inTime && unit == 'W':
			total += time.Duration(n) * 7 * 24 * time.Hour
		case !inTime && unit == 'D':
			total += time.Duration(n) * 24 * time.Hour
		case inTime && unit == 'H':
			total += time.Duration(n) * time.Hour
		case inTime && unit == 'M':
			total += time.Duration(n) * time.Minute
		case inTime && unit == 'S':
			total += time.Duration(n) * time.Second
		default:
			return 0, fmt.Errorf("unknown unit %c", unit)
		}
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}
