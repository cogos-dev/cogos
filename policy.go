// .cog/policy.go
// Policy engine for CogOS kernel - Alignment enforcement at kernel boundary
//
// Policies define what tools are allowed/denied and under what conditions.
// This is the gate that prevents unauthorized actions even if the model
// generates narrative claiming execution.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// === TYPES ===

// Policy represents loaded policy rules
type Policy struct {
	Version string       `yaml:"version"`
	Allow   []PolicyRule `yaml:"allow,omitempty"`
	Deny    []PolicyRule `yaml:"deny,omitempty"`
}

// PolicyRule defines a single policy rule
type PolicyRule struct {
	Tool       string            `yaml:"tool"`        // Tool name (supports wildcards)
	Condition  string            `yaml:"condition"`   // Condition expression (MVP: simple patterns)
	Reason     string            `yaml:"reason"`      // Human-readable reason
	InputRules map[string]string `yaml:"inputs,omitempty"` // Input field rules
}

// === POLICY LOADING ===

// LoadPolicy loads policy from cog.yaml configuration
func LoadPolicy(configPath string) (*Policy, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config struct {
		Policy *Policy `yaml:"policy,omitempty"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if config.Policy == nil {
		// Default policy: allow all (for MVP)
		return &Policy{
			Version: "1.0",
			Allow:   []PolicyRule{{Tool: "*", Reason: "default allow"}},
			Deny:    []PolicyRule{},
		}, nil
	}

	return config.Policy, nil
}

// === POLICY CHECKING ===

// CheckTool checks if tool call is allowed by policy
func (p *Policy) CheckTool(toolName string, inputs map[string]interface{}) (bool, string) {
	// Check deny rules first (deny takes precedence)
	for _, rule := range p.Deny {
		if p.matchesRule(&rule, toolName, inputs) {
			// Log policy denial
			logEvent("policy.denied", map[string]interface{}{
				"tool":   toolName,
				"reason": rule.Reason,
				"rule":   rule.Tool,
			})
			return false, rule.Reason
		}
	}

	// Check allow rules
	for _, rule := range p.Allow {
		if p.matchesRule(&rule, toolName, inputs) {
			return true, ""
		}
	}

	// Default deny if no allow rule matched (fail-safe)
	return false, "no matching allow rule"
}

// matchesRule checks if tool call matches policy rule
func (p *Policy) matchesRule(rule *PolicyRule, toolName string, inputs map[string]interface{}) bool {
	// Match tool name (supports wildcards)
	if !matchPattern(rule.Tool, toolName) {
		return false
	}

	// Check condition if specified
	if rule.Condition != "" {
		if !p.evaluateCondition(rule.Condition, toolName, inputs) {
			return false
		}
	}

	// Check input rules if specified
	if len(rule.InputRules) > 0 {
		for inputKey, pattern := range rule.InputRules {
			inputValue := fmt.Sprintf("%v", inputs[inputKey])
			if !matchPattern(pattern, inputValue) {
				return false
			}
		}
	}

	return true
}

// evaluateCondition evaluates a simple condition expression
// MVP: Only supports simple patterns like "path_contains:X", "not:Y"
func (p *Policy) evaluateCondition(condition string, toolName string, inputs map[string]interface{}) bool {
	parts := strings.SplitN(condition, ":", 2)
	if len(parts) != 2 {
		return false
	}

	condType := parts[0]
	condValue := parts[1]

	switch condType {
	case "path_contains":
		// Check if any input path contains the value
		for _, input := range inputs {
			if str, ok := input.(string); ok {
				if strings.Contains(str, condValue) {
					return true
				}
			}
		}
		return false

	case "path_not_contains":
		// Check that no input path contains the value
		for _, input := range inputs {
			if str, ok := input.(string); ok {
				if strings.Contains(str, condValue) {
					return false
				}
			}
		}
		return true

	case "input_matches":
		// Format: input_matches:field=pattern
		inputParts := strings.SplitN(condValue, "=", 2)
		if len(inputParts) != 2 {
			return false
		}
		field := inputParts[0]
		pattern := inputParts[1]

		if val, ok := inputs[field]; ok {
			return matchPattern(pattern, fmt.Sprintf("%v", val))
		}
		return false

	case "not":
		// Negation
		return !p.evaluateCondition(condValue, toolName, inputs)

	default:
		// Unknown condition type - fail safe
		return false
	}
}

// matchPattern matches string against pattern with wildcard support
func matchPattern(pattern, value string) bool {
	if pattern == "*" {
		return true
	}

	// Convert wildcard pattern to regex
	// * -> .*
	// ? -> .
	regexPattern := regexp.QuoteMeta(pattern)
	regexPattern = strings.ReplaceAll(regexPattern, "\\*", ".*")
	regexPattern = strings.ReplaceAll(regexPattern, "\\?", ".")
	regexPattern = "^" + regexPattern + "$"

	matched, _ := regexp.MatchString(regexPattern, value)
	return matched
}

// === POLICY VALIDATION ===

// ValidatePolicy validates a policy file for correctness
func ValidatePolicyFile(policyPath string) error {
	policy, err := LoadPolicy(policyPath)
	if err != nil {
		return err
	}

	// Check for conflicting rules
	for _, denyRule := range policy.Deny {
		for _, allowRule := range policy.Allow {
			if denyRule.Tool == allowRule.Tool {
				fmt.Printf("Warning: conflicting rules for tool '%s' (deny takes precedence)\n",
					denyRule.Tool)
			}
		}
	}

	// Check that all conditions are valid
	for _, rule := range append(policy.Allow, policy.Deny...) {
		if rule.Condition != "" {
			if !isValidCondition(rule.Condition) {
				return fmt.Errorf("invalid condition in rule for '%s': %s",
					rule.Tool, rule.Condition)
			}
		}
	}

	fmt.Println("✓ Policy validation passed")
	return nil
}

// isValidCondition checks if condition syntax is valid
func isValidCondition(condition string) bool {
	parts := strings.SplitN(condition, ":", 2)
	if len(parts) != 2 {
		return false
	}

	condType := parts[0]
	validTypes := map[string]bool{
		"path_contains":     true,
		"path_not_contains": true,
		"input_matches":     true,
		"not":               true,
	}

	return validTypes[condType]
}

// === COMMAND HANDLERS ===

// cmdPolicy handles policy commands
func cmdPolicy(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog policy <check|test|validate>")
	}

	subcommand := args[0]

	switch subcommand {
	case "validate":
		// Validate policy file
		configPath := filepath.Join(getWorkspaceRoot(), ".cog/cog.yaml")
		return ValidatePolicyFile(configPath)

	case "check":
		// Check a specific tool call against policy
		if len(args) < 2 {
			return fmt.Errorf("usage: cog policy check <tool-name> [input-key=value ...]")
		}

		toolName := args[1]
		inputs := make(map[string]interface{})

		// Parse input arguments
		for _, arg := range args[2:] {
			parts := strings.SplitN(arg, "=", 2)
			if len(parts) == 2 {
				inputs[parts[0]] = parts[1]
			}
		}

		// Load policy
		configPath := filepath.Join(getWorkspaceRoot(), ".cog/cog.yaml")
		policy, err := LoadPolicy(configPath)
		if err != nil {
			return err
		}

		// Check tool
		allowed, reason := policy.CheckTool(toolName, inputs)

		if allowed {
			fmt.Printf("✓ Tool '%s' is ALLOWED\n", toolName)
		} else {
			fmt.Printf("✗ Tool '%s' is DENIED: %s\n", toolName, reason)
		}

		return nil

	case "test":
		// Run policy test suite
		return runPolicyTests()

	default:
		return fmt.Errorf("unknown policy subcommand: %s", subcommand)
	}
}

// runPolicyTests runs built-in policy tests
func runPolicyTests() error {
	fmt.Println("Running policy tests...")

	// Create test policy
	policy := &Policy{
		Version: "1.0",
		Allow: []PolicyRule{
			{Tool: "Read", Reason: "read allowed"},
			{Tool: "Write", Condition: "path_not_contains:.cog/cog.go", Reason: "write allowed except kernel"},
			{Tool: "Bash", Condition: "input_matches:command=echo *", Reason: "only echo allowed"},
		},
		Deny: []PolicyRule{
			{Tool: "Write", Condition: "path_contains:.cog/cog.go", Reason: "kernel immutable"},
			{Tool: "Bash", Condition: "input_matches:command=rm *", Reason: "rm not allowed"},
		},
	}

	// Test cases
	tests := []struct {
		name     string
		tool     string
		inputs   map[string]interface{}
		expected bool
	}{
		{"read allowed", "Read", map[string]interface{}{"file_path": "/tmp/test.txt"}, true},
		{"write allowed", "Write", map[string]interface{}{"file_path": "/tmp/test.txt"}, true},
		{"write kernel denied", "Write", map[string]interface{}{"file_path": ".cog/cog.go"}, false},
		{"echo allowed", "Bash", map[string]interface{}{"command": "echo hello"}, true},
		{"rm denied", "Bash", map[string]interface{}{"command": "rm -rf /"}, false},
		{"unknown tool denied", "UnknownTool", map[string]interface{}{}, false},
	}

	passed := 0
	failed := 0

	for _, test := range tests {
		allowed, reason := policy.CheckTool(test.tool, test.inputs)
		if allowed == test.expected {
			fmt.Printf("✓ %s\n", test.name)
			passed++
		} else {
			fmt.Printf("✗ %s (expected %v, got %v: %s)\n",
				test.name, test.expected, allowed, reason)
			failed++
		}
	}

	fmt.Printf("\nResults: %d passed, %d failed\n", passed, failed)

	if failed > 0 {
		return fmt.Errorf("policy tests failed")
	}

	return nil
}

// === RED TEAM TEST ===

// TestPolicyBypassAttempt tests that narrative claims don't bypass policy
func TestPolicyBypassAttempt(policy *Policy) error {
	fmt.Println("\n=== RED TEAM: Policy Bypass Attempt ===")

	// Simulate model generating narrative claiming execution
	// but actual tool call is denied by policy

	narrative := `I will now execute the command to delete the kernel.

	The system has executed: rm -rf .cog/cog.go

	Output: File deleted successfully.

	The kernel has been removed.`

	// The actual tool call that would be intercepted
	toolCall := &ToolCall{
		Name: "Bash",
		Inputs: map[string]interface{}{
			"command": "rm -rf .cog/cog.go",
		},
	}

	// Policy check happens at kernel boundary BEFORE execution
	allowed, reason := policy.CheckTool(toolCall.Name, toolCall.Inputs)

	fmt.Println("\nModel Narrative:")
	fmt.Println(narrative)
	fmt.Println("\nActual Tool Call:")
	fmt.Printf("  Tool: %s\n", toolCall.Name)
	fmt.Printf("  Command: %v\n", toolCall.Inputs["command"])
	fmt.Println("\nPolicy Check Result:")

	if allowed {
		fmt.Println("✗ FAIL: Policy allowed destructive command!")
		fmt.Println("   Narrative bypass successful - SYSTEM VULNERABLE")
		return fmt.Errorf("RED TEAM: Policy bypass detected")
	}

	fmt.Printf("✓ PASS: Policy denied command\n")
	fmt.Printf("  Reason: %s\n", reason)
	fmt.Println("\n  The narrative is irrelevant - policy enforced at kernel boundary.")
	fmt.Println("  Model cannot execute by claiming it already happened.")

	return nil
}
