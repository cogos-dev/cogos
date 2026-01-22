// .cog/frontmatter.go
// Frontmatter Management for Cogdocs
//
// Structured YAML parsing and generation for memory documents.
// Replaces frontmatter.sh (423 LOC) with type-safe Go implementation.

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// === TYPES ===

// MemorySector represents the four memory sectors
type MemorySector string

const (
	SectorSemantic   MemorySector = "semantic"
	SectorEpisodic   MemorySector = "episodic"
	SectorProcedural MemorySector = "procedural"
	SectorReflective MemorySector = "reflective"
)

// SectorSchema defines default field values per sector
type SectorSchema struct {
	Sector      MemorySector
	DecayRate   float64
	Salience    string
	Description string
}

// MemoryFrontmatter represents memory-specific frontmatter fields
type MemoryFrontmatter struct {
	Title          string       `yaml:"title"`
	Created        string       `yaml:"created,omitempty"`
	Tags           []string     `yaml:"tags,omitempty"`
	MemorySector   MemorySector `yaml:"memory_sector"`
	MemoryStrength float64      `yaml:"memory_strength,omitempty"`
	DecayRate      float64      `yaml:"decay_rate,omitempty"`
	Salience       string       `yaml:"salience,omitempty"`
}

// FrontmatterDocument represents the full frontmatter + body structure
type FrontmatterDocument struct {
	Frontmatter string // Raw YAML between ---
	Body        string // Content after frontmatter
}

// === SCHEMA CONFIGURATION ===

var sectorSchemas = map[MemorySector]SectorSchema{
	SectorSemantic: {
		Sector:      SectorSemantic,
		DecayRate:   0.08,
		Salience:    "medium",
		Description: "Long-term knowledge and architecture",
	},
	SectorEpisodic: {
		Sector:      SectorEpisodic,
		DecayRate:   0.20,
		Salience:    "medium",
		Description: "Specific events and experiences",
	},
	SectorProcedural: {
		Sector:      SectorProcedural,
		DecayRate:   0.12,
		Salience:    "medium",
		Description: "How-to guides and workflows",
	},
	SectorReflective: {
		Sector:      SectorReflective,
		DecayRate:   0.05,
		Salience:    "high",
		Description: "Meta-cognitive insights",
	},
}

// === SECTOR DETECTION ===

// DetectSector determines the memory sector from a file path
func DetectSector(path string) MemorySector {
	lowerPath := strings.ToLower(path)

	if strings.Contains(lowerPath, "semantic") {
		return SectorSemantic
	}
	if strings.Contains(lowerPath, "episodic") {
		return SectorEpisodic
	}
	if strings.Contains(lowerPath, "procedural") {
		return SectorProcedural
	}
	if strings.Contains(lowerPath, "reflective") {
		return SectorReflective
	}

	// Default to semantic
	return SectorSemantic
}

// GetSectorSchema returns the schema for a given sector
func GetSectorSchema(sector MemorySector) SectorSchema {
	if schema, ok := sectorSchemas[sector]; ok {
		return schema
	}
	// Fallback
	return SectorSchema{
		Sector:    SectorSemantic,
		DecayRate: 0.10,
		Salience:  "medium",
	}
}

// === FRONTMATTER EXTRACTION ===

// HasFrontmatter checks if content starts with YAML frontmatter
func HasFrontmatter(content string) bool {
	return strings.HasPrefix(content, "---\n")
}

// ExtractFrontmatter extracts frontmatter and body from content
func ExtractFrontmatter(content string) (*FrontmatterDocument, error) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("no frontmatter")
	}

	end := strings.Index(content[4:], "\n---")
	if end == -1 {
		return nil, fmt.Errorf("unclosed frontmatter")
	}

	// Trim trailing newline from frontmatter
	fmContent := content[4 : 4+end]
	fmContent = strings.TrimSuffix(fmContent, "\n")

	return &FrontmatterDocument{
		Frontmatter: fmContent,
		Body:        strings.TrimPrefix(content[4+end+4:], "\n"),
	}, nil
}

// ParseFrontmatter parses YAML frontmatter into a map
func ParseFrontmatter(frontmatter string) (map[string]interface{}, error) {
	var data map[string]interface{}
	if err := yaml.Unmarshal([]byte(frontmatter), &data); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	return data, nil
}

// ParseMemoryFrontmatter parses frontmatter into a MemoryFrontmatter struct
func ParseMemoryFrontmatter(frontmatter string) (*MemoryFrontmatter, error) {
	var mf MemoryFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &mf); err != nil {
		return nil, fmt.Errorf("invalid memory frontmatter: %w", err)
	}
	return &mf, nil
}

// === TITLE EXTRACTION ===

// ExtractTitle extracts title from file (frontmatter > markdown > filename)
func ExtractTitle(filePath string) (string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	// Try frontmatter first
	if doc, err := ExtractFrontmatter(string(content)); err == nil {
		if title := ExtractTitleFromFrontmatter(doc.Frontmatter); title != "" {
			return title, nil
		}
	}

	// Try first markdown heading
	if title := ExtractTitleFromMarkdown(string(content)); title != "" {
		return title, nil
	}

	// Fall back to filename
	return ExtractTitleFromFilename(filePath), nil
}

