// mcp_ingest_filename_traversal_test.go — regression test for
// myrgic/cogos#489 round 4: toolIngest embedded the caller-supplied
// input.Source raw into the generated CogDoc filename
// ("{source}-{date}-{slug}.cog.md"), so a source like "http:cog" produced
// an NTFS-illegal colon in the on-disk filename — the same defect class
// #489 was filed against, reachable through the cog_ingest MCP tool.
package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolIngestSanitizesSourceInFilename(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root}
	m := &MCPServer{
		cfg:       cfg,
		cogdocSvc: NewCogDocService(cfg, nil),
	}

	input := ingestInput{
		Source: "http:cog",
		Format: string(FormatMessage),
		Data:   "hello world",
	}

	result, _, err := m.toolIngest(context.Background(), nil, input)
	if err != nil {
		t.Fatalf("toolIngest: %v", err)
	}

	text := resultText(t, result)
	var parsed struct {
		Ingested bool   `json:"ingested"`
		Path     string `json:"path"`
	}
	if jsonErr := json.Unmarshal([]byte(text), &parsed); jsonErr != nil {
		t.Fatalf("unmarshal result %q: %v", text, jsonErr)
	}

	if strings.Contains(parsed.Path, "http:cog") {
		t.Fatalf("ingest result path %q still contains the raw colon-bearing source; want it sanitized", parsed.Path)
	}
	// pkg/pathsafe.SanitizeComponent percent-escapes ':' as "%3A".
	if !strings.Contains(parsed.Path, "http%3Acog") {
		t.Errorf("ingest result path %q missing expected sanitized form %q", parsed.Path, "http%3Acog")
	}

	if parsed.Ingested {
		absPath := filepath.Join(root, ".cog", "mem", parsed.Path)
		if _, statErr := os.Stat(absPath); statErr != nil {
			t.Errorf("expected written cogdoc at %q: %v", absPath, statErr)
		}
		if strings.Contains(filepath.Base(absPath), ":") {
			t.Errorf("on-disk filename %q still contains a raw colon (NTFS-illegal)", filepath.Base(absPath))
		}
	}
}

// resultText extracts the text payload from a *mcp.CallToolResult built by
// marshalResult/textResult.
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("result has no content: %+v", result)
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result.Content[0] is %T, want *mcp.TextContent", result.Content[0])
	}
	return tc.Text
}
