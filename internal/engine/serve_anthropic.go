package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type anthropicMessagesRequest struct {
	Model     string                  `json:"model"`
	System    json.RawMessage         `json:"system,omitempty"`
	Messages  []anthropicInputMessage `json:"messages"`
	MaxTokens int                     `json:"max_tokens,omitempty"`
	Stream    bool                    `json:"stream,omitempty"`
	Metadata  anthropicRequestMeta    `json:"metadata,omitempty"`
}

// anthropicRequestMeta holds the optional metadata object from the Anthropic
// Messages API. The user_id field mirrors the OpenAI user field: a client-
// supplied string identifying the end-user, used here as the reqUser fallback
// for identity resolution when no X-Cogos-Session-Id header is present.
type anthropicRequestMeta struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock is defined in provider_anthropic.go (superset of fields).

type anthropicMessagesResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason,omitempty"`
	Usage      anthropicMessagesUsage  `json:"usage,omitempty"`
}

type anthropicMessagesUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

func anthropicToOpenAIRequest(req *anthropicMessagesRequest) *oaiChatRequest {
	if req == nil {
		return &oaiChatRequest{}
	}

	messages := make([]oaiMessage, 0, len(req.Messages)+1)
	if system := normalizeAnthropicContent(req.System); len(system) > 0 {
		messages = append(messages, oaiMessage{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		messages = append(messages, oaiMessage{
			Role:    message.Role,
			Content: normalizeAnthropicContent(message.Content),
		})
	}

	return &oaiChatRequest{
		Model:     req.Model,
		Messages:  messages,
		Stream:    req.Stream,
		MaxTokens: req.MaxTokens,
	}
}

func normalizeAnthropicContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return mustMarshalString("")
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return mustMarshalString(s)
	}

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return raw
	}

	return raw
}

