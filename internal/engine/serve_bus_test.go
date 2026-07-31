// serve_bus_test.go — HTTP tests for the /v1/bus/* and /v1/sessions surface.
//
// Track 5 Phase 3: byte-compat regression tests against the response shapes
// captured from the live cogos-v3 daemon on :6931.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/pathsafe"
)

// TestBusSendAndReadEvents hits POST /v1/bus/send followed by GET
// /v1/bus/{bus_id}/events and verifies the shape matches the live daemon.
func TestBusSendAndReadEvents(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Send a message.
	body := `{"bus_id":"phase3-test","message":"shape-probe","from":"phase3-test"}`
	resp, err := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/bus/send: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("send status = %d", resp.StatusCode)
	}

	var sendResp struct {
		OK   bool   `json:"ok"`
		Seq  int    `json:"seq"`
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
		t.Fatalf("decode send resp: %v", err)
	}
	if !sendResp.OK {
		t.Errorf("ok = false")
	}
	if sendResp.Seq != 1 {
		t.Errorf("seq = %d, want 1", sendResp.Seq)
	}
	if len(sendResp.Hash) != 64 {
		t.Errorf("hash wrong len: %q", sendResp.Hash)
	}

	// Read it back.
	eventsResp, err := http.Get(srv.URL + "/v1/bus/phase3-test/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != 200 {
		t.Fatalf("events status = %d", eventsResp.StatusCode)
	}

	raw, _ := io.ReadAll(eventsResp.Body)
	// Must be a JSON array.
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		t.Fatalf("events response not a JSON array: %s", raw)
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	// Byte-compat: every field that cog-sandbox-mcp's bridge consumes
	// must be present with the correct value + type.
	// Reference fixture: /tmp/phase3-fixture-after-send.json.
	wantFields := map[string]interface{}{
		"v":      float64(2),
		"bus_id": "phase3-test",
		"seq":    float64(1),
		"from":   "phase3-test",
		"type":   "message",
	}
	for k, wantv := range wantFields {
		got, ok := events[0][k]
		if !ok {
			t.Errorf("missing field %q", k)
			continue
		}
		if got != wantv {
			t.Errorf("field %q = %v, want %v", k, got, wantv)
		}
	}
	// Ts is RFC3339-style ending in Z (or offset).
	if ts, ok := events[0]["ts"].(string); !ok || ts == "" {
		t.Errorf("ts not a non-empty string: %v", events[0]["ts"])
	}
	// Hash is lowercase hex, 64 chars.
	if h, ok := events[0]["hash"].(string); !ok || len(h) != 64 {
		t.Errorf("hash malformed: %v", events[0]["hash"])
	}
	// Payload is an object with "content".
	pl, ok := events[0]["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload not an object: %v", events[0]["payload"])
	}
	if pl["content"] != "shape-probe" {
		t.Errorf("payload.content = %v, want 'shape-probe'", pl["content"])
	}
}

// TestBusOpenAndList hits POST /v1/bus/open then GET /v1/bus/list and checks
// the registry shape matches root's.
func TestBusOpenAndList(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := `{"bus_id":"open-test","participants":["alice","bob"]}`
	openResp, err := http.Post(srv.URL+"/v1/bus/open", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST open: %v", err)
	}
	defer openResp.Body.Close()
	if openResp.StatusCode != 200 {
		t.Fatalf("open status = %d", openResp.StatusCode)
	}

	var openBody map[string]interface{}
	_ = json.NewDecoder(openResp.Body).Decode(&openBody)
	if openBody["ok"] != true || openBody["bus_id"] != "open-test" || openBody["state"] != "active" {
		t.Errorf("open response shape wrong: %+v", openBody)
	}

	// Now list.
	listResp, err := http.Get(srv.URL + "/v1/bus/list")
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	defer listResp.Body.Close()
	var entries []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(entries) < 1 {
		t.Fatalf("list empty")
	}
	found := false
	for _, e := range entries {
		if e["bus_id"] == "open-test" {
			found = true
			// Shape fields captured from /tmp/phase3-fixture-list.json.
			wantFields := []string{"state", "participants", "transport", "endpoint", "created_at", "last_event_seq", "last_event_at", "event_count"}
			for _, f := range wantFields {
				if _, ok := e[f]; !ok {
					t.Errorf("list entry missing field %q", f)
				}
			}
			break
		}
	}
	if !found {
		t.Errorf("open-test not in list: %+v", entries)
	}
}

