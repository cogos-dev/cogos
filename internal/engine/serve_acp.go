// serve_acp.go — ACP JSON-RPC over WebSocket at /v1/acp.
//
// This is the transport leg named as missing in internal/acp/doc.go: "later
// (once ADR-093 lands) wrapped behind acp.ManagedSession and optionally
// surfaced as ACP JSON-RPC over a separate transport." The spike already had
// the subprocess driver (Spawn/Send/Events); it had no way for a UI to reach
// it. This is that way.
//
// WHY WEBSOCKET AND NOT SSE/POLLING: an agent session is bidirectional and
// long-lived. The client must be able to send a follow-up turn into a session
// that is mid-stream, and receive token deltas as they land. Polling cannot
// express that, and SSE only gives the downstream half.
//
// WHY THIS IS CHEAP ON EDGE DEVICES: the wire is newline-free JSON frames and
// the client is one file with no build step and no framework. A phone renders
// text deltas; it does not run an agent. All model work stays on the node
// holding the subprocess.
//
// SECURITY: same posture as the rest of the kernel's HTTP surface -- bound to
// localhost by default and reached over the LAN only through the operator's
// own forwarder. Origin is checked against the serving host to refuse
// drive-by WebSocket connects from arbitrary pages.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/myrgic/cogos/internal/acp"
)

// acpRequest is a JSON-RPC 2.0 request frame.
type acpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// acpResponse is a JSON-RPC 2.0 response or notification frame.
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
	ws   *websocket.Conn
	mu   sync.Mutex // serializes writes; a WS conn has one writer
	sub  *acp.Subprocess
	stop context.CancelFunc
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

func (c *acpConn) notify(ctx context.Context, method string, params any) {
	_ = c.write(ctx, acpResponse{JSONRPC: "2.0", Method: method, Params: params})
}

// handleACP serves GET /v1/acp as a WebSocket speaking ACP JSON-RPC.
func (s *Server) handleACP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Refuse drive-by connects from arbitrary origins; a page served by
		// this kernel (or a same-host origin) is what we accept.
		OriginPatterns: []string{"localhost:*", "127.0.0.1:*", "[::1]:*", r.Host},
	})
	if err != nil {
		return // Accept already wrote the error
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
			return // client gone
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
		// Advertise only what is actually implemented. Claiming a capability
		// the kernel does not have is worse than omitting it: the client
		// builds UI for a promise that never arrives.
		c.reply(ctx, req.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"promptCapabilities": map[string]any{"image": false, "audio": false},
			},
			"authMethods": []any{},
			"_cogos": map[string]any{
				"node":      s.cfg.WorkspaceRoot,
				"transport": "websocket",
			},
		})

	case "session/new":
		if c.sub != nil {
			c.fail(ctx, req.ID, -32600, "session already active on this connection")
			return
		}
		var p struct {
			Cwd    string `json:"cwd"`
			Resume string `json:"resume"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Cwd == "" {
			p.Cwd = s.WorkspaceRoot()
		}
		sctx, stop := context.WithCancel(context.Background())
		sub, err := acp.Spawn(sctx, acp.SpawnOpts{
			Cwd:            p.Cwd,
			SessionID:      p.Resume,
			ResumeExisting: p.Resume != "",
		})
		if err != nil {
			stop()
			c.fail(ctx, req.ID, -32000, "spawn failed: "+err.Error())
			return
		}
		c.sub, c.stop = sub, stop
		go c.pump(ctx, sub)
		c.reply(ctx, req.ID, map[string]any{"sessionId": fmt.Sprintf("%p", sub)})

	case "session/prompt":
		if c.sub == nil {
			c.fail(ctx, req.ID, -32600, "no session; call session/new first")
			return
		}
		var p struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			c.fail(ctx, req.ID, -32602, "bad params")
			return
		}
		text := ""
		for _, b := range p.Prompt {
			if b.Type == "text" || b.Type == "" {
				text += b.Text
			}
		}
		if err := c.sub.Send(acp.NewTextPrompt(text)); err != nil {
			c.fail(ctx, req.ID, -32000, "send failed: "+err.Error())
			return
		}
		// Turn completion is reported by the pump as a session/update
		// notification; the ack only means the prompt was accepted.
		c.reply(ctx, req.ID, map[string]any{"accepted": true})

	case "session/cancel":
		if c.stop != nil {
			c.stop()
			c.stop, c.sub = nil, nil
		}
		c.reply(ctx, req.ID, map[string]any{"cancelled": true})

	default:
		c.fail(ctx, req.ID, -32601, "method not found: "+req.Method)
	}
}

// pump translates driver events into ACP session/update notifications.
//
// The kinds are named for the CLIENT's benefit, not the wire's: a UI wants to
// know "is this a token delta, a finished message, or the end of a turn",
// which is a different partition than stream-json's frame types.
func (c *acpConn) pump(ctx context.Context, sub *acp.Subprocess) {
	for ev := range sub.Events() {
		update := map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}
		switch {
		case ev.Assistant != nil:
			var text string
			var tools []string
			for _, item := range ev.Assistant.Message.Content {
				switch item.Type {
				case "text":
					text += item.Text
				case "tool_use":
					tools = append(tools, item.Name)
				}
			}
			update["kind"] = "message"
			update["text"] = text
			if len(tools) > 0 {
				update["tools"] = tools
			}

		case ev.Stream != nil:
			// Mid-turn SSE. Only content_block_delta carries renderable text;
			// everything else is framing the client does not need.
			var d acp.StreamDelta
			if err := json.Unmarshal(ev.Stream.Event, &d); err != nil ||
				d.Delta.Text == "" {
				continue
			}
			update["kind"] = "delta"
			update["text"] = d.Delta.Text

		case ev.Result != nil:
			update["kind"] = "turn_end"
			update["text"] = ev.Result.Result
			update["duration_ms"] = ev.Result.DurationMs
			update["turns"] = ev.Result.NumTurns
			update["is_error"] = ev.Result.IsError

		case ev.System != nil:
			update["kind"] = "system"
			update["subtype"] = ev.System.Subtype
			if ev.System.SessionID != "" {
				update["sessionId"] = ev.System.SessionID
			}

		default:
			continue // unmodelled frame: skip rather than emit noise
		}
		c.notify(ctx, "session/update", update)
	}
	c.notify(ctx, "session/update", map[string]any{"kind": "ended"})
}
