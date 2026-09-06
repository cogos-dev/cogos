// serve_acp.go — ACP JSON-RPC over WebSocket at /v1/acp.
//
// Conformant to Agent Client Protocol as defined by @agentclientprotocol/sdk
// 1.4.0 (schema/types.gen.d.ts), vendored under the dsh recon tree. The wire
// shapes here are transcribed from that schema, not from memory.
//
// HISTORY, kept because it explains the shape of the tests: the first version
// of this file was written from recall and CALLED itself ACP while getting
// four things wrong -- protocolVersion 1 instead of 0, {accepted:true}
// instead of a stopReason, an invented session/update vocabulary, and a
// sessionId that was a Go pointer address. It passed every test it had,
// because every test asked "does it work" and none asked "is it the protocol
// it claims to be." A working demo is not evidence of conformance.
//
// WHY WEBSOCKET: an agent session is bidirectional and long-lived; the client
// must send a follow-up into a session that is mid-stream and receive chunks
// as they land. The SDK itself is WebSocket-first (204 references vs 3 for
// stdio), so this is a sanctioned transport, not a local invention.
//
// SECURITY: bound to localhost by default; Origin is checked against the
// serving host to refuse drive-by WebSocket connects from arbitrary pages.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/myrgic/cogos/internal/acp"
)

// acpProtocolVersion is the ACP version this agent speaks.
//
// Value transcribed from the vendored schema: schema/index.js line 51,
// `export const PROTOCOL_VERSION = 1`.
//
// This constant has now been wrong TWICE, in opposite directions, which is
// why the probe reads the number out of the SDK instead of trusting this
// file. First it was 1 by guess (right by luck, unverified). Then it was
// "corrected" to 0 because a grep for PROTOCOL_VERSION matched
// `MAX_PROTOCOL_VERSION = 0xffff` in protocol-router.js and captured the 0
// from the hex literal. A grep across a dist bundle is not a spec reading.
const acpProtocolVersion = 1

// ACP stop reasons (schema: StopReason).
const (
	acpStopEndTurn   = "end_turn"
	acpStopCancelled = "cancelled"
	acpStopRefusal   = "refusal"
)

type acpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type acpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
}

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// acpConn is one client connection and the session it owns.
type acpConn struct {
	ws        *websocket.Conn
	mu        sync.Mutex // serializes writes; a WS conn has one writer
	sub       *acp.Subprocess
	stop      context.CancelFunc
	sessionID string

	// turn holds the in-flight session/prompt request id. ACP requires the
	// prompt RESULT to carry the stopReason, so the reply cannot be sent
	// until the turn actually ends -- the pump completes it.
	turnMu sync.Mutex
	turnID json.RawMessage
}

func newACPSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "sess_fallback"
	}
	return "sess_" + hex.EncodeToString(b)
}

func (c *acpConn) write(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func (c *acpConn) reply(ctx context.Context, id json.RawMessage, result any) {
	_ = c.write(ctx, acpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *acpConn) fail(ctx context.Context, id json.RawMessage, code int, msg string) {
	_ = c.write(ctx, acpResponse{JSONRPC: "2.0", ID: id, Error: &acpError{Code: code, Message: msg}})
}

// notifyUpdate emits a spec-shaped session/update notification:
// { sessionId, update: { sessionUpdate: "<kind>", ... } }
func (c *acpConn) notifyUpdate(ctx context.Context, update map[string]any) {
	_ = c.write(ctx, acpResponse{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params:  map[string]any{"sessionId": c.sessionID, "update": update},
	})
}

// endTurn completes the in-flight session/prompt with a stopReason. ACP puts
// the stop reason in the prompt RESULT, so a client that awaits the call
// learns why the turn ended without listening for a side-channel event.
func (c *acpConn) endTurn(ctx context.Context, reason string, usage map[string]any) {
	c.turnMu.Lock()
	id := c.turnID
	c.turnID = nil
	c.turnMu.Unlock()
	if id == nil {
		return
	}
	res := map[string]any{"stopReason": reason}
	if usage != nil {
		res["usage"] = usage
	}
	c.reply(ctx, id, res)
}

// handleACP serves GET /v1/acp as a WebSocket speaking ACP JSON-RPC.
func (s *Server) handleACP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*", "[::1]:*", r.Host},
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := &acpConn{ws: ws}
	defer func() {
		if c.stop != nil {
			c.stop()
		}
	}()

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var req acpRequest
		if err := json.Unmarshal(data, &req); err != nil {
			c.fail(ctx, nil, -32700, "parse error")
			continue
		}
		s.dispatchACP(ctx, c, req)
	}
}

