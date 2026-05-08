// constellation_sessions.go - Index Claude Code session transcripts into constellation
//
// Parses JSONL transcripts from ~/.claude/projects/ and indexes them as
// searchable documents in the constellation knowledge graph.
//
// Each session becomes one document with:
//   - Title derived from the first user message
//   - Content: all user messages + assistant text blocks (no tool results)
//   - Tags: session ID, branch, tools used
//   - Type: "session_transcript"
//   - Sector: "episodic"

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/myrgic/cogos/sdk/constellation"
)

// sessionTranscriptEntry represents one line of a Claude Code JSONL transcript.
type sessionTranscriptEntry struct {
	Type      string                 `json:"type"`
	SessionID string                 `json:"sessionId"`
	Timestamp string                 `json:"timestamp"`
	GitBranch string                 `json:"gitBranch"`
	Message   *sessionMessage        `json:"message,omitempty"`
	Slug      string                 `json:"slug"`
	UUID      string                 `json:"uuid"`
	Extra     map[string]interface{} `json:"-"` // catch-all
}

// sessionMessage is the message payload inside user/assistant entries.
type sessionMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []contentBlock
}

// contentBlock is one block in a message's content array.
type contentBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"`  // for tool_use
	Input struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		Pattern     string `json:"pattern"`
	} `json:"input"` // for tool_use
}

// sessionSummary collects extracted content from a transcript.
type sessionSummary struct {
	SessionID     string
	Branch        string
	FirstTimestamp string
	LastTimestamp  string
	UserMessages  []string
	AssistantText []string
	ToolsUsed     map[string]int
	FilesPaths    map[string]bool
	MessageCount  int
}

// parseSessionTranscript reads a JSONL transcript and extracts searchable content.
func parseSessionTranscript(path string) (*sessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	summary := &sessionSummary{
		ToolsUsed:  make(map[string]int),
		FilesPaths: make(map[string]bool),
	}

	scanner := bufio.NewScanner(f)
	// Increase buffer for large lines (tool results can be huge)
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		var entry sessionTranscriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // skip malformed lines
		}

		if entry.SessionID != "" && summary.SessionID == "" {
			summary.SessionID = entry.SessionID
		}
		if entry.GitBranch != "" && summary.Branch == "" {
			summary.Branch = entry.GitBranch
		}
		if entry.Timestamp != "" {
			if summary.FirstTimestamp == "" {
				summary.FirstTimestamp = entry.Timestamp
			}
			summary.LastTimestamp = entry.Timestamp
		}

		if entry.Message == nil {
			continue
		}

		switch entry.Type {
		case "user":
			text := extractText(entry.Message.Content)
			if text != "" {
				// Strip system tags and IDE context for cleaner indexing
				cleaned := cleanUserMessage(text)
				if cleaned != "" {
					summary.UserMessages = append(summary.UserMessages, cleaned)
					summary.MessageCount++
				}
			}

		case "assistant":
			blocks := extractBlocks(entry.Message.Content)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						// Truncate very long assistant messages
						text := b.Text
						if len(text) > 2000 {
							text = text[:2000] + "..."
						}
						summary.AssistantText = append(summary.AssistantText, text)
						summary.MessageCount++
					}
				case "tool_use":
					summary.ToolsUsed[b.Name]++
					// Track files touched
					if b.Input.FilePath != "" {
						summary.FilesPaths[b.Input.FilePath] = true
					}
				}
			}
		}
	}

	if summary.MessageCount == 0 {
		return nil, fmt.Errorf("no messages found")
	}

	return summary, scanner.Err()
}

