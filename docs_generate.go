// docs_generate.go — Auto-documentation pipeline
//
// Walks the CogDoc corpus, parses frontmatter, groups by type/status/sector,
// and generates deterministic documentation outputs:
//
//   - DASHBOARD.md  — inbox health (raw/enriched/integrated counts)
//   - INDEX.md      — research index grouped by tags
//   - CATALOG.md    — tool/skill inventory
//   - README.md     — per-directory summaries
//
// This is the efferent pathway: knowledge flows OUT of the CogDoc substrate
// as human-readable documentation. No LLM calls — purely deterministic.
//
// Usage: cogos-v3 docs [--workspace PATH]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func runDocsCmd(args []string, workspace string) {
	if workspace == "" {
		wd, _ := os.Getwd()
		ws, err := findWorkspaceRoot(wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not detect workspace: %v\n", err)
			os.Exit(1)
		}
		workspace = ws
	}

	fmt.Printf("cogos-v3 docs: generating from %s\n", workspace)

	memDir := filepath.Join(workspace, ".cog", "mem")
	if _, err := os.Stat(memDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: .cog/mem not found in %s\n", workspace)
		os.Exit(1)
	}

	// Build the CogDoc index.
	idx, err := BuildIndex(workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: index build failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  indexed %d CogDocs\n", len(idx.ByURI))

	outDir := filepath.Join(workspace, ".cog", "docs", "generated")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: mkdir %s: %v\n", outDir, err)
		os.Exit(1)
	}

	// Generate all docs.
	generated := 0

	if n, err := generateDashboard(idx, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: dashboard: %v\n", err)
	} else {
		generated += n
	}

	if n, err := generateResearchIndex(idx, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: research index: %v\n", err)
	} else {
		generated += n
	}

	if n, err := generateSkillCatalog(workspace, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: skill catalog: %v\n", err)
	} else {
		generated += n
	}

	if n, err := generateInboxManifest(idx, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: inbox manifest: %v\n", err)
	} else {
		generated += n
	}

	fmt.Printf("  generated %d files in %s\n", generated, outDir)
}

