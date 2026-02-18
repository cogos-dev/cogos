package main

import (
	"strings"
	"testing"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

func TestQueryConstellationNoAnchor(t *testing.T) {
	// With no anchor and no goal, should return empty
	result, err := QueryConstellation(t.TempDir(), "", "", 5000)
	if err != nil {
		t.Fatalf("QueryConstellation error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for no anchor/goal, got %q", result)
	}
}

func TestFormatNodeWithConfig(t *testing.T) {
	node := constellation.Node{
		Title:   "Test Document",
		Type:    "cogdoc",
		Sector:  "semantic",
		Status:  "active",
		Content: "This is the test document content for TAA pipeline testing.",
	}

	result := formatNodeWithConfig(node, 2000)

	if !strings.Contains(result, "## Test Document") {
		t.Error("missing title header")
	}
	if !strings.Contains(result, "Type: cogdoc") {
		t.Error("missing type metadata")
	}
	if !strings.Contains(result, "Sector: semantic") {
		t.Error("missing sector metadata")
	}
	if !strings.Contains(result, "Status: active") {
		t.Error("missing status metadata")
	}
	if !strings.Contains(result, "test document content") {
		t.Error("missing content")
	}
}

func TestFormatNodeWithConfigTruncation(t *testing.T) {
	longContent := strings.Repeat("word ", 1000)
	node := constellation.Node{
		Title:   "Long Document",
		Type:    "cogdoc",
		Content: longContent,
	}

	result := formatNodeWithConfig(node, 100)

	if !strings.Contains(result, "...(truncated)") {
		t.Error("long content should be truncated")
	}
	if len(result) > 500 { // generous upper bound
		t.Errorf("truncated result too long: %d chars", len(result))
	}
}

func TestFormatNodeWithConfigDefaultTruncation(t *testing.T) {
	longContent := strings.Repeat("x", 3000)
	node := constellation.Node{
		Title:   "Default Truncation Test",
		Type:    "cogdoc",
		Content: longContent,
	}

	// maxContentChars=0 should use default of 2000
	result := formatNodeWithConfig(node, 0)

	if !strings.Contains(result, "...(truncated)") {
		t.Error("should truncate with default limit")
	}
}

func TestFormatNodeWithConfigEmptyContent(t *testing.T) {
	node := constellation.Node{
		Title: "Empty Document",
		Type:  "cogdoc",
	}

	result := formatNodeWithConfig(node, 2000)

	if !strings.Contains(result, "## Empty Document") {
		t.Error("missing title for empty content node")
	}
}

func TestFormatNodeWithConfigNoSector(t *testing.T) {
	node := constellation.Node{
		Title:   "No Sector",
		Type:    "cogdoc",
		Content: "content",
	}

	result := formatNodeWithConfig(node, 2000)

	// Should not have "Sector:" in metadata
	if strings.Contains(result, "Sector:") {
		t.Error("should not include empty sector")
	}
}