// TestBusStats exercises GET /v1/bus/{bus_id}/stats — fixture:
// /tmp/phase3-fixture-stats.json.
func TestBusStats(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Seed a few events.
	for i, msg := range []string{"a", "b", "c"} {
		body := fmt.Sprintf(`{"bus_id":"stats-test","message":%q,"from":"u%d","type":"m%d"}`, msg, i%2, i%2)
		if _, err := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	resp, err := http.Get(srv.URL + "/v1/bus/stats-test/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	var stats map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&stats)

	if stats["bus_id"] != "stats-test" {
		t.Errorf("bus_id wrong")
	}
	if stats["event_count"].(float64) != 3 {
		t.Errorf("event_count = %v, want 3", stats["event_count"])
	}
	if _, ok := stats["first_event_at"].(string); !ok {
		t.Errorf("first_event_at missing or wrong type")
	}
	if _, ok := stats["last_event_at"].(string); !ok {
		t.Errorf("last_event_at missing")
	}
	types, ok := stats["types"].(map[string]interface{})
	if !ok {
		t.Fatalf("types not object: %v", stats["types"])
	}
	if types["m0"].(float64)+types["m1"].(float64) != 3 {
		t.Errorf("types aggregate wrong: %v", types)
	}
	senders, ok := stats["senders"].(map[string]interface{})
	if !ok {
		t.Fatalf("senders not object")
	}
	if senders["u0"].(float64)+senders["u1"].(float64) != 3 {
		t.Errorf("senders aggregate wrong: %v", senders)
	}
}

// TestBusEventBySeq exercises GET /v1/bus/{id}/events/{seq}.
func TestBusEventBySeq(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"bus_id":"seq-test","message":"m%d","from":"x"}`, i)
		_, _ = http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
	}

	// Valid seq.
	resp, err := http.Get(srv.URL + "/v1/bus/seq-test/events/2")
	if err != nil {
		t.Fatalf("GET events/2: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var evt map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&evt)
	if evt["seq"].(float64) != 2 {
		t.Errorf("seq = %v, want 2", evt["seq"])
	}

	// Invalid seq → 400.
	resp2, _ := http.Get(srv.URL + "/v1/bus/seq-test/events/nope")
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("bad seq status = %d, want 400", resp2.StatusCode)
	}

	// Missing seq → 404.
	resp3, _ := http.Get(srv.URL + "/v1/bus/seq-test/events/99")
	resp3.Body.Close()
	if resp3.StatusCode != 404 {
		t.Errorf("missing seq status = %d, want 404", resp3.StatusCode)
	}
}

// TestBusEventsGlobal exercises cross-bus search (GET /v1/bus/events).
func TestBusEventsGlobal(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Register each bus first — byte-compat with root: the global
	// /v1/bus/events endpoint iterates the registry, so buses that were
	// only ever sent-to (never opened) do NOT appear. This mirrors root's
	// bus_api.go:handleBusEventsGlobal exactly.
	for _, bus := range []string{"g-a", "g-b", "g-c"} {
		openBody := fmt.Sprintf(`{"bus_id":%q}`, bus)
		_, _ = http.Post(srv.URL+"/v1/bus/open", "application/json", strings.NewReader(openBody))
		body := fmt.Sprintf(`{"bus_id":%q,"message":"hi","from":"x"}`, bus)
		_, _ = http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
	}

	resp, err := http.Get(srv.URL + "/v1/bus/events?limit=10")
	if err != nil {
		t.Fatalf("GET /v1/bus/events: %v", err)
	}
	defer resp.Body.Close()

	var events []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Each event must carry bus_id.
	seenBuses := map[string]bool{}
	for _, e := range events {
		bid, ok := e["bus_id"].(string)
		if !ok || bid == "" {
			t.Errorf("event missing bus_id: %+v", e)
			continue
		}
		seenBuses[bid] = true
	}
	for _, want := range []string{"g-a", "g-b", "g-c"} {
		if !seenBuses[want] {
			t.Errorf("global events missing bus %q", want)
		}
	}
}

// TestBusConsumers covers GET /v1/bus/consumers + DELETE /v1/bus/consumers/{id}.
func TestBusConsumers(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The empty-registry case should return the {"consumers": []} shape
	// (fixture: /tmp/phase3-fixture-consumers.json).
	resp, err := http.Get(srv.URL + "/v1/bus/consumers")
	if err != nil {
		t.Fatalf("GET consumers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["consumers"]; !ok {
		t.Errorf("missing 'consumers' key: %+v", body)
	}

	// DELETE unknown consumer → 404.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/bus/consumers/nope", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE consumer: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 404 {
		t.Errorf("delete unknown consumer status = %d, want 404", delResp.StatusCode)
	}
}

// TestBusSendValidation verifies that missing required fields yield 400.
func TestBusSendValidation(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cases := []struct {
		name string
		body string
		code int
	}{
		{"missing-busid", `{"message":"hi"}`, 400},
		{"missing-message", `{"bus_id":"x"}`, 400},
		{"bad-json", `{not json}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(tc.body))
			resp.Body.Close()
			if resp.StatusCode != tc.code {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.code)
			}
		})
	}
}