// extractText pulls plain text from a message content field.
func extractText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// extractBlocks parses content into typed blocks.
func extractBlocks(content interface{}) []contentBlock {
	arr, ok := content.([]interface{})
	if !ok {
		// String content — wrap as text block
		if s, ok := content.(string); ok {
			return []contentBlock{{Type: "text", Text: s}}
		}
		return nil
	}

	var blocks []contentBlock
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		b := contentBlock{
			Type: stringVal(m, "type"),
			Text: stringVal(m, "text"),
			Name: stringVal(m, "name"),
		}
		if inputMap, ok := m["input"].(map[string]interface{}); ok {
			b.Input.Command = stringVal(inputMap, "command")
			b.Input.Description = stringVal(inputMap, "description")
			b.Input.FilePath = stringVal(inputMap, "file_path")
			b.Input.Pattern = stringVal(inputMap, "pattern")
		}
		blocks = append(blocks, b)
	}
	return blocks
}

func stringVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// cleanUserMessage strips system tags, IDE context, and other noise.
func cleanUserMessage(text string) string {
	// Remove <system-reminder>...</system-reminder> blocks
	for {
		start := strings.Index(text, "<system-reminder>")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</system-reminder>")
		if end == -1 {
			// Unclosed tag — remove from start to end of string
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len("</system-reminder>"):]
	}

	// Remove <ide_selection>...</ide_selection> blocks
	for {
		start := strings.Index(text, "<ide_selection>")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</ide_selection>")
		if end == -1 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len("</ide_selection>"):]
	}

	// Remove <ide_opened_file> blocks
	for {
		start := strings.Index(text, "<ide_opened_file>")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "</ide_opened_file>")
		if end == -1 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len("</ide_opened_file>"):]
	}

	text = strings.TrimSpace(text)
	return text
}

// indexSessionTranscript indexes a parsed session into the constellation.
func indexSessionTranscript(c *constellation.Constellation, transcriptPath string, summary *sessionSummary) error {
	// Build deterministic document ID from session ID or file
	sessionID := summary.SessionID
	if sessionID == "" {
		sessionID = filepath.Base(transcriptPath)
		sessionID = strings.TrimSuffix(sessionID, ".jsonl")
	}
	docID := fmt.Sprintf("session:%s", sessionID)

	// Build title from first user message
	title := "(empty session)"
	if len(summary.UserMessages) > 0 {
		title = truncateAtWord(summary.UserMessages[0], 100)
	}

	// Build content: interleaved user/assistant messages
	var content strings.Builder
	maxContentChars := 8000 // Keep indexed content reasonable

	// Add session metadata header
	if summary.Branch != "" {
		content.WriteString(fmt.Sprintf("Branch: %s\n", summary.Branch))
	}
	content.WriteString(fmt.Sprintf("Messages: %d\n\n", summary.MessageCount))

	// Add user messages (the primary search targets)
	for i, msg := range summary.UserMessages {
		if content.Len() > maxContentChars {
			break
		}
		content.WriteString(fmt.Sprintf("User: %s\n\n", msg))
		// Interleave with assistant if available
		if i < len(summary.AssistantText) {
			text := summary.AssistantText[i]
			if len(text) > 500 {
				text = text[:500] + "..."
			}
			content.WriteString(fmt.Sprintf("Assistant: %s\n\n", text))
		}
	}

	// Add remaining assistant messages
	for i := len(summary.UserMessages); i < len(summary.AssistantText) && content.Len() < maxContentChars; i++ {
		text := summary.AssistantText[i]
		if len(text) > 500 {
			text = text[:500] + "..."
		}
		content.WriteString(fmt.Sprintf("Assistant: %s\n\n", text))
	}

	contentStr := content.String()

	// Content hash for deduplication
	h := sha256.Sum256([]byte(contentStr))
	contentHash := fmt.Sprintf("%x", h[:16])

	// Build tags
	var tags []string
	tags = append(tags, "claude-code", "session")
	if summary.Branch != "" {
		tags = append(tags, summary.Branch)
	}
	// Top tools used
	for tool := range summary.ToolsUsed {
		tags = append(tags, tool)
	}
	tagStr := strings.Join(tags, " ")

	// Timestamps
	created := summary.FirstTimestamp
	if created == "" {
		created = time.Now().Format(time.RFC3339)
	}
	updated := summary.LastTimestamp
	if updated == "" {
		updated = created
	}

	wordCount := len(strings.Fields(contentStr))
	lineCount := strings.Count(contentStr, "\n") + 1
	contentBytes := len(contentStr)
	now := time.Now().Format(time.RFC3339)

	// Synthetic path
	path := fmt.Sprintf("claude-code/sessions/%s.jsonl", sessionID)

	// Insert into documents table
	_, err := c.DB().Exec(`
		INSERT OR REPLACE INTO documents (
			id, path, type, title, created, updated, sector, status,
			content, content_hash, word_count, line_count,
			indexed_at, file_mtime,
			frontmatter_bytes, content_bytes, substance_ratio, ref_count, ref_density
		) VALUES (?, ?, 'session_transcript', ?, ?, ?, 'episodic', '',
			?, ?, ?, ?,
			?, ?,
			0, ?, 1.0, 0, 0.0)
	`, docID, path, title, created, updated,
		contentStr, contentHash, wordCount, lineCount,
		now, updated,
		contentBytes)
	if err != nil {
		return fmt.Errorf("failed to insert session document: %w", err)
	}

	// Insert into FTS
	if _, err := c.DB().Exec("DELETE FROM documents_fts WHERE id = ?", docID); err != nil {
		return fmt.Errorf("failed to clear FTS entry: %w", err)
	}
	_, err = c.DB().Exec(`
		INSERT INTO documents_fts(id, title, content, tags, sector, type)
		VALUES (?, ?, ?, ?, 'episodic', 'session_transcript')
	`, docID, title, contentStr, tagStr)
	if err != nil {
		return fmt.Errorf("failed to insert session into FTS: %w", err)
	}

	return nil
}

