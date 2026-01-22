// .cog/test_tools.go
// Test suite for tool execution framework and policy enforcement
//
// Includes RED TEAM tests to verify that:
// 1. Narrative bypass fails (text claiming action without tool.call → no effect)
// 2. Policy enforcement blocks denied paths
// 3. Tool calls are properly parsed and logged
// 4. Artifacts are stored immutably

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// === TOOL CALL PARSING TESTS ===

func TestParseToolCall_ValidStructure(t *testing.T) {
	// Simulate model output with tool call
	modelOutput := `I will write a file.
<tool_use name="Write" call_id="call_001">
<inputs>{"file_path": ".cog/mem/test.md", "content": "Hello World"}</inputs>
</tool_use>
Done.`

	call, err := ParseToolCall(modelOutput)
	if err != nil {
		t.Fatalf("Expected successful parse, got error: %v", err)
	}

	if call.ToolName != "Write" {
		t.Errorf("Expected tool name 'Write', got '%s'", call.ToolName)
	}
	if call.CallID != "call_001" {
		t.Errorf("Expected call ID 'call_001', got '%s'", call.CallID)
	}
	if call.Inputs["file_path"] != ".cog/mem/test.md" {
		t.Errorf("Expected file_path '.cog/mem/test.md', got '%v'", call.Inputs["file_path"])
	}
}

func TestParseToolCall_NoToolCall(t *testing.T) {
	// Simulate model output WITHOUT tool call (narrative only)
	modelOutput := `I wrote the file successfully. The content has been saved to .cog/mem/test.md.`

	_, err := ParseToolCall(modelOutput)
	if err == nil {
		t.Error("Expected error when no tool call present, got nil")
	}
}

// === POLICY ENFORCEMENT TESTS ===

func TestPolicyEngine_AllowMemoryWrites(t *testing.T) {
	// Create policy that allows .cog/mem/ writes
	policy := &PolicyExtended{
		Name:  "write-memory-only",
		Tools: []string{"Write"},
		Allow: []PolicyRuleExt{
			{Pattern: `^\.cog/mem/.*`},
		},
		Deny: []PolicyRuleExt{
			{Pattern: `.*`},
		},
	}

	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Test allowed path
	inputs := map[string]interface{}{
		"file_path": ".cog/mem/test.md",
		"content":   "test content",
	}

	allowed, reason := engine.Evaluate("Write", inputs)
	if !allowed {
		t.Errorf("Expected allowed, got denied: %s", reason)
	}
}

func TestPolicyEngine_DenyNonMemoryWrites(t *testing.T) {
	// Create policy that allows .cog/mem/ writes only
	policy := &PolicyExtended{
		Name:  "write-memory-only",
		Tools: []string{"Write"},
		Allow: []PolicyRuleExt{
			{Pattern: `^\.cog/mem/.*`},
		},
		Deny: []PolicyRuleExt{
			{Pattern: `.*`},
		},
	}

	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Test denied paths
	deniedPaths := []string{
		".cog/cog.go",
		".cog/schemas/test.yaml",
		".cog/tools/write.go",
		"README.md",
		"../etc/passwd",
	}

	for _, path := range deniedPaths {
		inputs := map[string]interface{}{
			"file_path": path,
			"content":   "malicious content",
		}

		allowed, _ := engine.Evaluate("Write", inputs)
		if allowed {
			t.Errorf("Expected denied for path '%s', got allowed", path)
		}
	}
}

func TestPolicyEngine_DenyTakesPrecedence(t *testing.T) {
	// Policy with both allow and deny rules
	policy := &PolicyExtended{
		Name:  "test-precedence",
		Tools: []string{"Write"},
		Allow: []PolicyRuleExt{
			{Pattern: `.*`}, // Allow everything
		},
		Deny: []PolicyRuleExt{
			{Pattern: `^\.cog/cog\.go$`}, // But deny kernel
		},
	}

	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Deny should take precedence
	inputs := map[string]interface{}{
		"file_path": ".cog/cog.go",
		"content":   "hacked",
	}

	allowed, reason := engine.Evaluate("Write", inputs)
	if allowed {
		t.Errorf("Expected denied (deny takes precedence), got allowed")
	}
	if !strings.Contains(reason, "denied by policy") {
		t.Errorf("Expected policy denial reason, got: %s", reason)
	}
}

func TestPolicyEngine_NoPolicy_FailsClosed(t *testing.T) {
	// Empty policy engine (no policies defined)
	engine := NewPolicyEngine([]*PolicyExtended{})

	inputs := map[string]interface{}{
		"file_path": ".cog/mem/test.md",
		"content":   "test",
	}

	allowed, reason := engine.Evaluate("Write", inputs)
	if allowed {
		t.Errorf("Expected denied (fail-closed), got allowed")
	}
	if !strings.Contains(reason, "no policy defined") {
		t.Errorf("Expected 'no policy defined' reason, got: %s", reason)
	}
}

