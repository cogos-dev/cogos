// serve.go — CogOS v3 HTTP API
//
// Core endpoints:
//
//	GET  /health                           — liveness + readiness probe
//	GET  /v1/context                       — current attentional field (debug)
//	GET  /v1/resolve                       — resolve a cog:// URI to a filesystem path
//	POST /v1/chat/completions              — OpenAI-compatible chat (streaming + non-streaming)
//	POST /v1/messages                      — Anthropic Messages-compatible chat
//	POST /v1/context/foveated              — foveated context assembly for Claude Code hook
//	GET  /v1/proprioceptive                — last 50 proprioceptive log entries + light cone status
//	GET  /v1/lightcone                     — light cone metadata (placeholder)
//
// Constellation / attention endpoints (Phase 3, see serve_attention.go):
//
//	POST /v1/attention                     — emit attention signal
//	GET  /v1/constellation/fovea           — current fovea state
//	GET  /v1/constellation/adjacent?uri=… — adjacent nodes by attentional proximity
//
// The chat endpoint routes through the inference Router when one is set,
// otherwise returns 501.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Server wraps the HTTP server and its dependencies.
type Server struct {
	cfg          *Config
	nucleus      *Nucleus
	process      *Process
	router       Router // nil until SetRouter is called
	srv          *http.Server
	debug        debugStore    // captures last request pipeline state
	attentionLog *attentionLog // per-server log (avoids global write race)
}

// NewServer constructs a Server bound to the configured port.
func NewServer(cfg *Config, nucleus *Nucleus, process *Process) *Server {
	s := &Server{cfg: cfg, nucleus: nucleus, process: process}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard)
	mux.HandleFunc("GET /canvas", handleCanvas)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/context", s.handleContext)
	mux.HandleFunc("GET /v1/resolve", s.handleResolve)
	mux.HandleFunc("GET /v1/cogdoc/read", s.handleCogDocRead)
	mux.HandleFunc("GET /v1/debug/last", s.handleDebugLast)
	mux.HandleFunc("GET /v1/debug/context", s.handleDebugContext)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("GET /v1/proprioceptive", s.handleProprioceptive)
	mux.HandleFunc("GET /v1/lightcone", s.handleLightCone)
	mux.HandleFunc("POST /v1/context/foveated", s.handleFoveatedContext)

	// Constellation / attention endpoints (Phase 3)
	s.registerAttentionRoutes(mux)

	// Block sync endpoints (Phase 3 block sync protocol)
	s.registerBlockRoutes(mux)
	s.registerCompatRoutes(mux)

	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second, // 5 min — streaming responses can be long
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// SetRouter wires an inference Router into the server.
func (s *Server) SetRouter(r Router) {
	s.router = r
}

// Start begins serving. It blocks until the server stops.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.srv.Addr, err)
	}
	slog.Info("server: listening", "addr", s.srv.Addr)
	return s.srv.Serve(ln)
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Handler returns the HTTP handler, useful for httptest.NewServer in tests.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// handleHealth is the liveness/readiness probe.
//
//	200 → healthy
//	503 → nucleus not loaded or process not running
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK

	if s.nucleus == nil {
		status = "nucleus_missing"
		code = http.StatusServiceUnavailable
	}

	identity := ""
	if s.nucleus != nil {
		identity = s.nucleus.Name
	}
	trust := s.process.TrustSnapshot()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   status,
		"version":  "v3-skeleton",
		"state":    s.process.State().String(),
		"identity": identity,
		"node_id":  s.process.NodeID,
		"trust": map[string]interface{}{
			"score":       trust.LocalScore,
			"scope":       "local",
			"fingerprint": s.process.Fingerprint(),
		},
		"workspace": s.cfg.WorkspaceRoot,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleContext returns the current attentional field (top-20 fovea).
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	fovea := s.process.Field().Fovea(20)

	type entry struct {
		Path  string  `json:"path"`
		Score float64 `json:"score"`
	}
	entries := make([]entry, len(fovea))
	for i, fs := range fovea {
		entries[i] = entry{Path: fs.Path, Score: fs.Score}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"nucleus":      s.nucleus.Name,
		"state":        s.process.State().String(),
		"field_size":   s.process.Field().Len(),
		"last_updated": s.process.Field().LastUpdated().Format(time.RFC3339),
		"fovea":        entries,
	})
}

