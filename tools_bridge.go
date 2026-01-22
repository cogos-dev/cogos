// .cog/tools_bridge.go
// Bridge layer for tool execution tests - provides functions expected by test suite
//
// This file bridges between the test expectations (Squad C requirements) and the
// existing kernel infrastructure.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// === TOOL EXECUTION TYPES (Test-compatible) ===

// ToolCallExtended extends ToolCall with test-compatible fields
type ToolCallExtended struct {
	CallID   string                 `json:"call_id"`
	ToolName string                 `json:"tool_name"`
	Inputs   map[string]interface{} `json:"inputs"`
}

// ToolResult represents the outcome of a tool execution
type ToolResult struct {
	CallID       string `json:"call_id"`
	ArtifactHash string `json:"artifact_hash"`
	ByteLength   int    `json:"byte_length"`
	ContentType  string `json:"content_type"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
}

// Tool defines the interface for executable tools
type Tool interface {
	Name() string
	Execute(inputs map[string]interface{}) (*ToolResult, error)
}

// === POLICY ENGINE (Test-compatible) ===

// PolicyExtended extends Policy for test compatibility
type PolicyExtended struct {
	Name  string             `yaml:"name"`
	Tools []string           `yaml:"tools"`
	Allow []PolicyRuleExt    `yaml:"allow"`
	Deny  []PolicyRuleExt    `yaml:"deny"`
}

// PolicyRuleExt extends PolicyRule for test compatibility
type PolicyRuleExt struct {
	Pattern string `yaml:"pattern"`
	Reason  string `yaml:"reason,omitempty"`
}

// PolicyEngine evaluates tool calls against policies
type PolicyEngine struct {
	Policies []*PolicyExtended
}

// NewPolicyEngine creates a policy engine from configuration
func NewPolicyEngine(policies []*PolicyExtended) *PolicyEngine {
	return &PolicyEngine{Policies: policies}
}

// Evaluate checks if a tool call is allowed by policy
func (pe *PolicyEngine) Evaluate(toolName string, inputs map[string]interface{}) (bool, string) {
	// Find policies that apply to this tool
	var applicablePolicies []*PolicyExtended
	for _, policy := range pe.Policies {
		for _, tool := range policy.Tools {
			if tool == toolName || tool == "*" {
				applicablePolicies = append(applicablePolicies, policy)
				break
			}
		}
	}

	if len(applicablePolicies) == 0 {
		// No policy = deny by default (fail-closed)
		return false, "no policy defined for tool"
	}

	// Evaluate each policy (deny takes precedence over allow)
	for _, policy := range applicablePolicies {
		// Check deny rules first
		for _, rule := range policy.Deny {
			if matchesRulePattern(rule, inputs) {
				reason := rule.Reason
				if reason == "" {
					reason = fmt.Sprintf("denied by policy %s (pattern: %s)", policy.Name, rule.Pattern)
				}
				return false, reason
			}
		}

		// Check allow rules
		for _, rule := range policy.Allow {
			if matchesRulePattern(rule, inputs) {
				return true, ""
			}
		}
	}

	// No allow rule matched = deny
	return false, "no allow rule matched"
}

// matchesRulePattern checks if inputs match a policy rule pattern
func matchesRulePattern(rule PolicyRuleExt, inputs map[string]interface{}) bool {
	// Compile pattern
	pattern, err := regexp.Compile(rule.Pattern)
	if err != nil {
		return false
	}

	// Check each input value
	for _, value := range inputs {
		strValue := fmt.Sprintf("%v", value)
		if pattern.MatchString(strValue) {
			return true
		}
	}

	return false
}

// === TOOL EXECUTION ===

// ExecuteTool gates and executes a tool call
func ExecuteTool(tool Tool, call *ToolCallExtended, policy *PolicyEngine) (*ToolResult, error) {
	// Log tool.call event
	logToolCallEvent(call)

	// Evaluate policy
	allowed, reason := policy.Evaluate(call.ToolName, call.Inputs)
	if !allowed {
		// Log policy.denied event
		logPolicyDeniedEvent(call, reason)
		return &ToolResult{
			CallID:  call.CallID,
			Success: false,
			Error:   fmt.Sprintf("policy denied: %s", reason),
		}, fmt.Errorf("policy denied: %s", reason)
	}

	// Execute tool
	result, err := tool.Execute(call.Inputs)
	if err != nil {
		return &ToolResult{
			CallID:  call.CallID,
			Success: false,
			Error:   err.Error(),
		}, err
	}

	// Log tool.result event
	logToolResultEvent(result)

	return result, nil
}

// ParseToolCall extracts structured tool call from model output
func ParseToolCall(modelOutput string) (*ToolCallExtended, error) {
	// Look for tool call pattern: <tool_use name="Write" call_id="...">
	toolUseRegex := regexp.MustCompile(`<tool_use\s+name="([^"]+)"\s+call_id="([^"]+)">`)
	matches := toolUseRegex.FindStringSubmatch(modelOutput)
	if len(matches) < 3 {
		return nil, fmt.Errorf("no tool call found in output")
	}

	toolName := matches[1]
	callID := matches[2]

	// Extract inputs
	inputsRegex := regexp.MustCompile(`<inputs>(.*?)</inputs>`)
	inputsMatch := inputsRegex.FindStringSubmatch(modelOutput)

	inputs := make(map[string]interface{})
	if len(inputsMatch) > 1 {
		// Parse JSON inputs
		if err := json.Unmarshal([]byte(inputsMatch[1]), &inputs); err != nil {
			// Fallback: treat as plain text
			inputs["content"] = inputsMatch[1]
		}
	}

	return &ToolCallExtended{
		CallID:   callID,
		ToolName: toolName,
		Inputs:   inputs,
	}, nil
}

// === ARTIFACT STORAGE ===

// StoreArtifact stores tool output as an immutable artifact
func StoreArtifact(sessionID string, data []byte, contentType string) (string, error) {
	// Compute artifact hash
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	// Store in .cog/ledger/{session}/artifacts/{hash}.{ext}
	artifactDir := filepath.Join(".cog", "ledger", sessionID, "artifacts")
	if err := os.MkdirAll(artifactDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create artifact directory: %w", err)
	}

	// Determine extension from content type
	ext := "bin"
	if strings.HasPrefix(contentType, "text/") {
		ext = "txt"
	}
	if contentType == "text/markdown" {
		ext = "md"
	}
	if contentType == "application/json" {
		ext = "json"
	}

	artifactPath := filepath.Join(artifactDir, fmt.Sprintf("%s.%s", hashStr, ext))

	// Write artifact (idempotent - hash-addressed)
	if err := os.WriteFile(artifactPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write artifact: %w", err)
	}

	return hashStr, nil
}

// === EVENT LOGGING ===

// logToolCallEvent logs a tool.call event to the session ledger
func logToolCallEvent(call *ToolCallExtended) {
	event := map[string]interface{}{
		"type":      "tool.call",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"call_id":   call.CallID,
		"tool_name": call.ToolName,
		"inputs":    call.Inputs,
	}
	writeEventToLedgerBridge(event)
}

// logToolResultEvent logs a tool.result event to the session ledger
func logToolResultEvent(result *ToolResult) {
	event := map[string]interface{}{
		"type":          "tool.result",
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
		"call_id":       result.CallID,
		"artifact_hash": result.ArtifactHash,
		"byte_length":   result.ByteLength,
		"content_type":  result.ContentType,
		"success":       result.Success,
	}
	if result.Error != "" {
		event["error"] = result.Error
	}
	writeEventToLedgerBridge(event)
}

// logPolicyDeniedEvent logs a policy.denied event to the session ledger
func logPolicyDeniedEvent(call *ToolCallExtended, reason string) {
	event := map[string]interface{}{
		"type":             "policy.denied",
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
		"tool_name":        call.ToolName,
		"reason":           reason,
		"attempted_inputs": call.Inputs,
	}
	writeEventToLedgerBridge(event)
}

// writeEventToLedgerBridge appends an event to the current session's event log
func writeEventToLedgerBridge(event map[string]interface{}) {
	// Get current session ID
	sessionID := os.Getenv("CLAUDE_SESSION_ID")
	if sessionID == "" {
		sessionID = "default"
	}

	// Write to .cog/ledger/{session}/events.jsonl
	ledgerDir := filepath.Join(".cog", "ledger", sessionID)
	if err := os.MkdirAll(ledgerDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create ledger directory: %v\n", err)
		return
	}

	eventPath := filepath.Join(ledgerDir, "events.jsonl")
	f, err := os.OpenFile(eventPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to open event log: %v\n", err)
		return
	}
	defer f.Close()

	eventJSON, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to marshal event: %v\n", err)
		return
	}

	if _, err := f.WriteString(string(eventJSON) + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write event: %v\n", err)
	}
}
