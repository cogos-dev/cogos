// serve_a0_client_tool_refusal_test.go — A0 fixture for ledger row L06.
//
// One request shape, both gateway surfaces. The client names a registered
// kernel tool (cog_read_cogdoc) in its own tools[] array. The kernel must
// refuse the definition: it must not reach the provider as a callable, and
// the provider's tool_use for it must not be executed via MCPServer.CallTool.
//
// The row asserts this for /v1/messages. Probing before the fix showed the
// OpenAI twin at /v1/chat/completions was equally open — the guard at
// serve.go's kernel-agent injection site is gated behind
// `len(creq.Tools) == 0`, so it is unreachable whenever the client supplies
// tools. Both halves are therefore fixtured here and both fail on old code.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// a0KernelToolName is the MCP-registered kernel tool the fixture impersonates.
const a0KernelToolName = "cog_read_cogdoc"

// a0ScriptedProvider returns a provider that emits a tool_use for the kernel
// tool on its first turn and a plain assistant message afterwards. If the
// kernel executes the tool, the provider is called twice (hop + final); if the
// kernel refuses, the call is forwarded to the client and the provider is
// called exactly once.
func a0ScriptedProvider() *scriptedToolUseProvider {
	return newScriptedToolUseProvider("a0",
		&CompletionResponse{
			StopReason: "tool_use",
			ToolCalls: []ToolCall{{
				ID:        "a0_call",
				Name:      a0KernelToolName,
				Arguments: `{"uri":"cog://mem/semantic/a0.md"}`,
			}},
			ProviderMeta: ProviderMeta{Provider: "a0", Model: "a0"},
		},
		&CompletionResponse{
			Content:      "done",
			StopReason:   "end_turn",
			ProviderMeta: ProviderMeta{Provider: "a0", Model: "a0"},
		},
	)
}

// a0AssertRefused is the shared assertion both halves run against the
// provider's captured requests.
func a0AssertRefused(t *testing.T, surface string, prov *scriptedToolUseProvider) {
	t.Helper()

	if len(prov.requests) == 0 {
		t.Fatalf("%s: provider was never called", surface)
	}

	// (1) The refused definition must never reach the provider.
	for i, req := range prov.requests {
		for _, td := range req.Tools {
			if td.Name == a0KernelToolName {
				t.Errorf("%s: request[%d].Tools contains kernel-owned %q; "+
					"client-supplied kernel tool definitions must be refused",
					surface, i, a0KernelToolName)
			}
		}
		for _, td := range req.ExternalTools {
			if td.Name == a0KernelToolName {
				t.Errorf("%s: request[%d].ExternalTools contains kernel-owned %q",
					surface, i, a0KernelToolName)
			}
		}
	}

	// (2) The kernel must not have executed the tool. A second provider call
	//     means the hop loop ran CallTool and fed a tool_result back in.
	if len(prov.requests) > 1 {
		t.Errorf("%s: provider called %d times; want 1. A second call means the "+
			"kernel executed the client-named kernel tool via CallTool",
			surface, len(prov.requests))
	}

	// (3) No tool_result for the refused call may appear in any transcript.
	for i, req := range prov.requests {
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == "a0_call" {
				t.Errorf("%s: request[%d] carries a tool_result for the refused call; "+
					"content=%q", surface, i, m.Content)
			}
		}
	}
}

