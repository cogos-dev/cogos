package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleAnthropicMessagesNonStreaming(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(NewStubProvider("stub", "hello world"))
	srv.SetRouter(router)

	body := `{"model":"claude","system":"kernel","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var resp anthropicMessagesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.ID, "msg_") {
		t.Fatalf("ID = %q; want msg_ prefix", resp.ID)
	}
	if resp.Type != "message" {
		t.Fatalf("Type = %q; want message", resp.Type)
	}
	if resp.Role != "assistant" {
		t.Fatalf("Role = %q; want assistant", resp.Role)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hello world" {
		t.Fatalf("Content = %+v; want single hello world block", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q; want end_turn", resp.StopReason)
	}
}

func TestHandleAnthropicMessagesStreaming(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	stub := NewStubProvider("stub", "")
	stub.chunks = []string{"hel", "lo", " world"}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; want text/event-stream", ct)
	}

	var (
		assembled strings.Builder
		lastEvent string
	)
	scanner := bufio.NewScanner(w.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			lastEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") || lastEvent != "content_block_delta" {
			continue
		}
		var payload struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("decode SSE payload: %v", err)
		}
		assembled.WriteString(payload.Delta.Text)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if assembled.String() != "hello world" {
		t.Fatalf("assembled = %q; want hello world", assembled.String())
	}
}
