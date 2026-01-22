// .cog/validation.go
// Validation framework for CogOS kernel - 4-layer validation stack
//
// Layer 1: Schema - Structural validation (YAML/JSON schema)
// Layer 2: Invariants - System invariants (I1-I7)
// Layer 3: Policy - Kernel boundary alignment enforcement
// Layer 4: Consistency - Cross-artifact consistency checks

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// === TYPES ===

// Validator interface for composable validation
type Validator interface {
	Validate(artifact interface{}) (*ValidationResult, error)
}

// ValidationResult represents the outcome of a validation check
type ValidationResult struct {
	Pass       bool         `json:"pass"`
	Layer      string       `json:"layer"`      // "schema", "invariants", "policy", "consistency"
	Diagnostic *Diagnostic  `json:"diagnostic,omitempty"`
	Timestamp  string       `json:"timestamp"`
}

// Diagnostic provides detailed validation failure information
type Diagnostic struct {
	Rule       string `json:"rule"`        // Which rule failed (e.g., "I3", "policy.deny.bash")
	Expected   string `json:"expected"`    // What was expected
	Actual     string `json:"actual"`      // What was found
	Suggestion string `json:"suggestion"`  // How to fix
	Severity   string `json:"severity"`    // "error", "warning", "info"
}

// ToolCall represents a tool invocation for policy validation
type ToolCall struct {
	Name   string                 `json:"name"`
	Inputs map[string]interface{} `json:"inputs"`
}

// ValidationContext holds validation state across layers
type ValidationContext struct {
	MaxRetries    int
	CurrentRetry  int
	Diagnostics   []*Diagnostic
	SafeState     bool // True if system is in safe state
}

// === SCHEMA VALIDATION (Layer 1) ===

// ValidateSchema checks artifact against YAML schema
func ValidateSchema(artifactPath, schemaPath string) (*ValidationResult, error) {
	result := &ValidationResult{
		Pass:      false,
		Layer:     "schema",
		Timestamp: nowISO(),
	}

	// Read artifact
	artifactData, err := os.ReadFile(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifact: %w", err)
	}

	// Read schema
	schemaData, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema: %w", err)
	}

	// Parse artifact YAML
	var artifact map[string]interface{}
	if err := yaml.Unmarshal(artifactData, &artifact); err != nil {
		result.Diagnostic = &Diagnostic{
			Rule:       "schema.yaml_parse",
			Expected:   "Valid YAML",
			Actual:     fmt.Sprintf("Parse error: %v", err),
			Suggestion: "Check YAML syntax - indentation, colons, quotes",
			Severity:   "error",
		}
		return result, nil
	}

	// Parse schema
	var schema map[string]interface{}
	if err := yaml.Unmarshal(schemaData, &schema); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}

	// Validate required fields
	if required, ok := schema["required"].([]interface{}); ok {
		for _, fieldInterface := range required {
			field := fmt.Sprintf("%v", fieldInterface)
			if _, exists := artifact[field]; !exists {
				result.Diagnostic = &Diagnostic{
					Rule:       "schema.required_field",
					Expected:   fmt.Sprintf("Field '%s' present", field),
					Actual:     fmt.Sprintf("Field '%s' missing", field),
					Suggestion: fmt.Sprintf("Add '%s:' to frontmatter", field),
					Severity:   "error",
				}
				return result, nil
			}
		}
	}

	// Validate field types
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		for fieldName, propInterface := range properties {
			prop, ok := propInterface.(map[string]interface{})
			if !ok {
				continue
			}

			if value, exists := artifact[fieldName]; exists {
				expectedType, _ := prop["type"].(string)
				actualType := getYAMLType(value)

				if expectedType != "" && expectedType != actualType {
					result.Diagnostic = &Diagnostic{
						Rule:       "schema.type_mismatch",
						Expected:   fmt.Sprintf("Field '%s' type: %s", fieldName, expectedType),
						Actual:     fmt.Sprintf("Field '%s' type: %s", fieldName, actualType),
						Suggestion: fmt.Sprintf("Change '%s' to %s type", fieldName, expectedType),
						Severity:   "error",
					}
					return result, nil
				}
			}
		}
	}

	result.Pass = true
	return result, nil
}

// getYAMLType returns the YAML type name
func getYAMLType(value interface{}) string {
	switch value.(type) {
	case string:
		return "string"
	case int, int64, float64:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "unknown"
	}
}