// TestBusEventsAfterSeq verifies that after_seq (the bridge's canonical
// pagination param) filters correctly. This is the hot path for
// cogos_bridge.py @ line 266.
func TestBusEventsAfterSeq(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"bus_id":"paging","message":"m%d","from":"x"}`, i)
		_, _ = http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
	}

	resp, err := http.Get(srv.URL + "/v1/bus/paging/events?after_seq=2&limit=10")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	var events []map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&events)
	if len(events) != 3 {
		t.Errorf("len(events) = %d, want 3 (seq 3,4,5 after seq>2)", len(events))
	}
	for _, e := range events {
		if e["seq"].(float64) <= 2 {
			t.Errorf("got event seq=%v which should be filtered out", e["seq"])
		}
	}
}

// TestSessionsListAndDetail exercises /v1/sessions + /v1/sessions/{id}.
//
// After fix #154, GET /v1/sessions routes to handleSessionPresence and returns
// the kernel-native session registry (registered via POST /v1/sessions/register).
// Individual TAA inference-context detail is still served via
// GET /v1/sessions/{id}[/context].
func TestSessionsListAndDetail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	nucleus := &Nucleus{Name: "test-sessions"}
	proc := NewProcess(cfg, nucleus)
	server := NewServer(cfg, nucleus, proc)
	t.Cleanup(func() { _ = proc.Broker().Close() })

	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	// Register a session via the kernel-native register endpoint so it lands
	// in sessionRegistry (the store that GET /v1/sessions now reads).
	regBody, _ := json.Marshal(map[string]any{
		"session_id": "sess-list-abc",
		"workspace":  "/tmp/test",
		"role":       "claude-code",
		"status":     "active",
	})
	regResp, err := http.Post(srv.URL+"/v1/sessions/register",
		"application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d, want 200", regResp.StatusCode)
	}

	// List — GET /v1/sessions now returns sessionRegistry data (fix #154).
	listResp, err := http.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatalf("GET sessions: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}

	var listBody struct {
		Count    int                      `json:"count"`
		Sessions []map[string]interface{} `json:"sessions"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listBody.Count != 1 {
		t.Errorf("count = %d, want 1", listBody.Count)
	}
	if len(listBody.Sessions) != 1 {
		t.Fatalf("sessions len = %d", len(listBody.Sessions))
	}
	// The registry shape carries session_id, workspace, role, active — not TAA fields.
	wantFields := []string{"session_id", "workspace", "role", "active"}
	for _, f := range wantFields {
		if _, ok := listBody.Sessions[0][f]; !ok {
			t.Errorf("list response missing %q", f)
		}
	}
	if got := listBody.Sessions[0]["session_id"]; got != "sess-list-abc" {
		t.Errorf("session_id = %v, want sess-list-abc", got)
	}

	// Individual TAA context detail is still available via /{id}/context.
	// Seed the TAA store for a separate session.
	server.sessions.Record(&SessionContextState{
		SessionID:     "sess-taa-xyz",
		Profile:       "agent_harness",
		TurnNumber:    1,
		IrisPressure:  0.25,
		LastRequestAt: time.Now(),
	})

	ctxResp, err := http.Get(srv.URL + "/v1/sessions/sess-taa-xyz/context")
	if err != nil {
		t.Fatalf("GET detail/context: %v", err)
	}
	defer ctxResp.Body.Close()
	if ctxResp.StatusCode != http.StatusOK {
		t.Fatalf("context alias status = %d", ctxResp.StatusCode)
	}
	var detail map[string]interface{}
	if err := json.NewDecoder(ctxResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["session_id"] != "sess-taa-xyz" {
		t.Errorf("session_id = %v", detail["session_id"])
	}

	// Missing TAA session → 404.
	missResp, _ := http.Get(srv.URL + "/v1/sessions/nope/context")
	missResp.Body.Close()
	if missResp.StatusCode != http.StatusNotFound {
		t.Errorf("missing session status = %d, want 404", missResp.StatusCode)
	}
}

