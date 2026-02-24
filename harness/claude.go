// claude.go handles Claude CLI argument construction and MCP bridge configuration.
//
// BuildClaudeArgs is the main entry point — it takes an InferenceRequest and
// produces the argument list for `claude -p ... --output-format stream-json`.
// This includes system prompt chaining (TAA context + client prompt), model
// mapping, tool forwarding (--allowed-tools), and MCP config.
//
// GenerateMCPConfig creates a temporary JSON file for --mcp-config that tells
// Claude CLI to spawn `cog mcp serve --bridge` as an MCP subprocess.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ClaudeCommand is the name of the Claude CLI binary.
const ClaudeCommand = "claude"

// chainSystemPrompt combines TAA context and client system prompt into a single
// header chain, separated by ---. TAA context comes first (identity, temporal,
// present, semantic), followed by any client-provided system instructions.
func chainSystemPrompt(req *InferenceRequest) string {
	var taaBlock string
	if req.ContextState != nil {
		taaBlock = req.ContextState.BuildContextString()
	}
	switch {
	case taaBlock != "" && req.SystemPrompt != "":
		return taaBlock + "\n\n---\n\n" + req.SystemPrompt
	case taaBlock != "":
		return taaBlock
	default:
		return req.SystemPrompt
	}
}

// BuildClaudeArgs constructs the Claude CLI arguments from an InferenceRequest.
// Supports both legacy mode (SystemPrompt) and new context-aware mode (ContextState).
func BuildClaudeArgs(req *InferenceRequest) []string {
	args := []string{
		"-p", req.Prompt,
		"--output-format", "stream-json",
		"--include-partial-messages", // Enable rich streaming with content_block_delta events
		"--verbose",
		"--dangerously-skip-permissions", // Allow tool execution without prompts
	}

	// Build system prompt: chain TAA context + client system prompt
	if sp := chainSystemPrompt(req); sp != "" {
		args = append(args, "--append-system-prompt", sp)
	}

	// Use request-level schema if provided
	var schema json.RawMessage
	if len(req.Schema) > 0 {
		schema = req.Schema
	}

	// Add JSON schema if requested
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}

	// Determine model source
	// Priority: ContextState.Model > req.Model
	model := req.Model
	if req.ContextState != nil && req.ContextState.Model != "" {
		model = req.ContextState.Model
	}

	// Map model IDs to Claude CLI aliases
	if model != "" && model != "claude" {
		switch model {
		case "claude-opus-4-5-20250929", "opus-4-5", "opus":
			model = "opus"
		case "claude-sonnet-4-5-20250929", "sonnet-4-5", "sonnet":
			model = "sonnet"
		}
		args = append(args, "--model", model)
	}

	// Forward tool control to Claude CLI
	if len(req.AllowedTools) > 0 {
		// Explicit allowed-tools list takes priority
		args = append(args, "--allowed-tools", strings.Join(req.AllowedTools, ","))
	} else if len(req.Tools) > 0 {
		// Map OpenAI-format tool definitions to Claude CLI tool names
		if mapped := MapToolsToCLINames(req.Tools); len(mapped) > 0 {
			args = append(args, "--allowed-tools", strings.Join(mapped, ","))
		}
	}

	// MCP bridge configuration
	if req.MCPConfig != "" {
		args = append(args, "--mcp-config", req.MCPConfig)
	}

	return args
}

// GenerateMCPConfig creates a temporary MCP config JSON file for Claude CLI's --mcp-config flag.
// The config tells Claude CLI to spawn `cog mcp serve --bridge` as an MCP server,
// enabling access to both CogOS and OpenClaw tools.
func GenerateMCPConfig(req *InferenceRequest, kernel KernelServices) (string, error) {
	if req.OpenClawURL == "" {
		return "", fmt.Errorf("OpenClawURL required for MCP bridge")
	}

	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	_, span := tracer.Start(ctx, "mcp.config.generate",
		trace.WithAttributes(
			attribute.String("openclaw.url", req.OpenClawURL),
			attribute.Int("tools.count", len(req.Tools)),
		),
	)
	defer span.End()

	// Find the cog binary path
	cogBin, err := os.Executable()
	if err != nil {
		cogBin = "cog" // Fallback to PATH lookup
	}

	// Resolve workspace root for COG_ROOT env var
	root := kernel.WorkspaceRoot()

	env := map[string]string{
		"COG_ROOT":       root,
		"OPENCLAW_URL":   req.OpenClawURL,
		"OPENCLAW_TOKEN": req.OpenClawToken,
		"SESSION_ID":     req.SessionID,
	}

	// Propagate trace context to the bridge subprocess via TRACEPARENT env var.
	spanCtx := span.SpanContext()
	if spanCtx.IsValid() {
		traceparent := fmt.Sprintf("00-%s-%s-%s",
			spanCtx.TraceID().String(),
			spanCtx.SpanID().String(),
			spanCtx.TraceFlags().String(),
		)
		env["TRACEPARENT"] = traceparent
		span.SetAttributes(attribute.String("traceparent", traceparent))
	}

	// Pass OTEL exporter endpoint so the bridge subprocess can also export traces
	if otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); otelEndpoint != "" {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = otelEndpoint
	}

	// Convert OpenAI-format tools from request body into MCP format for the bridge.
	if len(req.Tools) > 0 {
		mcpTools := kernel.ConvertOpenAIToolsToMCP(req.Tools)
		if len(mcpTools) > 0 {
			toolsJSON, err := json.Marshal(mcpTools)
			if err != nil {
				log.Printf("[MCP] Warning: failed to serialize tool registry: %v", err)
			} else {
				env["TOOL_REGISTRY"] = string(toolsJSON)
				log.Printf("[MCP] Tool registry: %d tools from request body", len(mcpTools))
				span.SetAttributes(attribute.Int("tools.registry_count", len(mcpTools)))
			}
		}
	}

	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"cogos-bridge": map[string]interface{}{
				"command": cogBin,
				"args":    []string{"mcp", "serve", "--bridge"},
				"env":     env,
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "cog-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("write config: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("close config: %w", err)
	}

	log.Printf("[MCP] Generated bridge config: %s (cog=%s, url=%s, session=%s)", tmpFile.Name(), cogBin, req.OpenClawURL, req.SessionID)
	return tmpFile.Name(), nil
}

// BuildContextMetrics extracts metrics from ContextState for response
func BuildContextMetrics(ctx *ContextState) *ContextMetrics {
	if ctx == nil {
		return nil
	}

	tierBreakdown := make(map[string]int)
	totalTokens := 0

	if ctx.Tier1Identity != nil {
		tierBreakdown["tier1_identity"] = ctx.Tier1Identity.Tokens
		totalTokens += ctx.Tier1Identity.Tokens
	}
	if ctx.Tier2Temporal != nil {
		tierBreakdown["tier2_temporal"] = ctx.Tier2Temporal.Tokens
		totalTokens += ctx.Tier2Temporal.Tokens
	}
	if ctx.Tier3Present != nil {
		tierBreakdown["tier3_present"] = ctx.Tier3Present.Tokens
		totalTokens += ctx.Tier3Present.Tokens
	}

	// Use provided total if available, otherwise use computed
	if ctx.TotalTokens > 0 {
		totalTokens = ctx.TotalTokens
	}

	return &ContextMetrics{
		TotalTokens:     totalTokens,
		TierBreakdown:   tierBreakdown,
		CoherenceScore:  ctx.CoherenceScore,
		CompressionUsed: false, // Set by caller if compression was applied
	}
}