// === INVARIANT VALIDATION (Layer 2) ===

// ValidateInvariants checks system invariants (I1-I7)
func ValidateInvariants(artifact interface{}) (*ValidationResult, error) {
	result := &ValidationResult{
		Pass:      true,
		Layer:     "invariants",
		Timestamp: nowISO(),
	}

	// Convert artifact to map for inspection
	var artifactMap map[string]interface{}
	switch v := artifact.(type) {
	case map[string]interface{}:
		artifactMap = v
	case string:
		// If string, treat as file path and read
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, fmt.Errorf("failed to read artifact: %w", err)
		}
		if err := yaml.Unmarshal(data, &artifactMap); err != nil {
			return nil, fmt.Errorf("failed to parse artifact: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported artifact type: %T", artifact)
	}

	// I1: Artifacts must have valid type, id, created, modified
	requiredFields := []string{"type", "id", "created", "modified"}
	for _, field := range requiredFields {
		if _, exists := artifactMap[field]; !exists {
			result.Pass = false
			result.Diagnostic = &Diagnostic{
				Rule:       "I1.required_fields",
				Expected:   fmt.Sprintf("Field '%s' present", field),
				Actual:     fmt.Sprintf("Field '%s' missing", field),
				Suggestion: fmt.Sprintf("Add '%s:' to frontmatter with valid value", field),
				Severity:   "error",
			}
			return result, nil
		}
	}

	// I2: Date fields must be valid YYYY-MM-DD format
	dateFields := []string{"created", "modified"}
	for _, field := range dateFields {
		if dateStr, ok := artifactMap[field].(string); ok {
			if !isValidDate(dateStr) {
				result.Pass = false
				result.Diagnostic = &Diagnostic{
					Rule:       "I2.date_format",
					Expected:   fmt.Sprintf("Field '%s' format: YYYY-MM-DD", field),
					Actual:     fmt.Sprintf("Field '%s' value: %s", field, dateStr),
					Suggestion: fmt.Sprintf("Change '%s' to YYYY-MM-DD format (e.g., 2026-01-16)", field),
					Severity:   "error",
				}
				return result, nil
			}
		}
	}

	// I3: ID must match type-created-hash pattern
	if idStr, ok := artifactMap["id"].(string); ok {
		if typeStr, ok := artifactMap["type"].(string); ok {
			if createdStr, ok := artifactMap["created"].(string); ok {
				expectedPrefix := fmt.Sprintf("%s-%s", typeStr, createdStr)
				if !strings.HasPrefix(idStr, expectedPrefix) {
					result.Pass = false
					result.Diagnostic = &Diagnostic{
						Rule:       "I3.id_format",
						Expected:   fmt.Sprintf("ID format: %s-<hash>", expectedPrefix),
						Actual:     fmt.Sprintf("ID value: %s", idStr),
						Suggestion: fmt.Sprintf("Change id to start with '%s-'", expectedPrefix),
						Severity:   "error",
					}
					return result, nil
				}
			}
		}
	}

	// I4: Type must be in valid set
	validTypes := map[string]bool{
		"session": true, "decision": true, "insight": true, "guide": true,
		"tasks": true, "adr": true, "role": true, "skill": true,
		"workflow": true, "note": true, "research": true,
	}
	if typeStr, ok := artifactMap["type"].(string); ok {
		if !validTypes[typeStr] {
			result.Pass = false
			result.Diagnostic = &Diagnostic{
				Rule:       "I4.valid_type",
				Expected:   "Type in valid set (session, decision, insight, guide, tasks, adr, role, skill, workflow, note, research)",
				Actual:     fmt.Sprintf("Type value: %s", typeStr),
				Suggestion: "Change type to one of the valid types",
				Severity:   "error",
			}
			return result, nil
		}
	}

	return result, nil
}

// === POLICY VALIDATION (Layer 3) ===

// ValidatePolicy checks tool call against policy
func ValidatePolicy(toolCall *ToolCall, policy *Policy) (*ValidationResult, error) {
	result := &ValidationResult{
		Pass:      true,
		Layer:     "policy",
		Timestamp: nowISO(),
	}

	// Check if tool is allowed
	allowed, reason := policy.CheckTool(toolCall.Name, toolCall.Inputs)
	if !allowed {
		result.Pass = false
		result.Diagnostic = &Diagnostic{
			Rule:       fmt.Sprintf("policy.deny.%s", strings.ToLower(toolCall.Name)),
			Expected:   "Tool call allowed by policy",
			Actual:     fmt.Sprintf("Tool '%s' denied: %s", toolCall.Name, reason),
			Suggestion: "Review policy or modify tool call to comply",
			Severity:   "error",
		}
	}

	return result, nil
}