// truncateAtWord returns the first N characters of content, truncated at a word boundary.
func truncateAtWord(content string, maxLen int) string {
	// Strip newlines for title use
	flat := strings.ReplaceAll(strings.ReplaceAll(content, "\n", " "), "\r", "")
	if len(flat) <= maxLen {
		return flat
	}
	truncated := flat[:maxLen]
	if idx := strings.LastIndexByte(truncated, ' '); idx > maxLen/2 {
		truncated = truncated[:idx]
	}
	return truncated + "..."
}

// backfillSessionTranscripts indexes all Claude Code JSONL transcripts.
func backfillSessionTranscripts(workspaceRoot string) error {
	c, err := getConstellationForWorkspace(workspaceRoot)
	if err != nil {
		return fmt.Errorf("failed to open constellation: %w", err)
	}

	// Find transcript directory
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	// Claude Code stores transcripts at ~/.claude/projects/{project-key}/*.jsonl
	// The project key is the workspace path with / replaced by -
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("failed to resolve workspace root: %w", err)
	}
	projectKey := strings.ReplaceAll(absRoot, "/", "-")
	transcriptDir := filepath.Join(home, ".claude", "projects", projectKey)

	if _, err := os.Stat(transcriptDir); os.IsNotExist(err) {
		return fmt.Errorf("transcript directory not found: %s", transcriptDir)
	}

	// Find all JSONL files
	files, err := filepath.Glob(filepath.Join(transcriptDir, "*.jsonl"))
	if err != nil {
		return fmt.Errorf("failed to glob transcripts: %w", err)
	}

	indexed := 0
	skipped := 0
	errors := 0

	for _, path := range files {
		summary, err := parseSessionTranscript(path)
		if err != nil {
			skipped++
			continue
		}

		if err := indexSessionTranscript(c, path, summary); err != nil {
			fmt.Fprintf(os.Stderr, "[session-indexer] failed to index %s: %v\n",
				filepath.Base(path), err)
			errors++
			continue
		}
		indexed++

		if indexed%100 == 0 {
			fmt.Printf("[session-indexer] indexed %d sessions so far\n", indexed)
		}
	}

	fmt.Printf("Session indexing complete: %d indexed, %d skipped, %d errors (from %d files)\n",
		indexed, skipped, errors, len(files))

	return nil
}
