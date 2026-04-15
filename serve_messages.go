// serve_messages.go — Anthropic Messages API proxy endpoint
//
// Provides a transparent proxy at POST /v1/messages that accepts Anthropic
// Messages API format, optionally injects foveated context, forwards to
// the real Anthropic API, and streams the response back. This enables
// Claude Code to route through the kernel via ANTHROPIC_BASE_URL.
//
// Split from serve.go per the file-per-concern convention.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// anthropicUpstreamURL returns the upstream Anthropic API base URL.
// Configurable via ANTHROPIC_UPSTREAM_URL for testing.
func anthropicUpstreamURL() string {
	if u := os.Getenv("ANTHROPIC_UPSTREAM_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.anthropic.com"
}

// messagesRequest is a minimal parse of an Anthropic Messages API request.
// We only extract what we need for logging; the full body is forwarded as-is.
type messagesRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
	System   json.RawMessage   `json:"system,omitempty"`
	Tools    []json.RawMessage `json:"tools,omitempty"`
}

// messagesResponseMeta is a minimal parse of a non-streaming response for logging.
type messagesResponseMeta struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage     struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// handleMessages proxies Anthropic Messages API requests to the upstream API.
func (s *serveServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Read the request body
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	// Minimal parse for logging
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON in request body")
		return
	}

	log.Printf("[messages] proxy request: model=%s stream=%v messages=%d tools=%d",
		req.Model, req.Stream, len(req.Messages), len(req.Tools))

	// Emit bus event for request start (best-effort)
	s.emitMessagesBusEvent("messages.request", map[string]interface{}{
		"model":         req.Model,
		"stream":        req.Stream,
		"message_count": len(req.Messages),
		"tool_count":    len(req.Tools),
	})

	// Resolve API key: prefer request headers, fall back to env var
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "No API key provided")
		return
	}

	// Build upstream request
	upstreamURL := anthropicUpstreamURL() + "/v1/messages"
	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Failed to create upstream request")
		return
	}

	// Forward relevant headers
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("x-api-key", apiKey)
	if v := r.Header.Get("anthropic-version"); v != "" {
		upstreamReq.Header.Set("anthropic-version", v)
	}
	if v := r.Header.Get("anthropic-beta"); v != "" {
		upstreamReq.Header.Set("anthropic-beta", v)
	}

	// Execute upstream request
	client := &http.Client{
		// No timeout — streaming responses can be long-lived.
		// The upstream connection will be bounded by the client's context.
	}
	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		log.Printf("[messages] upstream error: %v", err)
		s.emitMessagesBusEvent("messages.error", map[string]interface{}{
			"error": err.Error(),
			"phase": "upstream_connect",
		})
		writeAnthropicError(w, http.StatusBadGateway, "api_error",
			fmt.Sprintf("Failed to connect to upstream: %v", err))
		return
	}
	defer upstreamResp.Body.Close()

	// If upstream returned an error, pass it through transparently
	if upstreamResp.StatusCode >= 400 {
		log.Printf("[messages] upstream returned %d", upstreamResp.StatusCode)
		for k, vv := range upstreamResp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(upstreamResp.StatusCode)
		io.Copy(w, upstreamResp.Body)
		return
	}

	if req.Stream {
		s.proxyStreamingResponse(w, upstreamResp, startTime)
	} else {
		s.proxyNonStreamingResponse(w, upstreamResp, startTime)
	}
}

