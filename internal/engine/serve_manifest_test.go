package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleManifestShape verifies the manifest response is well-formed and
// contains the expected top-level fields.
func TestHandleManifestShape(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	w := httptest.NewRecorder()
	srv.handleManifest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	wantKeys := []string{"service", "version", "build_time", "node_id", "transports", "http_routes", "mcp_tools"}
	for _, k := range wantKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if body["service"] != "cogos-kernel" {
		t.Errorf("service = %v; want cogos-kernel", body["service"])
	}
}

// TestHandleManifestHTTPRoutesByFamily verifies that at least one route per
// expected family appears in the manifest. This is the headline invariant
// from the plan: the manifest reflects what's actually registered.
func TestHandleManifestHTTPRoutesByFamily(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	w := httptest.NewRecorder()
	srv.handleManifest(w, req)

	var body struct {
		HTTPRoutes []routeMeta `json:"http_routes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// Families we expect at least one route for after NewServer wires the
	// full mux. Drops "kernel" in for the / and /health sanity checks.
	// "observability" families from /v1/ledger etc. should be present.
	wantFamilies := []string{
		"kernel",
		"mcp",
		"openai",
		"anthropic",
		"bus",
		"sessions",
		"memory",
		"observability",
		"attention",
		"config",
		"compat",
	}
	got := map[string]bool{}
	for _, r := range body.HTTPRoutes {
		got[r.Family] = true
	}
	for _, fam := range wantFamilies {
		if !got[fam] {
			t.Errorf("family %q not present among %d routes (got families: %v)",
				fam, len(body.HTTPRoutes), keys(got))
		}
	}
}

// TestHandleManifestMCPToolClassification verifies that a known tool
// (mod3_speak) gets classified under the expected family (mod3), and that at
// least the core cog_* family is represented.
func TestHandleManifestMCPToolClassification(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	w := httptest.NewRecorder()
	srv.handleManifest(w, req)

	var body struct {
		MCPTools []mcpToolMeta `json:"mcp_tools"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if len(body.MCPTools) == 0 {
		t.Fatal("mcp_tools is empty; NewServer should have wired at least a handful of cog_* tools")
	}

	// Verify mod3_speak is classified under family "mod3".
	var foundMod3, foundCog bool
	for _, tool := range body.MCPTools {
		if tool.Name == "mod3_speak" {
			foundMod3 = true
			if tool.Family != "mod3" {
				t.Errorf("mod3_speak family = %q; want mod3", tool.Family)
			}
		}
		if tool.Family == "cog" {
			foundCog = true
		}
	}
	if !foundMod3 {
		t.Error("mod3_speak not present among registered tools")
	}
	if !foundCog {
		t.Error("no cog_* family tools found; at least a handful should exist")
	}
}

// TestClassifyHTTPFamily covers the prefix map directly.
func TestClassifyHTTPFamily(t *testing.T) {
	t.Parallel()
	cases := []struct{ path, want string }{
		{"/health", "kernel"},
		{"/", "kernel"},
		{"/canvas", "kernel"},
		{"/v1/chat/completions", "openai"},
		{"/v1/models", "openai"},
		{"/v1/messages", "anthropic"},
		{"/v1/bus/send", "bus"},
		{"/v1/events", "bus"},
		{"/v1/sessions", "sessions"},
		{"/v1/handoffs", "sessions"},
		{"/v1/channel-sessions", "sessions"},
		{"/v1/cogdoc/read", "memory"},
		{"/v1/resolve", "memory"},
		{"/v1/context", "memory"},
		{"/memory/search", "memory"},
		{"/v1/ledger", "observability"},
		{"/v1/traces", "observability"},
		{"/v1/tool-calls", "observability"},
		{"/v1/kernel-log", "observability"},
		{"/v1/debug/last", "observability"},
		{"/v1/attention", "attention"},
		{"/v1/constellation/fovea", "attention"},
		{"/v1/lightcone", "attention"},
		{"/mcp", "mcp"},
		{"/v1/card", "compat"},
		{"/v1/providers", "compat"},
		{"/v1/taa", "compat"},
		{"/coherence/check", "kernel"},
		{"/v1/config", "config"},
		{"/totally/made/up", "misc"},
	}
	for _, tc := range cases {
		if got := classifyHTTPFamily(tc.path); got != tc.want {
			t.Errorf("classifyHTTPFamily(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

// TestClassifyMCPFamily covers the prefix-before-underscore rule.
func TestClassifyMCPFamily(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, want string }{
		{"cog_search_memory", "cog"},
		{"mod3_speak", "mod3"},
		{"cogos_status", "cogos"},
		{"noprefix", "misc"},
	}
	for _, tc := range cases {
		if got := classifyMCPFamily(tc.name); got != tc.want {
			t.Errorf("classifyMCPFamily(%q) = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// TestTrackTool_InputSchemaBackfilled verifies that after NewMCPServer
// finishes construction, an eager tool's toolMeta entry carries a non-empty
// InputSchema — proving backfillEagerSchemas actually populates the field
// mcp.AddTool's inference never writes back onto the original *mcp.Tool
// pointer captured by trackTool.
func TestTrackTool_InputSchemaBackfilled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog"}
	nucleus := &Nucleus{Name: "schema-test"}
	proc := NewProcess(cfg, nucleus)
	m := NewMCPServer(cfg, nucleus, proc)

	var found *mcpToolMeta
	for i := range m.toolMeta {
		if m.toolMeta[i].Name == "cog_search_memory" {
			found = &m.toolMeta[i]
			break
		}
	}
	if found == nil {
		t.Fatal("cog_search_memory not present in toolMeta")
	}
	if !found.Eager {
		t.Error("cog_search_memory should be Eager=true (porcelain)")
	}
	if found.InputSchema == nil {
		t.Fatal("cog_search_memory InputSchema should be non-nil after backfillEagerSchemas")
	}
	schema, ok := found.InputSchema.(map[string]interface{})
	if !ok {
		t.Fatalf("InputSchema type = %T; want map[string]interface{}", found.InputSchema)
	}
	if schema["type"] != "object" {
		t.Errorf("InputSchema[\"type\"] = %v; want \"object\"", schema["type"])
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		t.Errorf("InputSchema properties missing or empty: %v", schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Errorf("expected \"query\" property in cog_search_memory schema; got %v", props)
	}
}

// keys is a local helper to extract map keys for test diagnostics.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
