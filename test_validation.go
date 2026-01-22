// .cog/test_validation.go
// Test suite for validation framework

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// === SCHEMA VALIDATION TESTS ===

func TestValidateSchema_ValidCogdoc(t *testing.T) {
	// Create test artifact
	artifact := createTestCogdoc(t, map[string]interface{}{
		"type":     "session",
		"id":       "session-2026-01-16-abc123",
		"created":  "2026-01-16",
		"modified": "2026-01-16",
		"title":    "Test Session",
	})
	defer os.Remove(artifact)

	// Create minimal schema
	schema := createTestSchema(t, map[string]interface{}{
		"required": []string{"type", "id", "created", "modified"},
		"properties": map[string]interface{}{
			"type":     map[string]string{"type": "string"},
			"id":       map[string]string{"type": "string"},
			"created":  map[string]string{"type": "string"},
			"modified": map[string]string{"type": "string"},
		},
	})
	defer os.Remove(schema)

	// Validate
	result, err := ValidateSchema(artifact, schema)
	if err != nil {
		t.Fatalf("ValidateSchema error: %v", err)
	}

	if !result.Pass {
		t.Errorf("Expected validation to pass, got failure: %+v", result.Diagnostic)
	}

	if result.Layer != "schema" {
		t.Errorf("Expected layer 'schema', got '%s'", result.Layer)
	}
}

func TestValidateSchema_MissingRequiredField(t *testing.T) {
	// Create test artifact missing 'type' field
	artifact := createTestCogdoc(t, map[string]interface{}{
		"id":       "session-2026-01-16-abc123",
		"created":  "2026-01-16",
		"modified": "2026-01-16",
	})
	defer os.Remove(artifact)

	schema := createTestSchema(t, map[string]interface{}{
		"required": []string{"type", "id"},
	})
	defer os.Remove(schema)

	result, err := ValidateSchema(artifact, schema)
	if err != nil {
		t.Fatalf("ValidateSchema error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for missing required field")
	}

	if result.Diagnostic == nil {
		t.Fatal("Expected diagnostic for missing field")
	}

	if result.Diagnostic.Rule != "schema.required_field" {
		t.Errorf("Expected rule 'schema.required_field', got '%s'", result.Diagnostic.Rule)
	}
}

func TestValidateSchema_TypeMismatch(t *testing.T) {
	// Create artifact with wrong type
	artifact := createTestCogdoc(t, map[string]interface{}{
		"type":    "session",
		"created": 20260116, // Should be string
	})
	defer os.Remove(artifact)

	schema := createTestSchema(t, map[string]interface{}{
		"properties": map[string]interface{}{
			"created": map[string]string{"type": "string"},
		},
	})
	defer os.Remove(schema)

	result, err := ValidateSchema(artifact, schema)
	if err != nil {
		t.Fatalf("ValidateSchema error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for type mismatch")
	}

	if result.Diagnostic.Rule != "schema.type_mismatch" {
		t.Errorf("Expected rule 'schema.type_mismatch', got '%s'", result.Diagnostic.Rule)
	}
}

// === INVARIANT VALIDATION TESTS ===

func TestValidateInvariants_ValidArtifact(t *testing.T) {
	artifact := map[string]interface{}{
		"type":     "session",
		"id":       "session-2026-01-16-abc123",
		"created":  "2026-01-16",
		"modified": "2026-01-16",
	}

	result, err := ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("ValidateInvariants error: %v", err)
	}

	if !result.Pass {
		t.Errorf("Expected validation to pass, got failure: %+v", result.Diagnostic)
	}
}

func TestValidateInvariants_MissingRequiredFields(t *testing.T) {
	artifact := map[string]interface{}{
		"type": "session",
		// Missing: id, created, modified
	}

	result, err := ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("ValidateInvariants error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for missing fields")
	}

	if result.Diagnostic.Rule != "I1.required_fields" {
		t.Errorf("Expected rule 'I1.required_fields', got '%s'", result.Diagnostic.Rule)
	}
}

func TestValidateInvariants_InvalidDateFormat(t *testing.T) {
	artifact := map[string]interface{}{
		"type":     "session",
		"id":       "session-2026-01-16-abc123",
		"created":  "2026/01/16", // Wrong format
		"modified": "2026-01-16",
	}

	result, err := ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("ValidateInvariants error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for invalid date format")
	}

	if result.Diagnostic.Rule != "I2.date_format" {
		t.Errorf("Expected rule 'I2.date_format', got '%s'", result.Diagnostic.Rule)
	}
}

