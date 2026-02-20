package main

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Section represents a markdown heading and its content range
type Section struct {
	Title    string    `yaml:"title" json:"title"`
	Anchor   string    `yaml:"anchor,omitempty" json:"anchor,omitempty"`
	Level    int       `yaml:"level" json:"level"`
	Line     int       `yaml:"line" json:"line"`         // 1-indexed start line
	EndLine  int       `yaml:"end_line" json:"end_line"` // 1-indexed end line (exclusive)
	Size     int       `yaml:"size" json:"size"`         // bytes
	Tags     []string  `yaml:"tags,omitempty" json:"tags,omitempty"`
	Children []Section `yaml:"-" json:"children,omitempty"` // for nested display
}

// anchorRe matches {#anchor-id} at end of heading
var anchorRe = regexp.MustCompile(`\{#([a-zA-Z0-9_-]+)\}\s*$`)

// headingRe matches markdown headings (# to ######)
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

// ParseSections scans markdown body for headings and returns a flat list of sections
// with computed line ranges and byte sizes.
func ParseSections(body string) []Section {
	lines := strings.Split(body, "\n")
	var sections []Section
	inCodeBlock := false

	for i, line := range lines {
		// Track fenced code blocks to avoid matching headings inside them
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		level := len(m[1])
		title := strings.TrimSpace(m[2])
		anchor := ""

		// Extract {#anchor} if present
		if am := anchorRe.FindStringSubmatch(title); am != nil {
			anchor = am[1]
			title = strings.TrimSpace(anchorRe.ReplaceAllString(title, ""))
		}

		sections = append(sections, Section{
			Title:  title,
			Anchor: anchor,
			Level:  level,
			Line:   i + 1, // 1-indexed
		})
	}

	// Compute EndLine and Size for each section
	for i := range sections {
		if i+1 < len(sections) {
			sections[i].EndLine = sections[i+1].Line
		} else {
			sections[i].EndLine = len(lines) + 1
		}

		// Calculate byte size of section content
		start := sections[i].Line - 1 // 0-indexed
		end := sections[i].EndLine - 1
		end = min(end, len(lines))
		size := 0
		for j := start; j < end; j++ {
			size += len(lines[j]) + 1 // +1 for newline
		}
		sections[i].Size = size
	}

	return sections
}

// GetSection returns the content of a section identified by heading title or #anchor.
// Matches are case-insensitive for titles. Returns from heading line through to the
// next same-or-higher-level heading (exclusive).
func GetSection(body string, selector string) (string, error) {
	lines := strings.Split(body, "\n")
	sections := ParseSections(body)

	if len(sections) == 0 {
		return "", fmt.Errorf("no sections found")
	}

	// Find the target section index
	targetIdx := -1
	isAnchor := strings.HasPrefix(selector, "#")
	anchorName := strings.TrimPrefix(selector, "#")

	for i := range sections {
		if isAnchor {
			// Match explicit {#anchor} first
			if strings.EqualFold(sections[i].Anchor, anchorName) {
				targetIdx = i
				break
			}
			// Fall back to auto-generated slug from title
			if titleToAnchor(sections[i].Title) == anchorName {
				targetIdx = i
				break
			}
		} else {
			if strings.EqualFold(sections[i].Title, selector) {
				targetIdx = i
				break
			}
		}
	}

	if targetIdx < 0 {
		return "", fmt.Errorf("section not found: %s", selector)
	}

	target := &sections[targetIdx]

	// Find end: next section at same or higher level (lower number)
	endLine := len(lines) + 1
	for i := targetIdx + 1; i < len(sections); i++ {
		if sections[i].Level <= target.Level {
			endLine = sections[i].Line
			break
		}
	}

	// Extract lines [Line-1, endLine-1) (0-indexed)
	start := target.Line - 1
	end := endLine - 1
	end = min(end, len(lines))

	// Trim trailing blank lines
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}

	return strings.Join(lines[start:end], "\n"), nil
}

// ListHeadings returns heading titles with level indicators (e.g., "## The Constants").
func ListHeadings(body string) []string {
	sections := ParseSections(body)
	result := make([]string, len(sections))
	for i, s := range sections {
		result[i] = strings.Repeat("#", s.Level) + " " + s.Title
	}
	return result
}

// GenerateSectionsYAML produces the YAML for a sections: frontmatter field.
func GenerateSectionsYAML(body string) string {
	sections := ParseSections(body)
	if len(sections) == 0 {
		return ""
	}

	// Build simplified section entries for frontmatter (level 2+ only, skip title heading)
	type sectionEntry struct {
		Title  string `yaml:"title"`
		Anchor string `yaml:"anchor,omitempty"`
		Line   int    `yaml:"line"`
		Size   int    `yaml:"size"`
	}

	var entries []sectionEntry
	for _, s := range sections {
		if s.Level < 2 {
			continue // skip document title
		}
		entries = append(entries, sectionEntry{
			Title:  s.Title,
			Anchor: s.Anchor,
			Line:   s.Line,
			Size:   s.Size,
		})
	}

	if len(entries) == 0 {
		return ""
	}

	data, err := yaml.Marshal(entries)
	if err != nil {
		return ""
	}

	return string(data)
}
