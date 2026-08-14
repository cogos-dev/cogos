// mcp_tools.go — MCP tool surface for the Conversations Observatory.
//
// Registers three MCP tools on a provided *mcp.Server:
//
//	cog_search_conversations  — full-text search over indexed turns
//	cog_get_conversation_turn — fetch one turn by session_id + turn_index
//	cog_list_conversations    — list indexed sessions with metadata
//
// Both cog_search_conversations and cog_get_conversation_turn accept an
// optional `uri` parameter (cog:conversations/… URI, RFC-query-aware-conversation-uris).
// When `uri` is provided it fully determines the query; mixing `uri` with other
// filter params is an error.
//
// Registration pattern mirrors internal/eval/mcp_tools.go:
// the caller (kernel boot or conversations_wiring.go) calls
// RegisterConversationTools(server, tracker, provider) after wiring the Provider.
// tracker is MCPServer.TrackTool — passed as a function value so this package
// does not import internal/engine (circular import).
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolTracker is a function that records a tool in the manifest registry and
// returns the pointer unchanged. Pass MCPServer.TrackTool as this argument.
// When nil (e.g. tests), tools are registered without manifest tracking.
type ToolTracker func(*mcp.Tool) *mcp.Tool

// identityTracker is a no-op tracker for callers that don't need manifest tracking.
func identityTracker(t *mcp.Tool) *mcp.Tool { return t }

// RegisterConversationTools registers the three conversation MCP tools on the
// given server. provider may be nil — tools return "not configured" in that
// case. tracker is MCPServer.TrackTool; pass nil to skip manifest tracking
// (tests). maxBytes caps the byte length of any text response; pass 0 to use
// the default (32 KiB) — set at registration time because the handlers are
// closures that capture it.
func RegisterConversationTools(server *mcp.Server, tracker ToolTracker, provider *Provider, maxBytes int) {
	if tracker == nil {
		tracker = identityTracker
	}
	if maxBytes <= 0 {
		maxBytes = defaultConvMaxBytes
	}
	if maxBytes < minConvMaxBytes {
		maxBytes = minConvMaxBytes
	}
	mcp.AddTool(server, tracker(&mcp.Tool{
		Name: "cog_search_conversations",
		Description: "Full-text search over indexed operator conversation history. " +
			"Returns hits with session_id, timestamp, role, and an excerpt centred on the match. " +
			"Use since/until (RFC3339) to narrow by time; use identity to filter by operator; " +
			"use limit to cap results (default 20). " +
			"Alternatively, pass uri=cog:conversations/… to dereference a query-aware URI directly " +
			"(RFC-query-aware-conversation-uris R1-R6); mixing uri with other params is an error.",
	}), makeSearchConversationsHandler(provider, maxBytes))

	mcp.AddTool(server, tracker(&mcp.Tool{
		Name: "cog_get_conversation_turn",
		Description: "Fetch the full text of one conversation turn by session_id and turn_index. " +
			"Use after cog_search_conversations to drill into a specific hit. " +
			"Alternatively, pass uri=cog:conversations/<source>/<session>#id-<uuid> or " +
			"#turn-N to address a turn via URI (RFC-query-aware-conversation-uris).",
	}), makeGetConversationTurnHandler(provider, maxBytes))

	mcp.AddTool(server, tracker(&mcp.Tool{
		Name: "cog_list_conversations",
		Description: "List indexed conversation sessions with metadata " +
			"(title, turn count, time bounds, identity, entrypoint). " +
			"Optional since/until (RFC3339) and identity filters. " +
			"Returns most-recent sessions first.",
	}), makeListConversationsHandler(provider, maxBytes))
}

const (
	defaultConvMaxBytes = 32 * 1024 // 32 KiB — mirrors engine.DefaultMaxToolOutputBytes
	minConvMaxBytes     = 4 * 1024  // 4 KiB floor
)

// capConvOutput trims s to at most maxBytes at a UTF-8 boundary and appends
// the truncation marker. Mirrors engine.capToolOutput without creating a
// cross-package dependency.
func capConvOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(
		"\n[output truncated: returned %d of %d bytes; narrow with offset/limit/filters, or dereference cog:conversations/... where applicable]",
		cut, len(s),
	)
}

// ─── cog_search_conversations ────────────────────────────────────────────────

