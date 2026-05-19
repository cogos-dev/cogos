// mcp_tools.go — MCP tool surface for the Conversations Observatory.
//
// Registers three MCP tools on a provided *mcp.Server:
//
//   cog_search_conversations  — full-text search over indexed turns
//   cog_get_conversation_turn — fetch one turn by session_id + turn_index
//   cog_list_conversations    — list indexed sessions with metadata
//
// Registration pattern mirrors internal/eval/mcp_tools.go:
// the caller (kernel boot or conversations_wiring.go) calls
// RegisterConversationTools(server, provider) after wiring the Provider.
package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterConversationTools registers the three conversation MCP tools on the
// given server. provider may be nil — tools return "not configured" in that case.
func RegisterConversationTools(server *mcp.Server, provider *Provider) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "cog_search_conversations",
		Description: "Full-text search over indexed operator conversation history. " +
			"Returns hits with session_id, timestamp, role, and an excerpt centred on the match. " +
			"Use since/until (RFC3339) to narrow by time; use identity to filter by operator; " +
			"use limit to cap results (default 20).",
	}, makeSearchConversationsHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name: "cog_get_conversation_turn",
		Description: "Fetch the full text of one conversation turn by session_id and turn_index. " +
			"Use after cog_search_conversations to drill into a specific hit.",
	}, makeGetConversationTurnHandler(provider))

	mcp.AddTool(server, &mcp.Tool{
		Name: "cog_list_conversations",
		Description: "List indexed conversation sessions with metadata " +
			"(title, turn count, time bounds, identity, entrypoint). " +
			"Optional since/until (RFC3339) and identity filters. " +
			"Returns most-recent sessions first.",
	}, makeListConversationsHandler(provider))
}

// ─── cog_search_conversations ────────────────────────────────────────────────

type searchConversationsInput struct {
	Query     string `json:"query"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Identity  string `json:"identity,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func makeSearchConversationsHandler(p *Provider) mcp.ToolHandlerFor[searchConversationsInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchConversationsInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return convErrorResult("conversations provider not wired"), nil, nil
		}
		if input.Query == "" {
			return convErrorResult("query is required"), nil, nil
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

		limit := input.Limit
		if limit <= 0 {
			limit = 20
		}

		hits := idx.Search(input.Query, since, until, input.SessionID, input.Identity, limit)

		type hitOut struct {
			SessionID    string `json:"session_id"`
			TurnIndex    int    `json:"turn_index"`
			UUID         string `json:"uuid,omitempty"`
			Timestamp    string `json:"timestamp,omitempty"`
			Role         string `json:"role"`
			Excerpt      string `json:"excerpt"`
			SessionTitle string `json:"session_title,omitempty"`
			Identity     string `json:"identity,omitempty"`
		}
		out := make([]hitOut, 0, len(hits))
		for _, h := range hits {
			ts := ""
			if !h.Timestamp.IsZero() {
				ts = h.Timestamp.Format(time.RFC3339)
			}
			out = append(out, hitOut{
				SessionID:    h.SessionID,
				TurnIndex:    h.TurnIndex,
				UUID:         h.UUID,
				Timestamp:    ts,
				Role:         string(h.Role),
				Excerpt:      h.Excerpt,
				SessionTitle: h.SessionTitle,
				Identity:     h.Identity,
			})
		}

		resp := map[string]any{
			"query":  input.Query,
			"count":  len(out),
			"hits":   out,
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}, resp, nil
	}
}

// ─── cog_get_conversation_turn ───────────────────────────────────────────────

type getConversationTurnInput struct {
	SessionID  string `json:"session_id"`
	TurnIndex  int    `json:"turn_index"`
}

func makeGetConversationTurnHandler(p *Provider) mcp.ToolHandlerFor[getConversationTurnInput, map[string]any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getConversationTurnInput) (*mcp.CallToolResult, map[string]any, error) {
		if p == nil {
			return convErrorResult("conversations provider not wired"), nil, nil
		}
		if input.SessionID == "" {
			return convErrorResult("session_id is required"), nil, nil
		}

		p.mu.Lock()
		idx := p.index
		p.mu.Unlock()
		if idx == nil {
			return convErrorResult("index not yet initialised — run cog reconcile conversations first"), nil, nil
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
			"session_id":  turn.SessionID,
			"turn_index":  turn.TurnIndex,
			"uuid":        turn.UUID,
			"parent_uuid": turn.ParentUUID,
			"role":        string(turn.Role),
			"timestamp":   ts,
			"text":        turn.Text,
			"is_tool_call": turn.IsToolCall,
		}
		b, _ := json.MarshalIndent(resp, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
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

func makeListConversationsHandler(p *Provider) mcp.ToolHandlerFor[listConversationsInput, map[string]any] {
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

		type sessionOut struct {
			SessionID   string `json:"session_id"`
			Title       string `json:"title,omitempty"`
			TurnCount   int    `json:"turn_count"`
			FirstTurnAt string `json:"first_turn_at,omitempty"`
			LastTurnAt  string `json:"last_turn_at,omitempty"`
			IndexedAt   string `json:"indexed_at,omitempty"`
			Identity    string `json:"identity,omitempty"`
			Entrypoint  string `json:"entrypoint,omitempty"`
		}
		out := make([]sessionOut, 0, len(metas))
		for _, m := range metas {
			so := sessionOut{
				SessionID:  m.SessionID,
				Title:      m.Title,
				TurnCount:  m.TurnCount,
				Identity:   m.Identity,
				Entrypoint: m.Entrypoint,
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
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
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