// TestBusStreamBrokerPublishSubscribe verifies that AppendEvent fans out to
// a subscribed channel via BusEventBroker (library-level test).
func TestBusStreamBrokerPublishSubscribe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr := NewBusSessionManager(root)
	broker := NewBusEventBroker()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := make(chan *BusBlock, 4)
	if !broker.Subscribe("bus-s", ch, ctx, "consumer-1") {
		t.Fatal("Subscribe returned false")
	}

	// Wire the broker into the manager so AppendEvent publishes.
	mgr.AddEventHandler("bus-publisher", func(busID string, block *BusBlock) {
		broker.Publish(busID, block)
	})

	_, _ = mgr.AppendEvent("bus-s", "m", "x", map[string]interface{}{"v": 1})

	select {
	case evt := <-ch:
		if evt == nil {
			t.Fatal("got nil event")
		}
		if evt.Seq != 1 {
			t.Errorf("seq = %d", evt.Seq)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for broker publish")
	}

	// Unsubscribe cleans up.
	broker.Unsubscribe("bus-s", ch)
	if broker.SubscriberCount("bus-s") != 0 {
		t.Errorf("subscriber count = %d after unsubscribe, want 0", broker.SubscriberCount("bus-s"))
	}
}

// TestBusStreamSSEEndToEnd is the regression test for issue #51: bus events
// must reach GET /v1/bus/{bus_id}/stream as SSE frames. Before the fix,
// BusEventBroker had no HTTP route and every client using the stream endpoint
// would sit silent indefinitely.
func TestBusStreamSSEEndToEnd(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "sse-regression-bus"

	// Pre-create the bus so the stream handler sees it exists.
	openBody := `{"bus_id":"` + busID + `"}`
	openResp, err := http.Post(srv.URL+"/v1/bus/open", "application/json", strings.NewReader(openBody))
	if err != nil {
		t.Fatalf("POST /v1/bus/open: %v", err)
	}
	openResp.Body.Close()

	// Open the SSE stream. no_replay=1 so we only see live events.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/v1/bus/"+busID+"/stream?no_replay=1", nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/bus/%s/stream: %v", busID, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q; want text/event-stream", ct)
	}

	// Collect SSE envelopes in the background.
	type sseEnvelope struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	received := make(chan sseEnvelope, 8)
	go func() {
		defer close(received)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var env sseEnvelope
			if err := json.Unmarshal([]byte(line[6:]), &env); err != nil || env.Data == nil {
				continue
			}
			received <- env
		}
	}()

	// Give the subscriber a moment to register, then send two events.
	time.Sleep(50 * time.Millisecond)
	for _, msg := range []string{"hello-stream", "world-stream"} {
		body := `{"bus_id":"` + busID + `","from":"test","message":"` + msg + `"}`
		postResp, err := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/bus/send (%s): %v", msg, err)
		}
		postResp.Body.Close()
	}

	// Expect both events on the stream.
	var got []sseEnvelope
	deadline := time.After(3 * time.Second)