func anthropicStopReason(stopReason string) string {
	switch stopReason {
	case "", "stop", "end_turn":
		return "end_turn"
	case "max_tokens":
		return "max_tokens"
	case "tool_use":
		return "tool_use"
	default:
		return stopReason
	}
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var anthropicReq anthropicMessagesRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	oaiReq := anthropicToOpenAIRequest(&anthropicReq)
	block := NormalizeAnthropicRequest(body, "http")
	block.SessionID = s.process.SessionID()

	// G1: resolve inbound client session → bound identity for attribution.
	// The Anthropic metadata.user_id mirrors OpenAI's user field and serves
	// as the reqUser fallback when no X-Cogos-Session-Id header is present.
	bound := s.resolveBoundIdentity(r, anthropicReq.Metadata.UserID)
	if bound.Bound {
		block.TargetIdentity = bound.Subject
	} else if s.nucleus != nil {
		block.TargetIdentity = s.nucleus.Name
	}

	block.WorkspaceID = filepath.Base(s.cfg.WorkspaceRoot)
	s.process.RecordBlock(block)

	clientMsgs := block.Messages
	query := ""
	for i := len(clientMsgs) - 1; i >= 0; i-- {
		if clientMsgs[i].Role == "user" {
			query = clientMsgs[i].Content
			break
		}
	}

	s.process.Send(NewGateEventFromBlock(block, "user.message", query))

	if s.router == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "not_implemented",
				"message": "no inference router configured; run with a providers.yaml",
			},
		})
		return
	}

	creq := &CompletionRequest{
		MaxTokens:     oaiReq.MaxTokens,
		InteractionID: block.ID,
		Metadata: RequestMetadata{
			RequestID:    uuid.New().String(),
			ProcessState: "active",
			Priority:     PriorityNormal,
			Source:       "http-anthropic",
			// #556: same rationale as handleChat in serve.go — see
			// provider.go's doc comment on RequestMetadata.Attribution.
			Attribution: attributionFor(bound),
		},
	}

	switch oaiReq.Model {
	case "", "local":
	case "claude":
		creq.Metadata.PreferProvider = "claude-code"
	case "codex":
		creq.Metadata.PreferProvider = "codex"
	case "ollama":
		// "ollama" is kept as a convenience spelling that routes to the live
		// local backend (lmstudio-darkstar) after PR #417 decommissioned the
		// Ollama provider. On a stock install no "ollama" provider exists, so
		// the old value routed to nothing; installs that still declare an
		// "ollama" provider select it by its provider name via the default arm.
		creq.Metadata.PreferProvider = "lmstudio-darkstar"
	default:
		creq.ModelOverride = oaiReq.Model
	}

	// Allow per-request budget override via the X-Cogos-Context-Budget header.
	// A value of 0 (absent or unparseable) defers to the kernel's configured
	// default_budget (or the package-level DefaultBudget fallback).
	contextBudget := 0
	if hv := r.Header.Get("X-Cogos-Context-Budget"); hv != "" {
		if v, err := strconv.Atoi(hv); err == nil && v > 0 {
			contextBudget = v
		}
	}

	// G1: embodiment gating — mirrors handleChat exactly.
	//
	// When IdentityNakedDefault is false (default), behavior is exactly
	// today's: run AssembleContext + nucleus card on every request. The ONLY
	// observable difference from before G1 is that bound sessions carry their
	// own subject in block.TargetIdentity (set above).
	//
	// When IdentityNakedDefault is true:
	//   • nucleus-bound request (bound.Subject == nucleus.Name): full embodiment.
	//   • foreign-bound or unbound: clean transport — skip AssembleContext,
	//     forward client messages verbatim (including role:system), no nucleus card.
	nucleusName := ""
	if s.nucleus != nil {
		nucleusName = s.nucleus.Name
	}
	useFullEmbodiment := !s.cfg.IdentityNakedDefault ||
		(bound.Bound && bound.Subject == nucleusName)

	// G3 Part A: spawn embodiment — mirrors handleChat exactly.
	// flag OFF → WorkDir empty (today's behavior).
	// flag ON + bound    → resolve cog:// WorkspaceRoot to fs path.
	// flag ON + unbound  → neutral temp dir so no CLAUDE.md loads.
	if s.cfg.IdentityNakedDefault {
		if bound.Bound && bound.WorkspaceRoot != "" {
			if fsPath := resolveWorkspaceRootPath(s.cfg.WorkspaceRoot, bound.WorkspaceRoot); fsPath != "" {
				creq.WorkDir = fsPath
			}
		}
		// Note: unbound anon-temp is handled on the chat path (serve.go);
		// the anthropic path follows the same policy but defers temp creation
		// to avoid resource leak on the simpler BrowserOS / Zed flow.
	}

	// G3 Part B: memory scope — mirrors handleChat exactly.
	var assembleScopeOpts []AssembleOption
	if s.cfg.IdentityNakedDefault && bound.Bound && bound.MemoryNamespace != "" {
		assembleScopeOpts = append(assembleScopeOpts, WithMemoryScope(bound.MemoryNamespace))
	}

	// Foveation / light-cone key: stable per-conversation, never the per-request
	// UUID and never Process.SessionID(). Mirrors handleChat. The Anthropic
	// metadata.user_id is the "user" field equivalent. See foveation_session_key.go.
	foveationKey := foveationSessionKey(r.Header.Get(foveationKeyHeader), anthropicReq.Metadata.UserID, clientMsgs)

	if useFullEmbodiment {
		assembleOpts := []AssembleOption{
			WithContext(r.Context()),
			WithConversationID(foveationKey),
			WithManifestMode(true),
		}
		assembleOpts = append(assembleOpts, assembleScopeOpts...)
		if pkg, err := s.process.AssembleContext(query, clientMsgs, contextBudget, assembleOpts...); err != nil {
			slog.Warn("anthropic: context assembly failed", "err", err)
			// Fallback: preserve role=system messages as the provider SystemPrompt
			// so an explicit user/BrowserOS prompt isn't silently dropped. The
			// Anthropic upstream API rejects role=system inside messages, so we
			// MUST extract them on this path.
			var clientSysParts []string
			var nonSysMsgs []ProviderMessage
			for _, m := range clientMsgs {
				if m.Role == "system" {
					if strings.TrimSpace(m.Content) != "" {
						clientSysParts = append(clientSysParts, m.Content)
					}
					continue
				}
				nonSysMsgs = append(nonSysMsgs, m)
			}
			creq.Messages = nonSysMsgs
			creq.SystemPrompt = mergeSystemPrompts(s.nucleusCard(), clientSysParts)
		} else {
			systemPrompt, managedMsgs := pkg.FormatForProvider()
			creq.SystemPrompt = systemPrompt
			creq.Messages = managedMsgs
		}
	} else {
		// Clean transport path (IdentityNakedDefault=true, foreign or unbound).
		// Forward client messages verbatim; no nucleus card; AssembleContext skipped.
		var clientSysParts []string
		var nonSysMsgs []ProviderMessage
		for _, m := range clientMsgs {
			if m.Role == "system" {
				if strings.TrimSpace(m.Content) != "" {
					clientSysParts = append(clientSysParts, m.Content)
				}
				continue
			}
			nonSysMsgs = append(nonSysMsgs, m)
		}
		creq.Messages = nonSysMsgs
		creq.SystemPrompt = mergeSystemPrompts("", clientSysParts)
	}

	provider, _, err := s.router.Route(r.Context(), creq)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "no_provider", "message": err.Error()},
		})
		return
	}

	respID := "msg_" + uuid.NewString()
	model := provider.Name()
	if anthropicReq.Model != "" && anthropicReq.Model != "local" {
		model = anthropicReq.Model
	}

	// Prepare the turn record — fills in during complete/stream, persisted below.
	turn := &TurnRecord{
		TurnID:    uuid.NewString(),
		TurnIndex: NextTurnIndex(s.cfg.WorkspaceRoot, block.SessionID),
		SessionID: block.SessionID,
		Timestamp: time.Now().UTC(),
		Prompt:    query,
		Provider:  provider.Name(),
		Model:     model,
		BlockID:   block.ID,
	}

	// #556: see the matching comment in handleChat (serve.go) — carries this
	// request's own queue admission stats out of the queuedProvider call
	// stack for the X-Cogos-Queue-Depth / X-Cogos-Queue-Wait-Ms headers.
	queueObs := &queueObservation{}
	inferCtx := withQueueObservation(r.Context(), queueObs)

	turnStart := time.Now()
	if anthropicReq.Stream {
		s.streamAnthropicMessages(w, inferCtx, creq, provider, respID, model, turn)
	} else {
		s.completeAnthropicMessages(w, inferCtx, creq, provider, respID, model, turn)
	}

	turn.DurationMs = time.Since(turnStart).Milliseconds()
	if err := s.process.RecordTurn(turn); err != nil {
		slog.Warn("anthropic: RecordTurn failed", "err", err, "session", turn.SessionID)
	}
}