// handleResolve resolves a cog:// URI to a filesystem path.
//
// GET /v1/resolve?uri=cog://mem/semantic/foo.cog.md
//
//	200 → { uri, path, fragment, exists }
//	400 → { error }
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "uri parameter required"})
		return
	}

	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_, statErr := os.Stat(res.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri":      uri,
		"path":     res.Path,
		"fragment": res.Fragment,
		"exists":   statErr == nil,
	})
}

// handleCogDocRead resolves a cog:// URI and returns the file content as text.
//
//	GET /v1/cogdoc/read?uri=cog://mem/semantic/insights/foo.md
//	200 → { uri, path, content, exists }
func (s *Server) handleCogDocRead(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "uri parameter required"})
		return
	}

	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	content, readErr := os.ReadFile(res.Path)
	exists := readErr == nil

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"uri":    uri,
		"path":   res.Path,
		"exists": exists,
	}
	if exists {
		resp["content"] = string(content)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ── OpenAI-compatible wire types ─────────────────────────────────────────────

type oaiChatRequest struct {
	Model               string               `json:"model"`
	Messages            []oaiMessage          `json:"messages"`
	Stream              bool                  `json:"stream"`
	Temperature         *float64              `json:"temperature,omitempty"`
	MaxTokens           int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                   `json:"max_completion_tokens,omitempty"`
	TopP                *float64              `json:"top_p,omitempty"`
	Stop                []string              `json:"stop,omitempty"`
	Tools               []oaiToolDefinition   `json:"tools,omitempty"`
	ToolChoice          json.RawMessage       `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                 `json:"parallel_tool_calls,omitempty"`
	FrequencyPenalty    *float64              `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64              `json:"presence_penalty,omitempty"`
	Seed                *int                  `json:"seed,omitempty"`
	User                string                `json:"user,omitempty"`
	N                   *int                  `json:"n,omitempty"`
	StreamOptions       *oaiStreamOpts        `json:"stream_options,omitempty"`
}

// oaiStreamOpts carries OpenAI stream_options (e.g. include_usage).
type oaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// oaiToolDefinition is the OpenAI-format tool envelope: {"type":"function","function":{...}}.
type oaiToolDefinition struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

// oaiToolFunction carries the tool name, description, and JSON Schema parameters.
type oaiToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// oaiToolCall is the OpenAI-format tool call in a response message.
type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiToolCallFunc `json:"function"`
}

