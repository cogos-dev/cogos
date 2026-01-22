// Package sdk provides cogdoc validation functionality.
//
// This implements two-tier validation matching the kernel:
// - Structural refs (frontmatter refs field) -> errors if broken
// - Inline refs (markdown body) -> warnings if broken
package sdk

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/cogos-dev/cogos/sdk/types"
	"gopkg.in/yaml.v3"
)

// ValidationResult holds the result of cogdoc validation.
type ValidationResult struct {
	Path           string     `json:"path"`
	Valid          bool       `json:"valid"`
	Errors         []string   `json:"errors,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
	StructuralRefs []TypedRef `json:"structural_refs,omitempty"`
	InlineRefs     []string   `json:"inline_refs,omitempty"`
}

// CogdocFrontmatter represents the parsed frontmatter of a cogdoc.
type CogdocFrontmatter struct {
	Type    string      `yaml:"type"`
	ID      string      `yaml:"id"`
	Title   string      `yaml:"title"`
	Created string      `yaml:"created"`
	Status  string      `yaml:"status,omitempty"`
	Tags    []string    `yaml:"tags,omitempty"`
	Refs    interface{} `yaml:"refs,omitempty"`
}

// kebabCasePattern validates kebab-case identifiers
var kebabCasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// datePattern validates YYYY-MM-DD format
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidateCogdoc performs full two-tier validation on a cogdoc file.
// Returns a ValidationResult with errors (blocking) and warnings (advisory).
func ValidateCogdoc(path string) *ValidationResult {
	result := &ValidationResult{Path: path, Valid: true}

	data, err := os.ReadFile(path)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	content := string(data)

	// Must end in .cog.md
	if !strings.HasSuffix(path, ".cog.md") {
		result.Valid = false
		result.Errors = append(result.Errors, "not a cogdoc (must end in .cog.md)")
		return result
	}

	// Check frontmatter
	if !strings.HasPrefix(content, "---\n") {
		result.Valid = false
		result.Errors = append(result.Errors, "no frontmatter")
		return result
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		result.Valid = false
		result.Errors = append(result.Errors, "unclosed frontmatter")
		return result
	}
	fmContent := content[4 : 4+end]
	bodyContent := content[4+end+4:] // Content after frontmatter

	var doc CogdocFrontmatter
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid YAML: %v", err))
		return result
	}

	// Required fields
	if doc.Type == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: type")
	}
	if doc.ID == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: id")
	}
	if doc.Title == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: title")
	}
	if doc.Created == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: created")
	}

	// Field value validation
	if doc.Type != "" && !types.CogdocType(doc.Type).IsValid() {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid type '%s'", doc.Type))
	}
	if doc.ID != "" && !IsKebabCase(doc.ID) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid id '%s' (must be kebab-case)", doc.ID))
	}
	if doc.Created != "" && !IsValidDate(doc.Created) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid created '%s' (must be YYYY-MM-DD)", doc.Created))
	}

	// STRUCTURAL REFS (errors if broken)
	if doc.Refs != nil {
		refs := parseRefs(doc.Refs)
		result.StructuralRefs = refs
		for _, ref := range refs {
			if !strings.HasPrefix(ref.URI, "cog://") {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("invalid ref URI '%s' (must start with cog://)", ref.URI))
			} else if ref.Rel != "" && !ValidRefRelations[ref.Rel] {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("invalid ref relation '%s'", ref.Rel))
			} else {
				// Check if ref is parseable (namespace validation)
				if _, err := ParseURI(ref.URI); err != nil {
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("broken structural ref '%s': %v", ref.URI, err))
				}
			}
		}
	}

	// INLINE/NAVIGATIONAL REFS (warnings if broken)
	inlineRefs := extractInlineRefs(bodyContent)
	result.InlineRefs = inlineRefs
	for _, uri := range inlineRefs {
		if _, err := ParseURI(uri); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("broken inline ref '%s': %v", uri, err))
		}
	}

	return result
}

// ValidateCogdocSimple performs simple validation (errors only, no warnings).
// Returns an error if validation fails, nil otherwise.
func ValidateCogdocSimple(path string) error {
	result := ValidateCogdoc(path)
	if !result.Valid {
		return fmt.Errorf("validation failed: %s", strings.Join(result.Errors, "; "))
	}
	return nil
}

// IsKebabCase validates that a string is kebab-case.
func IsKebabCase(s string) bool {
	if len(s) == 0 {
		return false
	}
	return kebabCasePattern.MatchString(s)
}

// IsValidDate validates YYYY-MM-DD format.
func IsValidDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	return datePattern.MatchString(s)
}

// ValidateFrontmatter validates just the frontmatter portion of a cogdoc.
func ValidateFrontmatter(fmContent string) *ValidationResult {
	result := &ValidationResult{Valid: true}

	var doc CogdocFrontmatter
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid YAML: %v", err))
		return result
	}

	// Required fields
	if doc.Type == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: type")
	}
	if doc.ID == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: id")
	}
	if doc.Title == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: title")
	}
	if doc.Created == "" {
		result.Valid = false
		result.Errors = append(result.Errors, "missing: created")
	}

	// Field value validation
	if doc.Type != "" && !types.CogdocType(doc.Type).IsValid() {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid type '%s'", doc.Type))
	}
	if doc.ID != "" && !IsKebabCase(doc.ID) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid id '%s' (must be kebab-case)", doc.ID))
	}
	if doc.Created != "" && !IsValidDate(doc.Created) {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid created '%s' (must be YYYY-MM-DD)", doc.Created))
	}

	return result
}

// ParseFrontmatter extracts and parses the frontmatter from cogdoc content.
func ParseFrontmatter(content string) (*CogdocFrontmatter, string, error) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter (must start with ---)")
	}
	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return nil, "", fmt.Errorf("unclosed frontmatter")
	}
	fmContent := content[4 : 4+end]
	bodyContent := content[4+end+4:]

	var doc CogdocFrontmatter
	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		return nil, "", fmt.Errorf("invalid YAML: %w", err)
	}

	return &doc, bodyContent, nil
}

// ValidateType checks if a type string is a valid cogdoc type.
func ValidateType(t string) bool {
	return types.CogdocType(t).IsValid()
}

// AllValidTypes returns all valid cogdoc type strings.
func AllValidTypes() []string {
	result := make([]string, 0, len(types.ValidCogdocTypes))
	for t := range types.ValidCogdocTypes {
		result = append(result, string(t))
	}
	return result
}
