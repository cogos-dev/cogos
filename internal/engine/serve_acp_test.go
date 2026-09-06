package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The ACP transport must speak JSON-RPC correctly BEFORE any subprocess is
// involved: a client that cannot initialize or that gets a malformed error
// frame has no way to report why it failed. These assert the protocol edges
// that do not require spawning claude.
func acpDial(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	s := &Server{cfg: &Config{WorkspaceRoot: t.TempDir()}}
	srv := httptest.NewServer(http.HandlerFunc(s.handleACP))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return c, func() { c.CloseNow(); cancel(); srv.Close() }
}

func acpRT(t *testing.T, c *websocket.Conn, req string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad json %q: %v", data, err)
	}
	return m
}

func TestACP_InitializeAdvertisesOnlyWhatExists(t *testing.T) {
	c, done := acpDial(t)
	defer done()

	m := acpRT(t, c, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if m["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", m["jsonrpc"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	if res["protocolVersion"] == nil {
		t.Error("initialize must report protocolVersion")
	}
	// Claiming a capability the kernel lacks is worse than omitting it: the
	// client builds UI for a promise that never arrives.
	caps, _ := res["agentCapabilities"].(map[string]any)
	pc, _ := caps["promptCapabilities"].(map[string]any)
	if pc["image"] != false || pc["audio"] != false {
		t.Errorf("must not advertise unimplemented modalities: %v", pc)
	}
}

func TestACP_PromptWithoutSessionIsRefusedNotIgnored(t *testing.T) {
	c, done := acpDial(t)
	defer done()

	m := acpRT(t, c, `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"hi"}]}}`)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("prompt without a session must error, got %v", m)
	}
	if !strings.Contains(e["message"].(string), "no session") {
		t.Errorf("error should say why: %v", e["message"])
	}
	// The id must round-trip, or the client cannot match the failure to its call.
	if m["id"] == nil {
		t.Error("error frame dropped the request id")
	}
}

func TestACP_UnknownMethodAndBadJSONAreDistinct(t *testing.T) {
	c, done := acpDial(t)
	defer done()

	m := acpRT(t, c, `{"jsonrpc":"2.0","id":2,"method":"session/teleport"}`)
	e, _ := m["error"].(map[string]any)
	if e == nil || e["code"].(float64) != -32601 {
		t.Errorf("unknown method should be -32601, got %v", m["error"])
	}

	m = acpRT(t, c, `{not json`)
	e, _ = m["error"].(map[string]any)
	if e == nil || e["code"].(float64) != -32700 {
		t.Errorf("malformed frame should be -32700, got %v", m["error"])
	}
}