type searchConversationsInput struct {
	// URI mode: pass a cog:conversations/… URI to dereference directly.
	// Mixing uri with any other field is an error.
	URI string `json:"uri,omitempty"`

	// Standard filter params (mutually exclusive with URI).
	// Query is omitempty so the SDK-generated input schema does not mark it
	// required — a uri-only call must pass schema validation. The handler
	// enforces "query required unless uri is set" at runtime.
	Query     string `json:"query,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Identity  string `json:"identity,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func makeSearchConversationsHandler(p *Provider, maxBytes int) mcp.ToolHandlerFor[searchConversationsInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchConversationsInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return convErrorResult("conversations provider not wired"), nil, nil
		}

		p.mu.Lock()
		idx := p.index
		ont := p.ontology
		p.mu.Unlock()
		if idx == nil {
			return convErrorResult("index not yet initialised — run cog reconcile conversations first"), nil, nil
		}

		// URI mode: dereference the URI directly.
		if input.URI != "" {
			// Detect mixing.
			hasOtherParams := input.Query != "" || input.Since != "" || input.Until != "" ||
				input.SessionID != "" || input.Identity != "" || input.Limit != 0
			if hasOtherParams {
				return convErrorResult(ErrURIMixedParams.Error()), nil, nil
			}

			slice, err := ResolveConversationURIWithOntology(input.URI, idx, ont)
			if err != nil {
				return convErrorResult(fmt.Sprintf("resolve uri %q: %v", input.URI, err)), nil, nil
			}

			resp := sliceToMap(slice)
			b, _ := json.MarshalIndent(resp, "", "  ")
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
			}, resp, nil
		}

		// Standard mode.
		if input.Query == "" {
			return convErrorResult("query is required (or provide uri=)"), nil, nil
		}

		since, until, err := parseTimeRange(input.Since, input.Until)
		if err != nil {
			return convErrorResult(fmt.Sprintf("parse time range: %v", err)), nil, nil
		}

		limit := input.Limit
		if limit <= 0 {
			limit = 20
		}

		hits := idx.Search(input.Query, since, until, input.SessionID, input.Identity, limit)

		type hitOut struct {
			SessionID    string `json:"session_id"`
			TurnIndex    int    `json:"turn_index"`
			UUID         string `json:"uuid,omitempty"`
			IDAnchor     string `json:"id_anchor,omitempty"`
			Timestamp    string `json:"timestamp,omitempty"`
			Role         string `json:"role"`
			Excerpt      string `json:"excerpt"`
			SessionTitle string `json:"session_title,omitempty"`
			Identity     string `json:"identity,omitempty"`
			Source       string `json:"source,omitempty"`
		}
		out := make([]hitOut, 0, len(hits))
		for _, h := range hits {
			ts := ""
			if !h.Timestamp.IsZero() {
				ts = h.Timestamp.Format(time.RFC3339)
			}
			idAnchor := ""
			if h.UUID != "" {
				idAnchor = "#id-" + h.UUID
			}
			out = append(out, hitOut{
				SessionID:    h.SessionID,
				TurnIndex:    h.TurnIndex,
				UUID:         h.UUID,
				IDAnchor:     idAnchor,
				Timestamp:    ts,
				Role:         string(h.Role),
				Excerpt:      h.Excerpt,
				SessionTitle: h.SessionTitle,
				Identity:     h.Identity,
				Source:       h.Source,
			})
		}

		resp := map[string]any{
			"query": input.Query,
			"count": len(out),
			"hits":  out,
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		text := capConvOutput(string(b), maxBytes)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, resp, nil
	}
}

// ─── cog_get_conversation_turn ───────────────────────────────────────────────

type getConversationTurnInput struct {
	// URI mode: cog:conversations/<source>/<session>#id-<uuid> or #turn-N.
	// Mixing uri with SessionID/TurnIndex is an error.
	URI string `json:"uri,omitempty"`

	// session_id/turn_index are omitempty so the SDK-generated input schema
	// does not mark them required — a uri-only call must pass schema
	// validation. The handler enforces "session_id required unless uri is
	// set" at runtime.
	SessionID string `json:"session_id,omitempty"`
	TurnIndex int    `json:"turn_index,omitempty"`
}

func makeGetConversationTurnHandler(p *Provider, maxBytes int) mcp.ToolHandlerFor[getConversationTurnInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getConversationTurnInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return convErrorResult("conversations provider not wired"), nil, nil
		}

		p.mu.Lock()
		idx := p.index
		ont := p.ontology
		p.mu.Unlock()
		if idx == nil {
			return convErrorResult("index not yet initialised — run cog reconcile conversations first"), nil, nil
		}

		// URI mode.
		if input.URI != "" {
			hasOtherParams := input.SessionID != "" || input.TurnIndex != 0
			if hasOtherParams {
				return convErrorResult(ErrURIMixedParams.Error()), nil, nil
			}

			slice, err := ResolveConversationURIWithOntology(input.URI, idx, ont)
			if err != nil {
				return convErrorResult(fmt.Sprintf("resolve uri %q: %v", input.URI, err)), nil, nil
			}

			resp := sliceToMap(slice)
			b, _ := json.MarshalIndent(resp, "", "  ")
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
			}, resp, nil
		}

		// Standard mode.
		if input.SessionID == "" {
			return convErrorResult("session_id is required (or provide uri=)"), nil, nil
		}

		turn, ok := idx.GetTurn(input.SessionID, input.TurnIndex)
		if !ok {
			return convErrorResult(fmt.Sprintf("turn %d not found in session %s", input.TurnIndex, input.SessionID)), nil, nil
		}

		ts := ""
		if !turn.Timestamp.IsZero() {
			ts = turn.Timestamp.Format(time.RFC3339)
		}
		resp := map[string]any{
			"session_id":   turn.SessionID,
			"turn_index":   turn.TurnIndex,
			"uuid":         turn.UUID,
			"id_anchor":    "#id-" + turn.UUID,
			"parent_uuid":  turn.ParentUUID,
			"role":         string(turn.Role),
			"timestamp":    ts,
			"text":         turn.Text,
			"is_tool_call": turn.IsToolCall,
		}
		if turn.ThreadID != "" {
			resp["thread_id"] = turn.ThreadID
			if meta, ok := idx.sessions[turn.SessionID]; ok {
				for _, tm := range meta.Threads {
					if tm.ThreadID == turn.ThreadID {
						resp["thread_role"] = string(tm.Role)
						break
					}
				}
			}
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		text := capConvOutput(string(b), maxBytes)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, resp, nil
	}
}

