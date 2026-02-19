// inference_test.go
// Tests for tool forwarding logic in inference.go

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// === buildClaudeArgs TESTS ===

// TestBuildClaudeArgs_NoTools verifies default behavior is preserved when no tools are set.
func TestBuildClaudeArgs_NoTools(t *testing.T) {
	req := &InferenceRequest{
		Prompt: "hello",
		Model:  "claude",
	}
	args := buildClaudeArgs(req)
	for _, arg := range args {
		if arg == "--allowed-tools" {
			t.Error("--allowed-tools should not be present when no tools set")
		}
	}
}

// TestBuildClaudeArgs_ExplicitAllowedTools verifies --allowed-tools is passed through
// when AllowedTools is explicitly set on the request.
func TestBuildClaudeArgs_ExplicitAllowedTools(t *testing.T) {
	req := &InferenceRequest{
		Prompt:       "hello",
		AllowedTools: []string{"Bash", "Read", "Write"},
	}
	args := buildClaudeArgs(req)
	found := false
	for i, arg := range args {
		if arg == "--allowed-tools" && i+1 < len(args) {
			if args[i+1] != "Bash,Read,Write" {
				t.Errorf("expected 'Bash,Read,Write', got %q", args[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Error("--allowed-tools not found in args")
	}
}

// TestBuildClaudeArgs_OpenAITools verifies that OpenAI-format tool definitions
// are auto-mapped and produce --allowed-tools in the output.
func TestBuildClaudeArgs_OpenAITools(t *testing.T) {
	req := &InferenceRequest{
		Prompt: "hello",
		Tools: []json.RawMessage{
			json.RawMessage(`{"type":"function","function":{"name":"bash"}}`),
			json.RawMessage(`{"type":"function","function":{"name":"read"}}`),
		},
	}
	args := buildClaudeArgs(req)
	found := false
	for i, arg := range args {
		if arg == "--allowed-tools" {
			found = true
			if i+1 < len(args) {
				val := args[i+1]
				if !strings.Contains(val, "Bash") || !strings.Contains(val, "Read") {
					t.Errorf("expected mapped tools Bash,Read, got %q", val)
				}
			}
		}
	}
	if !found {
		t.Error("--allowed-tools should be present when Tools are set")
	}
}

// TestBuildClaudeArgs_AllowedToolsPriority verifies that explicit AllowedTools
// takes priority over auto-mapped Tools.
func TestBuildClaudeArgs_AllowedToolsPriority(t *testing.T) {
	req := &InferenceRequest{
		Prompt:       "hello",
		AllowedTools: []string{"Bash"},
		Tools: []json.RawMessage{
			json.RawMessage(`{"type":"function","function":{"name":"read"}}`),
		},
	}
	args := buildClaudeArgs(req)
	for i, arg := range args {
		if arg == "--allowed-tools" && i+1 < len(args) {
			if args[i+1] != "Bash" {
				t.Errorf("AllowedTools should take priority, got %q", args[i+1])
			}
		}
	}
}

// === mapToolName TESTS ===

// TestMapToolName verifies each known OpenAI tool name maps to the correct Claude CLI name.
func TestMapToolName(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"exec", "Bash"},
		{"bash", "Bash"},
		{"shell", "Bash"},
		{"read", "Read"},
		{"file_read", "Read"},
		{"write", "Write"},
		{"file_write", "Write"},
		{"edit", "Edit"},
		{"apply-patch", "Edit"},
		{"apply_patch", "Edit"},
		{"search", "Grep"},
		{"grep", "Grep"},
		{"glob", "Glob"},
		{"find", "Glob"},
		{"unknown_tool", ""}, // unknown returns empty
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := mapToolName(tc.input)
			if got != tc.expected {
				t.Errorf("mapToolName(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestMapToolName_CaseInsensitive verifies that mapToolName handles mixed-case inputs.
func TestMapToolName_CaseInsensitive(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"BASH", "Bash"},
		{"Read", "Read"},
		{"WRITE", "Write"},
		{"Grep", "Grep"},
		{"GLOB", "Glob"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := mapToolName(tc.input)
			if got != tc.expected {
				t.Errorf("mapToolName(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// === mapToolsToCLINames TESTS ===

// TestMapToolsToCLINames verifies that OpenAI-format tool arrays are correctly
// mapped and deduplicated.
func TestMapToolsToCLINames(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"exec","description":"Execute command"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"read","description":"Read file"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"exec","description":"Duplicate"}}`), // duplicate
	}
	names := mapToolsToCLINames(tools)
	if len(names) != 2 {
		t.Errorf("expected 2 unique names, got %d: %v", len(names), names)
	}
	// Verify the expected names are present
	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["Bash"] {
		t.Error("expected Bash in mapped names")
	}
	if !nameSet["Read"] {
		t.Error("expected Read in mapped names")
	}
}

// TestMapToolsToCLINames_UnknownToolsSkipped verifies unknown tool names are silently skipped.
func TestMapToolsToCLINames_UnknownToolsSkipped(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"bash"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"custom_magic_tool"}}`),
	}
	names := mapToolsToCLINames(tools)
	if len(names) != 1 {
		t.Errorf("expected 1 name (unknown skipped), got %d: %v", len(names), names)
	}
	if len(names) > 0 && names[0] != "Bash" {
		t.Errorf("expected Bash, got %q", names[0])
	}
}

// TestMapToolsToCLINames_InvalidJSON verifies malformed JSON tools are skipped gracefully.
func TestMapToolsToCLINames_InvalidJSON(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"read"}}`),
		json.RawMessage(`{invalid json}`),
		json.RawMessage(`{"type":"function","function":{}}`), // empty name
	}
	names := mapToolsToCLINames(tools)
	if len(names) != 1 {
		t.Errorf("expected 1 name (invalid skipped), got %d: %v", len(names), names)
	}
	if len(names) > 0 && names[0] != "Read" {
		t.Errorf("expected Read, got %q", names[0])
	}
}

// TestMapToolsToCLINames_Empty verifies empty tool list returns empty result.
func TestMapToolsToCLINames_Empty(t *testing.T) {
	names := mapToolsToCLINames(nil)
	if len(names) != 0 {
		t.Errorf("expected 0 names for nil input, got %d: %v", len(names), names)
	}

	names = mapToolsToCLINames([]json.RawMessage{})
	if len(names) != 0 {
		t.Errorf("expected 0 names for empty input, got %d: %v", len(names), names)
	}
}