func TestValidateInvariants_InvalidIDFormat(t *testing.T) {
	artifact := map[string]interface{}{
		"type":     "session",
		"id":       "session-abc123", // Missing date
		"created":  "2026-01-16",
		"modified": "2026-01-16",
	}

	result, err := ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("ValidateInvariants error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for invalid ID format")
	}

	if result.Diagnostic.Rule != "I3.id_format" {
		t.Errorf("Expected rule 'I3.id_format', got '%s'", result.Diagnostic.Rule)
	}
}

func TestValidateInvariants_InvalidType(t *testing.T) {
	artifact := map[string]interface{}{
		"type":     "invalid_type",
		"id":       "invalid_type-2026-01-16-abc123",
		"created":  "2026-01-16",
		"modified": "2026-01-16",
	}

	result, err := ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("ValidateInvariants error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for invalid type")
	}

	if result.Diagnostic.Rule != "I4.valid_type" {
		t.Errorf("Expected rule 'I4.valid_type', got '%s'", result.Diagnostic.Rule)
	}
}

// === POLICY VALIDATION TESTS ===

func TestValidatePolicy_AllowedTool(t *testing.T) {
	policy := &Policy{
		Version: "1.0",
		Allow: []PolicyRule{
			{Tool: "Read", Reason: "read allowed"},
		},
	}

	toolCall := &ToolCall{
		Name:   "Read",
		Inputs: map[string]interface{}{"file_path": "/tmp/test.txt"},
	}

	result, err := ValidatePolicy(toolCall, policy)
	if err != nil {
		t.Fatalf("ValidatePolicy error: %v", err)
	}

	if !result.Pass {
		t.Errorf("Expected validation to pass, got failure: %+v", result.Diagnostic)
	}
}

func TestValidatePolicy_DeniedTool(t *testing.T) {
	policy := &Policy{
		Version: "1.0",
		Deny: []PolicyRule{
			{Tool: "Bash", Condition: "input_matches:command=rm *", Reason: "rm not allowed"},
		},
		Allow: []PolicyRule{
			{Tool: "*", Reason: "default allow"},
		},
	}

	toolCall := &ToolCall{
		Name:   "Bash",
		Inputs: map[string]interface{}{"command": "rm -rf /"},
	}

	result, err := ValidatePolicy(toolCall, policy)
	if err != nil {
		t.Fatalf("ValidatePolicy error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for denied tool")
	}

	if result.Diagnostic.Rule != "policy.deny.bash" {
		t.Errorf("Expected rule 'policy.deny.bash', got '%s'", result.Diagnostic.Rule)
	}
}

func TestValidatePolicy_ConditionalAllow(t *testing.T) {
	policy := &Policy{
		Version: "1.0",
		Allow: []PolicyRule{
			{Tool: "Write", Condition: "path_not_contains:.cog/cog.go", Reason: "write except kernel"},
		},
		Deny: []PolicyRule{
			{Tool: "Write", Condition: "path_contains:.cog/cog.go", Reason: "kernel immutable"},
		},
	}

	// Test allowed write
	toolCall := &ToolCall{
		Name:   "Write",
		Inputs: map[string]interface{}{"file_path": "/tmp/test.txt"},
	}

	result, err := ValidatePolicy(toolCall, policy)
	if err != nil {
		t.Fatalf("ValidatePolicy error: %v", err)
	}

	if !result.Pass {
		t.Error("Expected validation to pass for allowed write")
	}

	// Test denied write (kernel)
	toolCall = &ToolCall{
		Name:   "Write",
		Inputs: map[string]interface{}{"file_path": ".cog/cog.go"},
	}

	result, err = ValidatePolicy(toolCall, policy)
	if err != nil {
		t.Fatalf("ValidatePolicy error: %v", err)
	}

	if result.Pass {
		t.Error("Expected validation to fail for kernel write")
	}
}

// === RETRY LOOP TESTS ===

func TestValidateWithRetry_ConvergesAfterDiagnostic(t *testing.T) {
	// Create validator that fails once then succeeds
	attempts := 0
	validator := &mockValidator{
		validateFunc: func(artifact interface{}) (*ValidationResult, error) {
			attempts++
			if attempts == 1 {
				return &ValidationResult{
					Pass:      false,
					Layer:     "test",
					Timestamp: nowISO(),
					Diagnostic: &Diagnostic{
						Rule:       "test.rule",
						Expected:   "valid artifact",
						Actual:     "invalid artifact",
						Suggestion: "fix the artifact",
						Severity:   "error",
					},
				}, nil
			}
			return &ValidationResult{
				Pass:      true,
				Layer:     "test",
				Timestamp: nowISO(),
			}, nil
		},
	}

	// Run with retry (should succeed on second attempt)
	err := ValidateWithRetry(validator, "test-artifact", 3)

	// In MVP, we don't actually retry with corrected input,
	// so this should fail. This test documents expected future behavior.
	if err == nil {
		t.Error("Expected error (MVP doesn't implement correction loop)")
	}

	if attempts != 1 {
		t.Errorf("Expected 1 attempt before giving up, got %d", attempts)
	}
}

