// serve_inference.go — Chat completions handler, streaming/non-streaming responses, tool bridge, and thread persistence

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/cogos-dev/cogos/sdk"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (s *serveServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Parse request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error(), "invalid_request")
		return
	}

	// Enrich OTEL span with request attributes
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(
		attribute.String("model", req.Model),
		attribute.Bool("stream", req.Stream),
	)

	// Parse UCP headers (Universal Context Protocol)
	workspaceRoot := s.kernel.Root()
	ucpContext, err := parseUCPHeaders(r, workspaceRoot)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid UCP headers: "+err.Error(), "invalid_request")
		return
	}

	// Log UCP packet presence
	if ucpContext != nil {
		var packets []string
		if ucpContext.Identity != nil {
			packets = append(packets, "identity")
		}
		if ucpContext.TAA != nil {
			packets = append(packets, "taa")
		}
		if ucpContext.Memory != nil {
			packets = append(packets, "memory")
		}
		if ucpContext.History != nil {
			packets = append(packets, "history")
		}
		if ucpContext.Workspace != nil {
			packets = append(packets, "workspace")
		}
		if ucpContext.User != nil {
			packets = append(packets, "user")
		}
		if len(packets) > 0 {
			log.Printf("[UCP] Request with %d packets: %v", len(packets), packets)
		}
	}

	// Extract system prompt and user messages.
	// Conversation history is flattened into a single prompt string because
	// Claude CLI's -p flag is one-shot. Tool call/result history from prior
	// turns is preserved as text context so the model sees what happened.
	systemPrompt := req.SystemPrompt
	var userPrompt strings.Builder

	for _, msg := range req.Messages {
		content := msg.GetContent()
		switch msg.Role {
		case "system":
			if systemPrompt == "" {
				systemPrompt = content
			} else {
				systemPrompt += "\n\n" + content
			}
		case "user":
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\n")
			}
			userPrompt.WriteString(content)
		case "assistant":
			// Include prior assistant messages as context.
			// Handle both text content and tool_calls (which may have null content).
			if userPrompt.Len() > 0 {
				userPrompt.WriteString("\n\nAssistant: ")
				if content != "" {
					userPrompt.WriteString(content)
				}
				// Include tool calls so the model knows what tools were used
				if toolSummary := msg.GetToolCallsSummary(); toolSummary != "" {
					if content != "" {
						userPrompt.WriteString("\n")
					}
					userPrompt.WriteString(toolSummary)
				}
				userPrompt.WriteString("\n\nUser: ")
			}
		case "tool":
			// Include tool results so the model sees the full tool interaction.
			// Format: [Tool result for <call_id>: <content>]
			if userPrompt.Len() > 0 && content != "" {
				toolResult := content
				if len(toolResult) > 500 {
					toolResult = toolResult[:500] + "...(truncated)"
				}
				if msg.ToolCallID != "" {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result (%s): %s]", msg.ToolCallID, toolResult))
				} else {
					userPrompt.WriteString(fmt.Sprintf("\n[Tool result: %s]", toolResult))
				}
			}
		}
	}

	if userPrompt.Len() == 0 {
		s.writeError(w, http.StatusBadRequest, "No user message provided", "invalid_request")
		return
	}

	// Extract session ID for thread persistence and context
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Eidolon-ID")
	}

	// Auto-derive session from origin + agent identity when no explicit session provided.
	// Different agents (e.g., Cog vs Whirl) get separate Claude sessions even when
	// both route through the same OpenClaw gateway with the same origin.
	if sessionID == "" {
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}
		// Include agent name in session key so different agents don't share sessions
		if ucpContext != nil && ucpContext.Identity != nil && ucpContext.Identity.Name != "" {
			sessionID = origin + ":" + strings.ToLower(ucpContext.Identity.Name)
		} else {
			sessionID = origin
		}
	}

	// === LIFECYCLE: track session and inject first-turn context ===
	lcOrigin := r.Header.Get("X-Origin")
	if lcOrigin == "" {
		lcOrigin = "http"
	}
	lcAgentName := ""
	if ucpContext != nil && ucpContext.Identity != nil {
		lcAgentName = strings.ToLower(ucpContext.Identity.Name)
	}

	lcSession, isFirstTurn := s.lifecycle.GetOrCreate(sessionID, lcOrigin, lcAgentName)

	if isFirstTurn {
		sessionCtx := LoadSessionContext(workspaceRoot, lcSession)
		if sessionCtx != "" {
			if systemPrompt == "" {
				systemPrompt = sessionCtx
			} else {
				systemPrompt = sessionCtx + "\n\n---\n\n" + systemPrompt
			}
		}
		if err := CreateWorkingMemory(workspaceRoot, sessionID); err != nil {
			log.Printf("[lifecycle] Failed to create working memory: %v", err)
		}
	}

	// Persist user message to thread (substrate-based memory)
	if err := s.appendToThread("user", userPrompt.String(), sessionID); err != nil {
		log.Printf("[TAA] Failed to persist user message: %v", err)
	}

	// Check if TAA context injection is requested
	// Priority: UCP-TAA header > X-TAA-Profile header > body field
	var contextState *ContextState
	var taaProfile string
	var taaEnabled bool

	if ucpContext != nil && ucpContext.TAA != nil {
		// Use UCP-TAA packet (explicit mode)
		taaProfile = ucpContext.TAA.Profile
		taaEnabled = true
		log.Printf("[UCP] Using UCP-TAA packet: profile=%s, total_tokens=%d", taaProfile, ucpContext.TAA.TotalTokens)
	} else {
		// Fall back to legacy TAA header/body field
		taaHeader := r.Header.Get("X-TAA-Profile")
		taaProfile, taaEnabled = req.GetTAAProfileWithHeader(taaHeader)
	}

	if taaEnabled {
		var err error

		// Try bus-sourced context first (multi-turn history)
		if s.busChat != nil && sessionID != "" {
			busCtxID := fmt.Sprintf("bus_chat_%s", sessionID)
			contextState = s.busChat.buildContextFromBus(busCtxID, userPrompt.String())
			if contextState != nil {
				log.Printf("[TAA] Using bus history: %s (%d total tokens) instead of request messages",
					busCtxID, contextState.TotalTokens)
			}
		}

		// Fall back to request-only context
		if contextState == nil {
			contextState, err = ConstructContextStateWithProfile(req.Messages, sessionID, workspaceRoot, taaProfile)
		}

		if err != nil {
			// Log but don't fail - context is optional enhancement
			log.Printf("[TAA] Context construction warning (profile=%s): %v", taaProfile, err)
		} else if contextState != nil {
			log.Printf("[TAA] Context loaded: profile=%s, tokens=%d, coherence=%.2f",
				taaProfile, contextState.TotalTokens, contextState.CoherenceScore)

			// Populate UCP response metrics if UCP was used
			if ucpContext != nil && ucpContext.TAA != nil {
				constructedTokens := contextState.TotalTokens
				ucpContext.TAA.ConstructedTokens = &constructedTokens

				// Extract tier token counts from ContextTier structs
				tierBreakdown := make(map[string]int)
				if contextState.Tier1Identity != nil {
					tierBreakdown["tier1"] = contextState.Tier1Identity.Tokens
				}
				if contextState.Tier2Temporal != nil {
					tierBreakdown["tier2"] = contextState.Tier2Temporal.Tokens
				}
				if contextState.Tier3Present != nil {
					tierBreakdown["tier3"] = contextState.Tier3Present.Tokens
				}
				if contextState.Tier4Semantic != nil {
					tierBreakdown["tier4"] = contextState.Tier4Semantic.Tokens
				}
				ucpContext.TAA.TierBreakdown = tierBreakdown
			}

			// Log low coherence for observability
			if contextState.ShouldRefresh {
				log.Printf("[TAA] Low coherence (%.2f) — context may be degraded", contextState.CoherenceScore)
			}
		}
	}

	// Emit bus chat.request event (side-effect for CogField visibility)
	var busID string
	var requestSeq int
	var requestHash string
	if s.busChat != nil && sessionID != "" {
		origin := r.Header.Get("X-Origin")
		if origin == "" {
			origin = "http"
		}

		reqOpts := ChatRequestOpts{
			SessionID: sessionID,
			Content:   userPrompt.String(),
			Origin:    origin,
			Model:     req.Model,
			Stream:    req.Stream,
		}
		// Identity from UCP or fallback headers
		if ucpContext != nil && ucpContext.User != nil {
			reqOpts.UserID = ucpContext.User.ID
			reqOpts.UserName = ucpContext.User.DisplayName
		} else {
			reqOpts.UserID = r.Header.Get("X-OpenClaw-User-ID")
			reqOpts.UserName = r.Header.Get("X-OpenClaw-User-Name")
		}
		if ucpContext != nil && ucpContext.Identity != nil {
			reqOpts.AgentName = ucpContext.Identity.Name
		}
		// OTEL trace context from headers
		reqOpts.TraceID = r.Header.Get("X-Trace-ID")
		reqOpts.SpanID = r.Header.Get("X-Span-ID")
		// TAA context
		reqOpts.HasTAA = taaEnabled
		reqOpts.TAAProfile = taaProfile

		var reqEvt *CogBlock
		busID, reqEvt, _ = s.busChat.emitRequest(reqOpts)
		if reqEvt != nil {
			requestSeq = reqEvt.Seq
			requestHash = reqEvt.Hash
		}
	}

	// Build InferenceRequest using shared engine
	var schema json.RawMessage
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		schema = req.ResponseFormat.JSONSchema
	}

	// When tools are present, use a detached context instead of the HTTP
	// request context. This prevents the CLI from being killed when Request 1's
	// handler returns (tool bridge parks the channel across HTTP boundaries).
	var inferCtx context.Context
	if len(req.Tools) > 0 {
		inferCtx = context.Background()
	} else {
		inferCtx = r.Context()
	}

	inferReq := &InferenceRequest{
		Prompt:       userPrompt.String(),
		SystemPrompt: systemPrompt,
		Model:        req.Model,
		Schema:       schema,
		MaxTokens:    req.MaxTokens,
		Origin:       "http",
		Stream:       req.Stream,
		Context:      inferCtx,
		ContextState: contextState,
		Tools:        req.Tools,
	}

	// Always set session ID for the harness (needed by MCP bridge's SESSION_ID env var)
	inferReq.SessionID = sessionID

	// === CONTEXT ENGINE (normalizes thread, manages sessions, compresses context) ===
	// When the context engine is available, it replaces the simple claudeSessionStore
	// lookup with full thread parsing, dedup, and budget-constrained context windows.
	if s.contextEngine != nil {
		ctxWindow, ctxErr := s.contextEngine.Build(req.Messages, sessionID, taaProfile, RequestHeaders{
			Origin:       r.Header.Get("X-Origin"),
			SessionReset: r.Header.Get("X-Session-Reset") == "true",
			UserID:       r.Header.Get("X-OpenClaw-User-ID"),
			UserName:     r.Header.Get("X-OpenClaw-User-Name"),
		})
		if ctxErr != nil {
			log.Printf("[context-engine] Error: %v (falling back to legacy path)", ctxErr)
			// Fall through to legacy session lookup below
		} else {
			inferReq.Prompt = ctxWindow.Prompt
			if ctxWindow.SystemPrompt != "" {
				inferReq.SystemPrompt = ctxWindow.SystemPrompt
			}
			inferReq.ClaudeSessionID = ctxWindow.ClaudeSession
			log.Printf("[context-engine] Applied: strategy=%s tokens=%d", ctxWindow.Strategy, ctxWindow.TotalTokens)
		}
	}

	// Legacy fallback: Claude CLI session continuity via simple map lookup.
	// Only used when context engine is nil or errored.
	if inferReq.ClaudeSessionID == "" {
		s.claudeSessionStoreMu.RLock()
		if csid, ok := s.claudeSessionStore[sessionID]; ok {
			inferReq.ClaudeSessionID = csid
			lastUserMsg := extractLastUserMessage(req.Messages)
			if lastUserMsg != "" {
				inferReq.Prompt = lastUserMsg
				log.Printf("[session] Resuming Claude CLI session %s for %s (legacy path)", csid, sessionID)
			}
		}
		s.claudeSessionStoreMu.RUnlock()
	}

	// Use UCP workspace root as Claude CLI working directory when provided.
	// This lets callers (e.g., OpenClaw) specify which workspace the backend
	// should operate in, rather than always using the kernel's workspace.
	if ucpContext != nil && ucpContext.Workspace != nil && ucpContext.Workspace.Root != "" {
		inferReq.WorkspaceRoot = ucpContext.Workspace.Root
	}

	// Simple workspace override: X-Workspace-Root header sets the working
	// directory without requiring a full UCP workspace packet. This is a
	// lightweight alternative for callers that only need to specify the cwd.
	if inferReq.WorkspaceRoot == "" {
		if simpleRoot := r.Header.Get("X-Workspace-Root"); simpleRoot != "" {
			inferReq.WorkspaceRoot = simpleRoot
		}
	}

	// Parse X-Allowed-Tools header for explicit tool control
	if allowedToolsHeader := r.Header.Get("X-Allowed-Tools"); allowedToolsHeader != "" {
		var tools []string
		for _, t := range strings.Split(allowedToolsHeader, ",") {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				tools = append(tools, trimmed)
			}
		}
		inferReq.AllowedTools = tools
	}

	// Agent-aware tool policy enforcement from CRD.
	// If no explicit X-Allowed-Tools header was provided, look up the agent's
	// CRD and apply its modelConfig.allowedTools. The UCP Identity packet
	// carries the agent name (e.g., "Sentinel", "Whirl").
	if ucpContext != nil && ucpContext.Identity != nil && ucpContext.Identity.Name != "" {
		agentName := strings.ToLower(ucpContext.Identity.Name)
		policy, err := GetAgentCRDToolPolicy(workspaceRoot, agentName)
		if err != nil {
			log.Printf("[CRD] Warning: failed to load agent CRD for %q: %v", agentName, err)
		} else if policy != nil {
			// Headless agent gate: headless agents do NOT go through inference.
			// They should only receive tool dispatch via the bus ToolRouter.
			if policy.AgentType == "headless" {
				log.Printf("[CRD] Agent %q is headless — rejecting inference request", agentName)
				http.Error(w, `{"error":"headless agents do not support inference — use bus tool.invoke"}`, http.StatusBadRequest)
				return
			}

			// CRD-defined tools — only apply if no explicit header override
			if len(inferReq.AllowedTools) == 0 && len(policy.AllowedTools) > 0 {
				inferReq.AllowedTools = policy.AllowedTools
				log.Printf("[CRD] Applied tool policy for agent %q: %v", agentName, policy.AllowedTools)
			}
		}
	}

	// Parse OpenClaw bridge headers for MCP bridge mode (headers override env vars).
	// The harness auto-generates the MCP config when it sees OpenClawURL set.
	openClawURL := r.Header.Get("X-OpenClaw-URL")
	if openClawURL == "" {
		openClawURL = os.Getenv("OPENCLAW_URL")
	}
	if openClawURL != "" {
		inferReq.OpenClawURL = openClawURL
		inferReq.OpenClawToken = r.Header.Get("X-OpenClaw-Token")
		if inferReq.OpenClawToken == "" {
			inferReq.OpenClawToken = os.Getenv("OPENCLAW_TOKEN")
		}
		inferReq.SessionID = sessionID // Use already-resolved session ID from earlier parsing
	}

	// Extract user identity from UCP or OpenClaw fallback headers.
	// Priority: X-UCP-User header > X-OpenClaw-User-ID / X-OpenClaw-User-Name fallback
	if ucpContext != nil && ucpContext.User != nil {
		inferReq.UserID = ucpContext.User.ID
		inferReq.UserName = ucpContext.User.DisplayName
		log.Printf("[UCP] User identity: id=%s name=%s source=%s",
			ucpContext.User.ID, ucpContext.User.DisplayName, ucpContext.User.Source)
	} else {
		// Fallback: OpenClaw sends user identity via simpler headers
		if userID := r.Header.Get("X-OpenClaw-User-ID"); userID != "" {
			inferReq.UserID = userID
		}
		if userName := r.Header.Get("X-OpenClaw-User-Name"); userName != "" {
			inferReq.UserName = userName
		}
		if inferReq.UserID != "" {
			log.Printf("[UCP] User identity (fallback headers): id=%s name=%s",
				inferReq.UserID, inferReq.UserName)
		}
	}

	// Wire user memory scope if we have a user identity and an agent CRD
	if inferReq.UserID != "" && ucpContext != nil && ucpContext.Identity != nil {
		agentName := strings.ToLower(ucpContext.Identity.Name)
		if crd, err := LoadAgentCRD(workspaceRoot, agentName); err == nil {
			if scope := BuildUserScope(crd, inferReq.UserID); scope != nil {
				log.Printf("[memory-scope] user=%s agent=%s level=%s scope=%s",
					inferReq.UserID, agentName, scope.Level, scope.UserScope)
			}
		}
	}

	// Set UCP response headers if UCP was used
	if ucpContext != nil {
		if err := setUCPResponseHeaders(w, ucpContext); err != nil {
			log.Printf("[UCP] Failed to set response headers: %v", err)
		}
	}

	// Collect bus enrichment context for response/error handlers
	bctx := busEventCtx{
		BusID:       busID,
		RequestSeq:  requestSeq,
		RequestHash: requestHash,
		TraceID:     r.Header.Get("X-Trace-ID"),
		SpanID:      r.Header.Get("X-Span-ID"),
	}

	// Check for tool bridge continuation: if the request contains role:"tool"
	// messages and there's an active bridge session, deliver results and resume
	// streaming from the parked output channel instead of starting a new CLI.
	startTime := time.Now()
	if s.toolBridge != nil && s.hasToolMessages(req.Messages) {
		if sess := s.toolBridge.GetSession(sessionID); sess != nil {
			log.Printf("[tool-bridge] Continuation detected for session %s", sessionID)
			s.handleToolBridgeContinuation(w, &req, sess, sessionID, bctx, startTime, r.Context())
			return
		}
	}

	// Handle streaming vs non-streaming
	// Pass the HTTP request context so streaming handlers can detect client
	// disconnect and cancel the inference process (important when using
	// context.Background() for tool bridge support).
	httpCtx := r.Context()
	if req.Stream {
		s.handleStreamingResponse(w, inferReq, sessionID, bctx, startTime, httpCtx)
	} else {
		s.handleNonStreamingResponse(w, inferReq, sessionID, bctx, startTime)
	}
}

