// tools.go maps OpenAI-format tool definitions to Claude CLI tool names.
//
// When a client sends tools in the OpenAI format ({"type":"function","function":{"name":"bash"}}),
// MapToolsToCLINames converts them to Claude CLI --allowed-tools names (e.g., "Bash").
// This is called by BuildClaudeArgs when the request has Tools but no AllowedTools.
package harness

import (
	"encoding/json"
	"strings"
)

// MapToolsToCLINames extracts function names from OpenAI-format tool definitions
// and maps them to Claude CLI built-in tool names where possible.
func MapToolsToCLINames(tools []json.RawMessage) []string {
	var result []string
	seen := make(map[string]bool)

	for _, raw := range tools {
		var tool struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Function.Name == "" {
			continue
		}

		cliName := mapToolName(tool.Function.Name)
		if cliName == "" {
			// MCP and external tools are registered through --mcp-config, not
			// --allowed-tools, so silently skip anything we don't recognise.
			continue
		}
		if !seen[cliName] {
			seen[cliName] = true
			result = append(result, cliName)
		}
	}
	return result
}

// mapToolName maps an OpenAI-format tool function name to a Claude CLI tool name.
func mapToolName(name string) string {
	switch strings.ToLower(name) {
	case "exec", "bash", "shell":
		return "Bash"
	case "read", "file_read":
		return "Read"
	case "write", "file_write":
		return "Write"
	case "edit", "apply-patch", "apply_patch":
		return "Edit"
	case "search", "grep":
		return "Grep"
	case "glob", "find":
		return "Glob"
	default:
		return ""
	}
}