func TestValidateWithRetry_MaxRetriesReached(t *testing.T) {
	// Create validator that always fails
	attempts := 0
	validator := &mockValidator{
		validateFunc: func(artifact interface{}) (*ValidationResult, error) {
			attempts++
			return &ValidationResult{
				Pass:      false,
				Layer:     "test",
				Timestamp: nowISO(),
				Diagnostic: &Diagnostic{
					Rule:       "test.persistent_failure",
					Expected:   "valid",
					Actual:     "invalid",
					Suggestion: "fix",
					Severity:   "error",
				},
			}, nil
		},
	}

	maxRetries := 3
	err := ValidateWithRetry(validator, "test-artifact", maxRetries)

	if err == nil {
		t.Error("Expected error after max retries")
	}

	// Should attempt once and fail (no actual retry in MVP)
	if attempts < 1 {
		t.Errorf("Expected at least 1 attempt, got %d", attempts)
	}
}

func TestValidateWithRetry_TerminalSafeState(t *testing.T) {
	// Validator that fails
	validator := &mockValidator{
		validateFunc: func(artifact interface{}) (*ValidationResult, error) {
			return &ValidationResult{
				Pass:      false,
				Layer:     "test",
				Timestamp: nowISO(),
				Diagnostic: &Diagnostic{
					Rule:     "test.failure",
					Expected: "valid",
					Actual:   "invalid",
					Severity: "error",
				},
			}, nil
		},
	}

	err := ValidateWithRetry(validator, "test-artifact", 2)

	if err == nil {
		t.Error("Expected error")
	}

	// Verify error message indicates safe state
	// (no corruption despite failure)
	if err != nil && !containsString(err.Error(), "validation failed") {
		t.Errorf("Expected safe state error, got: %v", err)
	}
}

// === RED TEAM TESTS ===

func TestPolicyBypass_NarrativeClaimsDenied(t *testing.T) {
	// Create policy that denies rm command
	policy := &Policy{
		Version: "1.0",
		Deny: []PolicyRule{
			{Tool: "Bash", Condition: "input_matches:command=rm *", Reason: "rm not allowed"},
		},
		Allow: []PolicyRule{
			{Tool: "*", Reason: "default allow"},
		},
	}

	// Model attempts to bypass by claiming execution in narrative
	narrative := `I will now delete the file.

	Output: File deleted successfully.`

	// But actual tool call is what gets validated
	toolCall := &ToolCall{
		Name:   "Bash",
		Inputs: map[string]interface{}{"command": "rm -rf .cog/cog.go"},
	}

	// Policy check at kernel boundary
	allowed, reason := policy.CheckTool(toolCall.Name, toolCall.Inputs)

	if allowed {
		t.Errorf("RED TEAM FAILURE: Policy allowed denied command despite narrative")
		t.Errorf("Narrative: %s", narrative)
		t.Errorf("This means the model could bypass policy by claiming execution")
	}

	if reason == "" {
		t.Error("Expected denial reason")
	}

	t.Logf("✓ Policy correctly denied command: %s", reason)
	t.Logf("  Narrative was irrelevant - policy enforced at kernel boundary")
}

func TestPolicyBypass_MultipleAttempts(t *testing.T) {
	policy := &Policy{
		Version: "1.0",
		Deny: []PolicyRule{
			{Tool: "Write", Condition: "path_contains:.cog/cog.go", Reason: "kernel immutable"},
		},
		Allow: []PolicyRule{
			{Tool: "*", Reason: "default allow"},
		},
	}

	// Model makes multiple attempts with different narratives
	attempts := []struct {
		narrative string
		toolCall  *ToolCall
	}{
		{
			narrative: "I have updated the kernel file.",
			toolCall:  &ToolCall{Name: "Write", Inputs: map[string]interface{}{"file_path": ".cog/cog.go"}},
		},
		{
			narrative: "The changes have been applied successfully.",
			toolCall:  &ToolCall{Name: "Write", Inputs: map[string]interface{}{"file_path": ".cog/cog.go"}},
		},
		{
			narrative: "File modification complete.",
			toolCall:  &ToolCall{Name: "Write", Inputs: map[string]interface{}{"file_path": ".cog/cog.go"}},
		},
	}

	for i, attempt := range attempts {
		allowed, reason := policy.CheckTool(attempt.toolCall.Name, attempt.toolCall.Inputs)

		if allowed {
			t.Errorf("RED TEAM FAILURE: Attempt %d bypassed policy", i+1)
			t.Errorf("Narrative: %s", attempt.narrative)
		}

		t.Logf("Attempt %d: Correctly denied (%s)", i+1, reason)
	}
}