collect:
	for len(got) < 2 {
		select {
		case env, ok := <-received:
			if !ok {
				break collect
			}
			got = append(got, env)
		case <-deadline:
			t.Fatalf("timed out: received %d/2 events on /v1/bus/%s/stream (issue #51 regression)", len(got), busID)
		}
	}

	if len(got) < 2 {
		t.Fatalf("received %d events; want 2", len(got))
	}

	// Both envelopes must have a non-empty ID (hash) and non-nil data.
	for i, env := range got {
		if env.ID == "" {
			t.Errorf("got[%d].ID empty — hash not forwarded", i)
		}
		if env.Data == nil {
			t.Errorf("got[%d].Data nil — BusBlock not embedded", i)
		}
	}
}

// TestConsumerRegistryAckAndList covers the consumer cursor ack + list path
// — orthogonal to the HTTP surface but exercising the same code handleBus*
// handlers call.
func TestConsumerRegistryAckAndList(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reg := NewConsumerRegistry(root)

	// Unknown bus → error.
	if _, err := reg.Ack("nope", "c1", 1); err == nil {
		t.Error("Ack on unknown bus should error")
	}

	// GetOrCreate establishes the cursor.
	cur := reg.GetOrCreate("bus-c", "c1")
	if cur.LastAckedSeq != 0 {
		t.Errorf("initial LastAckedSeq = %d", cur.LastAckedSeq)
	}

	// Ack advances.
	ack, err := reg.Ack("bus-c", "c1", 5)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if ack.LastAckedSeq != 5 {
		t.Errorf("after ack(5): %d", ack.LastAckedSeq)
	}

	// Monotonic — older seq ignored.
	ack2, _ := reg.Ack("bus-c", "c1", 3)
	if ack2.LastAckedSeq != 5 {
		t.Errorf("after lower ack: %d, want 5", ack2.LastAckedSeq)
	}

	// List (all / by bus).
	all := reg.List("")
	if len(all) != 1 {
		t.Errorf("list(all): %d", len(all))
	}
	filtered := reg.List("bus-c")
	if len(filtered) != 1 {
		t.Errorf("list(bus-c): %d", len(filtered))
	}
	empty := reg.List("other-bus")
	if len(empty) != 0 {
		t.Errorf("list(other-bus): %d", len(empty))
	}

	// Remove.
	if !reg.Remove("c1") {
		t.Error("Remove should return true")
	}
	if reg.Remove("c1") {
		t.Error("Remove twice should return false")
	}
}