func (s *Server) completeAnthropicMessages(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string, turn *TurnRecord) {

	// Cancel-safe (#432): same rationale as completeChat in serve.go — the
	// Anthropic-compat non-streaming surface hits the same provider pool and
	// is subject to the same zombie-generation risk on abandoned requests.
	resp, err := CompleteCancelSafeIfSupported(ctx, provider, req)
	if err != nil {
		recordAbandonedInference("anthropic-complete", req.Metadata.RequestID, err)
		slog.Warn("anthropic: complete error", "err", err)
		if turn != nil {
			turn.Status = "error"
			turn.Error = err.Error()
		}
		// #556 repair (round 2): matches completeChat's round-1 fix in
		// serve.go — a request that waited in the FIFO and then failed
		// upstream must still report the queue headers, otherwise the
		// caller has no way to see the failure was preceded by queueing.
		// Must be written before WriteHeader, same as the success path
		// below. writeQueueHeaders is nil-safe/self-suppressing when
		// nothing was recorded, so this is a no-op for non-queued providers.
		writeQueueHeaders(w, queueObservationFromContext(ctx))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "inference_error", "message": err.Error()},
		})
		return
	}

	// #556: this request's own queue admission stats, observed at the
	// moment it was dispatched — see the matching comment in completeChat.
	writeQueueHeaders(w, queueObservationFromContext(ctx))

	if turn != nil {
		turn.Response = resp.Content
		turn.Usage = TurnUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
		if len(resp.ToolCalls) > 0 {
			for _, tc := range resp.ToolCalls {
				turn.ToolCalls = append(turn.ToolCalls, ToolCallRecord{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: tc.Arguments,
				})
			}
		}
	}

	response := anthropicMessagesResponse{
		ID:         respID,
		Type:       "message",
		Role:       "assistant",
		Content:    []anthropicContentBlock{{Type: "text", Text: resp.Content}},
		Model:      model,
		StopReason: anthropicStopReason(resp.StopReason),
		Usage: anthropicMessagesUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) streamAnthropicMessages(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string, turn *TurnRecord) {

	chunks, err := provider.Stream(ctx, req)
	if err != nil {
		slog.Warn("anthropic: stream error", "err", err)
		if turn != nil {
			turn.Status = "error"
			turn.Error = err.Error()
		}
		// #556 repair (round 3): matches streamChat's round-3 fix in
		// serve.go and completeAnthropicMessages' round-2 fix above — a
		// request admitted after a real FIFO wait, whose provider.Stream
		// call then failed on connect, must still report the queue
		// headers so the caller can see the failure was preceded by
		// queueing. Must be written before http.Error's WriteHeader.
		// writeQueueHeaders is nil-safe/self-suppressing when nothing was
		// recorded, so this is a no-op for non-queued providers.
		writeQueueHeaders(w, queueObservationFromContext(ctx))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// #556: provider.Stream already acquired its queue slot synchronously
	// before returning chunks above — set headers now, before any SSE byte
	// is written. See the matching comment in streamChat.
	writeQueueHeaders(w, queueObservationFromContext(ctx))

	flusher, canFlush := w.(http.Flusher)
	bw := bufio.NewWriter(w)
	flush := func() {
		_ = bw.Flush()
		if canFlush {
			flusher.Flush()
		}
	}
	writeEvent := func(event string, data any) {
		payload, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(bw, "event: %s\n", event)
		_, _ = fmt.Fprintf(bw, "data: %s\n\n", payload)
		flush()
	}

	writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      respID,
			"type":    "message",
			"role":    "assistant",
			"content": []anthropicContentBlock{},
			"model":   model,
		},
	})
	writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})

	var respBuf strings.Builder
	usage := anthropicMessagesUsage{}
	for sc := range chunks {
		if sc.Error != nil {
			slog.Warn("anthropic: stream chunk error", "err", sc.Error)
			if turn != nil {
				turn.Status = "error"
				turn.Error = sc.Error.Error()
				turn.Response = respBuf.String()
			}
			break
		}
		if sc.Usage != nil {
			usage.InputTokens = sc.Usage.InputTokens
			usage.OutputTokens = sc.Usage.OutputTokens
		}
		if sc.Delta != "" {
			if turn != nil {
				respBuf.WriteString(sc.Delta)
			}
			writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": sc.Delta,
				},
			})
		}
		if sc.Done {
			if turn != nil {
				turn.Response = respBuf.String()
				turn.Usage = TurnUsage{
					InputTokens:  usage.InputTokens,
					OutputTokens: usage.OutputTokens,
					TotalTokens:  usage.InputTokens + usage.OutputTokens,
				}
			}
			writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			writeEvent("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason": anthropicStopReason(sc.StopReason),
				},
				"usage": usage,
			})
			writeEvent("message_stop", map[string]any{"type": "message_stop"})
			return
		}
	}
	// In case the loop exited without sc.Done (error path), preserve what we captured.
	if turn != nil && turn.Response == "" {
		turn.Response = respBuf.String()
	}
}