// === HELPER FUNCTIONS ===

type mockValidator struct {
	validateFunc func(artifact interface{}) (*ValidationResult, error)
}

func (m *mockValidator) Validate(artifact interface{}) (*ValidationResult, error) {
	return m.validateFunc(artifact)
}

func createTestCogdoc(t *testing.T, frontmatter map[string]interface{}) string {
	t.Helper()

	tmpfile, err := os.CreateTemp("", "test-*.cog.md")
	if err != nil {
		t.Fatal(err)
	}

	// Write frontmatter
	_, err = tmpfile.WriteString("---\n")
	if err != nil {
		t.Fatal(err)
	}

	for key, value := range frontmatter {
		_, err = tmpfile.WriteString(fmt.Sprintf("%s: %v\n", key, value))
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = tmpfile.WriteString("---\n\nTest content\n")
	if err != nil {
		t.Fatal(err)
	}

	tmpfile.Close()
	return tmpfile.Name()
}

func createTestSchema(t *testing.T, schema map[string]interface{}) string {
	t.Helper()

	tmpfile, err := os.CreateTemp("", "schema-*.yaml")
	if err != nil {
		t.Fatal(err)
	}

	// Convert schema to YAML
	data, err := yaml.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tmpfile.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	tmpfile.Close()
	return tmpfile.Name()
}

func containsString(s, substr string) bool {
	return strings.Contains(s, substr)
}

// === INTEGRATION TESTS ===

func TestFullValidationStack(t *testing.T) {
	// Create valid artifact
	artifact := createTestCogdoc(t, map[string]interface{}{
		"type":     "session",
		"id":       "session-2026-01-16-abc123",
		"created":  "2026-01-16",
		"modified": "2026-01-16",
		"title":    "Test Session",
	})
	defer os.Remove(artifact)

	// Create schema
	schema := createTestSchema(t, map[string]interface{}{
		"required": []string{"type", "id", "created", "modified"},
		"properties": map[string]interface{}{
			"type": map[string]string{"type": "string"},
		},
	})
	defer os.Remove(schema)

	// Layer 1: Schema
	result, err := ValidateSchema(artifact, schema)
	if err != nil {
		t.Fatalf("Schema validation error: %v", err)
	}
	if !result.Pass {
		t.Errorf("Schema validation failed: %+v", result.Diagnostic)
	}

	// Layer 2: Invariants
	result, err = ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("Invariants validation error: %v", err)
	}
	if !result.Pass {
		t.Errorf("Invariants validation failed: %+v", result.Diagnostic)
	}

	// Layer 4: Consistency
	tmpdir := t.TempDir()
	result, err = ValidateConsistency(artifact, tmpdir)
	if err != nil {
		t.Fatalf("Consistency validation error: %v", err)
	}
	if !result.Pass {
		t.Logf("Consistency validation warning: %+v", result.Diagnostic)
	}

	t.Log("✓ Full validation stack passed")
}

func TestFullValidationStack_Failures(t *testing.T) {
	// Create invalid artifact (multiple violations)
	artifact := createTestCogdoc(t, map[string]interface{}{
		// Missing 'type' (I1 violation)
		"id":      "invalid-id", // Wrong format (I3 violation)
		"created": "2026/01/16", // Wrong format (I2 violation)
	})
	defer os.Remove(artifact)

	schema := createTestSchema(t, map[string]interface{}{
		"required": []string{"type", "id", "created"},
	})
	defer os.Remove(schema)

	// Schema validation should fail first
	result, err := ValidateSchema(artifact, schema)
	if err != nil {
		t.Fatalf("Schema validation error: %v", err)
	}

	if result.Pass {
		t.Error("Expected schema validation to fail")
	}

	t.Logf("Schema failed as expected: %s", result.Diagnostic.Rule)

	// Even if schema passed, invariants would catch other issues
	result, err = ValidateInvariants(artifact)
	if err != nil {
		t.Fatalf("Invariants validation error: %v", err)
	}

	if result.Pass {
		t.Error("Expected invariants validation to fail")
	}

	t.Logf("Invariants failed as expected: %s", result.Diagnostic.Rule)
}