// busEventCtx carries enrichment fields from the HTTP handler into streaming/non-streaming
// response handlers for bus event emission. Avoids threading the full *http.Request.
type busEventCtx struct {
	BusID       string
	RequestSeq  int
	RequestHash string
	TraceID     string
	SpanID      string
}

func (s *serveServer) handleStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string, bctx busEventCtx, startTime time.Time, httpCtx context.Context) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Track whether the handler exits normally (completion or tool bridge
	// suspension) vs. abnormally (client disconnect). When the HTTP request
	// context is cancelled and we haven't set normalExit, the client
	// disconnected and the inference should be cancelled.
	var normalExit atomic.Bool
	go func() {
		<-httpCtx.Done()
		if !normalExit.Load() {
			if GlobalRegistry.Cancel(inferReq.ID) {
				log.Printf("[streaming] Client disconnected, cancelled request %s", inferReq.ID)
			}
		}
	}()

	// Add TAA context as headers (before streaming starts, so clients can read immediately)
	if inferReq.ContextState != nil {
		ctx := inferReq.ContextState
		w.Header().Set("X-TAA-Total-Tokens", strconv.Itoa(ctx.TotalTokens))
		w.Header().Set("X-TAA-Coherence", fmt.Sprintf("%.2f", ctx.CoherenceScore))
		if ctx.Anchor != "" {
			w.Header().Set("X-TAA-Anchor", ctx.Anchor)
		}
		if ctx.Goal != "" {
			w.Header().Set("X-TAA-Goal", ctx.Goal)
		}
		if ctx.Tier1Identity != nil {
			w.Header().Set("X-TAA-Tier1-Tokens", strconv.Itoa(ctx.Tier1Identity.Tokens))
		}
		if ctx.Tier2Temporal != nil {
			w.Header().Set("X-TAA-Tier2-Tokens", strconv.Itoa(ctx.Tier2Temporal.Tokens))
		}
		if ctx.Tier3Present != nil {
			w.Header().Set("X-TAA-Tier3-Tokens", strconv.Itoa(ctx.Tier3Present.Tokens))
		}
		if ctx.Tier4Semantic != nil {
			w.Header().Set("X-TAA-Tier4-Tokens", strconv.Itoa(ctx.Tier4Semantic.Tokens))
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		normalExit.Store(true)
		s.writeError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	// Rolling write deadline — prevents the server's global WriteTimeout
	// (5 min) from killing long-running inference streams.  Each call pushes
	// the deadline forward by sseWriteWindow so the timeout becomes per-idle
	// rather than absolute.
	rc := http.NewResponseController(w)
	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
	}

	// Store TAA context for /v1/taa endpoint and emit as SSE event
	// Deep copy to prevent data races — the original may be mutated concurrently.
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = deepCopyContextState(inferReq.ContextState)
		s.taaStateMutex.Unlock()
		extendDeadline()
		s.emitTAAContext(w, flusher, inferReq.ContextState, inferReq.Model)
	}

	// Pre-create tool bridge session if external tools are present.
	// MCP bridges may POST to /v1/tool-bridge/pending before message_stop,
	// since Claude CLI invokes MCP tools eagerly during streaming.
	hasExternalTools := len(inferReq.Tools) > 0
	if hasExternalTools && s.toolBridge != nil {
		s.toolBridge.EnsureSession(sessionID)
	}

	// Delegate to harness inference engine
	chunks, err := HarnessRunInferenceStream(inferReq, GlobalRegistry)
	if err != nil {
		normalExit.Store(true)
		s.writeSSEError(w, flusher, "Failed to start inference: "+err.Error())
		if s.busChat != nil && bctx.BusID != "" {
			s.busChat.emitError(ChatErrorOpts{
				BusID:        bctx.BusID,
				RequestSeq:   bctx.RequestSeq,
				RequestHash:  bctx.RequestHash,
				ErrorMessage: err.Error(),
				ErrorType:    "inference_start",
				DurationMs:   time.Since(startTime).Milliseconds(),
				Model:        inferReq.Model,
				Stream:       true,
				TraceID:      bctx.TraceID,
				SpanID:       bctx.SpanID,
			})
		}
		return
	}

	model := inferReq.Model
	if model == "" {
		model = "claude"
	}
	created := time.Now().Unix()

	// Accumulate content for thread persistence
	var accumulatedContent strings.Builder

	// Process chunks from the inference engine
	for chunk := range chunks {
		extendDeadline()

		if chunk.Error != nil {
			normalExit.Store(true)
			s.writeSSEError(w, flusher, "Inference error: "+chunk.Error.Error())
			if s.busChat != nil && bctx.BusID != "" {
				s.busChat.emitError(ChatErrorOpts{
					BusID:        bctx.BusID,
					RequestSeq:   bctx.RequestSeq,
					RequestHash:  bctx.RequestHash,
					ErrorMessage: chunk.Error.Error(),
					ErrorType:    "inference_stream",
					DurationMs:   time.Since(startTime).Milliseconds(),
					Model:        inferReq.Model,
					Stream:       true,
					TraceID:      bctx.TraceID,
					SpanID:       bctx.SpanID,
				})
			}
			return
		}

		// Handle rich event types
		// Note: All custom events include empty "choices" array for OpenAI SDK compatibility
		switch chunk.EventType {
		case "session_info", "session_start":
			// Emit session info as custom event
			// Note: We only send tool count, not full definitions (can be 100KB+)
			if chunk.SessionInfo != nil {
				// Store Claude CLI session ID for --resume on next message
				if chunk.SessionInfo.ClaudeSessionID != "" {
					s.claudeSessionStoreMu.Lock()
					s.claudeSessionStore[sessionID] = chunk.SessionInfo.ClaudeSessionID
					s.claudeSessionStoreMu.Unlock()
					if s.contextEngine != nil {
						s.contextEngine.sessionMgr.RecordClaudeSession(sessionID, chunk.SessionInfo.ClaudeSessionID)
					}
					log.Printf("[session] Stored Claude CLI session %s for %s", chunk.SessionInfo.ClaudeSessionID, sessionID)
				}
				toolCount := 0
				if chunk.SessionInfo.Tools != nil {
					toolCount = len(chunk.SessionInfo.Tools)
				}
				sessionChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{}, // Required for OpenAI SDK compatibility
					"event_type": chunk.EventType,
					"session": map[string]any{
						"session_id": chunk.SessionInfo.SessionID,
						"model":      chunk.SessionInfo.Model,
						"tool_count": toolCount,
					},
				}
				data, _ := json.Marshal(sessionChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue

		case "tool_use", "tool_use_start":
			// Claude CLI handles tool calls internally. Emit as informational
			// events (empty choices) so OpenAI-compatible clients don't try to
			// execute them. CogOS-aware clients can use the event_type field.
			if chunk.ToolCall != nil {
				// Eagerly register external tool calls with the tool bridge.
				// MCP bridges may arrive before message_stop, so calls must
				// be registered as soon as content_block_stop events arrive.
				if hasExternalTools && s.toolBridge != nil && chunk.EventType == "tool_use" {
					const mcpPrefix = "mcp__cogos-bridge__"
					if strings.HasPrefix(chunk.ToolCall.Name, mcpPrefix) {
						origName := strings.TrimPrefix(chunk.ToolCall.Name, mcpPrefix)
						s.toolBridge.RegisterCall(sessionID, &ToolBridgeCall{
							ToolCallID: chunk.ToolCall.ID,
							Name:       origName,
							Arguments:  string(chunk.ToolCall.Arguments),
							ResultCh:   make(chan ToolBridgeResult, 1),
						})
					}
				}

				toolStartChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call",
					"tool_call": map[string]any{
						"id":   chunk.ToolCall.ID,
						"name": chunk.ToolCall.Name,
					},
				}
				data, _ := json.Marshal(toolStartChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue

		case "tool_use_delta":
			// Informational only — tool is being executed by Claude CLI internally.
			if chunk.ToolCall != nil {
				toolDeltaChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call_delta",
					"tool_call": map[string]any{
						"arguments": string(chunk.ToolCall.Arguments),
					},
				}
				data, _ := json.Marshal(toolDeltaChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		}

		// Handle tool result — no OpenAI standard for streaming tool results,
		// so this remains a CogOS extension. We include a choices entry with the
		// result content so SDKs don't silently drop the event.
		if chunk.ToolResult != nil {
			resultChunk := map[string]any{
				"id":      chunk.ID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": nil,
					},
					"finish_reason": nil,
				}},
				"event_type": "tool_result",
				"tool_result": map[string]any{
					"tool_call_id": chunk.ToolResult.ToolCallID,
					"content":      chunk.ToolResult.Content,
					"is_error":     chunk.ToolResult.IsError,
				},
			}
			data, _ := json.Marshal(resultChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if chunk.Content != "" {
			// Accumulate content for thread persistence
			accumulatedContent.WriteString(chunk.Content)

			// Content chunk
			openAIChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{
					{
						Index: 0,
						Delta: &ChatMessage{
							Role:    "assistant",
							Content: StringToRawContent(chunk.Content),
						},
					},
				},
			}
			data, _ := json.Marshal(openAIChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		if chunk.Done && chunk.Suspended {
			// CLI is suspended waiting for external tool results.
			// Emit tool_calls to the client, park the output channel, and return.
			log.Printf("[tool-bridge] Suspended stream: %d external tool calls, parking channel for session %s",
				len(chunk.ExternalToolCalls), sessionID)

			// Emit external tool_calls as OpenAI streaming deltas
			for i, tc := range chunk.ExternalToolCalls {
				toolCallStart := map[string]any{
					"id":      chunk.ID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{{
								"index": i,
								"id":    tc.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tc.Name,
									"arguments": string(tc.Arguments),
								},
							}},
						},
					}},
				}
				data, _ := json.Marshal(toolCallStart)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			// Emit finish chunk with tool_calls reason
			var usageInfo *UsageInfo
			if chunk.Usage != nil {
				usageInfo = &UsageInfo{
					PromptTokens:      chunk.Usage.InputTokens,
					CompletionTokens:  chunk.Usage.OutputTokens,
					TotalTokens:       chunk.Usage.InputTokens + chunk.Usage.OutputTokens,
					CacheReadTokens:   chunk.Usage.CacheReadTokens,
					CacheCreateTokens: chunk.Usage.CacheCreateTokens,
					CostUSD:           chunk.Usage.CostUSD,
				}
			}
			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: "tool_calls",
				}},
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Park the output channel in the tool bridge for resumption.
			// The harness goroutine is still running (CLI blocked on MCP bridge).
			// When the client sends a follow-up request with tool results,
			// handleToolBridgeContinuation will resume reading from this channel.
			// Calls were already eagerly registered via tool_use events above.
			normalExit.Store(true) // Prevent disconnect watcher from killing suspended process
			s.toolBridge.RegisterSession(sessionID, chunks, inferReq, nil)
			return
		}

		if chunk.Done {
			// Capture Claude CLI session ID from the Done chunk for --resume on next message
			if chunk.SessionInfo != nil && chunk.SessionInfo.ClaudeSessionID != "" {
				s.claudeSessionStoreMu.Lock()
				s.claudeSessionStore[sessionID] = chunk.SessionInfo.ClaudeSessionID
				s.claudeSessionStoreMu.Unlock()
				if s.contextEngine != nil {
					s.contextEngine.sessionMgr.RecordClaudeSession(sessionID, chunk.SessionInfo.ClaudeSessionID)
				}
				log.Printf("[session] Stored Claude CLI session %s for %s (from Done)", chunk.SessionInfo.ClaudeSessionID, sessionID)
			}

			// Persist accumulated assistant response to thread
			if accumulatedContent.Len() > 0 {
				if err := s.appendToThread("assistant", accumulatedContent.String(), sessionID); err != nil {
					log.Printf("[TAA] Failed to persist streamed assistant response: %v", err)
				}
				// Check if thread needs summarization
				s.checkSummarization(sessionID)
			}

			// Lifecycle: record turn and update working memory
			claudeSessionForLC := ""
			if chunk.SessionInfo != nil {
				claudeSessionForLC = chunk.SessionInfo.ClaudeSessionID
			}
			s.lifecycle.RecordTurn(sessionID, claudeSessionForLC)
			if accumulatedContent.Len() > 0 {
				if err := UpdateWorkingMemory(s.kernel.Root(), sessionID, accumulatedContent.String()); err != nil {
					log.Printf("[lifecycle] Failed to update working memory: %v", err)
				}
			}

			// Build usage info if available
			var usageInfo *UsageInfo
			if chunk.Usage != nil {
				usageInfo = &UsageInfo{
					PromptTokens:      chunk.Usage.InputTokens,
					CompletionTokens:  chunk.Usage.OutputTokens,
					TotalTokens:       chunk.Usage.InputTokens + chunk.Usage.OutputTokens,
					CacheReadTokens:   chunk.Usage.CacheReadTokens,
					CacheCreateTokens: chunk.Usage.CacheCreateTokens,
					CostUSD:           chunk.Usage.CostUSD,
				}
			}

			// Emit external tool_calls as proper OpenAI streaming deltas.
			// Per the spec, tool_calls are emitted as delta chunks BEFORE
			// the finish_reason chunk.
			if len(chunk.ExternalToolCalls) > 0 {
				for i, tc := range chunk.ExternalToolCalls {
					// First chunk: tool call header (id, type, function name)
					toolCallStart := map[string]any{
						"id":      chunk.ID,
						"object":  "chat.completion.chunk",
						"created": created,
						"model":   model,
						"choices": []map[string]any{{
							"index": 0,
							"delta": map[string]any{
								"tool_calls": []map[string]any{{
									"index": i,
									"id":    tc.ID,
									"type":  "function",
									"function": map[string]any{
										"name":      tc.Name,
										"arguments": string(tc.Arguments),
									},
								}},
							},
						}},
					}
					data, _ := json.Marshal(toolCallStart)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				}
			}

			// OpenAI streaming spec: finish_reason chunk first, then
			// usage-only chunk, then [DONE]. Include usage on the
			// finish_reason chunk too — some SDKs read it there.
			finishReason := chunk.FinishReason
			if finishReason == "" {
				finishReason = "stop"
			}
			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{
					{
						Index:        0,
						Delta:        &ChatMessage{},
						FinishReason: finishReason,
					},
				},
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			// Dedicated usage-only chunk (standard OpenAI stream_options
			// include_usage format). Emitted after finish_reason, before
			// [DONE], with empty choices array.
			if usageInfo != nil {
				usageChunk := &StreamChunk{
					ID:      chunk.ID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []ChatChoice{},
					Usage:   usageInfo,
				}
				data, _ = json.Marshal(usageChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Emit bus chat.response event
			if s.busChat != nil && bctx.BusID != "" && accumulatedContent.Len() > 0 {
				respOpts := ChatResponseOpts{
					BusID:        bctx.BusID,
					RequestSeq:   bctx.RequestSeq,
					Content:      accumulatedContent.String(),
					Model:        model,
					DurationMs:   time.Since(startTime).Milliseconds(),
					Stream:       true,
					RequestHash:  bctx.RequestHash,
					FinishReason: finishReason,
					TraceID:      bctx.TraceID,
					SpanID:       bctx.SpanID,
				}
				if chunk.Usage != nil {
					respOpts.PromptTokens = chunk.Usage.InputTokens
					respOpts.CompletionTokens = chunk.Usage.OutputTokens
					respOpts.TokensUsed = chunk.Usage.InputTokens + chunk.Usage.OutputTokens
					respOpts.CacheReadTokens = chunk.Usage.CacheReadTokens
					respOpts.CacheCreateTokens = chunk.Usage.CacheCreateTokens
					respOpts.CostUSD = chunk.Usage.CostUSD
				}
				// TAA context metrics
				if inferReq.ContextState != nil {
					respOpts.TAATokens = inferReq.ContextState.TotalTokens
					respOpts.TAACoherence = inferReq.ContextState.CoherenceScore
				}
				s.busChat.emitResponse(respOpts)
			}

			// Clean up pre-created session if no suspension happened
			if hasExternalTools && s.toolBridge != nil {
				s.toolBridge.CleanupSession(sessionID)
			}

			normalExit.Store(true)
		}
	}
}

