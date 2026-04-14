package constellation

import (
	"fmt"
	"strings"
	"time"
)

// BusEvent represents a bus event to be indexed in the constellation.
// Only chat.request and chat.response events with non-empty content
// should be indexed — system events, tool invocations, etc. are skipped.
type BusEvent struct {
	BusID     string // Bus identifier (e.g., "bus_chat_cog-discord-100000000000000001")
	Seq       int    // Sequence number within the bus
	Timestamp string // RFC3339Nano timestamp
	From      string // Sender (e.g., "http:user", "kernel:cogos")
	Type      string // Event type ("chat.request" or "chat.response")
	Content   string // Message content
	Hash      string // Content-addressed hash of the CogBlock
	Origin    string // Origin platform (e.g., "discord", "http", "claude-code")
	Agent     string // Agent name (optional)
	UserID    string // User identifier (optional)
	UserName  string // User display name (optional)
}

// IndexBusEvent indexes a single bus event into the constellation for full-text search.
// Inserts into both the documents table and documents_fts for immediate searchability.
// Idempotent: uses INSERT OR REPLACE keyed on the deterministic document ID.
func (c *Constellation) IndexBusEvent(evt BusEvent) error {
	if evt.Content == "" {
		return nil // nothing to index
	}

	// Deterministic ID: bus:{busID}:seq:{seq}
	docID := fmt.Sprintf("bus:%s:seq:%d", evt.BusID, evt.Seq)

	// Path: synthetic path for uniqueness constraint
	// Uses the actual events.jsonl location with fragment identifier
	path := fmt.Sprintf(".cog/.state/buses/%s/events.jsonl#%d", evt.BusID, evt.Seq)

	// Title: descriptive summary for search result display
	role := "user"
	if evt.Type == "chat.response" {
		role = "assistant"
	}
	origin := evt.Origin
	if origin == "" {
		origin = "unknown"
	}
	title := fmt.Sprintf("[%s] %s via %s", role, summarizeContent(evt.Content, 80), origin)

	// Use block hash as content hash, or compute from content
	contentHash := evt.Hash
	if contentHash == "" {
		contentHash = docID // fallback — still unique
	}

	wordCount := len(strings.Fields(evt.Content))
	lineCount := strings.Count(evt.Content, "\n") + 1
	contentBytes := len(evt.Content)
	now := time.Now().Format(time.RFC3339)

	// Build searchable tags from metadata
	var tags []string
	if evt.Origin != "" {
		tags = append(tags, evt.Origin)
	}
	if evt.Agent != "" {
		tags = append(tags, evt.Agent)
	}
	if evt.UserName != "" {
		tags = append(tags, evt.UserName)
	}
	tags = append(tags, role)
	tagStr := strings.Join(tags, " ")

	// Insert into documents table
	_, err := c.db.Exec(`
		INSERT OR REPLACE INTO documents (
			id, path, type, title, created, updated, sector, status,
			content, content_hash, word_count, line_count,
			indexed_at, file_mtime,
			frontmatter_bytes, content_bytes, substance_ratio, ref_count, ref_density
		) VALUES (?, ?, 'bus_message', ?, ?, ?, 'episodic', '',
			?, ?, ?, ?,
			?, ?,
			0, ?, 1.0, 0, 0.0)
	`, docID, path, title, evt.Timestamp, evt.Timestamp,
		evt.Content, contentHash, wordCount, lineCount,
		now, evt.Timestamp,
		contentBytes)
	if err != nil {
		return fmt.Errorf("failed to insert bus event document: %w", err)
	}

	// Insert into FTS for immediate searchability
	// Delete first to handle REPLACE (FTS5 doesn't support INSERT OR REPLACE)
	if _, err := c.db.Exec("DELETE FROM documents_fts WHERE id = ?", docID); err != nil {
		return fmt.Errorf("failed to clear FTS entry: %w", err)
	}
	_, err = c.db.Exec(`
		INSERT INTO documents_fts(id, title, content, tags, sector, type)
		VALUES (?, ?, ?, ?, 'episodic', 'bus_message')
	`, docID, title, evt.Content, tagStr)
	if err != nil {
		return fmt.Errorf("failed to insert bus event into FTS: %w", err)
	}

	return nil
}

// summarizeContent returns the first N characters of content, truncated at word boundary.
func summarizeContent(content string, maxLen int) string {
	if len(content) <= maxLen {
		// Strip newlines for title
		return strings.ReplaceAll(strings.ReplaceAll(content, "\n", " "), "\r", "")
	}
	// Truncate at word boundary
	truncated := content[:maxLen]
	if idx := strings.LastIndexByte(truncated, ' '); idx > maxLen/2 {
		truncated = truncated[:idx]
	}
	truncated = strings.ReplaceAll(strings.ReplaceAll(truncated, "\n", " "), "\r", "")
	return truncated + "..."
}
