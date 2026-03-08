// cmd_constellation.go - Constellation knowledge graph CLI commands

package main

import (
	"fmt"
	"os"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// cmdConstellation handles constellation subcommands
func cmdConstellation(args []string) error {
	if len(args) == 0 {
		fmt.Println("Usage: cog constellation {index|index-bus|search|health|substance}")
		fmt.Println()
		fmt.Println("Commands:")
		fmt.Println("  index           Index all cogdocs + bus events in the workspace")
		fmt.Println("  index-bus       Index bus events only (backfill historical chat)")
		fmt.Println("  search <query>  Search constellation knowledge graph")
		fmt.Println("  health          Show constellation database stats")
		fmt.Println("  substance       Analyze document substance vs metadata")
		return nil
	}

	workspaceRoot, _, err := ResolveWorkspace()
	if err != nil {
		return fmt.Errorf("failed to resolve workspace: %w", err)
	}

	switch args[0] {
	case "index":
		return constellationIndex(workspaceRoot)
	case "index-bus":
		return constellationIndexBus(workspaceRoot)
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("search requires a query argument")
		}
		return constellationSearch(workspaceRoot, args[1])
	case "health":
		return constellationHealth(workspaceRoot)
	case "substance":
		return constellationSubstance(workspaceRoot, args[1:])
	default:
		return fmt.Errorf("unknown constellation subcommand: %s", args[0])
	}
}

// constellationIndex indexes all cogdocs and bus events in the workspace
func constellationIndex(workspaceRoot string) error {
	fmt.Println("Opening constellation database...")
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	fmt.Println("Indexing all cogdocs in workspace...")
	err = c.IndexWorkspace()
	if err != nil {
		// IndexWorkspace returns first non-fatal error (e.g. bad frontmatter)
		// but still indexes all valid docs. Treat as warning, not fatal.
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}

	// Backfill bus events into constellation
	fmt.Println("Indexing bus events...")
	if err := backfillBusEvents(workspaceRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: bus event indexing failed: %v\n", err)
	}

	// Run embedding backfill synchronously (the async goroutine in IndexWorkspace
	// dies when the CLI process exits — run it here instead)
	if client := getEmbedClient(workspaceRoot); client != nil {
		fmt.Println("Backfilling embeddings...")
		indexer := constellation.NewEmbedIndexer(c, client)
		n, err := indexer.BackfillAll(20)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: embedding backfill failed after %d docs: %v\n", n, err)
		} else {
			fmt.Printf("Embedded %d documents\n", n)
		}
	}

	// Show stats after indexing
	return constellationHealth(workspaceRoot)
}

// constellationIndexBus indexes only bus events (backfill historical chat)
func constellationIndexBus(workspaceRoot string) error {
	fmt.Println("Indexing bus events into constellation...")
	if err := backfillBusEvents(workspaceRoot); err != nil {
		return fmt.Errorf("bus event indexing failed: %w", err)
	}
	return constellationHealth(workspaceRoot)
}

// constellationSearch performs a full-text search
func constellationSearch(workspaceRoot, query string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	results, err := c.Search(query, 10)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, node := range results {
		fmt.Printf("%d. %s (%s)\n", i+1, node.Title, node.Type)
		if node.Sector != "" {
			fmt.Printf("   Sector: %s\n", node.Sector)
		}
		fmt.Printf("   URI: %s\n", PathToURI(workspaceRoot, node.Path))
		fmt.Printf("   Rank: %.2f\n\n", node.Rank)
	}

	return nil
}

// constellationHealth shows database statistics
func constellationHealth(workspaceRoot string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	health, err := c.Health()
	if err != nil {
		return fmt.Errorf("failed to get health: %w", err)
	}

	fmt.Println("Constellation Health:")
	fmt.Printf("  Documents:      %d\n", health["documents"])
	fmt.Printf("  Tags:           %d\n", health["tags"])
	fmt.Printf("  Doc References: %d\n", health["doc_references"])

	return nil
}

// constellationSubstance analyzes document substance vs metadata ratios
func constellationSubstance(workspaceRoot string, args []string) error {
	c, err := getConstellation()
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	// Parse flags
	mode := "summary"
	if len(args) > 0 {
		switch args[0] {
		case "--by-sector", "-s":
			mode = "sector"
		case "--by-type", "-t":
			mode = "type"
		case "--routing", "-r":
			mode = "routing"
		case "--leaf", "-l":
			mode = "leaf"
		case "--help", "-h":
			fmt.Println("Usage: cog constellation substance [flags]")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  (none)         Show workspace summary")
			fmt.Println("  --by-sector    Aggregate by sector")
			fmt.Println("  --by-type      Aggregate by document type")
			fmt.Println("  --routing      Find routing layers (high refs, low substance)")
			fmt.Println("  --leaf         Find leaf nodes (high substance, low refs)")
			return nil
		default:
			return fmt.Errorf("unknown flag: %s", args[0])
		}
	}

	switch mode {
	case "summary":
		return showSubstanceSummary(c)
	case "sector":
		return showSubstanceBySector(c)
	case "type":
		return showSubstanceByType(c)
	case "routing":
		return showRoutingLayers(c)
	case "leaf":
		return showLeafNodes(c)
	}

	return nil
}