// oaiToolCallFunc carries the function name and stringified arguments.
type oaiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// oaiStreamToolCall is a tool call delta in a streaming response.
type oaiStreamToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function *oaiToolCallFunc `json:"function,omitempty"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

// extractContent normalises the OpenAI "content" field which may arrive as
// either a plain JSON string or an array of content-parts (the multi-part
// format used by Discord gateway and other clients):
//
//	"hello"                                   → "hello"
//	[{"type":"text","text":"hello"}]           → "hello"
func extractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Fast path: plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Slow path: array of content parts.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Unrecognised shape — return the raw bytes as-is so nothing is lost.
		return string(raw)
	}

	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// oaiContentPart represents a single element in the OpenAI multi-part content array.
type oaiContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *oaiImageURL    `json:"image_url,omitempty"`
}

// oaiImageURL carries the URL (typically a data: base64 URI) for an image content part.
type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// extractContentParts normalises the OpenAI "content" field into structured
// parts, preserving both text and image_url entries. This is used instead of
// extractContent when the caller needs to forward image data to providers.
func extractContentParts(raw json.RawMessage) []oaiContentPart {
	if len(raw) == 0 {
		return nil
	}

	// Fast path: plain string → single text part.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []oaiContentPart{{Type: "text", Text: s}}
	}

	// Slow path: array of content parts.
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Unrecognised shape — wrap raw bytes as text so nothing is lost.
		return []oaiContentPart{{Type: "text", Text: string(raw)}}
	}
	return parts
}

// mustMarshalString wraps a Go string as a JSON-encoded string suitable for
// json.RawMessage (i.e. it adds the surrounding quotes and escapes).
func mustMarshalString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message,omitempty"`
	Delta        *oaiMessage `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

// handleChat is the OpenAI-compatible /v1/chat/completions endpoint.
// Routes through the inference Router when set; returns 501 otherwise.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("cogos-v3").Start(r.Context(), "chat.request")
	defer span.End()
	r = r.WithContext(ctx)

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MB limit
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req oaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	block := NormalizeOpenAIRequest(&req, body, "http")
	block.SessionID = s.process.SessionID()
	if s.nucleus != nil {
		block.TargetIdentity = s.nucleus.Name
	}
	block.WorkspaceID = filepath.Base(s.cfg.WorkspaceRoot)
	s.process.RecordBlock(block)

	clientMsgs := block.Messages

	// Extract the user's latest message as the query for relevance scoring.
	query := ""
	for i := len(clientMsgs) - 1; i >= 0; i-- {
		if clientMsgs[i].Role == "user" {
			query = clientMsgs[i].Content
			break
		}
	}

	// Notify the process of the incoming interaction.
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

	// Assemble foveated context — engine owns the full window.
	// It decomposes client messages, scores them alongside CogDocs,
	// and manages the budget including conversation history.

	// Resolve max tokens: prefer max_completion_tokens (OpenAI v2 field,
	// sent by Zed and newer clients) over legacy max_tokens.
	maxToks := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		maxToks = req.MaxCompletionTokens
	}

	creq := &CompletionRequest{
		MaxTokens:     maxToks,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Stop:          req.Stop,
		InteractionID: block.ID,
		Metadata: RequestMetadata{
			RequestID:    uuid.New().String(),
			ProcessState: "active", // chat requests are always active interactions
			Priority:     PriorityNormal,
			Source:       "http",
		},
	}

	// Convert OpenAI-format tool definitions to internal ToolDefinition.
	if len(req.Tools) > 0 {
		creq.Tools = make([]ToolDefinition, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			creq.Tools = append(creq.Tools, ToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	// Convert tool_choice: OpenAI sends either a string ("auto"/"none"/"required")
	// or an object {"type":"function","function":{"name":"..."}}.
	if len(req.ToolChoice) > 0 {
		var tcStr string
		if err := json.Unmarshal(req.ToolChoice, &tcStr); err == nil {
			creq.ToolChoice = tcStr
		} else {
			var tcObj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &tcObj); err == nil && tcObj.Function.Name != "" {
				creq.ToolChoice = tcObj.Function.Name
			}
		}
	}

	// Map OpenClaw model names to provider routing.
	// "claude", "codex", "ollama" are provider aliases, not model names.
	switch req.Model {
	case "", "local":
		// use default routing
	case "claude":
		creq.Metadata.PreferProvider = "claude-code"
	case "codex":
		creq.Metadata.PreferProvider = "codex"
	case "ollama":
		creq.Metadata.PreferProvider = "ollama"
	default:
		// Pass through as model override (e.g. "opus", "haiku", "gpt-5.4")
		creq.ModelOverride = req.Model
	}

	var pkg *ContextPackage
	conversationTurnsIn := 0
	for _, m := range clientMsgs {
		if m.Role != "system" {
			conversationTurnsIn++
		}
	}

	if p, err := s.process.AssembleContext(query, clientMsgs, 0,
		WithContext(r.Context()),
		WithConversationID(creq.Metadata.RequestID),
		WithManifestMode(true),
	); err != nil {
		slog.Warn("chat: context assembly failed", "err", err)
		creq.Messages = clientMsgs
	} else {
		pkg = p
		systemPrompt, managedMsgs := pkg.FormatForProvider()
		creq.SystemPrompt = systemPrompt
		creq.Messages = managedMsgs

		// Record metrics + span attributes.
		span.SetAttributes(
			attribute.Int("cogos.context.total_tokens", pkg.TotalTokens),
			attribute.Int("cogos.context.docs_injected", len(pkg.FovealDocs)),
			attribute.Int("cogos.context.conv_turns_kept", len(pkg.Conversation)),
			attribute.Int("cogos.context.conv_turns_in", conversationTurnsIn),
		)
		if instruments.ContextTokens != nil {
			instruments.ContextTokens.Record(ctx, int64(pkg.TotalTokens))
		}
		if instruments.DocsInjected != nil {
			instruments.DocsInjected.Record(ctx, int64(len(pkg.FovealDocs)))
		}
		evicted := conversationTurnsIn - len(pkg.Conversation)
		if evicted > 0 && instruments.TurnsEvicted != nil {
			instruments.TurnsEvicted.Add(ctx, int64(evicted))
		}

		if len(pkg.InjectedPaths) > 0 {
			slog.Info("chat: context injected",
				"docs", len(pkg.InjectedPaths),
				"conv_turns", len(pkg.Conversation),
				"tokens", pkg.TotalTokens,
			)
		}
	}

	provider, _, err := s.router.Route(r.Context(), creq)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
		return
	}

	respID := "chatcmpl-" + uuid.New().String()
	model := provider.Name()
	if req.Model != "" && req.Model != "local" {
		model = req.Model
	}

	span.SetAttributes(
		attribute.String("cogos.provider", provider.Name()),
		attribute.String("cogos.model", model),
	)
	if instruments.ChatRequests != nil {
		instruments.ChatRequests.Add(ctx, 1)
	}

	inferStart := time.Now()
	if req.Stream {
		s.streamChat(w, r.Context(), creq, provider, respID, model, req.StreamOptions)
	} else {
		s.completeChat(w, r.Context(), creq, provider, respID, model)
	}

	inferMs := float64(time.Since(inferStart).Milliseconds())
	span.SetAttributes(attribute.Float64("cogos.inference.latency_ms", inferMs))
	if instruments.InferenceLatency != nil {
		instruments.InferenceLatency.Record(ctx, inferMs)
	}

	// Capture debug snapshot (best-effort, non-blocking).
	go func() {
		snap := captureDebugSnapshot(
			clientMsgs, query, req.Model, pkg, conversationTurnsIn,
			provider.Name(), model, 0, time.Since(inferStart),
		)
		s.debug.Store(snap)
	}()
}

// completeChat handles non-streaming chat completions.
func (s *Server) completeChat(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string) {

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		slog.Warn("chat: complete error", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
		return
	}

	msg := &oaiMessage{Role: "assistant", Content: mustMarshalString(resp.Content)}
	finishReason := mapStopReasonToOpenAI(resp.StopReason)
	if finishReason == "" {
		finishReason = "stop"
	}

	// Wrap tool calls in the OpenAI response format.
	if len(resp.ToolCalls) > 0 {
		finishReason = "tool_calls"
		// OpenAI spec: tool-call-only messages must have "content": null, not "".
		if resp.Content == "" {
			msg.Content = json.RawMessage("null")
		}
		calls := make([]oaiToolCall, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			calls[i] = oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: oaiToolCallFunc{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			}
		}
		raw, _ := json.Marshal(calls)
		msg.ToolCalls = json.RawMessage(raw)
	}

	oai := oaiChatResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []oaiChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: &oaiUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(oai)
}

// streamChat handles streaming chat completions via SSE.
func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string, streamOpts *oaiStreamOpts) {

	chunks, err := provider.Stream(ctx, req)
	if err != nil {
		slog.Warn("chat: stream error", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
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

	writeSSE := func(data []byte) {
		_, _ = fmt.Fprintf(bw, "data: %s\n\n", data)
		flush()
	}

	// Track whether any tool calls were streamed (affects finish_reason).
	sawToolCall := false

	for sc := range chunks {
		if sc.Error != nil {
			slog.Warn("chat: stream chunk error", "err", sc.Error)
			break
		}

		if sc.Done {
			// Final chunk: emit finish_reason.
			// Prefer the provider-reported stop reason, falling back to heuristic.
			finishReason := mapStopReasonToOpenAI(sc.StopReason)
			if finishReason == "" {
				finishReason = "stop"
				if sawToolCall {
					finishReason = "tool_calls"
				}
			}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, FinishReason: &finishReason}},
			}
			// When stream_options.include_usage is set, include usage in the final chunk.
			if streamOpts != nil && streamOpts.IncludeUsage && sc.Usage != nil {
				data.Usage = &oaiUsage{
					PromptTokens:     sc.Usage.InputTokens,
					CompletionTokens: sc.Usage.OutputTokens,
					TotalTokens:      sc.Usage.InputTokens + sc.Usage.OutputTokens,
				}
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
			break
		}

		// Tool call delta — wrap in OpenAI streaming tool_calls format.
		if sc.ToolCallDelta != nil {
			sawToolCall = true
			tc := oaiStreamToolCall{
				Index: sc.ToolCallDelta.Index,
			}
			if sc.ToolCallDelta.ID != "" {
				tc.ID = sc.ToolCallDelta.ID
				tc.Type = "function"
			}
			// Always create Function — OpenAI spec requires it on every tool call
			// delta, even the initial chunk where only Name is set and Arguments
			// is empty. Omitting Function causes clients to see function: null.
			tc.Function = &oaiToolCallFunc{
				Name:      sc.ToolCallDelta.Name,
				Arguments: sc.ToolCallDelta.ArgsDelta,
			}
			tcRaw, _ := json.Marshal([]oaiStreamToolCall{tc})
			// Content left nil → serialises as "content": null (OpenAI spec for tool-call-only deltas).
			delta := &oaiMessage{Role: "assistant", ToolCalls: json.RawMessage(tcRaw)}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, Delta: delta}},
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
			continue
		}

		// Text delta.
		if sc.Delta != "" {
			delta := &oaiMessage{Role: "assistant", Content: mustMarshalString(sc.Delta)}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, Delta: delta}},
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
		}
	}
	_, _ = fmt.Fprint(bw, "data: [DONE]\n\n")
	flush()
}

// handleDebugLast returns the full pipeline snapshot from the most recent chat request.
func (s *Server) handleDebugLast(w http.ResponseWriter, r *http.Request) {
	snap := s.debug.Load()
	if snap == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no requests yet"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap)
}

// handleDebugContext returns the current context window as stability-ordered zones.
func (s *Server) handleDebugContext(w http.ResponseWriter, r *http.Request) {
	snap := s.debug.Load()
	if snap == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no requests yet"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap.Context)
}

// handleProprioceptive returns the last 50 entries from the proprioceptive JSONL log
// plus a placeholder light cone status.
//
//	GET /v1/proprioceptive
//	200 → { entries, light_cone }
func (s *Server) handleProprioceptive(w http.ResponseWriter, r *http.Request) {
	logPath := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "run", "proprioceptive.jsonl")

	entries := readLastJSONLEntries(logPath, 50)

	// Build light cone summary from real data when available.
	lcStatus := map[string]interface{}{
		"active":          false,
		"layers":          0,
		"layer_norms":     []float64{},
		"compressed_norm": 0.0,
	}
	if lcm := s.process.LightCones(); lcm != nil {
		count := lcm.Count()
		lcStatus["active"] = count > 0
		lcStatus["count"] = count
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":    entries,
		"light_cone": lcStatus,
	})
}

// handleLightCone returns light cone metadata from the LightConeManager.
// When TRM is loaded, returns real per-conversation light cone states.
// When TRM is not available, returns a placeholder indicating TRM is disabled.
//
//	GET /v1/lightcone
//	200 → { active, count, cones: [...] } or placeholder
func (s *Server) handleLightCone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	lcm := s.process.LightCones()
	if lcm == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":          false,
			"count":           0,
			"cones":           []LightConeInfo{},
			"layers":          0,
			"layer_norms":     []float64{},
			"compressed_norm": 0.0,
			"note":            "TRM not loaded. Configure trm_weights_path in kernel.yaml to enable.",
		})
		return
	}

	cones := lcm.List()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"active": len(cones) > 0,
		"count":  len(cones),
		"cones":  cones,
	})
}

// readLastJSONLEntries reads the last n lines from a JSONL file and returns
// them as a slice of parsed JSON objects. If the file does not exist or is
// empty, it returns an empty slice (never nil).
func readLastJSONLEntries(path string, n int) []json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return []json.RawMessage{}
	}
	defer f.Close()

	// Read all lines, keeping the last n. For typical proprioceptive logs
	// (hundreds to low-thousands of entries) this is efficient enough.
	var lines []string
	scanner := bufio.NewScanner(f)
	// Allow up to 1 MB per line to handle large entries.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	// Take the last n lines.
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	entries := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		// Validate that it's valid JSON before including it.
		raw := json.RawMessage(line)
		if json.Valid(raw) {
			entries = append(entries, raw)
		} else {
			slog.Warn("proprioceptive: skipping invalid JSON line", "path", path)
		}
	}
	return entries
}

// sanitizeErrorMessage strips URLs and long alphanumeric strings (potential API
// keys) from an error message before returning it to clients.
var (
	reURL    = regexp.MustCompile(`https?://[^\s"',]+`)
	reAPIKey = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
)

func sanitizeErrorMessage(msg string) string {
	msg = reURL.ReplaceAllString(msg, "[redacted-url]")
	msg = reAPIKey.ReplaceAllString(msg, "[redacted]")
	return msg
}