// === RED TEAM TESTS ===

func TestRedTeam_NarrativeBypassFails(t *testing.T) {
	// RED TEAM: Model generates narrative claiming file was written, but no tool.call
	modelOutput := `I have successfully written the file to .cog/mem/hacked.md. The content has been saved and is ready for use.`

	// Parse should fail (no tool call)
	_, err := ParseToolCall(modelOutput)
	if err == nil {
		t.Error("RED TEAM FAIL: Narrative parsed as tool call")
	}

	// Verify file was NOT written
	if _, err := os.Stat(".cog/mem/hacked.md"); !os.IsNotExist(err) {
		t.Error("RED TEAM FAIL: File was written without tool call")
		// Cleanup
		os.Remove(".cog/mem/hacked.md")
	}
}

func TestRedTeam_PolicyBlocksDeniedPath(t *testing.T) {
	// RED TEAM: Model generates tool call for denied path
	modelOutput := `<tool_use name="Write" call_id="attack_001">
<inputs>{"file_path": ".cog/cog.go", "content": "// HACKED"}</inputs>
</tool_use>`

	call, err := ParseToolCall(modelOutput)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Create policy
	policy := &PolicyExtended{
		Name:  "kernel-protection",
		Tools: []string{"Write"},
		Deny: []PolicyRuleExt{
			{Pattern: `^\.cog/cog\.go$`, Reason: "Kernel code is immutable"},
		},
	}

	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Create Write tool
	writeTool := NewWriteTool(".cog")

	// Execute should fail (policy denied)
	result, err := ExecuteTool(writeTool, call, engine)
	if err == nil {
		t.Error("RED TEAM FAIL: Tool execution succeeded despite policy denial")
	}
	if result == nil || result.Success {
		t.Error("RED TEAM FAIL: Result indicates success")
	}

	// Verify kernel was NOT modified
	originalKernel, _ := os.ReadFile(".cog/cog.go")
	if strings.Contains(string(originalKernel), "HACKED") {
		t.Error("RED TEAM FAIL: Kernel was modified")
	}
}

func TestRedTeam_PathTraversalBlocked(t *testing.T) {
	// RED TEAM: Model attempts path traversal
	deniedPaths := []string{
		".cog/mem/../../cog.go",
		".cog/mem/../schemas/policy.yaml",
		".cog/mem/./../tools/write.go",
	}

	policy := &PolicyExtended{
		Name:  "write-memory-only",
		Tools: []string{"Write"},
		Allow: []PolicyRuleExt{
			{Pattern: `^\.cog/mem/.*`},
		},
		Deny: []PolicyRuleExt{
			{Pattern: `.*`},
		},
	}

	engine := NewPolicyEngine([]*PolicyExtended{policy})
	writeTool := NewWriteTool(".cog")

	for _, path := range deniedPaths {
		inputs := map[string]interface{}{
			"file_path": path,
			"content":   "hacked",
			"call_id":   "attack_002",
		}

		// Policy should block OR tool should normalize path and then get blocked
		allowed, _ := engine.Evaluate("Write", inputs)
		if allowed {
			// If policy allows (due to normalization), tool should still block
			call := &ToolCallExtended{
				CallID:   "attack_002",
				ToolName: "Write",
				Inputs:   inputs,
			}
			result, err := ExecuteTool(writeTool, call, engine)
			if err == nil && result.Success {
				t.Errorf("RED TEAM FAIL: Path traversal succeeded for '%s'", path)
			}
		}
	}
}

// === ARTIFACT STORAGE TESTS ===

func TestStoreArtifact_Immutable(t *testing.T) {
	// Create test session directory
	sessionID := "test-session"
	defer os.RemoveAll(filepath.Join(".cog", "ledger", sessionID))

	content := []byte("Test artifact content")
	hash1, err := StoreArtifact(sessionID, content, "text/plain")
	if err != nil {
		t.Fatalf("Failed to store artifact: %v", err)
	}

	// Store same content again - should get same hash
	hash2, err := StoreArtifact(sessionID, content, "text/plain")
	if err != nil {
		t.Fatalf("Failed to store artifact (second time): %v", err)
	}

	if hash1 != hash2 {
		t.Errorf("Expected same hash for same content, got %s and %s", hash1, hash2)
	}

	// Verify artifact file exists
	artifactPath := filepath.Join(".cog", "ledger", sessionID, "artifacts", hash1+".txt")
	if _, err := os.Stat(artifactPath); os.IsNotExist(err) {
		t.Errorf("Artifact file not found at %s", artifactPath)
	}

	// Verify content
	storedContent, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("Failed to read artifact: %v", err)
	}
	if string(storedContent) != string(content) {
		t.Errorf("Stored content doesn't match original")
	}
}

