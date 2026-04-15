// agent_tools.go — Kernel-native tool implementations for the agent harness.
//
// Each tool is a Go function with the ToolFunc signature that wraps existing
// kernel functionality. Tools are registered with the AgentHarness and
// dispatched when the model invokes them via tool_calls.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RegisterCoreTools adds the standard kernel tools to the harness.
// workspaceRoot is the absolute path to the .cog workspace.
func RegisterCoreTools(h *AgentHarness, workspaceRoot string) {
	h.RegisterTool(memorySearchDef(), newMemorySearchFunc(workspaceRoot))
	h.RegisterTool(memoryReadDef(), newMemoryReadFunc(workspaceRoot))
	h.RegisterTool(memoryWriteDef(), newMemoryWriteFunc(workspaceRoot))
	h.RegisterTool(coherenceCheckDef(), newCoherenceCheckFunc(workspaceRoot))
	h.RegisterTool(busEmitDef(), newBusEmitFunc(workspaceRoot))
	h.RegisterTool(workspaceStatusDef(), newWorkspaceStatusFunc(workspaceRoot))
}

// --- memory_search ---

func memorySearchDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "memory_search",
			Description: "Search workspace memory (CogDocs) for documents matching a query. Returns paths and excerpts.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Search term or phrase"}
				},
				"required": ["query"]
			}`),
		},
	}
}

func newMemorySearchFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Query == "" {
			return json.Marshal(map[string]string{"error": "query is required"})
		}
		return runCogCommand(ctx, root, "memory", "search", p.Query)
	}
}

// --- memory_read ---

func memoryReadDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "memory_read",
			Description: "Read a specific memory document by its memory-relative path (e.g. 'semantic/insights/topic.md').",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Memory-relative path to the document"},
					"section": {"type": "string", "description": "Optional section name to read"}
				},
				"required": ["path"]
			}`),
		},
	}
}

func newMemoryReadFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Path    string `json:"path"`
			Section string `json:"section"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Path == "" {
			return json.Marshal(map[string]string{"error": "path is required"})
		}
		cmdArgs := []string{"memory", "read", p.Path}
		if p.Section != "" {
			cmdArgs = append(cmdArgs, "--section", p.Section)
		}
		return runCogCommand(ctx, root, cmdArgs...)
	}
}

// --- memory_write ---

func memoryWriteDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "memory_write",
			Description: "Write or update a memory document at the given memory-relative path.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Memory-relative path (e.g. 'semantic/insights/topic.md')"},
					"title": {"type": "string", "description": "Document title"},
					"content": {"type": "string", "description": "Document content (markdown)"}
				},
				"required": ["path", "content"]
			}`),
		},
	}
}

func newMemoryWriteFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Path    string `json:"path"`
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Path == "" || p.Content == "" {
			return json.Marshal(map[string]string{"error": "path and content are required"})
		}

		// Write content to the memory path via the cog CLI.
		cmdArgs := []string{"memory", "write", p.Path}
		if p.Title != "" {
			cmdArgs = append(cmdArgs, p.Title)
		}
		// The cog memory write command reads from stdin for content.
		return runCogCommandWithStdin(ctx, root, p.Content, cmdArgs...)
	}
}

// --- coherence_check ---

func coherenceCheckDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "coherence_check",
			Description: "Run coherence validation on the workspace. Returns drift detection results.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"required": []
			}`),
		},
	}
}

func newCoherenceCheckFunc(root string) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		return runCogCommand(ctx, root, "coherence", "check")
	}
}

// --- bus_emit ---

func busEmitDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "bus_emit",
			Description: "Emit an event to the CogBus. Events are typed with a topic and JSON payload.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"topic": {"type": "string", "description": "Event topic (e.g. 'agent.observation', 'memory.updated')"},
					"payload": {"type": "object", "description": "Event payload as JSON object"}
				},
				"required": ["topic", "payload"]
			}`),
		},
	}
}

func newBusEmitFunc(root string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Topic   string          `json:"topic"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Topic == "" {
			return json.Marshal(map[string]string{"error": "topic is required"})
		}
		payloadStr := string(p.Payload)
		if payloadStr == "" || payloadStr == "null" {
			payloadStr = "{}"
		}
		return runCogCommand(ctx, root, "bus", "emit", p.Topic, payloadStr)
	}
}

// --- workspace_status ---

func workspaceStatusDef() ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: ToolFunction{
			Name:        "workspace_status",
			Description: "Get current workspace state: git status, active sessions, recent changes, and health signals.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"required": []
			}`),
		},
	}
}

func newWorkspaceStatusFunc(root string) ToolFunc {
	return func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		var sections []string

		// Git status
		if out, err := runGitCommand(ctx, root, "status", "--porcelain"); err == nil {
			sections = append(sections, "## Git Status\n"+string(out))
		}

		// Recent commits
		if out, err := runGitCommand(ctx, root, "log", "--oneline", "-5"); err == nil {
			sections = append(sections, "## Recent Commits\n"+string(out))
		}

		// Health check via cog CLI
		if out, err := runCogCommand(ctx, root, "health"); err == nil {
			sections = append(sections, "## Health\n"+string(out))
		}

		// Active sessions
		sessionsDir := filepath.Join(root, ".cog", "mem", "episodic", "sessions")
		if entries, err := os.ReadDir(sessionsDir); err == nil {
			var recent []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), "-metadata.json") {
					recent = append(recent, e.Name())
				}
			}
			if len(recent) > 5 {
				recent = recent[len(recent)-5:]
			}
			sections = append(sections, "## Recent Sessions\n"+strings.Join(recent, "\n"))
		}

		result := strings.Join(sections, "\n\n")
		return json.Marshal(map[string]string{"status": result})
	}
}

// --- Helpers ---

// runCogCommand executes ./scripts/cog with the given arguments and returns the output as JSON.
func runCogCommand(ctx context.Context, root string, args ...string) (json.RawMessage, error) {
	cogScript := filepath.Join(root, "scripts", "cog")
	cmd := exec.CommandContext(ctx, cogScript, args...)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		return json.Marshal(map[string]string{
			"error":  err.Error(),
			"output": string(out),
		})
	}
	// Wrap output in a JSON object.
	return json.Marshal(map[string]string{"output": string(out)})
}

// runCogCommandWithStdin executes ./scripts/cog with stdin content.
func runCogCommandWithStdin(ctx context.Context, root, stdin string, args ...string) (json.RawMessage, error) {
	cogScript := filepath.Join(root, "scripts", "cog")
	cmd := exec.CommandContext(ctx, cogScript, args...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return json.Marshal(map[string]string{
			"error":  err.Error(),
			"output": string(out),
		})
	}
	return json.Marshal(map[string]string{"output": string(out)})
}

// runGitCommand executes git with the given arguments in the workspace root.
func runGitCommand(ctx context.Context, root string, args ...string) (json.RawMessage, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return out, nil
}
