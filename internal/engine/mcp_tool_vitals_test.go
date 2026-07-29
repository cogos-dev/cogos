package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolVitalsWindow_MissingParamsReturnsFallbackNotError(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	m := &MCPServer{}

	cases := []vitalsWindowInput{
		{Since: "24h", Resolution: "raw"},                              // missing metric
		{Metric: "disk_free_bytes", Resolution: "raw"},                 // missing since
		{Metric: "disk_free_bytes", Since: "24h"},                      // missing resolution
		{Metric: "not_a_real_metric", Since: "24h", Resolution: "raw"}, // unknown metric
	}
	for i, input := range cases {
		result, _, err := m.toolVitalsWindow(context.Background(), &mcp.CallToolRequest{}, input)
		if err != nil {
			t.Errorf("case %d: expected a fallback result (nil error), got protocol error: %v", i, err)
		}
		if result == nil || !result.IsError {
			t.Errorf("case %d: expected IsError=true fallback result, got %+v", i, result)
		}
	}
}

func TestToolVitalsWindow_EmptyHistorySucceeds(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	m := &MCPServer{}

	input := vitalsWindowInput{Metric: "disk_free_bytes", Since: "24h", Resolution: "raw"}
	result, _, err := m.toolVitalsWindow(context.Background(), &mcp.CallToolRequest{}, input)
	if err != nil {
		t.Fatalf("expected no error for a metric/window with no data, got %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected a successful (IsError=false) result, got %+v", result)
	}
}

// TestToolVitalsWindow_StorageErrorReturnsRealProtocolError is the MCP-side
// counterpart to serve_vitals_test.go's HTTP 500 regression: a genuine
// storage failure (here: the metric's raw directory replaced by a plain
// file) must come back as a real MCP-protocol error, not the same
// fallbackResult IsError:true content blob used for a malformed request —
// cog-review's third pass on PR #493 (4dcbd00) flagged that the HTTP fix
// wasn't mirrored onto this sibling caller of vitalsretention.Window.
func TestToolVitalsWindow_StorageErrorReturnsRealProtocolError(t *testing.T) {
	root := withVitalsWorkspace(t, "node-a")

	blockedDir := filepath.Join(root, ".cog", "observatory", "vitals", "node-a", "raw", "disk_free_bytes")
	if err := os.MkdirAll(filepath.Dir(blockedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &MCPServer{}
	input := vitalsWindowInput{Metric: "disk_free_bytes", Since: "24h", Resolution: "raw"}
	result, _, err := m.toolVitalsWindow(context.Background(), &mcp.CallToolRequest{}, input)

	if err == nil {
		t.Fatalf("expected a real protocol-level error for a storage failure, got result=%+v", result)
	}
	if result != nil {
		t.Errorf("expected a nil result alongside a protocol error, got %+v", result)
	}
}