// TestConsumerRegistryPersistSanitizesColonBusID covers the sibling
// path-construction seam cog-review flagged alongside bus_id (myrgic/cogos
// PR #504 round 2, "unverified notes"): persistLocked builds
// {bus_id}.cursors.jsonl the same unsanitized way EventsPath used to build
// {bus_id}/events.jsonl. GetOrCreate/Ack aren't wired to any HTTP/MCP route
// today (only List/Remove are, and neither persists), so this guards against
// a future caller reintroducing the defect, mirroring the bus_session.go
// tests above.
func TestConsumerRegistryPersistSanitizesColonBusID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reg := NewConsumerRegistry(root)

	busID := "http:cog"
	reg.GetOrCreate(busID, "c1")
	if _, err := reg.Ack(busID, "c1", 1); err != nil {
		t.Fatalf("Ack(%q): %v", busID, err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cursor dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cursor dir has %d entries, want 1: %+v", len(entries), entries)
	}
	if strings.Contains(entries[0].Name(), ":") {
		t.Fatalf("on-disk cursor file %q still contains a colon", entries[0].Name())
	}
	want := pathsafe.SanitizeComponent(busID) + ".cursors.jsonl"
	if entries[0].Name() != want {
		t.Fatalf("cursor file = %q, want %q", entries[0].Name(), want)
	}

	// LoadFromDisk must be able to read the file back without error (the
	// busID it recovers from the filename is the sanitized form — a known,
	// documented asymmetry with the raw busID used as the live in-memory map
	// key, acceptable because this path is not currently reachable from any
	// wired-up route).
	reg2 := NewConsumerRegistry(root)
	if err := reg2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	sanitizedBusID := filepath.Base(want[:len(want)-len(".cursors.jsonl")])
	loaded := reg2.List(sanitizedBusID)
	if len(loaded) != 1 {
		t.Fatalf("List(%q) after LoadFromDisk = %d entries, want 1", sanitizedBusID, len(loaded))
	}
}

// TestBusStreamSinceReplayFromMid verifies that ?since=N replays only events
// with seq > N, not the full history.
func TestBusStreamSinceReplayFromMid(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "since-mid-bus"
	openBody := `{"bus_id":"` + busID + `"}`
	openResp, _ := http.Post(srv.URL+"/v1/bus/open", "application/json", strings.NewReader(openBody))
	openResp.Body.Close()

	// Append 5 events (seq 1..5).
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"bus_id":%q,"from":"test","message":"m%d"}`, busID, i)
		r, _ := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
		r.Body.Close()
	}

	// Connect with ?since=2 — expect replay of seq 3, 4, 5 only.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/v1/bus/"+busID+"/stream?since=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Collect replay frames (prefixed "replay_") until we've seen all expected
	// or the deadline fires.
	var replayed []float64
	scanner := bufio.NewScanner(resp.Body)
	timeout := time.After(3 * time.Second)
	lineCh := make(chan string, 32)
	go func() {
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
	}()
	for len(replayed) < 3 {
		select {
		case line, ok := <-lineCh:
			if !ok {
				goto done
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var env map[string]interface{}
			if err := json.Unmarshal([]byte(line[6:]), &env); err != nil {
				continue
			}
			id, _ := env["id"].(string)
			if !strings.HasPrefix(id, "replay_") {
				continue
			}
			data, ok := env["data"].(map[string]interface{})
			if !ok {
				continue
			}
			seq, _ := data["seq"].(float64)
			replayed = append(replayed, seq)
		case <-timeout:
			goto done
		}
	}
done:
	cancel()

	if len(replayed) != 3 {
		t.Fatalf("replayed %d events, want 3 (seq 3,4,5 after since=2)", len(replayed))
	}
	for _, seq := range replayed {
		if seq <= 2 {
			t.Errorf("replay included seq=%v which should be filtered by since=2", seq)
		}
	}
}

// TestBusStreamSinceReplayFromTip verifies that ?since=N where N equals the
// current tip skips replay entirely and proceeds directly to live.
func TestBusStreamSinceReplayFromTip(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "since-tip-bus"
	openResp, _ := http.Post(srv.URL+"/v1/bus/open", "application/json",
		strings.NewReader(`{"bus_id":"`+busID+`"}`))
	openResp.Body.Close()

	// Append 3 events (seq 1..3). Tip is seq 3.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"bus_id":%q,"from":"test","message":"m%d"}`, busID, i)
		r, _ := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
		r.Body.Close()
	}

	// ?since=3 (the tip) — no replay frames expected, only connected + live.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/v1/bus/"+busID+"/stream?since=3", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read the stream for a short window; no replay_ frames should appear.
	replayCount := 0
	liveReceived := make(chan struct{}, 1)
	lineCh := make(chan string, 32)
	scanner := bufio.NewScanner(resp.Body)
	go func() {
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
	}()

	// Send a live event after the connection is established.
	time.Sleep(50 * time.Millisecond)
	liveBody := fmt.Sprintf(`{"bus_id":%q,"from":"test","message":"live"}`, busID)
	lr, _ := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(liveBody))
	lr.Body.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				goto checkTip
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var env map[string]interface{}
			if err := json.Unmarshal([]byte(line[6:]), &env); err != nil {
				continue
			}
			id, _ := env["id"].(string)
			if strings.HasPrefix(id, "replay_") {
				replayCount++
			} else if id != "" {
				select {
				case liveReceived <- struct{}{}:
				default:
				}
				goto checkTip
			}
		case <-deadline:
			goto checkTip
		}
	}
