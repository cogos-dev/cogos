// .cog/tools/write.go
// Write tool implementation with mem-only enforcement
//
// The Write tool allows the model to write files, but policy restricts it to
// .cog/mem/ only. This ensures the model cannot modify kernel code or
// configuration, only its own memory.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WriteTool implements the Tool interface for file writing
type WriteTool struct {
	cogRoot string // Path to .cog/ directory
}

// NewWriteTool creates a new Write tool instance
func NewWriteTool(cogRoot string) *WriteTool {
	return &WriteTool{cogRoot: cogRoot}
}

// Name returns the tool name
func (w *WriteTool) Name() string {
	return "Write"
}

// Execute writes a file to the filesystem
func (w *WriteTool) Execute(inputs map[string]interface{}) (*ToolResult, error) {
	// Extract file_path and content from inputs
	filePath, ok := inputs["file_path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing required input: file_path")
	}

	content, ok := inputs["content"].(string)
	if !ok {
		return nil, fmt.Errorf("missing required input: content")
	}

	// Normalize path (remove ../ traversal attempts)
	filePath = filepath.Clean(filePath)

	// Policy enforcement happens at ExecuteTool level, but we double-check here
	// This is defense-in-depth: policy should block non-mem writes
	if !strings.HasPrefix(filePath, ".cog/mem/") {
		return nil, fmt.Errorf("Write tool restricted to .cog/mem/ only (attempted: %s)", filePath)
	}

	// Validate cogdoc schema if path ends in .cog.md
	if strings.HasSuffix(filePath, ".cog.md") {
		if err := w.validateCogdoc([]byte(content)); err != nil {
			return nil, fmt.Errorf("cogdoc validation failed: %w", err)
		}
	}

	// Ensure directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Write file
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	// Get session ID for artifact storage
	sessionID := os.Getenv("CLAUDE_SESSION_ID")
	if sessionID == "" {
		sessionID = "default"
	}

	// Store artifact
	artifactHash, err := StoreArtifact(sessionID, []byte(content), "text/markdown")
	if err != nil {
		return nil, fmt.Errorf("failed to store artifact: %w", err)
	}

	// Return result
	return &ToolResult{
		CallID:       inputs["call_id"].(string),
		ArtifactHash: artifactHash,
		ByteLength:   len(content),
		ContentType:  "text/markdown",
		Success:      true,
	}, nil
}

// validateCogdoc validates cogdoc frontmatter structure
func (w *WriteTool) validateCogdoc(content []byte) error {
	// Parse frontmatter
	contentStr := string(content)
	if !strings.HasPrefix(contentStr, "---\n") {
		return fmt.Errorf("cogdoc must start with ---")
	}

	end := strings.Index(contentStr[4:], "\n---")
	if end == -1 {
		return fmt.Errorf("cogdoc must have closing ---")
	}

	fmContent := contentStr[4 : 4+end]

	// Parse as YAML
	var doc struct {
		Type    string `yaml:"type"`
		ID      string `yaml:"id"`
		Title   string `yaml:"title"`
		Created string `yaml:"created"`
	}

	if err := yaml.Unmarshal([]byte(fmContent), &doc); err != nil {
		return fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	// Validate required fields
	if doc.Type == "" {
		return fmt.Errorf("missing required field: type")
	}
	if doc.ID == "" {
		return fmt.Errorf("missing required field: id")
	}
	if doc.Title == "" {
		return fmt.Errorf("missing required field: title")
	}
	if doc.Created == "" {
		return fmt.Errorf("missing required field: created")
	}

	// Validate date format (YYYY-MM-DD)
	if !isValidDate(doc.Created) {
		return fmt.Errorf("created must be in YYYY-MM-DD format (got: %s)", doc.Created)
	}

	return nil
}