func (s *serveServer) handleNonStreamingResponse(w http.ResponseWriter, inferReq *InferenceRequest, sessionID string, bctx busEventCtx, startTime time.Time) {
	// Store TAA context for /v1/taa endpoint
	// Deep copy to prevent data races — the original may be mutated concurrently.
	if inferReq.ContextState != nil {
		s.taaStateMutex.Lock()
		s.lastTAAState = deepCopyContextState(inferReq.ContextState)
		s.taaStateMutex.Unlock()
	}

	// Delegate to harness inference engine
	resp, err := HarnessRunInference(inferReq, GlobalRegistry)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Inference failed: "+err.Error(), "server_error")
		if s.busChat != nil && bctx.BusID != "" {
			s.busChat.emitError(ChatErrorOpts{
				BusID:        bctx.BusID,
				RequestSeq:   bctx.RequestSeq,
				RequestHash:  bctx.RequestHash,
				ErrorMessage: err.Error(),
				ErrorType:    "inference_error",
				DurationMs:   time.Since(startTime).Milliseconds(),
				Model:        inferReq.Model,
				TraceID:      bctx.TraceID,
				SpanID:       bctx.SpanID,
			})
		}
		return
	}

	// Store Claude CLI session ID for --resume on next message
	if resp.ClaudeSessionID != "" {
		s.claudeSessionStoreMu.Lock()
		s.claudeSessionStore[sessionID] = resp.ClaudeSessionID
		s.claudeSessionStoreMu.Unlock()
		if s.contextEngine != nil {
			s.contextEngine.sessionMgr.RecordClaudeSession(sessionID, resp.ClaudeSessionID)
		}
		log.Printf("[session] Stored Claude CLI session %s for %s", resp.ClaudeSessionID, sessionID)
	}

	// Persist assistant response to thread (substrate-based memory)
	if err := s.appendToThread("assistant", resp.Content, sessionID); err != nil {
		log.Printf("[TAA] Failed to persist assistant response: %v", err)
	}

	// Check if thread needs summarization
	s.checkSummarization(sessionID)

	// Lifecycle: record turn and update working memory
	s.lifecycle.RecordTurn(sessionID, resp.ClaudeSessionID)
	if resp.Content != "" {
		if err := UpdateWorkingMemory(s.kernel.Root(), sessionID, resp.Content); err != nil {
			log.Printf("[lifecycle] Failed to update working memory: %v", err)
		}
	}

	model := inferReq.Model
	if model == "" {
		model = "claude"
	}

	usage := UsageInfo{
		PromptTokens:      resp.PromptTokens,
		CompletionTokens:  resp.CompletionTokens,
		TotalTokens:       resp.PromptTokens + resp.CompletionTokens,
		CacheReadTokens:   resp.CacheReadTokens,
		CacheCreateTokens: resp.CacheCreateTokens,
		CostUSD:           resp.CostUSD,
	}

	finishReason := resp.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	// Build the assistant message
	assistantMsg := &ChatMessage{
		Role:    "assistant",
		Content: StringToRawContent(resp.Content),
	}

	// Include tool_calls in the response if the model called external tools
	if len(resp.ToolCalls) > 0 {
		var toolCalls []map[string]any
		for _, tc := range resp.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(tc.Arguments),
				},
			})
		}
		tcJSON, _ := json.Marshal(toolCalls)
		assistantMsg.ToolCalls = json.RawMessage(tcJSON)
	}

	// Build response
	response := ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      assistantMsg,
				FinishReason: finishReason,
			},
		},
		Usage: &usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	// Emit bus chat.response event
	if s.busChat != nil && bctx.BusID != "" && resp.Content != "" {
		respOpts := ChatResponseOpts{
			BusID:             bctx.BusID,
			RequestSeq:        bctx.RequestSeq,
			Content:           resp.Content,
			Model:             model,
			DurationMs:        time.Since(startTime).Milliseconds(),
			PromptTokens:      resp.PromptTokens,
			CompletionTokens:  resp.CompletionTokens,
			TokensUsed:        resp.PromptTokens + resp.CompletionTokens,
			CacheReadTokens:   resp.CacheReadTokens,
			CacheCreateTokens: resp.CacheCreateTokens,
			CostUSD:           resp.CostUSD,
			FinishReason:      finishReason,
			RequestHash:       bctx.RequestHash,
			TraceID:           bctx.TraceID,
			SpanID:            bctx.SpanID,
		}
		// TAA context metrics
		if inferReq.ContextState != nil {
			respOpts.TAATokens = inferReq.ContextState.TotalTokens
			respOpts.TAACoherence = inferReq.ContextState.CoherenceScore
		}
		s.busChat.emitResponse(respOpts)
	}
}