func showSubstanceSummary(c *constellation.Constellation) error {
	summary, err := c.SubstanceSummary()
	if err != nil {
		return fmt.Errorf("failed to get summary: %w", err)
	}

	fmt.Println("Workspace Substance Summary")
	fmt.Println("===========================")
	fmt.Printf("  Documents:        %d\n", summary.DocCount)
	fmt.Printf("  Frontmatter:      %s\n", formatBytes(summary.FrontmatterBytes))
	fmt.Printf("  Content:          %s\n", formatBytes(summary.ContentBytes))
	fmt.Printf("  Substance Ratio:  %.1f%%\n", summary.SubstanceRatio*100)
	fmt.Printf("  Total References: %d\n", summary.RefCount)
	fmt.Printf("  Avg Ref Density:  %.2f refs/KB\n", summary.RefDensity)

	return nil
}

func showSubstanceBySector(c *constellation.Constellation) error {
	metrics, err := c.SubstanceReport()
	if err != nil {
		return fmt.Errorf("failed to get sector report: %w", err)
	}

	fmt.Println("Substance by Sector")
	fmt.Println("===================")
	fmt.Printf("%-20s %5s %10s %10s %8s %5s %8s\n",
		"SECTOR", "DOCS", "FRONTMTR", "CONTENT", "RATIO", "REFS", "DENSITY")
	fmt.Println("-------------------- ----- ---------- ---------- -------- ----- --------")

	for _, m := range metrics {
		sector := m.Sector
		if len(sector) > 20 {
			sector = sector[:17] + "..."
		}
		fmt.Printf("%-20s %5d %10s %10s %7.1f%% %5d %7.2f\n",
			sector, m.DocCount,
			formatBytes(m.FrontmatterBytes), formatBytes(m.ContentBytes),
			m.SubstanceRatio*100, m.RefCount, m.RefDensity)
	}

	return nil
}

func showSubstanceByType(c *constellation.Constellation) error {
	metrics, err := c.SubstanceReportByType()
	if err != nil {
		return fmt.Errorf("failed to get type report: %w", err)
	}

	fmt.Println("Substance by Type")
	fmt.Println("=================")
	fmt.Printf("%-20s %5s %10s %10s %8s %5s %8s\n",
		"TYPE", "DOCS", "FRONTMTR", "CONTENT", "RATIO", "REFS", "DENSITY")
	fmt.Println("-------------------- ----- ---------- ---------- -------- ----- --------")

	for _, m := range metrics {
		docType := m.Type
		if len(docType) > 20 {
			docType = docType[:17] + "..."
		}
		fmt.Printf("%-20s %5d %10s %10s %7.1f%% %5d %7.2f\n",
			docType, m.DocCount,
			formatBytes(m.FrontmatterBytes), formatBytes(m.ContentBytes),
			m.SubstanceRatio*100, m.RefCount, m.RefDensity)
	}

	return nil
}

func showRoutingLayers(c *constellation.Constellation) error {
	// Find documents with < 50% substance and >= 3 refs
	metrics, err := c.FindRoutingLayers(0.5, 3)
	if err != nil {
		return fmt.Errorf("failed to find routing layers: %w", err)
	}

	if len(metrics) == 0 {
		fmt.Println("No routing layers found (documents with <50% substance and 3+ refs)")
		return nil
	}

	fmt.Println("Routing Layers (Low Substance, High Refs)")
	fmt.Println("=========================================")
	fmt.Printf("Documents that may be over-abstracted 'wiring' with little actual content:\n\n")
	fmt.Printf("%-50s %8s %5s %8s\n", "PATH", "RATIO", "REFS", "DENSITY")
	fmt.Println("-------------------------------------------------- -------- ----- --------")

	for _, m := range metrics {
		path := m.Path
		if len(path) > 50 {
			path = "..." + path[len(path)-47:]
		}
		fmt.Printf("%-50s %7.1f%% %5d %7.2f\n",
			path, m.SubstanceRatio*100, m.RefCount, m.RefDensity)
	}

	return nil
}

func showLeafNodes(c *constellation.Constellation) error {
	// Find documents with >= 70% substance and <= 2 refs
	metrics, err := c.FindLeafNodes(0.7, 2)
	if err != nil {
		return fmt.Errorf("failed to find leaf nodes: %w", err)
	}

	if len(metrics) == 0 {
		fmt.Println("No leaf nodes found (documents with >=70% substance and <=2 refs)")
		return nil
	}

	fmt.Println("Leaf Nodes (High Substance, Low Refs)")
	fmt.Println("=====================================")
	fmt.Printf("Documents with actual knowledge content:\n\n")
	fmt.Printf("%-50s %8s %5s %10s\n", "PATH", "RATIO", "REFS", "CONTENT")
	fmt.Println("-------------------------------------------------- -------- ----- ----------")

	for _, m := range metrics {
		path := m.Path
		if len(path) > 50 {
			path = "..." + path[len(path)-47:]
		}
		fmt.Printf("%-50s %7.1f%% %5d %10s\n",
			path, m.SubstanceRatio*100, m.RefCount, formatBytes(m.ContentBytes))
	}

	return nil
}

// formatBytes formats byte count as human-readable string
func formatBytes(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	} else {
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}