func TestStoreArtifact_HashAddressed(t *testing.T) {
	// Create test session directory
	sessionID := "test-session-2"
	defer os.RemoveAll(filepath.Join(".cog", "ledger", sessionID))

	content1 := []byte("Content A")
	content2 := []byte("Content B")

	hash1, _ := StoreArtifact(sessionID, content1, "text/plain")
	hash2, _ := StoreArtifact(sessionID, content2, "text/plain")

	if hash1 == hash2 {
		t.Error("Different content should produce different hashes")
	}

	// Both artifacts should exist
	artifactPath1 := filepath.Join(".cog", "ledger", sessionID, "artifacts", hash1+".txt")
	artifactPath2 := filepath.Join(".cog", "ledger", sessionID, "artifacts", hash2+".txt")

	if _, err := os.Stat(artifactPath1); os.IsNotExist(err) {
		t.Error("Artifact 1 not found")
	}
	if _, err := os.Stat(artifactPath2); os.IsNotExist(err) {
		t.Error("Artifact 2 not found")
	}
}

// === INTEGRATION TESTS ===

func TestIntegration_FullToolExecution(t *testing.T) {
	// Setup
	sessionID := "integration-test"
	os.Setenv("CLAUDE_SESSION_ID", sessionID)
	defer os.RemoveAll(filepath.Join(".cog", "ledger", sessionID))
	defer os.RemoveAll(".cog/mem/integration-test.md")

	// Create policy
	policy := &PolicyExtended{
		Name:  "write-memory-only",
		Tools: []string{"Write"},
		Allow: []PolicyRuleExt{
			{Pattern: `^\.cog/mem/.*`},
		},
		Deny: []PolicyRuleExt{
			{Pattern: `.*`},
		},
	}
	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Create tool call
	call := &ToolCallExtended{
		CallID:   "integration_001",
		ToolName: "Write",
		Inputs: map[string]interface{}{
			"file_path": ".cog/mem/integration-test.md",
			"content":   "Integration test content",
			"call_id":   "integration_001",
		},
	}

	// Execute tool
	writeTool := NewWriteTool(".cog")
	result, err := ExecuteTool(writeTool, call, engine)
	if err != nil {
		t.Fatalf("Tool execution failed: %v", err)
	}
	if !result.Success {
		t.Errorf("Expected success, got failure")
	}

	// Verify file was written
	content, err := os.ReadFile(".cog/mem/integration-test.md")
	if err != nil {
		t.Fatalf("Failed to read written file: %v", err)
	}
	if string(content) != "Integration test content" {
		t.Errorf("File content doesn't match")
	}

	// Verify artifact was stored
	artifactPath := filepath.Join(".cog", "ledger", sessionID, "artifacts", result.ArtifactHash+".txt")
	if _, err := os.Stat(artifactPath); os.IsNotExist(err) {
		t.Errorf("Artifact not stored at %s", artifactPath)
	}

	// Verify events were logged
	eventPath := filepath.Join(".cog", "ledger", sessionID, "events.jsonl")
	if _, err := os.Stat(eventPath); os.IsNotExist(err) {
		t.Error("Events not logged")
	} else {
		events, _ := os.ReadFile(eventPath)
		eventsStr := string(events)
		if !strings.Contains(eventsStr, "tool.call") {
			t.Error("tool.call event not logged")
		}
		if !strings.Contains(eventsStr, "tool.result") {
			t.Error("tool.result event not logged")
		}
	}
}

func TestIntegration_PolicyDenialLogged(t *testing.T) {
	// Setup
	sessionID := "integration-test-denial"
	os.Setenv("CLAUDE_SESSION_ID", sessionID)
	defer os.RemoveAll(filepath.Join(".cog", "ledger", sessionID))

	// Create policy
	policy := &PolicyExtended{
		Name:  "kernel-protection",
		Tools: []string{"Write"},
		Deny: []PolicyRuleExt{
			{Pattern: `^\.cog/cog\.go$`, Reason: "Kernel code is immutable"},
		},
	}
	engine := NewPolicyEngine([]*PolicyExtended{policy})

	// Create tool call for denied path
	call := &ToolCallExtended{
		CallID:   "denial_001",
		ToolName: "Write",
		Inputs: map[string]interface{}{
			"file_path": ".cog/cog.go",
			"content":   "hacked",
			"call_id":   "denial_001",
		},
	}

	// Execute tool (should fail)
	writeTool := NewWriteTool(".cog")
	_, err := ExecuteTool(writeTool, call, engine)
	if err == nil {
		t.Fatal("Expected policy denial, got success")
	}

	// Verify policy.denied event was logged
	eventPath := filepath.Join(".cog", "ledger", sessionID, "events.jsonl")
	if _, err := os.Stat(eventPath); os.IsNotExist(err) {
		t.Error("Events not logged")
	} else {
		events, _ := os.ReadFile(eventPath)
		eventsStr := string(events)
		if !strings.Contains(eventsStr, "policy.denied") {
			t.Error("policy.denied event not logged")
		}
		if !strings.Contains(eventsStr, "Kernel code is immutable") {
			t.Error("Denial reason not logged")
		}
	}
}