// ExtractTitleFromFrontmatter extracts title field from frontmatter
func ExtractTitleFromFrontmatter(frontmatter string) string {
	data, err := ParseFrontmatter(frontmatter)
	if err != nil {
		return ""
	}

	if title, ok := data["title"].(string); ok {
		return strings.Trim(title, `"'`)
	}
	return ""
}

// ExtractTitleFromMarkdown extracts first # heading from markdown
func ExtractTitleFromMarkdown(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}

// ExtractTitleFromFilename generates title from filename
func ExtractTitleFromFilename(path string) string {
	base := filepath.Base(path)
	// Remove extensions
	base = strings.TrimSuffix(base, ".md")
	base = strings.TrimSuffix(base, ".cog.md")
	base = strings.TrimSuffix(base, ".cog")

	// Replace separators with spaces
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")

	return base
}

// === FRONTMATTER GENERATION ===

// GenerateFrontmatter generates full frontmatter for a path and title
func GenerateFrontmatter(path, title string) string {
	sector := DetectSector(path)
	schema := GetSectorSchema(sector)
	today := time.Now().Format("2006-01-02")

	fm := MemoryFrontmatter{
		Title:          title,
		Created:        today,
		Tags:           []string{},
		MemorySector:   sector,
		MemoryStrength: 1.0,
		DecayRate:      schema.DecayRate,
		Salience:       schema.Salience,
	}

	data, _ := yaml.Marshal(&fm)
	return "---\n" + string(data) + "---\n"
}

// GenerateMinimalFrontmatter generates minimal frontmatter (title + sector)
func GenerateMinimalFrontmatter(path, title string) string {
	sector := DetectSector(path)

	fm := MemoryFrontmatter{
		Title:        title,
		MemorySector: sector,
	}

	data, _ := yaml.Marshal(&fm)
	return "---\n" + string(data) + "---\n"
}

// InferMissingFields adds missing fields to existing frontmatter based on schema
func InferMissingFields(frontmatter string, path string) (string, error) {
	data, err := ParseFrontmatter(frontmatter)
	if err != nil {
		return "", err
	}

	sector := SectorSemantic
	if s, ok := data["memory_sector"].(string); ok {
		sector = MemorySector(s)
	} else {
		sector = DetectSector(path)
		data["memory_sector"] = string(sector)
	}

	schema := GetSectorSchema(sector)

	// Add missing title
	if _, hasTitle := data["title"]; !hasTitle {
		title, _ := ExtractTitle(path)
		if title != "" {
			data["title"] = title
		}
	}

	// Add missing created
	if _, hasCreated := data["created"]; !hasCreated {
		data["created"] = time.Now().Format("2006-01-02")
	}

	// Add missing decay_rate
	if _, hasDecay := data["decay_rate"]; !hasDecay {
		data["decay_rate"] = schema.DecayRate
	}

	// Add missing salience
	if _, hasSalience := data["salience"]; !hasSalience {
		data["salience"] = schema.Salience
	}

	// Re-marshal with inferred fields
	updatedData, err := yaml.Marshal(data)
	if err != nil {
		return "", err
	}

	return string(updatedData), nil
}

// === VALIDATION ===

// IsCogdoc checks if a file is a valid cogdoc (has frontmatter with required fields)
func IsCogdoc(filePath string) (bool, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}

	// Must have frontmatter
	if !HasFrontmatter(string(content)) {
		return false, nil
	}

	// Extract and parse frontmatter
	doc, err := ExtractFrontmatter(string(content))
	if err != nil {
		return false, nil
	}

	data, err := ParseFrontmatter(doc.Frontmatter)
	if err != nil {
		return false, nil
	}

	// Must have either memory_sector or cogn8 block
	if _, hasSector := data["memory_sector"]; hasSector {
		return true, nil
	}
	if _, hasCogn8 := data["cogn8"]; hasCogn8 {
		return true, nil
	}

	return false, nil
}

// === FILE OPERATIONS ===

// ApplyFrontmatter adds or fixes frontmatter on a file
func ApplyFrontmatter(filePath string, force bool) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	title, _ := ExtractTitle(filePath)
	if title == "" {
		title = ExtractTitleFromFilename(filePath)
	}

	if HasFrontmatter(string(content)) {
		if !force {
			// Check if frontmatter is complete
			doc, err := ExtractFrontmatter(string(content))
			if err == nil {
				data, err := ParseFrontmatter(doc.Frontmatter)
				if err == nil {
					_, hasTitle := data["title"]
					_, hasSector := data["memory_sector"]
					if hasTitle && hasSector {
						return nil // Already complete
					}
				}
			}
		}

		// Fix existing frontmatter
		return FixExistingFrontmatter(filePath)
	}

	// Add new frontmatter
	newFrontmatter := GenerateFrontmatter(filePath, title)
	newContent := newFrontmatter + "\n" + string(content)

	return os.WriteFile(filePath, []byte(newContent), 0644)
}