// proxyStreamingResponse forwards SSE events from upstream to the client,
// flushing after each event for real-time streaming.
func (s *serveServer) proxyStreamingResponse(w http.ResponseWriter, upstream *http.Response, startTime time.Time) {
	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering if present
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[messages] ResponseWriter does not support Flush")
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Streaming not supported")
		return
	}

	scanner := bufio.NewScanner(upstream.Body)
	// Allow large SSE events (up to 1MB per line)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var toolUseCount int
	var outputTokens int

	for scanner.Scan() {
		line := scanner.Text()

		// Forward the line as-is
		fmt.Fprintf(w, "%s\n", line)

		// SSE events are separated by blank lines — flush on blank
		if line == "" {
			flusher.Flush()
			continue
		}

		// Light parsing of SSE data lines for logging
		if strings.HasPrefix(line, "data: ") {
			data := line[6:]
			s.parseStreamEvent(data, &toolUseCount, &outputTokens)
		}
	}

	// Final flush
	flusher.Flush()

	elapsed := time.Since(startTime)
	log.Printf("[messages] stream complete: %s, tool_calls=%d, output_tokens=%d",
		elapsed.Round(time.Millisecond), toolUseCount, outputTokens)

	s.emitMessagesBusEvent("messages.completion", map[string]interface{}{
		"stream":        true,
		"duration_ms":   elapsed.Milliseconds(),
		"tool_calls":    toolUseCount,
		"output_tokens": outputTokens,
	})
}

// parseStreamEvent does lightweight parsing of SSE data payloads to count
// tool_use blocks and track token usage. It does not allocate full structures.
func (s *serveServer) parseStreamEvent(data string, toolUseCount *int, outputTokens *int) {
	// Quick type detection without full unmarshal
	var envelope struct {
		Type  string `json:"type"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
		ContentBlock *struct {
			Type string `json:"type"`
		} `json:"content_block,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return
	}

	switch envelope.Type {
	case "content_block_start":
		if envelope.ContentBlock != nil && envelope.ContentBlock.Type == "tool_use" {
			*toolUseCount++
			s.emitMessagesBusEvent("messages.tool_use", map[string]interface{}{
				"count": *toolUseCount,
			})
		}
	case "message_delta":
		if envelope.Usage != nil {
			*outputTokens = envelope.Usage.OutputTokens
		}
	}
}

// proxyNonStreamingResponse reads the full upstream response, logs it, and returns it.
func (s *serveServer) proxyNonStreamingResponse(w http.ResponseWriter, upstream *http.Response, startTime time.Time) {
	respBody, err := io.ReadAll(upstream.Body)
	if err != nil {
		log.Printf("[messages] failed to read upstream response: %v", err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "Failed to read upstream response")
		return
	}

	// Parse for logging
	var meta messagesResponseMeta
	if err := json.Unmarshal(respBody, &meta); err == nil {
		elapsed := time.Since(startTime)
		log.Printf("[messages] response: id=%s model=%s stop=%s in=%d out=%d elapsed=%s",
			meta.ID, meta.Model, meta.StopReason,
			meta.Usage.InputTokens, meta.Usage.OutputTokens,
			elapsed.Round(time.Millisecond))

		s.emitMessagesBusEvent("messages.completion", map[string]interface{}{
			"stream":        false,
			"message_id":    meta.ID,
			"model":         meta.Model,
			"stop_reason":   meta.StopReason,
			"input_tokens":  meta.Usage.InputTokens,
			"output_tokens": meta.Usage.OutputTokens,
			"duration_ms":   elapsed.Milliseconds(),
		})
	}

	// Forward response headers and body
	for k, vv := range upstream.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstream.StatusCode)
	w.Write(respBody)
}

// writeAnthropicError writes an Anthropic-format error response.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// emitMessagesBusEvent emits a bus event for the messages proxy (best-effort).
// Uses the server's default busChat if available. Falls back to log.Printf.
func (s *serveServer) emitMessagesBusEvent(eventType string, payload map[string]interface{}) {
	if s.busChat == nil {
		return // No bus available, event already logged via log.Printf
	}
	const messagesBusID = "bus_messages_proxy"
	if _, err := s.busChat.manager.appendBusEvent(messagesBusID, eventType, "kernel:messages-proxy", payload); err != nil {
		log.Printf("[messages] bus emit error: %v", err)
	}
}
