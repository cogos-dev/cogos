package engine

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

// TestHandleAnthropicMessagesStreaming_SDKShape decodes the stream the way
// the Anthropic SDK (and pi-ai over it) does — dereferencing
// message.usage.input_tokens on message_start and usage on message_delta
// unconditionally. A missing usage object is a client CRASH
// ("Cannot read properties of undefined (reading 'input_tokens')"), which is
// exactly what dsh hit against the kernel. Shape is asserted structurally,
// not by substring.
func TestHandleAnthropicMessagesStreaming_SDKShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	stub := NewStubProvider("stub", "")
	stub.chunks = []string{"ok"}
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	events := map[string]map[string]any{}
	for _, block := range strings.Split(w.Body.String(), "\n\n") {
		var ev, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" || data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event %s: not JSON: %v", ev, err)
		}
		if _, seen := events[ev]; !seen {
			events[ev] = m
		}
	}

	// message_start: message.usage must exist with both integer fields.
	ms, ok := events["message_start"]
	if !ok {
		t.Fatal("no message_start event")
	}
	msg, _ := ms["message"].(map[string]any)
	usage, ok := msg["usage"].(map[string]any)
	if !ok {
		t.Fatalf("message_start.message.usage missing — SDK clients crash dereferencing it; message=%v", msg)
	}
	for _, k := range []string{"input_tokens", "output_tokens"} {
		if _, ok := usage[k].(float64); !ok {
			t.Errorf("message_start.message.usage.%s missing or non-numeric: %v", k, usage[k])
		}
	}
	for _, k := range []string{"stop_reason", "stop_sequence"} {
		if _, present := msg[k]; !present {
			t.Errorf("message_start.message.%s key absent; Anthropic sends it as null", k)
		}
	}

	// message_delta: usage must be an object with output_tokens.
	md, ok := events["message_delta"]
	if !ok {
		t.Fatal("no message_delta event")
	}
	du, ok := md["usage"].(map[string]any)
	if !ok {
		t.Fatalf("message_delta.usage missing: %v", md)
	}
	if _, ok := du["output_tokens"].(float64); !ok {
		t.Errorf("message_delta.usage.output_tokens missing or non-numeric: %v", du)
	}
	if _, ok := events["message_stop"]; !ok {
		t.Error("no message_stop event")
	}
}
