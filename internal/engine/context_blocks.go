// context_blocks.go — builder functions for well-known context blocks
//
// Each build* function produces a single ContextBlock (or nil when the source
// data is unavailable). The foveated context pipeline calls these builders,
// collects non-nil results into a ContextFrame, and renders the frame.
package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// buildProjectBlock reads CLAUDE.md from the workspace root and returns it
// as a tier-0 context block. Returns nil if CLAUDE.md doesn't exist.
func buildProjectBlock(workspaceRoot string) *ContextBlock {
	path := filepath.Join(workspaceRoot, "CLAUDE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	// Truncate if over 2000 tokens (~8000 chars) to fit budget.
	if len(content) > 8000 {
		content = content[:8000] + "\n\n[truncated — full file at CLAUDE.md]"
	}
	block := NewBlock(BlockProject, content)
	return &block
}

// buildNodeBlock renders sibling service health as a markdown table block.
// Returns nil if NodeHealth is nil or has no services.
func buildNodeBlock(process *Process) *ContextBlock {
	if process == nil {
		return nil
	}
	nh := process.NodeHealth()
	if nh == nil {
		return nil
	}
	snap := nh.Snapshot()
	if len(snap) == 0 {
		return nil
	}

	// Sort service names alphabetically.
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString("## Node Health\n")
	sb.WriteString("| Service | Status |\n")
	sb.WriteString("|---------|--------|\n")
	for _, name := range names {
		fmt.Fprintf(&sb, "| %s | %s |\n", name, snap[name].Status)
	}

	block := NewBlock(BlockNode, sb.String())
	return &block
}

// buildFieldBlock renders the top-10 attentional field entries as a markdown table.
// Returns nil if the field is nil or empty.
func buildFieldBlock(process *Process, workspaceRoot string) *ContextBlock {
	if process == nil {
		return nil
	}
	field := process.Field()
	if field == nil {
		return nil
	}
	fovea := field.Fovea(10)
	if len(fovea) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("## Active Attention\n")
	sb.WriteString("| Document | Salience |\n")
	sb.WriteString("|----------|----------|\n")
	for _, fs := range fovea {
		uri := FieldKeyToURI(workspaceRoot, fs.Path)
		fmt.Fprintf(&sb, "| %s | %.3f |\n", uri, fs.Score)
	}

	block := NewBlock(BlockField, sb.String())
	return &block
}

// buildEventsBlock renders the last 5 ledger events from the flat events.jsonl file.
// Returns nil if the file doesn't exist or contains no parseable events.
func buildEventsBlock(workspaceRoot string) *ContextBlock {
	ledgerPath := filepath.Join(workspaceRoot, ".cog", "ledger", "events.jsonl")

	f, err := os.Open(ledgerPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Read all lines, then take the last 5.
	var lines []string
	scanner := bufio.NewScanner(f)
	// Increase buffer for potentially large JSONL lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}

	// Take the last 5 lines.
	start := 0
	if len(lines) > 5 {
		start = len(lines) - 5
	}
	tail := lines[start:]

	var sb strings.Builder
	sb.WriteString("## Recent Events\n")
	rendered := 0

	for _, line := range tail {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		timestamp, _ := event["timestamp"].(string)
		eventType, _ := event["type"].(string)

		// Extract first key from payload for a summary line.
		payloadSummary := ""
		if payload, ok := event["payload"].(map[string]any); ok {
			payloadSummary = firstPayloadValue(payload)
		}

		if payloadSummary != "" {
			fmt.Fprintf(&sb, "- [%s] %s: %s\n", timestamp, eventType, payloadSummary)
		} else {
			fmt.Fprintf(&sb, "- [%s] %s\n", timestamp, eventType)
		}
		rendered++
	}

	if rendered == 0 {
		return nil
	}

	block := NewBlock(BlockEvents, sb.String())
	return &block
}

// firstPayloadValue returns the string representation of the first key
// (alphabetically) in the payload map. Returns "" if the map is empty.
func firstPayloadValue(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("%v", payload[keys[0]])
}
