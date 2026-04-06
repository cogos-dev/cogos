package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/google/uuid"
)

type anthropicMessagesRequest struct {
	Model     string                  `json:"model"`
	System    json.RawMessage         `json:"system,omitempty"`
	Messages  []anthropicInputMessage `json:"messages"`
	MaxTokens int                     `json:"max_tokens,omitempty"`
	Stream    bool                    `json:"stream,omitempty"`
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
	if s.nucleus != nil {
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
		},
	}

	switch oaiReq.Model {
	case "", "local":
	case "claude":
		creq.Metadata.PreferProvider = "claude-code"
	case "codex":
		creq.Metadata.PreferProvider = "codex"
	case "ollama":
		creq.Metadata.PreferProvider = "ollama"
	default:
		creq.ModelOverride = oaiReq.Model
	}

	if pkg, err := s.process.AssembleContext(query, clientMsgs, 0,
		WithContext(r.Context()),
		WithConversationID(creq.Metadata.RequestID),
		WithManifestMode(true),
	); err != nil {
		slog.Warn("anthropic: context assembly failed", "err", err)
		creq.Messages = clientMsgs
	} else {
		systemPrompt, managedMsgs := pkg.FormatForProvider()
		creq.SystemPrompt = systemPrompt
		creq.Messages = managedMsgs
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

	if anthropicReq.Stream {
		s.streamAnthropicMessages(w, r.Context(), creq, provider, respID, model)
		return
	}
	s.completeAnthropicMessages(w, r.Context(), creq, provider, respID, model)
}

func (s *Server) completeAnthropicMessages(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string) {

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		slog.Warn("anthropic: complete error", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "inference_error", "message": err.Error()},
		})
		return
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
	provider Provider, respID, model string) {

	chunks, err := provider.Stream(ctx, req)
	if err != nil {
		slog.Warn("anthropic: stream error", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

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

	usage := anthropicMessagesUsage{}
	for sc := range chunks {
		if sc.Error != nil {
			slog.Warn("anthropic: stream chunk error", "err", sc.Error)
			break
		}
		if sc.Usage != nil {
			usage.InputTokens = sc.Usage.InputTokens
			usage.OutputTokens = sc.Usage.OutputTokens
		}
		if sc.Delta != "" {
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
}