func (s *Server) dispatchACP(ctx context.Context, c *acpConn, req acpRequest) {
	switch req.Method {
	case "initialize":
		// Advertise only what is implemented. Claiming a capability the
		// kernel lacks is worse than omitting it: the client builds UI for a
		// promise that never arrives.
		c.reply(ctx, req.ID, map[string]any{
			"protocolVersion": acpProtocolVersion,
			"agentCapabilities": map[string]any{
				"loadSession": false,
				"promptCapabilities": map[string]any{
					"image": false, "audio": false, "embeddedContext": false,
				},
			},
			"authMethods": []any{},
			"agentInfo":   map[string]any{"name": "cogos", "version": "0.1.0"},
			"_meta": map[string]any{
				"cogos": map[string]any{"workspace": s.WorkspaceRoot(), "transport": "websocket"},
			},
		})

	case "session/new":
		if c.sub != nil {
			c.fail(ctx, req.ID, -32600, "session already active on this connection")
			return
		}
		var p struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Cwd == "" {
			p.Cwd = s.WorkspaceRoot()
		}
		sctx, stop := context.WithCancel(context.Background())
		sub, err := acp.Spawn(sctx, acp.SpawnOpts{Cwd: p.Cwd})
		if err != nil {
			stop()
			c.fail(ctx, req.ID, -32000, "spawn failed: "+err.Error())
			return
		}
		// A SessionId is an opaque protocol identifier the client echoes back.
		// The first version used fmt.Sprintf("%p", sub) -- a Go heap address,
		// which leaks memory layout and is not stable across a restart.
		c.sessionID = newACPSessionID()
		c.sub, c.stop = sub, stop
		go c.pump(ctx, sub)
		c.reply(ctx, req.ID, map[string]any{"sessionId": c.sessionID})

	case "session/prompt":
		if c.sub == nil {
			c.fail(ctx, req.ID, -32600, "no session; call session/new first")
			return
		}
		var p struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.fail(ctx, req.ID, -32602, "bad params")
			return
		}
		var sb strings.Builder
		for _, b := range p.Prompt {
			if b.Type == "text" || b.Type == "" {
				sb.WriteString(b.Text)
			}
		}
		// Echo the user's own turn, per the spec's user_message_chunk.
		c.notifyUpdate(ctx, map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": sb.String()},
		})
		c.turnMu.Lock()
		c.turnID = req.ID
		c.turnMu.Unlock()
		if err := c.sub.Send(acp.NewTextPrompt(sb.String())); err != nil {
			c.turnMu.Lock()
			c.turnID = nil
			c.turnMu.Unlock()
			c.fail(ctx, req.ID, -32000, "send failed: "+err.Error())
			return
		}
		// No reply here: the prompt result carries stopReason and is sent by
		// endTurn when the turn actually finishes.

	case "session/cancel":
		// session/cancel is a NOTIFICATION in ACP. The in-flight prompt is
		// completed with stopReason "cancelled" rather than left hanging.
		if c.stop != nil {
			c.stop()
			c.stop, c.sub = nil, nil
		}
		c.endTurn(ctx, acpStopCancelled, nil)
		if req.ID != nil {
			c.reply(ctx, req.ID, map[string]any{})
		}

	default:
		c.fail(ctx, req.ID, -32601, "method not found: "+req.Method)
	}
}

// acpToolKind maps a Claude tool name onto the spec's ToolKind enum so a
// conformant client can pick an icon without knowing our tool names.
func acpToolKind(name string) string {
	switch {
	case name == "Read" || name == "NotebookRead":
		return "read"
	case name == "Edit" || name == "Write" || name == "NotebookEdit":
		return "edit"
	case name == "Bash" || name == "BashOutput" || name == "KillShell":
		return "execute"
	case name == "Grep" || name == "Glob":
		return "search"
	case name == "WebFetch" || name == "WebSearch":
		return "fetch"
	case strings.HasPrefix(name, "mcp__"):
		return "other"
	default:
		return "other"
	}
}

// pump translates driver events into spec-shaped session/update notifications.
func (c *acpConn) pump(ctx context.Context, sub *acp.Subprocess) {
	for ev := range sub.Events() {
		switch {
		case ev.Stream != nil:
			// Mid-turn SSE. Only content_block_delta carries renderable text.
			var d acp.StreamDelta
			if err := json.Unmarshal(ev.Stream.Event, &d); err != nil || d.Delta.Text == "" {
				continue
			}
			c.notifyUpdate(ctx, map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": d.Delta.Text},
			})

		case ev.Assistant != nil:
			// A tool_use is a tool_call update in ACP, NOT a message with a
			// tools array. Emitting it as a message is what produced empty
			// chat bubbles in the first live session.
			for _, item := range ev.Assistant.Message.Content {
				switch item.Type {
				case "thinking":
					c.notifyUpdate(ctx, map[string]any{
						"sessionUpdate": "agent_thought_chunk",
						"content":       map[string]any{"type": "text", "text": item.Text},
					})
				case "tool_use":
					u := map[string]any{
						"sessionUpdate": "tool_call",
						"toolCallId":    item.ID,
						"title":         item.Name,
						"kind":          acpToolKind(item.Name),
						"status":        "pending",
					}
					if len(item.Input) > 0 {
						u["rawInput"] = json.RawMessage(item.Input)
					}
					c.notifyUpdate(ctx, u)
				}
			}

		case ev.Result != nil:
			reason := acpStopEndTurn
			if ev.Result.IsError {
				reason = acpStopRefusal
			}
			c.endTurn(ctx, reason, nil)

		case ev.System != nil:
			// Carries claude's own session id; surface under _meta rather than
			// inventing a nonstandard update kind for it.
			if ev.System.SessionID != "" {
				c.notifyUpdate(ctx, map[string]any{
					"sessionUpdate": "session_info_update",
					"_meta": map[string]any{
						"cogos": map[string]any{"upstreamSessionId": ev.System.SessionID},
					},
				})
			}
		}
	}
	// The subprocess died. A client awaiting a prompt result must not hang.
	c.endTurn(ctx, acpStopCancelled, nil)
}
