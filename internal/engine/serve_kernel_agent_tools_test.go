// serve_kernel_agent_tools_test.go — coverage for the kernel-agent
// tool-registry auto-injection added in myrgic/cogos#89.
//
// The dashboard chat path forwards `{model, messages, stream}` with no
// `tools` array. Before #89 the kernel-agent route would advertise zero
// tools and the model would narrate tool calls in prose that never fire.
// These tests pin the new behavior:
//
//   - model="kernel-agent" + empty req.Tools → provider sees the kernel's
//     MCP tool registry on creq.Tools (cog_*, mod3_*, ... — at minimum
//     non-empty and including a known kernel tool).
//   - model="kernel-agent" + caller-supplied req.Tools → caller wins; the
//     auto-injection does NOT fire.
//   - model!="kernel-agent" → no auto-injection (no behavioural change for
//     other routes).
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// findToolDef returns the first ToolDefinition whose Name matches, or nil.
func findToolDef(defs []ToolDefinition, name string) *ToolDefinition {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}

// TestKernelAgentAutoInjectsMCPTools is the primary regression test for #89.
// A chat request with model="kernel-agent" and no client-supplied tools must
// reach the inference provider with creq.Tools populated from the kernel's
// MCP tool registry snapshot.
func TestKernelAgentAutoInjectsMCPTools(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer wired; auto-inject path cannot run")
	}
	if defs := srv.mcpServer.ToolDefinitions(); len(defs) == 0 {
		t.Fatalf("MCP tool snapshot is empty; expected at least one cog_* tool")
	}

	stub := NewStubProvider("ollama", "ack")
	router := NewSimpleRouter(RoutingConfig{Default: "ollama"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"kernel-agent","messages":[{"role":"user","content":"please read those files"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.lastRequest == nil {
		t.Fatal("stub provider was not called")
	}
	if len(stub.lastRequest.Tools) == 0 {
		t.Fatal("creq.Tools is empty; kernel-agent auto-inject did not fire")
	}

	// The snapshot is built at MCPServer construction; assert that a known
	// kernel-tool name surfaced. cog_read_cogdoc is the canonical example
	// in #89's user-visible failure mode ("I will use cog_read_cogdoc...
	// turn ends"), so it's the most diagnostic name to pin.
	if findToolDef(stub.lastRequest.Tools, "cog_read_cogdoc") == nil {
		names := make([]string, 0, len(stub.lastRequest.Tools))
		for _, td := range stub.lastRequest.Tools {
			names = append(names, td.Name)
		}
		t.Errorf("creq.Tools missing cog_read_cogdoc; got %v", names)
	}

	// Every injected tool def must carry a JSON Schema object so the
	// provider's tool-call serializer doesn't choke.
	for _, td := range stub.lastRequest.Tools {
		if td.InputSchema == nil {
			t.Errorf("tool %q has nil InputSchema", td.Name)
			continue
		}
		typ, _ := td.InputSchema["type"].(string)
		if typ != "object" {
			t.Errorf("tool %q InputSchema.type = %q; want object", td.Name, typ)
		}
	}
}

// TestKernelAgentRespectsClientTools verifies the caller-wins guard: an
// explicit (even partial) req.Tools must not be overwritten by auto-inject.
func TestKernelAgentRespectsClientTools(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	stub := NewStubProvider("ollama", "ack")
	router := NewSimpleRouter(RoutingConfig{Default: "ollama"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	// Client supplies a single bespoke tool. The auto-injector must defer.
	clientReq := map[string]any{
		"model":  "kernel-agent",
		"stream": false,
		"messages": []map[string]any{
			{"role": "user", "content": "do a thing"},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "browser_click",
					"description": "click an element",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}
	body, err := json.Marshal(clientReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.lastRequest == nil {
		t.Fatal("stub provider was not called")
	}
	if len(stub.lastRequest.Tools) != 1 {
		names := make([]string, 0, len(stub.lastRequest.Tools))
		for _, td := range stub.lastRequest.Tools {
			names = append(names, td.Name)
		}
		t.Fatalf("creq.Tools = %v; want exactly [browser_click] (caller-wins)", names)
	}
	if stub.lastRequest.Tools[0].Name != "browser_click" {
		t.Errorf("creq.Tools[0].Name = %q; want browser_click", stub.lastRequest.Tools[0].Name)
	}
}

// TestNonKernelAgentNoInjection verifies that requests with model!="kernel-agent"
// are not affected by the new auto-inject path.
func TestNonKernelAgentNoInjection(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	stub := NewStubProvider("stub", "hi")
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(stub)
	srv.SetRouter(router)

	body := `{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.lastRequest == nil {
		t.Fatal("stub provider was not called")
	}
	if len(stub.lastRequest.Tools) != 0 {
		t.Errorf("creq.Tools = %v; want empty (model=local must not auto-inject)",
			stub.lastRequest.Tools)
	}
}