// === TOOL BRIDGE HANDLERS ===

// hasToolMessages checks if any messages in the request have role:"tool".
func (s *serveServer) hasToolMessages(messages []ChatMessage) bool {
	for _, msg := range messages {
		if msg.Role == "tool" {
			return true
		}
	}
	return false
}

// handleToolBridgePending handles POST /v1/tool-bridge/pending.
// Called by the MCP bridge subprocess when a passthrough tool is invoked.
// Blocks until the client delivers the real tool result via a follow-up request.
func (s *serveServer) handleToolBridgePending(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		ToolName  string `json:"tool_name"`
		Arguments string `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("[tool-bridge] MCP bridge arrived: session=%s tool=%s", req.SessionID, req.ToolName)

	// Try immediate match, or register as waiter and block until call arrives.
	// Claude CLI invokes MCP tools eagerly (before content_block_stop), so the
	// MCP bridge often arrives before the harness registers the call.
	call, waiterCh := s.toolBridge.WaitForPending(req.SessionID, req.ToolName)
	if call == nil && waiterCh == nil {
		log.Printf("[tool-bridge] No session for MCP bridge: session=%s tool=%s", req.SessionID, req.ToolName)
		http.Error(w, "No session", http.StatusNotFound)
		return
	}

	if call == nil {
		// Block until RegisterCall wakes us or timeout
		select {
		case c, ok := <-waiterCh:
			if !ok || c == nil {
				log.Printf("[tool-bridge] Waiter cancelled: session=%s tool=%s", req.SessionID, req.ToolName)
				http.Error(w, "Session cancelled", http.StatusGone)
				return
			}
			call = c
		case <-time.After(2 * time.Minute):
			log.Printf("[tool-bridge] Waiter timeout: session=%s tool=%s", req.SessionID, req.ToolName)
			http.Error(w, "Timeout waiting for call registration", http.StatusGatewayTimeout)
			return
		case <-r.Context().Done():
			log.Printf("[tool-bridge] MCP bridge disconnected: session=%s tool=%s", req.SessionID, req.ToolName)
			return
		}
	}

	log.Printf("[tool-bridge] Blocking on result for: session=%s tool=%s id=%s", req.SessionID, req.ToolName, call.ToolCallID)

	// Block until the client delivers the result (or timeout)
	select {
	case result := <-call.ResultCh:
		log.Printf("[tool-bridge] Result delivered: session=%s tool=%s id=%s (len=%d)",
			req.SessionID, req.ToolName, call.ToolCallID, len(result.Content))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	case <-time.After(10 * time.Minute):
		log.Printf("[tool-bridge] Timeout waiting for result: session=%s tool=%s", req.SessionID, req.ToolName)
		http.Error(w, "Timeout waiting for tool result", http.StatusGatewayTimeout)
	case <-r.Context().Done():
		log.Printf("[tool-bridge] Client disconnected: session=%s tool=%s", req.SessionID, req.ToolName)
	}
}

// handleToolBridgeContinuation handles a follow-up request with role:"tool" results.
// It delivers the results to the blocked MCP bridge and resumes streaming from
// the parked output channel.
func (s *serveServer) handleToolBridgeContinuation(w http.ResponseWriter, req *ChatCompletionRequest, sess *ToolBridgeSession, sessionID string, bctx busEventCtx, startTime time.Time, httpCtx context.Context) {
	// Set SSE headers for streaming response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Detect client disconnect and cancel the inference process.
	// The process was started with context.Background() so HTTP disconnects
	// don't automatically kill it — we must cancel explicitly via the registry.
	var normalExit atomic.Bool
	if sess.InferReq != nil {
		requestID := sess.InferReq.ID
		go func() {
			<-httpCtx.Done()
			if !normalExit.Load() {
				if GlobalRegistry.Cancel(requestID) {
					log.Printf("[tool-bridge] Client disconnected, cancelled request %s", requestID)
				}
			}
		}()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		normalExit.Store(true)
		s.writeError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	rc := http.NewResponseController(w)
	extendDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
	}

	// Deliver tool results to the pending calls.
	// Only deliver results for tool_call_ids that exist in this session's CallsByID.
	// BrowserOS sends the full conversation history, so most role:"tool" messages
	// are from previous turns and should be silently skipped.
	delivered := 0
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if sess.CallsByID[msg.ToolCallID] != nil {
				content := msg.GetContent()
				s.toolBridge.DeliverResult(sessionID, msg.ToolCallID, ToolBridgeResult{
					Content: content,
				})
				delivered++
			}
		}
	}
	log.Printf("[tool-bridge] Delivered %d tool results for session %s", delivered, sessionID)

	// Resume reading from the parked output channel.
	// The harness goroutine is still running and will produce new chunks
	// after the MCP bridge delivers the results to Claude CLI.
	model := req.Model
	if model == "" {
		model = "claude"
	}
	created := time.Now().Unix()
	var accumulatedContent strings.Builder

	for chunk := range sess.OutputCh {
		extendDeadline()

		if chunk.Error != nil {
			normalExit.Store(true)
			s.writeSSEError(w, flusher, "Inference error: "+chunk.Error.Error())
			s.toolBridge.CleanupSession(sessionID)
			return
		}

		// Handle rich event types (same as handleStreamingResponse)
		switch chunk.EventType {
		case "session_info", "session_start":
			if chunk.SessionInfo != nil {
				toolCount := 0
				if chunk.SessionInfo.Tools != nil {
					toolCount = len(chunk.SessionInfo.Tools)
				}
				sessionChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": chunk.EventType,
					"session": map[string]any{
						"session_id": chunk.SessionInfo.SessionID,
						"model":      chunk.SessionInfo.Model,
						"tool_count": toolCount,
					},
				}
				data, _ := json.Marshal(sessionChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		case "tool_use", "tool_use_start":
			if chunk.ToolCall != nil {
				// Eagerly register external tool calls for re-suspension rounds
				if s.toolBridge != nil && chunk.EventType == "tool_use" {
					const mcpPrefix = "mcp__cogos-bridge__"
					if strings.HasPrefix(chunk.ToolCall.Name, mcpPrefix) {
						origName := strings.TrimPrefix(chunk.ToolCall.Name, mcpPrefix)
						s.toolBridge.RegisterCall(sessionID, &ToolBridgeCall{
							ToolCallID: chunk.ToolCall.ID,
							Name:       origName,
							Arguments:  string(chunk.ToolCall.Arguments),
							ResultCh:   make(chan ToolBridgeResult, 1),
						})
					}
				}

				toolStartChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call",
					"tool_call": map[string]any{
						"id":   chunk.ToolCall.ID,
						"name": chunk.ToolCall.Name,
					},
				}
				data, _ := json.Marshal(toolStartChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		case "tool_use_delta":
			if chunk.ToolCall != nil {
				toolDeltaChunk := map[string]any{
					"id":         chunk.ID,
					"object":     "chat.completion.chunk",
					"created":    created,
					"model":      model,
					"choices":    []any{},
					"event_type": "tool_call_delta",
					"tool_call": map[string]any{
						"arguments": string(chunk.ToolCall.Arguments),
					},
				}
				data, _ := json.Marshal(toolDeltaChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			continue
		}

		// Tool result
		if chunk.ToolResult != nil {
			resultChunk := map[string]any{
				"id":      chunk.ID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": nil,
					},
					"finish_reason": nil,
				}},
				"event_type": "tool_result",
				"tool_result": map[string]any{
					"tool_call_id": chunk.ToolResult.ToolCallID,
					"content":      chunk.ToolResult.Content,
					"is_error":     chunk.ToolResult.IsError,
				},
			}
			data, _ := json.Marshal(resultChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Content
		if chunk.Content != "" {
			accumulatedContent.WriteString(chunk.Content)
			openAIChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index: 0,
					Delta: &ChatMessage{
						Role:    "assistant",
						Content: StringToRawContent(chunk.Content),
					},
				}},
			}
			data, _ := json.Marshal(openAIChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Suspended again — nested tool calls
		if chunk.Done && chunk.Suspended {
			log.Printf("[tool-bridge] Re-suspended: %d more external tool calls", len(chunk.ExternalToolCalls))
			for i, tc := range chunk.ExternalToolCalls {
				toolCallStart := map[string]any{
					"id":      chunk.ID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{{
								"index": i,
								"id":    tc.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      tc.Name,
									"arguments": string(tc.Arguments),
								},
							}},
						},
					}},
				}
				data, _ := json.Marshal(toolCallStart)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: "tool_calls",
				}},
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Calls were already eagerly registered via tool_use events above.
			normalExit.Store(true) // Prevent disconnect watcher from killing re-suspended process
			return
		}

		// Final Done
		if chunk.Done {
			var usageInfo *UsageInfo
			if chunk.Usage != nil {
				usageInfo = &UsageInfo{
					PromptTokens:      chunk.Usage.InputTokens,
					CompletionTokens:  chunk.Usage.OutputTokens,
					TotalTokens:       chunk.Usage.InputTokens + chunk.Usage.OutputTokens,
					CacheReadTokens:   chunk.Usage.CacheReadTokens,
					CacheCreateTokens: chunk.Usage.CacheCreateTokens,
					CostUSD:           chunk.Usage.CostUSD,
				}
			}

			finishReason := chunk.FinishReason
			if finishReason == "" {
				finishReason = "stop"
			}
			finishChunk := &StreamChunk{
				ID:      chunk.ID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChatChoice{{
					Index:        0,
					Delta:        &ChatMessage{},
					FinishReason: finishReason,
				}},
				Usage: usageInfo,
			}
			data, _ := json.Marshal(finishChunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

			if usageInfo != nil {
				usageChunk := &StreamChunk{
					ID:      chunk.ID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []ChatChoice{},
					Usage:   usageInfo,
				}
				data, _ = json.Marshal(usageChunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

			// Clean up the bridge session — CLI has exited
			normalExit.Store(true)
			s.toolBridge.CleanupSession(sessionID)
			return
		}
	}

	// Channel closed unexpectedly
	normalExit.Store(true)
	s.toolBridge.CleanupSession(sessionID)
}

// extractLastUserMessage returns only the last user message from a message array.
// Used in --resume mode where Claude already has prior conversation context.
func extractLastUserMessage(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].GetContent()
		}
	}
	return ""
}

// handleRequests handles GET /v1/requests - list in-flight requests
func (s *serveServer) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	// Get query parameter for filtering
	statusFilter := r.URL.Query().Get("status")

	var entries []RequestEntry
	if statusFilter == "running" {
		entries = GlobalRegistry.ListRunning()
	} else {
		entries = GlobalRegistry.List()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   entries,
		"count":  len(entries),
	})
}

// handleRequestByID handles GET/DELETE /v1/requests/:id
func (s *serveServer) handleRequestByID(w http.ResponseWriter, r *http.Request) {
	// Extract request ID from path
	path := strings.TrimPrefix(r.URL.Path, "/v1/requests/")
	requestID := strings.TrimSuffix(path, "/")

	if requestID == "" {
		s.writeError(w, http.StatusBadRequest, "Request ID required", "invalid_request")
		return
	}

	switch r.Method {
	case "GET":
		// Get specific request
		entry := GlobalRegistry.Get(requestID)
		if entry == nil {
			s.writeError(w, http.StatusNotFound, "Request not found", "not_found")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entry)

	case "DELETE":
		// Cancel request
		if GlobalRegistry.Cancel(requestID) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":        requestID,
				"cancelled": true,
			})
		} else {
			s.writeError(w, http.StatusNotFound, "Request not found or already completed", "not_found")
		}

	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
	}
}

func (s *serveServer) writeError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func (s *serveServer) writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	errResp := ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    "server_error",
		},
	}
	data, _ := json.Marshal(errResp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// emitTAAContext emits the TAA context state as an SSE event for debugging/visibility
// This allows clients like cogcode to display the constructed context tiers
func (s *serveServer) emitTAAContext(w http.ResponseWriter, flusher http.Flusher, ctx *ContextState, model string) {
	if ctx == nil {
		return
	}

	// Build tier breakdown
	tiers := make(map[string]any)

	if ctx.Tier1Identity != nil {
		tiers["tier1_identity"] = map[string]any{
			"tokens": ctx.Tier1Identity.Tokens,
			"source": ctx.Tier1Identity.Source,
		}
	}

	if ctx.Tier2Temporal != nil {
		tiers["tier2_temporal"] = map[string]any{
			"tokens": ctx.Tier2Temporal.Tokens,
			"source": ctx.Tier2Temporal.Source,
		}
	}

	if ctx.Tier3Present != nil {
		tiers["tier3_present"] = map[string]any{
			"tokens": ctx.Tier3Present.Tokens,
			"source": ctx.Tier3Present.Source,
		}
	}

	if ctx.Tier4Semantic != nil {
		tiers["tier4_semantic"] = map[string]any{
			"tokens": ctx.Tier4Semantic.Tokens,
			"source": ctx.Tier4Semantic.Source,
		}
	}

	// Build TAA context event
	taaEvent := map[string]any{
		"id":         fmt.Sprintf("taa-%d", time.Now().UnixNano()),
		"object":     "chat.completion.chunk",
		"created":    time.Now().Unix(),
		"model":      model,
		"choices":    []any{}, // Required for OpenAI SDK compatibility
		"event_type": "taa_context",
		"taa": map[string]any{
			"total_tokens":    ctx.TotalTokens,
			"coherence_score": ctx.CoherenceScore,
			"tiers":           tiers,
			"anchor":          ctx.Anchor,
			"goal":            ctx.Goal,
		},
	}

	data, _ := json.Marshal(taaEvent)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// === THREAD PERSISTENCE ===

// appendToThread appends a message to the conversation thread via SDK.
// This enables substrate-based memory: threads persist across sessions and devices.
func (s *serveServer) appendToThread(role, content, sessionID string) error {
	if s.kernel == nil {
		return nil // Thread persistence requires SDK
	}

	// Construct message
	msg := map[string]interface{}{
		"role":      role,
		"content":   content,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Determine thread URI
	threadURI := "cog://thread/current"
	if sessionID != "" {
		threadURI = "cog://thread/" + sessionID
	}

	// Append via SDK
	mutation := sdk.NewAppendMutation(msgBytes)
	return s.kernel.MutateContext(context.Background(), threadURI, mutation)
}

// checkSummarization checks if thread needs summarization.
// Logs a recommendation when thread exceeds 12 messages (6 conversational turns).
// Actual summarization is handled by async background tasks.
func (s *serveServer) checkSummarization(sessionID string) {
	if s.kernel == nil {
		return
	}

	// Load thread
	threadURI := "cog://thread/current"
	if sessionID != "" {
		threadURI = "cog://thread/" + sessionID
	}

	resource, err := s.kernel.ResolveContext(context.Background(), threadURI)
	if err != nil {
		return // No thread yet or error loading
	}

	// Parse thread data
	var thread struct {
		Messages []interface{} `json:"messages"`
	}
	if err := json.Unmarshal(resource.Content, &thread); err != nil {
		return
	}

	// Check if we need summarization (>12 messages = >6 turns)
	if len(thread.Messages) > 12 {
		log.Printf("[TAA] Thread %s has %d messages, summarization recommended", sessionID, len(thread.Messages))
		// TODO: Trigger async summarization task
		// This will be implemented by the Memory Integration Specialist
	}
}

