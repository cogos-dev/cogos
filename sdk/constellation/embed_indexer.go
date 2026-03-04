package constellation

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// EmbedIndexer handles embedding generation for documents in the constellation.
type EmbedIndexer struct {
	c      *Constellation
	client *EmbedClient
}

// NewEmbedIndexer creates an indexer that generates embeddings for documents.
func NewEmbedIndexer(c *Constellation, client *EmbedClient) *EmbedIndexer {
	return &EmbedIndexer{c: c, client: client}
}

// frontmatterRe strips YAML frontmatter (---\n...\n---) from content.
var frontmatterRe = regexp.MustCompile(`(?s)\A---\n.*?\n---\n?`)

// stripFrontmatter removes YAML frontmatter from document content for embedding.
func stripFrontmatter(content string) string {
	return frontmatterRe.ReplaceAllString(content, "")
}

// BackfillAll generates embeddings for all documents that don't have them yet,
// or whose content has changed since last embedding. Processes in batches.
// Returns the number of documents embedded.
func (ei *EmbedIndexer) BackfillAll(batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 20
	}

	total := 0
	for {
		n, err := ei.backfillBatch(batchSize)
		if err != nil {
			return total, fmt.Errorf("backfill batch failed after %d docs: %w", total, err)
		}
		total += n
		if n < batchSize {
			break // no more stale documents
		}
		fmt.Fprintf(os.Stderr, "[embed-indexer] embedded %d documents so far\n", total)
	}

	return total, nil
}

// backfillBatch processes one batch of stale/unembedded documents.
func (ei *EmbedIndexer) backfillBatch(limit int) (int, error) {
	// Find documents needing embedding:
	// 1. No embedding yet (embedding_768 IS NULL)
	// 2. Content changed (content_hash != embedding_hash)
	rows, err := ei.c.db.Query(`
		SELECT id, title, content, content_hash
		FROM documents
		WHERE embedding_768 IS NULL
		   OR embedding_hash IS NULL
		   OR embedding_hash != content_hash
		LIMIT ?
	`, limit)
	if err != nil {
		return 0, fmt.Errorf("query stale docs: %w", err)
	}
	defer rows.Close()

	type docInfo struct {
		id          string
		title       string
		content     string
		contentHash string
	}
	var docs []docInfo

	for rows.Next() {
		var d docInfo
		if err := rows.Scan(&d.id, &d.title, &d.content, &d.contentHash); err != nil {
			return 0, err
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(docs) == 0 {
		return 0, nil
	}

	// Prepare texts for embedding (title + stripped content)
	texts := make([]string, len(docs))
	for i, d := range docs {
		stripped := stripFrontmatter(d.content)
		texts[i] = d.title + "\n" + stripped
		// Truncate very long texts to avoid timeouts
		if len(texts[i]) > 8000 {
			texts[i] = texts[i][:8000]
		}
	}

	// Call embed server
	results, err := ei.client.Embed(texts, "search_document")
	if err != nil {
		return 0, fmt.Errorf("embed batch: %w", err)
	}

	// Store embeddings
	tx, err := ei.c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "Warning: embed tx rollback: %v\n", err)
		}
	}()

	stmt, err := tx.Prepare(`
		UPDATE documents
		SET embedding_768 = ?, embedding_128 = ?, embedding_hash = ?
		WHERE id = ?
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for i, d := range docs {
		_, err := stmt.Exec(
			Float32ToBytes(results[i].Embedding768),
			Float32ToBytes(results[i].Embedding128),
			d.contentHash,
			d.id,
		)
		if err != nil {
			return 0, fmt.Errorf("store embedding for %s: %w", d.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return len(docs), nil
}

// EmbedSingleDoc generates and stores an embedding for a single document by ID.
// Used by the write hook for incremental updates.
func (ei *EmbedIndexer) EmbedSingleDoc(docID string) error {
	var title, content, contentHash string
	err := ei.c.db.QueryRow(
		`SELECT title, content, content_hash FROM documents WHERE id = ?`, docID,
	).Scan(&title, &content, &contentHash)
	if err != nil {
		return fmt.Errorf("fetch doc %s: %w", docID, err)
	}

	stripped := stripFrontmatter(content)
	text := title + "\n" + stripped
	if len(text) > 8000 {
		text = text[:8000]
	}

	result, err := ei.client.EmbedOne(text, "search_document")
	if err != nil {
		return fmt.Errorf("embed doc %s: %w", docID, err)
	}

	_, err = ei.c.db.Exec(`
		UPDATE documents
		SET embedding_768 = ?, embedding_128 = ?, embedding_hash = ?
		WHERE id = ?
	`,
		Float32ToBytes(result.Embedding768),
		Float32ToBytes(result.Embedding128),
		contentHash,
		docID,
	)
	return err
}

// ---------------------------------------------------------------------------
// Freshness checking (B.3)
// ---------------------------------------------------------------------------

// EmbedStatus reports on the state of embeddings in the constellation.
type EmbedStatus struct {
	TotalDocs    int // Total documents in constellation
	Embedded     int // Documents with embeddings
	Stale        int // Documents where content changed since embedding
	Missing      int // Documents without any embedding
	StalePaths   []string // Paths of stale documents (for diagnostics)
}

// CheckFreshness reports how many documents need re-embedding.
func (ei *EmbedIndexer) CheckFreshness() (*EmbedStatus, error) {
	status := &EmbedStatus{}

	// Total docs
	err := ei.c.db.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&status.TotalDocs)
	if err != nil {
		return nil, err
	}

	// Embedded docs (have embedding and hash matches)
	err = ei.c.db.QueryRow(`
		SELECT COUNT(*) FROM documents
		WHERE embedding_768 IS NOT NULL AND embedding_hash = content_hash
	`).Scan(&status.Embedded)
	if err != nil {
		return nil, err
	}

	// Missing embeddings
	err = ei.c.db.QueryRow(`
		SELECT COUNT(*) FROM documents WHERE embedding_768 IS NULL
	`).Scan(&status.Missing)
	if err != nil {
		return nil, err
	}

	// Stale embeddings (have embedding but content changed)
	err = ei.c.db.QueryRow(`
		SELECT COUNT(*) FROM documents
		WHERE embedding_768 IS NOT NULL
		  AND (embedding_hash IS NULL OR embedding_hash != content_hash)
	`).Scan(&status.Stale)
	if err != nil {
		return nil, err
	}

	// Get stale paths for diagnostics
	rows, err := ei.c.db.Query(`
		SELECT path FROM documents
		WHERE embedding_768 IS NULL
		   OR embedding_hash IS NULL
		   OR embedding_hash != content_hash
		ORDER BY updated DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		status.StalePaths = append(status.StalePaths, path)
	}

	return status, nil
}

// FormatStatus returns a human-readable summary of embedding freshness.
func (s *EmbedStatus) FormatStatus() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Embedding Status:\n"))
	sb.WriteString(fmt.Sprintf("  Total documents: %d\n", s.TotalDocs))
	sb.WriteString(fmt.Sprintf("  Embedded (fresh): %d\n", s.Embedded))
	sb.WriteString(fmt.Sprintf("  Missing: %d\n", s.Missing))
	sb.WriteString(fmt.Sprintf("  Stale: %d\n", s.Stale))

	if len(s.StalePaths) > 0 {
		sb.WriteString(fmt.Sprintf("\n  Documents needing embedding (%d shown):\n", len(s.StalePaths)))
		for _, p := range s.StalePaths {
			sb.WriteString(fmt.Sprintf("    - %s\n", p))
		}
	}
	return sb.String()
}