// ─── cog_list_conversations ──────────────────────────────────────────────────

type listConversationsInput struct {
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
	Identity string `json:"identity,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

func makeListConversationsHandler(p *Provider, maxBytes int) mcp.ToolHandlerFor[listConversationsInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input listConversationsInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return convErrorResult("conversations provider not wired"), nil, nil
		}

		p.mu.Lock()
		idx := p.index
		p.mu.Unlock()
		if idx == nil {
			return convErrorResult("index not yet initialised — run cog reconcile conversations first"), nil, nil
		}

		since, until, err := parseTimeRange(input.Since, input.Until)
		if err != nil {
			return convErrorResult(fmt.Sprintf("parse time range: %v", err)), nil, nil
		}

		metas := idx.ListSessions(since, until, input.Identity)
		limit := input.Limit
		if limit > 0 && len(metas) > limit {
			metas = metas[:limit]
		}

		type threadOut struct {
			ThreadID     string `json:"thread_id"`
			Role         string `json:"role"`
			MessageCount int    `json:"message_count"`
		}
		type sessionOut struct {
			SessionID   string      `json:"session_id"`
			Source      string      `json:"source,omitempty"`
			Title       string      `json:"title,omitempty"`
			TurnCount   int         `json:"turn_count"`
			FirstTurnAt string      `json:"first_turn_at,omitempty"`
			LastTurnAt  string      `json:"last_turn_at,omitempty"`
			IndexedAt   string      `json:"indexed_at,omitempty"`
			Identity    string      `json:"identity,omitempty"`
			Entrypoint  string      `json:"entrypoint,omitempty"`
			Threads     []threadOut `json:"threads,omitempty"`
		}
		out := make([]sessionOut, 0, len(metas))
		for _, m := range metas {
			so := sessionOut{
				SessionID:  m.SessionID,
				Source:     m.Source,
				Title:      m.Title,
				TurnCount:  m.TurnCount,
				Identity:   m.Identity,
				Entrypoint: m.Entrypoint,
			}
			for _, tm := range m.Threads {
				so.Threads = append(so.Threads, threadOut{
					ThreadID:     tm.ThreadID,
					Role:         string(tm.Role),
					MessageCount: tm.MessageCount,
				})
			}
			if !m.FirstTurnAt.IsZero() {
				so.FirstTurnAt = m.FirstTurnAt.Format(time.RFC3339)
			}
			if !m.LastTurnAt.IsZero() {
				so.LastTurnAt = m.LastTurnAt.Format(time.RFC3339)
			}
			if !m.IndexedAt.IsZero() {
				so.IndexedAt = m.IndexedAt.Format(time.RFC3339)
			}
			out = append(out, so)
		}

		resp := map[string]any{
			"sessions": out,
			"count":    len(out),
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		text := capConvOutput(string(b), maxBytes)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, resp, nil
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// parseTimeRange parses optional since/until RFC3339 strings.
func parseTimeRange(since, until string) (time.Time, time.Time, error) {
	var s, u time.Time
	if since != "" {
		var err error
		s, err = time.Parse(time.RFC3339, since)
		if err != nil {
			return s, u, fmt.Errorf("invalid since %q: %w", since, err)
		}
	}
	if until != "" {
		var err error
		u, err = time.Parse(time.RFC3339, until)
		if err != nil {
			return s, u, fmt.Errorf("invalid until %q: %w", until, err)
		}
	}
	return s, u, nil
}

// convErrorResult builds a CallToolResult carrying an error message.
func convErrorResult(msg string) *mcp.CallToolResult {
	resp := map[string]any{"error": msg, "ok": false}
	b, _ := json.Marshal(resp)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		IsError: true,
	}
}

// sliceToMap converts a ResolvedSlice to a map[string]any for JSON output.
func sliceToMap(slice *ResolvedSlice) map[string]any {
	m := map[string]any{
		"uri":         slice.URI,
		"resolved_at": slice.ResolvedAt.Format(time.RFC3339),
		"count":       slice.Count,
		"sources":     slice.Sources,
		"bounded":     slice.Bounded,
		"turns":       slice.Turns,
	}
	if slice.ContentHash != "" {
		m["content_hash"] = slice.ContentHash
	}
	return m
}
