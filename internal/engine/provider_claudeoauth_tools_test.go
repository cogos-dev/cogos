package engine

import "testing"

// Anthropic's OAuth billing gate routes ^mcp_[a-z] tool names to the overage
// lane (HTTP 400 "out of extra usage" on a Max subscription). These tests pin
// the outbound rewrite to the sanctioned mcp__ namespace and the inbound
// reversal that keeps client-side tool-call routing intact.

func TestToolNameToWire(t *testing.T) {
	cases := map[string]string{
		"cog_get_state":            "mcp__cog_get_state",       // bare -> double (agentic-set fix)
		"terminal":                 "mcp__terminal",            // bare -> double
		"mcp_cogos_cog_get_state":  "mcp__cogos_cog_get_state", // single -> double (the gate fix)
		"mcp_terminal":             "mcp__terminal",            // single -> double
		"mcp__server__tool":        "mcp__server__tool",        // already sanctioned: unchanged
		"mcp__cogos_cog_get_state": "mcp__cogos_cog_get_state", // already double: unchanged
	}
	for in, want := range cases {
		if got := toolNameToWire(in); got != want {
			t.Errorf("toolNameToWire(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteAndRestoreOAuthToolNames(t *testing.T) {
	payload := &anthropicRequest{
		Tools: []anthropicTool{
			{Name: "mcp_cogos_cog_get_state"},
			{Name: "terminal"},
			{Name: "mcp__already__double"},
		},
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []anthropicContentBlock{
				{Type: "text"},
				{Type: "tool_use", Name: "mcp_cogos_cog_search_memory"},
			}},
			{Role: "user", Content: "plain string content"},
		},
	}

	rev := rewriteOAuthToolNames(payload)

	// Outbound: single-underscore names rewritten; bare + double untouched.
	if payload.Tools[0].Name != "mcp__cogos_cog_get_state" {
		t.Errorf("tool[0] = %q, want mcp__cogos_cog_get_state", payload.Tools[0].Name)
	}
	if payload.Tools[1].Name != "mcp__terminal" {
		t.Errorf("tool[1] = %q, want mcp__terminal (bare -> double)", payload.Tools[1].Name)
	}
	if rev["mcp__terminal"] != "terminal" {
		t.Errorf("rev map must restore bare name: %#v", rev)
	}
	if payload.Tools[2].Name != "mcp__already__double" {
		t.Errorf("tool[2] = %q, want mcp__already__double (unchanged)", payload.Tools[2].Name)
	}
	// History tool_use rewritten too.
	blocks := payload.Messages[0].Content.([]anthropicContentBlock)
	if blocks[1].Name != "mcp__cogos_cog_search_memory" {
		t.Errorf("history tool_use = %q, want mcp__cogos_cog_search_memory", blocks[1].Name)
	}
	// Reverse map only contains names we actually rewrote.
	if rev["mcp__cogos_cog_get_state"] != "mcp_cogos_cog_get_state" {
		t.Errorf("rev map missing tool rewrite: %#v", rev)
	}
	if rev["mcp__cogos_cog_search_memory"] != "mcp_cogos_cog_search_memory" {
		t.Errorf("rev map missing history rewrite: %#v", rev)
	}
	if _, ok := rev["mcp__already__double"]; ok {
		t.Errorf("genuine double-underscore must not be in reverse map: %#v", rev)
	}

	// Inbound: reverse only mapped names; a genuine double-underscore tool_use
	// from the model must be left untouched.
	content := []anthropicContent{
		{Type: "tool_use", Name: "mcp__cogos_cog_get_state"},
		{Type: "tool_use", Name: "mcp__already__double"},
		{Type: "text"},
	}
	restoreOAuthToolNames(content, rev)
	if content[0].Name != "mcp_cogos_cog_get_state" {
		t.Errorf("restore[0] = %q, want mcp_cogos_cog_get_state", content[0].Name)
	}
	if content[1].Name != "mcp__already__double" {
		t.Errorf("restore[1] = %q, want mcp__already__double (untouched)", content[1].Name)
	}
}