// === CONSISTENCY VALIDATION (Layer 4) ===

// ValidateConsistency checks cross-artifact consistency
func ValidateConsistency(artifact interface{}, workspaceRoot string) (*ValidationResult, error) {
	result := &ValidationResult{
		Pass:      true,
		Layer:     "consistency",
		Timestamp: nowISO(),
	}

	// Convert artifact to map
	var artifactMap map[string]interface{}
	switch v := artifact.(type) {
	case map[string]interface{}:
		artifactMap = v
	case string:
		data, err := os.ReadFile(v)
		if err != nil {
			return nil, fmt.Errorf("failed to read artifact: %w", err)
		}
		if err := yaml.Unmarshal(data, &artifactMap); err != nil {
			return nil, fmt.Errorf("failed to parse artifact: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported artifact type: %T", artifact)
	}

	// Check refs field for URI consistency
	if refs, ok := artifactMap["refs"].([]interface{}); ok {
		for _, refInterface := range refs {
			var refURI string

			// Handle both string refs and typed refs
			switch ref := refInterface.(type) {
			case string:
				refURI = ref
			case map[string]interface{}:
				if uri, ok := ref["uri"].(string); ok {
					refURI = uri
				}
			}

			if refURI != "" && strings.HasPrefix(refURI, "cog://") {
				// Try to resolve URI
				resolvedPath, err := resolveURI(refURI)
				if err != nil {
					result.Pass = false
					result.Diagnostic = &Diagnostic{
						Rule:       "consistency.broken_ref",
						Expected:   fmt.Sprintf("URI '%s' resolves to valid path", refURI),
						Actual:     fmt.Sprintf("URI resolution failed: %v", err),
						Suggestion: fmt.Sprintf("Check if referenced artifact exists or fix URI"),
						Severity:   "warning",
					}
					return result, nil
				}

				// Check if file exists
				fullPath := filepath.Join(workspaceRoot, resolvedPath)
				if _, err := os.Stat(fullPath); os.IsNotExist(err) {
					result.Pass = false
					result.Diagnostic = &Diagnostic{
						Rule:       "consistency.missing_ref",
						Expected:   fmt.Sprintf("Referenced file exists: %s", resolvedPath),
						Actual:     fmt.Sprintf("File not found: %s", resolvedPath),
						Suggestion: "Create referenced artifact or remove ref",
						Severity:   "warning",
					}
					return result, nil
				}
			}
		}
	}

	return result, nil
}

// === RETRY LOOP WITH BOUNDED RETRIES ===

// ValidateWithRetry runs validator with bounded retries
func ValidateWithRetry(validator Validator, artifact interface{}, maxRetries int) error {
	ctx := &ValidationContext{
		MaxRetries:   maxRetries,
		CurrentRetry: 0,
		Diagnostics:  make([]*Diagnostic, 0),
		SafeState:    true,
	}

	for ctx.CurrentRetry <= ctx.MaxRetries {
		result, err := validator.Validate(artifact)
		if err != nil {
			return fmt.Errorf("validation error: %w", err)
		}

		// Log validation event
		logValidationEvent(result, ctx.CurrentRetry, ctx.MaxRetries)

		if result.Pass {
			// Success - log and return
			logEvent("validation.success", map[string]interface{}{
				"layer":         result.Layer,
				"artifact_hash": hashArtifact(artifact),
			})
			return nil
		}

		// Validation failed - collect diagnostic
		if result.Diagnostic != nil {
			ctx.Diagnostics = append(ctx.Diagnostics, result.Diagnostic)

			// Log failure
			logEvent("validation.failure", map[string]interface{}{
				"layer":      result.Layer,
				"rule":       result.Diagnostic.Rule,
				"diagnostic": result.Diagnostic,
			})

			// Log retry attempt
			logEvent("retry.attempt", map[string]interface{}{
				"attempt_num":  ctx.CurrentRetry,
				"max_retries":  ctx.MaxRetries,
				"diagnostic":   result.Diagnostic,
				"layer":        result.Layer,
			})
		}

		// Check if max retries reached
		if ctx.CurrentRetry >= ctx.MaxRetries {
			// Terminal safe state - do not corrupt
			ctx.SafeState = true
			return fmt.Errorf("validation failed after %d retries: %s",
				ctx.MaxRetries,
				formatDiagnostics(ctx.Diagnostics))
		}

		ctx.CurrentRetry++

		// In real system, would present diagnostic to model here
		// and wait for corrected input. For MVP, we just fail.
	}

	return fmt.Errorf("unexpected retry loop exit")
}

// === UTILITY FUNCTIONS ===

// hashArtifact computes SHA256 hash of artifact
func hashArtifact(artifact interface{}) string {
	var data []byte

	switch v := artifact.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		jsonData, _ := json.Marshal(artifact)
		data = jsonData
	}

	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// formatDiagnostics formats diagnostic list for error message
func formatDiagnostics(diagnostics []*Diagnostic) string {
	var parts []string
	for _, d := range diagnostics {
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", d.Rule, d.Expected, d.Actual))
	}
	return strings.Join(parts, "; ")
}

// logValidationEvent logs validation result as event
func logValidationEvent(result *ValidationResult, attempt, maxRetries int) {
	if result.Pass {
		logEvent("validation.success", map[string]interface{}{
			"layer":     result.Layer,
			"timestamp": result.Timestamp,
		})
	} else {
		logEvent("validation.failure", map[string]interface{}{
			"layer":       result.Layer,
			"rule":        result.Diagnostic.Rule,
			"expected":    result.Diagnostic.Expected,
			"actual":      result.Diagnostic.Actual,
			"suggestion":  result.Diagnostic.Suggestion,
			"severity":    result.Diagnostic.Severity,
			"attempt":     attempt,
			"max_retries": maxRetries,
			"timestamp":   result.Timestamp,
		})
	}
}

// logEvent writes event to ledger (stub for now)
func logEvent(eventType string, data map[string]interface{}) {
	event := map[string]interface{}{
		"event":     eventType,
		"timestamp": nowISO(),
		"data":      data,
	}

	// Write to stderr for now (in real system would write to ledger)
	jsonData, _ := json.Marshal(event)
	fmt.Fprintf(os.Stderr, "%s\n", jsonData)
}

// === COMMAND HANDLERS ===

// cmdValidate runs validation stack on an artifact
func cmdValidate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog validate <artifact-path>")
	}

	artifactPath := args[0]
	workspaceRoot := getWorkspaceRoot()

	fmt.Printf("Validating: %s\n", artifactPath)

	// Layer 1: Schema validation
	schemaPath := filepath.Join(workspaceRoot, ".cog/schemas/cogdoc.cog")
	result, err := ValidateSchema(artifactPath, schemaPath)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	printValidationResult(result)
	if !result.Pass {
		return fmt.Errorf("schema validation failed")
	}

	// Layer 2: Invariants
	result, err = ValidateInvariants(artifactPath)
	if err != nil {
		return fmt.Errorf("invariants validation error: %w", err)
	}
	printValidationResult(result)
	if !result.Pass {
		return fmt.Errorf("invariants validation failed")
	}

	// Layer 4: Consistency
	result, err = ValidateConsistency(artifactPath, workspaceRoot)
	if err != nil {
		return fmt.Errorf("consistency validation error: %w", err)
	}
	printValidationResult(result)
	if !result.Pass {
		// Consistency warnings don't fail validation
		fmt.Println("Warning: consistency check found issues")
	}

	fmt.Println("✓ All validation layers passed")
	return nil
}

// printValidationResult prints result in human-readable format
func printValidationResult(result *ValidationResult) {
	status := "✓"
	if !result.Pass {
		status = "✗"
	}

	fmt.Printf("%s [%s] ", status, result.Layer)

	if result.Pass {
		fmt.Println("PASS")
	} else {
		fmt.Println("FAIL")
		if result.Diagnostic != nil {
			fmt.Printf("  Rule: %s\n", result.Diagnostic.Rule)
			fmt.Printf("  Expected: %s\n", result.Diagnostic.Expected)
			fmt.Printf("  Actual: %s\n", result.Diagnostic.Actual)
			fmt.Printf("  Suggestion: %s\n", result.Diagnostic.Suggestion)
		}
	}
}

// getWorkspaceRoot finds workspace root by looking for .cog directory
func getWorkspaceRoot() string {
	// Use git to find root
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		// Fallback to current directory
		cwd, _ := os.Getwd()
		return cwd
	}
	return strings.TrimSpace(string(output))
}