// generateDashboard produces DASHBOARD.md with inbox/corpus health metrics.
func generateDashboard(idx *CogDocIndex, outDir string) (int, error) {
	var sb strings.Builder
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	sb.WriteString("# CogOS Dashboard\n\n")
	fmt.Fprintf(&sb, "_Generated: %s_\n\n", now)

	// Count by status.
	statusCounts := map[string]int{}
	sectorCounts := map[string]int{}
	typeCounts := map[string]int{}
	inboxBySubdir := map[string]int{}

	for _, doc := range idx.ByURI {
		status := strings.ToLower(doc.Status)
		if status == "" {
			status = "(none)"
		}
		statusCounts[status]++

		// Derive sector from path: .cog/mem/{sector}/...
		sector := "(unknown)"
		if parts := strings.Split(filepath.ToSlash(doc.Path), "/.cog/mem/"); len(parts) > 1 {
			sub := strings.SplitN(parts[1], "/", 2)
			if len(sub) > 0 {
				sector = sub[0]
			}
		}
		sectorCounts[sector]++

		docType := doc.Type
		if docType == "" {
			docType = "(untyped)"
		}
		typeCounts[docType]++

		if strings.Contains(filepath.ToSlash(doc.Path), "/inbox/") {
			// Extract subdirectory name.
			parts := strings.Split(filepath.ToSlash(doc.Path), "/inbox/")
			if len(parts) > 1 {
				subParts := strings.SplitN(parts[1], "/", 2)
				inboxBySubdir[subParts[0]]++
			}
		}
	}

	// Corpus overview.
	sb.WriteString("## Corpus Overview\n\n")
	fmt.Fprintf(&sb, "| Metric | Count |\n|--------|-------|\n")
	fmt.Fprintf(&sb, "| Total CogDocs | %d |\n", len(idx.ByURI))
	for _, sector := range sortedKeys(sectorCounts) {
		fmt.Fprintf(&sb, "| Sector: %s | %d |\n", sector, sectorCounts[sector])
	}
	sb.WriteString("\n")

	// Status distribution.
	sb.WriteString("## Status Distribution\n\n")
	fmt.Fprintf(&sb, "| Status | Count |\n|--------|-------|\n")
	for _, status := range sortedKeys(statusCounts) {
		fmt.Fprintf(&sb, "| %s | %d |\n", status, statusCounts[status])
	}
	sb.WriteString("\n")

	// Inbox health.
	inboxTotal := 0
	for _, c := range inboxBySubdir {
		inboxTotal += c
	}
	sb.WriteString("## Inbox Health\n\n")
	fmt.Fprintf(&sb, "| Queue | Items |\n|-------|-------|\n")
	fmt.Fprintf(&sb, "| **Total inbox** | **%d** |\n", inboxTotal)
	for _, sub := range sortedKeys(inboxBySubdir) {
		fmt.Fprintf(&sb, "| %s | %d |\n", sub, inboxBySubdir[sub])
	}
	sb.WriteString("\n")

	// Type distribution.
	sb.WriteString("## Document Types\n\n")
	fmt.Fprintf(&sb, "| Type | Count |\n|------|-------|\n")
	for _, t := range sortedKeys(typeCounts) {
		fmt.Fprintf(&sb, "| %s | %d |\n", t, typeCounts[t])
	}

	path := filepath.Join(outDir, "DASHBOARD.md")
	return 1, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// generateResearchIndex produces INDEX.md grouped by tags.
func generateResearchIndex(idx *CogDocIndex, outDir string) (int, error) {
	var sb strings.Builder
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	sb.WriteString("# Research Index\n\n")
	fmt.Fprintf(&sb, "_Generated: %s_\n\n", now)

	// Collect research-type docs.
	type researchDoc struct {
		Title string
		URI   string
		Tags  []string
		Type  string
	}

	var docs []researchDoc
	tagGroups := map[string][]int{} // tag → indices into docs

	for _, doc := range idx.ByURI {
		switch doc.Type {
		case "link", "paper", "research", "insight", "connection":
			rd := researchDoc{
				Title: doc.Title,
				URI:   doc.URI,
				Tags:  doc.Tags,
				Type:  doc.Type,
			}
			idx := len(docs)
			docs = append(docs, rd)
			for _, tag := range doc.Tags {
				tagGroups[tag] = append(tagGroups[tag], idx)
			}
		}
	}

	fmt.Fprintf(&sb, "Total research items: %d\n\n", len(docs))

	// Render by tag (only tags with 3+ items).
	sb.WriteString("## By Tag\n\n")
	for _, tag := range sortedKeys(tagGroups) {
		indices := tagGroups[tag]
		if len(indices) < 3 {
			continue
		}
		fmt.Fprintf(&sb, "### %s (%d)\n\n", tag, len(indices))
		for _, i := range indices {
			d := docs[i]
			fmt.Fprintf(&sb, "- **%s** `[%s]` — %s\n", d.Title, d.Type, d.URI)
		}
		sb.WriteString("\n")
	}

	path := filepath.Join(outDir, "INDEX.md")
	return 1, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// generateSkillCatalog produces CATALOG.md from .claude/skills/*/SKILL.md.
func generateSkillCatalog(workspace, outDir string) (int, error) {
	var sb strings.Builder
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	sb.WriteString("# Skill Catalog\n\n")
	fmt.Fprintf(&sb, "_Generated: %s_\n\n", now)

	skillsDir := filepath.Join(workspace, ".claude", "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return 0, fmt.Errorf("read skills dir: %w", err)
	}

	sb.WriteString("| Skill | Description |\n|-------|-------------|\n")
	count := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillFile := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		// Extract title and first paragraph from SKILL.md.
		lines := strings.Split(string(data), "\n")
		title := entry.Name()
		desc := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "# ") {
				title = strings.TrimPrefix(line, "# ")
				continue
			}
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "#") && desc == "" {
				desc = strings.TrimSpace(line)
				if len(desc) > 120 {
					desc = desc[:117] + "..."
				}
			}
		}

		fmt.Fprintf(&sb, "| %s | %s |\n", title, desc)
		count++
	}

	fmt.Fprintf(&sb, "\n_Total: %d skills_\n", count)

	path := filepath.Join(outDir, "CATALOG.md")
	return 1, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// generateInboxManifest produces INBOX-MANIFEST.md listing all pending items.
func generateInboxManifest(idx *CogDocIndex, outDir string) (int, error) {
	var sb strings.Builder
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	sb.WriteString("# Inbox Manifest\n\n")
	fmt.Fprintf(&sb, "_Generated: %s_\n\n", now)

	type inboxItem struct {
		Title  string
		Path   string
		Status string
		Type   string
		Source string
	}

	var items []inboxItem
	for _, doc := range idx.ByURI {
		if !strings.Contains(filepath.ToSlash(doc.Path), "/inbox/") {
			continue
		}
		source := ""
		for _, tag := range doc.Tags {
			switch tag {
			case "chatgpt", "discord", "claude", "arxiv":
				source = tag
			}
		}
		items = append(items, inboxItem{
			Title:  doc.Title,
			Path:   doc.URI,
			Status: doc.Status,
			Type:   doc.Type,
			Source: source,
		})
	}

	// Sort by source then title.
	sort.Slice(items, func(i, j int) bool {
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		return items[i].Title < items[j].Title
	})

	// Group by source.
	currentSource := ""
	for _, item := range items {
		if item.Source != currentSource {
			currentSource = item.Source
			src := currentSource
			if src == "" {
				src = "(other)"
			}
			fmt.Fprintf(&sb, "\n## %s\n\n", src)
			sb.WriteString("| Title | Status | Type |\n|-------|--------|------|\n")
		}
		fmt.Fprintf(&sb, "| %s | %s | %s |\n", item.Title, item.Status, item.Type)
	}

	fmt.Fprintf(&sb, "\n_Total: %d inbox items_\n", len(items))

	path := filepath.Join(outDir, "INBOX-MANIFEST.md")
	return 1, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// sortedKeys returns the keys of a map sorted alphabetically.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