// FixExistingFrontmatter fixes existing frontmatter by inferring missing fields
func FixExistingFrontmatter(filePath string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	doc, err := ExtractFrontmatter(string(content))
	if err != nil {
		return fmt.Errorf("no frontmatter to fix: %w", err)
	}

	// Infer missing fields
	fixedFrontmatter, err := InferMissingFields(doc.Frontmatter, filePath)
	if err != nil {
		return err
	}

	// Reconstruct document
	newContent := "---\n" + fixedFrontmatter + "---\n" + doc.Body

	return os.WriteFile(filePath, []byte(newContent), 0644)
}

// CogifyFile renames .md to .cog.md if it's a valid cogdoc
func CogifyFile(filePath string) (string, error) {
	// Skip if already .cog.md
	if strings.HasSuffix(filePath, ".cog.md") {
		return filePath, nil
	}

	// Skip if not a valid cogdoc
	isValid, err := IsCogdoc(filePath)
	if err != nil {
		return filePath, err
	}
	if !isValid {
		return filePath, nil
	}

	// Generate new name
	newPath := strings.TrimSuffix(filePath, ".md") + ".cog.md"

	// Rename file
	if err := os.Rename(filePath, newPath); err != nil {
		return filePath, err
	}

	return newPath, nil
}

// === BULK OPERATIONS ===

// FixAllFrontmatter fixes all memory files that need frontmatter
// Returns (fixed, skipped, error)
func FixAllFrontmatter(baseDir string, sector *MemorySector, dryRun bool) (int, int, error) {
	searchDir := baseDir
	if sector != nil {
		searchDir = filepath.Join(baseDir, string(*sector))
	}

	fixed := 0
	skipped := 0

	err := filepath.Walk(searchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip non-markdown files
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		// Skip special files
		base := filepath.Base(path)
		if base == "README.md" || base == "TEMPLATE.md" {
			return nil
		}

		// Check if needs fixing
		needsFix := false

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if !HasFrontmatter(string(content)) {
			needsFix = true
		} else {
			doc, err := ExtractFrontmatter(string(content))
			if err == nil {
				data, err := ParseFrontmatter(doc.Frontmatter)
				if err == nil {
					_, hasTitle := data["title"]
					_, hasSector := data["memory_sector"]
					if !hasTitle || !hasSector {
						needsFix = true
					}
				}
			}
		}

		if needsFix {
			if dryRun {
				fmt.Printf("Would fix: %s\n", path)
			} else {
				if err := ApplyFrontmatter(path, false); err != nil {
					return err
				}
				fmt.Printf("Fixed frontmatter: %s\n", path)
			}
			fixed++
		} else {
			skipped++
		}

		return nil
	})

	return fixed, skipped, err
}

// CogifyAll cogifies all valid cogdocs (rename .md to .cog.md)
// Returns (renamed, skipped, error)
func CogifyAll(baseDir string, sector *MemorySector, dryRun bool) (int, int, error) {
	searchDir := baseDir
	if sector != nil {
		searchDir = filepath.Join(baseDir, string(*sector))
	}

	renamed := 0
	skipped := 0

	err := filepath.Walk(searchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories, already-cogified, and special files
		if info.IsDir() || strings.HasSuffix(path, ".cog.md") {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		base := filepath.Base(path)
		if base == "README.md" || base == "TEMPLATE.md" {
			return nil
		}

		// Check if valid cogdoc
		isValid, err := IsCogdoc(path)
		if err != nil {
			return err
		}

		if isValid {
			newPath := strings.TrimSuffix(path, ".md") + ".cog.md"
			if dryRun {
				fmt.Printf("Would rename: %s -> %s\n", path, filepath.Base(newPath))
			} else {
				if err := os.Rename(path, newPath); err != nil {
					return err
				}
				fmt.Printf("Renamed: %s -> %s\n", path, filepath.Base(newPath))
			}
			renamed++
		} else {
			skipped++
		}

		return nil
	})

	return renamed, skipped, err
}

// MigrateFrontmatter performs full migration: fix frontmatter + cogify
func MigrateFrontmatter(baseDir string, sector *MemorySector, dryRun bool) error {
	fmt.Println("=== Frontmatter Migration ===")
	fmt.Println()

	// Step 1: Fix missing frontmatter
	fmt.Println("Step 1: Adding missing frontmatter...")
	fixed, skipped, err := FixAllFrontmatter(baseDir, sector, dryRun)
	if err != nil {
		return err
	}
	fmt.Printf("Fixed: %d, Skipped: %d\n", fixed, skipped)
	fmt.Println()

	// Step 2: Rename to .cog.md
	fmt.Println("Step 2: Renaming to .cog.md...")
	renamed, skippedCog, err := CogifyAll(baseDir, sector, dryRun)
	if err != nil {
		return err
	}
	fmt.Printf("Renamed: %d, Skipped: %d\n", renamed, skippedCog)
	fmt.Println()

	fmt.Println("=== Migration Complete ===")
	return nil
}