checkTip:
	cancel()

	if replayCount != 0 {
		t.Errorf("replay frames = %d, want 0 (since=tip)", replayCount)
	}
	select {
	case <-liveReceived:
	default:
		t.Error("no live event received after since=tip connection")
	}
}

// TestBusStreamSincePastTip verifies that ?since=N where N exceeds the current
// tip returns 416 Range Not Satisfiable before upgrading to SSE.
func TestBusStreamSincePastTip(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "since-past-tip-bus"
	openResp, _ := http.Post(srv.URL+"/v1/bus/open", "application/json",
		strings.NewReader(`{"bus_id":"`+busID+`"}`))
	openResp.Body.Close()

	// Append 2 events (tip = seq 2).
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"bus_id":%q,"from":"test","message":"m%d"}`, busID, i)
		r, _ := http.Post(srv.URL+"/v1/bus/send", "application/json", strings.NewReader(body))
		r.Body.Close()
	}

	// ?since=99 (beyond tip) — expect 416.
	resp, err := http.Get(srv.URL + "/v1/bus/" + busID + "/stream?since=99")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}

	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["error"]; !ok {
		t.Errorf("response missing 'error' field: %+v", body)
	}
}

// TestBusStreamSinceEmptyBus is a regression test for issue #136: an empty bus
// with ?since=1 (or any positive value) must return 416 Range Not Satisfiable,
// not silently fall through to a 200 SSE stream. The empty bus has tip=0, so
// any since>0 is out of range per RFC 7233.
func TestBusStreamSinceEmptyBus(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "since-empty-bus"
	openResp, _ := http.Post(srv.URL+"/v1/bus/open", "application/json",
		strings.NewReader(`{"bus_id":"`+busID+`"}`))
	openResp.Body.Close()

	// Bus has no events (tip=0). since=1 must return 416.
	resp, err := http.Get(srv.URL + "/v1/bus/" + busID + "/stream?since=1")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416 (empty bus + since=1 must not fall through to 200)", resp.StatusCode)
	}

	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["error"]; !ok {
		t.Errorf("response missing 'error' field: %+v", body)
	}
}

// TestBusStreamSinceMutualExclusion verifies that passing both ?since=N and
// ?no_replay=1 returns 400 Bad Request.
func TestBusStreamSinceMutualExclusion(t *testing.T) {
	t.Parallel()
	handler, _, _ := newEventsTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	const busID = "since-mutex-bus"
	openResp, _ := http.Post(srv.URL+"/v1/bus/open", "application/json",
		strings.NewReader(`{"bus_id":"`+busID+`"}`))
	openResp.Body.Close()

	resp, err := http.Get(srv.URL + "/v1/bus/" + busID + "/stream?since=1&no_replay=1")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["error"]; !ok {
		t.Errorf("response missing 'error' field: %+v", body)
	}
}
