// serve_events_test.go — integration tests for the HTTP event bus surface.
//
// Tests spin up a real *Server via httptest.NewServer + Process (so the
// EventBroker wiring matches production). The SSE tests are bounded by a
// short timeout because the stream blocks indefinitely otherwise.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newEventsTestServer builds a real Server + Process pair rooted at a tmpdir and
// returns its Handler. Cleanup is registered via t.Cleanup.
func newEventsTestServer(t *testing.T) (http.Handler, *Process, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0, WriteRouteGrantAuthDisabled: true}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, proc)
	t.Cleanup(func() {
		_ = proc.Broker().Close()
	})
	return srv.Handler(), proc, root
}

// readSSEUntil reads lines from the SSE stream until N `event: ledger.appended`
// frames are seen, or timeout expires. Returns collected events as parsed
// LedgerEvent structs plus all raw headers encountered.
func readSSEUntil(t *testing.T, r io.Reader, n int, timeout time.Duration) ([]LedgerEvent, []string) {
	t.Helper()
	out := []LedgerEvent{}
	headers := []string{}
	done := make(chan struct{})
	var data string
	var lastID string
	var lastEvent string

	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				lastID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				lastEvent = strings.TrimPrefix(line, "event: ")
				headers = append(headers, lastEvent)
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if lastEvent == "ledger.appended" && data != "" {
					var e LedgerEvent
					if err := json.Unmarshal([]byte(data), &e); err != nil {
						continue
					}
					// Trust the stream's id: header as source of truth.
					if e.Hash == "" {
						e.Hash = lastID
					}
					out = append(out, e)
					if len(out) >= n {
						return
					}
				}
				data = ""
				lastEvent = ""
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(timeout):
	}
	return out, headers
}

func TestServeEventsHTTPHistorical(t *testing.T) {
	t.Parallel()
	handler, proc, root := newEventsTestServer(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sid := proc.SessionID()
	// Emit a couple of events through the real path.
	for i := 0; i < 3; i++ {
		if err := proc.EmitEvent(fmt.Sprintf("evt.%d", i), map[string]any{"i": i}, "kernel-v3"); err != nil {
			t.Fatalf("EmitEvent[%d]: %v", i, err)
		}
	}
	_ = root

	resp, err := http.Get(server.URL + "/v1/events?session_id=" + sid + "&limit=10")
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var result EventQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Count != 3 {
		t.Errorf("count=%d; want 3", result.Count)
	}
}

func TestServeEventsSSEEndToEnd(t *testing.T) {
	t.Parallel()
	handler, proc, _ := newEventsTestServer(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sid := proc.SessionID()

	// Open the SSE stream with a short max_duration so the test is bounded.
	req, err := http.NewRequestWithContext(context.Background(), "GET",
		server.URL+"/v1/events/stream?session_id="+sid+"&max_duration=3s&max_events=2", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type=%s; want text/event-stream", got)
	}

	// Publish 2 events a beat apart so the reader has time to subscribe.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = proc.EmitEvent("sse.one", nil, "kernel-v3")
		time.Sleep(50 * time.Millisecond)
		_ = proc.EmitEvent("sse.two", nil, "kernel-v3")
	}()

	got, headers := readSSEUntil(t, resp.Body, 2, 3*time.Second)

	// Expect `connected` frame + two `ledger.appended` frames.
	if len(got) < 2 {
		t.Fatalf("received %d events; want 2; headers=%v", len(got), headers)
	}
	if got[0].Type != "sse.one" || got[1].Type != "sse.two" {
		t.Errorf("event types=[%s,%s]; want [sse.one,sse.two]", got[0].Type, got[1].Type)
	}
	if got[0].Hash == "" {
		t.Errorf("first event missing id/hash")
	}
	haveConnected := false
	for _, h := range headers {
		if h == "connected" {
			haveConnected = true
		}
	}
	if !haveConnected {
		t.Errorf("no 'connected' event observed in SSE headers: %v", headers)
	}
}

func TestServeBusAckRemoved(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	resp, err := http.Post(server.URL+"/v1/bus/some-bus/ack", "application/json",
		strings.NewReader(`{"consumer_id":"x","seq":1}`))
	if err != nil {
		t.Fatalf("POST /v1/bus/.../ack: %v", err)
	}
	defer resp.Body.Close()
	// The old stub returned 200 with {"ok":true,"seq":N}. Removal means the
	// route is no longer registered — Go's mux maps to either 404 (no match)
	// or 405 (path collides with another method's pattern). Anything other
	// than a 2xx is fine; what matters is the stub is gone.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Errorf("status=%d; want non-2xx (handleBusAck stub removed)", resp.StatusCode)
	}
}

func TestEmitEventReachesLedgerAndBus(t *testing.T) {
	t.Parallel()
	handler, proc, root := newEventsTestServer(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sid := proc.SessionID()
	broker := proc.Broker()
	if broker == nil {
		t.Fatal("process broker nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Subscribe first so we see the live event even if the ring is warm.
	sub, err := broker.Subscribe(ctx, EventFilter{SessionID: sid})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Drive an MCP-style emit through the process method the tool uses.
	if err := proc.EmitEvent("insight.captured",
		map[string]any{"summary": "round-trip"}, "mcp-client"); err != nil {
		t.Fatalf("EmitEvent: %v", err)
	}

	// Broker should deliver the event.
	select {
	case env := <-sub.Events:
		if env.HashedPayload.Type != "insight.captured" {
			t.Errorf("broker got type=%s; want insight.captured", env.HashedPayload.Type)
		}
		if env.Metadata.Source != "mcp-client" {
			t.Errorf("broker got source=%s; want mcp-client", env.Metadata.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("broker did not deliver emitted event")
	}

	// Ledger should also have the event in the per-session dir (not orphan
	// file). QueryLedger confirms.
	res, err := QueryLedger(root, LedgerQuery{SessionID: sid, EventType: "insight.captured"})
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if res.Count != 1 {
		t.Errorf("ledger count=%d; want 1 (cogos#10 fix: event must reach per-session ledger)", res.Count)
	}
	// And crucially: NO flat orphan file. The old bug wrote to
	// .cog/ledger/events.jsonl — verify that's not there.
	orphan := root + "/.cog/ledger/events.jsonl"
	if _, err := readFileIfExists(orphan); err == nil {
		t.Errorf("orphan file %s exists — cogos#10 regression", orphan)
	}
}

// readFileIfExists is a thin helper used only for the orphan-file assertion
// above. Returns nil error if the file exists (so the test fails), error
// otherwise.
func readFileIfExists(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