// TestA0ClientSuppliedKernelToolRefused_OpenAISurface is the
// /v1/chat/completions half of the A0 fixture.
func TestA0ClientSuppliedKernelToolRefused_OpenAISurface(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil || !srv.mcpServer.IsInternalTool(a0KernelToolName) {
		t.Fatalf("test server lacks an mcpServer registering %q", a0KernelToolName)
	}

	prov := a0ScriptedProvider()
	router := NewSimpleRouter(RoutingConfig{Default: "a0"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"local","stream":false,` +
		`"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"type":"function","function":{"name":"` + a0KernelToolName + `",` +
		`"description":"client-supplied impostor",` +
		`"parameters":{"type":"object","properties":{"uri":{"type":"string"}}}}}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	a0AssertRefused(t, "openai", prov)
}

// TestA0ClientSuppliedKernelToolRefused_AnthropicSurface is the /v1/messages
// half — the surface ledger row L06 names.
func TestA0ClientSuppliedKernelToolRefused_AnthropicSurface(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil || !srv.mcpServer.IsInternalTool(a0KernelToolName) {
		t.Fatalf("test server lacks an mcpServer registering %q", a0KernelToolName)
	}

	prov := a0ScriptedProvider()
	router := NewSimpleRouter(RoutingConfig{Default: "a0"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"local","stream":false,"max_tokens":256,` +
		`"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"name":"` + a0KernelToolName + `",` +
		`"description":"client-supplied impostor",` +
		`"input_schema":{"type":"object","properties":{"uri":{"type":"string"}}}}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	a0AssertRefused(t, "anthropic", prov)
}

// TestA0AdmitClientSuppliedTools_UnitTable pins the helper's contract
// directly: kernel-owned names are rejected, CLI-builtin names are admitted
// to Tools but withheld from ExternalTools, and ordinary client tools reach
// both pools. A nil MCPServer admits everything (legacy/test path).
func TestA0AdmitClientSuppliedTools_UnitTable(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil {
		t.Fatal("test server has no mcpServer")
	}

	defs := []ToolDefinition{
		{Name: a0KernelToolName},
		{Name: "bash"},
		{Name: "browser_click"},
	}

	tools, external, rejected := admitClientSuppliedTools(defs, srv.mcpServer)

	gotTools := a0ToolNames(tools)
	if strings.Join(gotTools, ",") != "bash,browser_click" {
		t.Errorf("tools = %v; want [bash browser_click]", gotTools)
	}
	gotExternal := a0ToolNames(external)
	if strings.Join(gotExternal, ",") != "browser_click" {
		t.Errorf("external = %v; want [browser_click]", gotExternal)
	}
	if len(rejected) != 1 || rejected[0] != a0KernelToolName {
		t.Errorf("rejected = %v; want [%s]", rejected, a0KernelToolName)
	}

	// Nil MCPServer: nothing is refused.
	tools2, _, rejected2 := admitClientSuppliedTools(defs, nil)
	if len(rejected2) != 0 {
		t.Errorf("nil MCPServer rejected %v; want none", rejected2)
	}
	if len(tools2) != 3 {
		t.Errorf("nil MCPServer admitted %d tools; want 3", len(tools2))
	}

	// Empty input is a no-op.
	if tools3, ext3, rej3 := admitClientSuppliedTools(nil, srv.mcpServer); tools3 != nil || ext3 != nil || rej3 != nil {
		t.Errorf("empty input returned (%v,%v,%v); want all nil", tools3, ext3, rej3)
	}
}

func a0ToolNames(defs []ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// a0Unused keeps encoding/json imported if future assertions need it without
// churn; referenced here so the compiler is satisfied.
var _ = json.Marshal

// TestA0RefusedClientTool_ThenKernelInjection_StillExecutes — review finding
// on #606. A client on a kernel-tools model ("kernel-agent") names a kernel
// tool in its request. The definition is refused as client-supplied; that
// leaves creq.Tools empty, so injectKernelAgentTools legitimately offers the
// kernel's OWN definition of the same tool. The refusal set must not outlive
// the intake it belongs to: the model calls the injected tool, and the kernel
// must EXECUTE it (a second provider call carrying the tool_result), not drop
// the call as "refused". Negative control: on the pre-fix code the refusal
// set persists, the call is dropped, and the provider is called once.
func TestA0RefusedClientTool_ThenKernelInjection_StillExecutes(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	if srv.mcpServer == nil || !srv.mcpServer.IsInternalTool(a0KernelToolName) {
		t.Fatalf("test server lacks an mcpServer registering %q", a0KernelToolName)
	}
	prov := a0ScriptedProvider()
	router := NewSimpleRouter(RoutingConfig{Default: "a0"})
	router.RegisterProvider(prov)
	srv.SetRouter(router)

	body := `{"model":"kernel-agent","stream":false,` +
		`"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"type":"function","function":{"name":"` + a0KernelToolName + `",` +
		`"description":"client-supplied impostor",` +
		`"parameters":{"type":"object","properties":{"uri":{"type":"string"}}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}

	// The kernel's own definition must have reached the provider (injection ran).
	offered := false
	for _, td := range prov.requests[0].Tools {
		if td.Name == a0KernelToolName {
			offered = true
		}
	}
	if !offered {
		t.Fatalf("kernel injection did not offer %q on a kernel-tools model; request[0].Tools=%d", a0KernelToolName, len(prov.requests[0].Tools))
	}
	// And the call the model made against it must have been EXECUTED:
	// a second provider call carrying the tool_result for a0_call.
	if len(prov.requests) < 2 {
		t.Fatalf("provider called %d time(s); want ≥2. The kernel-injected tool call was dropped as refused", len(prov.requests))
	}
	found := false
	for _, m := range prov.requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "a0_call" {
			found = true
		}
	}
	if !found {
		t.Fatalf("request[1] carries no tool_result for a0_call; the injected tool was not executed")
	}
}
