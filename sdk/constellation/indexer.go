package constellation

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Cogdoc represents a parsed cogdoc with frontmatter and content.
type Cogdoc struct {
	ID               string
	Path             string
	Type             string
	Title            string
	Created          string
	Updated          string
	Sector           string
	Status           string
	Tags             []string
	Refs             []Reference
	Content          string
	FrontmatterBytes int // Size of YAML frontmatter in bytes
}

// Reference represents a document reference from frontmatter.
type Reference struct {
	URI string
	Rel string
}

// IndexWorkspace scans the workspace and indexes all cogdocs.
func (c *Constellation) IndexWorkspace() error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// Fix 3: Transaction Rollback Safety
	// Defer rollback with proper error handling
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "Warning: transaction rollback failed: %v\n", err)
		}
	}()

	indexed := 0
	skipped := 0
	var indexErr error

	// Walk .cog directory for cogdocs
	err = filepath.WalkDir(filepath.Join(c.root, ".cog"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip .state directory
		if d.IsDir() && d.Name() == ".state" {
			return fs.SkipDir
		}

		// Index *.cog.md files
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".cog.md") {
			if err := c.indexCogdoc(tx, path); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to index %s: %v\n", path, err)
				skipped++
				// Store first error but continue indexing
				if indexErr == nil {
					indexErr = err
				}
			} else {
				indexed++
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk workspace: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Fix 1: Two-pass URI resolution
	// After all documents are indexed, resolve unresolved references
	// This fixes the chicken-and-egg problem where doc A references doc B
	// but B hasn't been indexed yet during A's indexing
	fmt.Printf("Resolving unresolved references (second pass)...\n")
	resolved, err := c.resolveUnresolvedRefs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to resolve refs: %v\n", err)
	} else {
		fmt.Printf("Resolved %d additional references\n", resolved)
	}

	// Fix 2: Rebuild FTS index to sync tags
	// Tags are inserted AFTER documents, so we need to rebuild FTS after commit
	fmt.Printf("Rebuilding FTS index with tags...\n")
	if err := c.rebuildFTS(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to rebuild FTS: %v\n", err)
	}

	fmt.Printf("Indexed %d cogdocs (%d skipped)\n", indexed, skipped)

	// Async: trigger embedding backfill if embed client is configured
	if c.embedClient != nil {
		go func() {
			indexer := NewEmbedIndexer(c, c.embedClient)
			n, err := indexer.BackfillAll(20)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[embed-indexer] backfill error: %v\n", err)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "[embed-indexer] backfilled %d documents\n", n)
			}
		}()
	}

	return indexErr
}

// indexCogdoc parses and indexes a single cogdoc file.
func (c *Constellation) indexCogdoc(tx *sql.Tx, path string) error {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Get file mtime
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mtime := info.ModTime().Format(time.RFC3339)

	// Parse cogdoc
	doc, err := parseCogdoc(data, path)
	if err != nil {
		return err
	}

	// Compute content hash
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(doc.Content)))

	// Check if already indexed with same hash
	var existingHash string
	err = tx.QueryRow("SELECT content_hash FROM documents WHERE path = ?", path).Scan(&existingHash)
	if err == nil && existingHash == contentHash {
		// Already indexed, no change
		return nil
	}

	// Calculate stats
	wordCount := len(strings.Fields(doc.Content))
	lineCount := strings.Count(doc.Content, "\n") + 1

	// Calculate substance metrics
	frontmatterBytes := doc.FrontmatterBytes
	contentBytes := len(doc.Content)
	substanceRatio := 0.0
	if frontmatterBytes+contentBytes > 0 {
		substanceRatio = float64(contentBytes) / float64(contentBytes+frontmatterBytes)
	}
	refCount := len(doc.Refs)
	refDensity := 0.0
	if contentBytes > 0 {
		refDensity = float64(refCount) / (float64(contentBytes) / 1024.0) // refs per KB
	}

	// Insert or replace document
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO documents (
			id, path, type, title, created, updated, sector, status,
			content, content_hash, word_count, line_count,
			indexed_at, file_mtime,
			frontmatter_bytes, content_bytes, substance_ratio, ref_count, ref_density
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, doc.ID, path, doc.Type, doc.Title, doc.Created, doc.Updated, doc.Sector, doc.Status,
		doc.Content, contentHash, wordCount, lineCount, time.Now().Format(time.RFC3339), mtime,
		frontmatterBytes, contentBytes, substanceRatio, refCount, refDensity)

	if err != nil {
		return fmt.Errorf("failed to insert document: %w", err)
	}

	// Delete old tags and doc_references
	if _, err := tx.Exec("DELETE FROM tags WHERE document_id = ?", doc.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM doc_references WHERE source_id = ?", doc.ID); err != nil {
		return err
	}

	// Insert tags
	for _, tag := range doc.Tags {
		_, err := tx.Exec("INSERT INTO tags (document_id, tag) VALUES (?, ?)", doc.ID, tag)
		if err != nil {
			return fmt.Errorf("failed to insert tag: %w", err)
		}
	}

	// Insert doc_references
	for _, ref := range doc.Refs {
		// Try to resolve target_id from URI
		targetID := resolveURIToID(tx, ref.URI)

		_, err := tx.Exec(`
			INSERT INTO doc_references (source_id, target_uri, target_id, relation)
			VALUES (?, ?, ?, ?)
		`, doc.ID, ref.URI, targetID, ref.Rel)

		if err != nil {
			return fmt.Errorf("failed to insert reference: %w", err)
		}
	}

	return nil
}

// parseCogdoc parses frontmatter and content from a cogdoc file.
func parseCogdoc(data []byte, path string) (*Cogdoc, error) {
	// Split frontmatter and content
	parts := strings.SplitN(string(data), "---", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("invalid cogdoc: missing frontmatter")
	}

	frontmatterYAML := parts[1]
	content := strings.TrimSpace(parts[2])

	// Parse frontmatter
	var fm struct {
		ID      string                   `yaml:"id"`
		Type    string                   `yaml:"type"`
		Title   string                   `yaml:"title"`
		Created string                   `yaml:"created"`
		Updated string                   `yaml:"updated"`
		Sector  string                   `yaml:"sector"`
		Status  string                   `yaml:"status"`
		Tags    []string                 `yaml:"tags"`
		Refs    []interface{}            `yaml:"refs"`
	}

	// Fix 8: YAML Parsing Robustness
	// Keep strict parsing but provide better error context
	if err := yaml.Unmarshal([]byte(frontmatterYAML), &fm); err != nil {
		// Enhanced error message with file context
		return nil, fmt.Errorf("failed to parse frontmatter in %s: %w", filepath.Base(path), err)
	}

	// Fix 9: Empty Title Fallback
	// Implement title fallback cascade: frontmatter → H1 → filename
	if fm.Title == "" {
		// Try to extract from first H1 heading in content
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "# ") {
				fm.Title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
				break
			}
		}

		// If still empty, use filename without .cog.md extension
		if fm.Title == "" {
			fm.Title = strings.TrimSuffix(filepath.Base(path), ".cog.md")
		}
	}

	// Fix 10: Auto-generate ID from path if missing
	// This prevents all files without IDs from colliding on empty string
	if fm.ID == "" {
		// Generate ID from path relative to .cog/
		// Example: /path/to/.cog/mem/semantic/foo.cog.md → mem-semantic-foo
		relPath := path
		// Find .cog/ in path and take everything after it
		if idx := strings.Index(path, ".cog/"); idx != -1 {
			relPath = path[idx+5:] // Skip ".cog/"
		}
		relPath = strings.TrimSuffix(relPath, ".cog.md")
		// Replace slashes and dots with dashes for a valid ID
		fm.ID = strings.ReplaceAll(strings.ReplaceAll(relPath, "/", "-"), ".", "-")
	}

	// Parse refs (can be simple strings or typed objects)
	var refs []Reference
	for _, refRaw := range fm.Refs {
		switch ref := refRaw.(type) {
		case string:
			refs = append(refs, Reference{URI: ref, Rel: "refs"})
		case map[string]interface{}:
			uri, _ := ref["uri"].(string)
			rel, _ := ref["rel"].(string)
			if rel == "" {
				rel = "refs"
			}
			refs = append(refs, Reference{URI: uri, Rel: rel})
		}
	}

	return &Cogdoc{
		ID:               fm.ID,
		Path:             path,
		Type:             fm.Type,
		Title:            fm.Title,
		Created:          fm.Created,
		Updated:          fm.Updated,
		Sector:           fm.Sector,
		Status:           fm.Status,
		Tags:             fm.Tags,
		Refs:             refs,
		Content:          content,
		FrontmatterBytes: len(frontmatterYAML),
	}, nil
}

// Fix 1: Implement URI Resolution
// resolveURIToID attempts to resolve a cog:// URI to a document ID.
//
// Supported URI patterns:
//   - cog://mem/semantic/path/to/doc → .cog/mem/semantic/path/to/doc.cog.md
//   - cog://adr/004 → .cog/adr/004-*.cog.md (glob pattern)
//   - cog://kernel/path → .cog/kernel/path.cog.md
//   - cog://type/identifier → ID lookup
func resolveURIToID(tx *sql.Tx, uri string) sql.NullString {
	if !strings.HasPrefix(uri, "cog://") {
		return sql.NullString{Valid: false}
	}

	// Strip cog:// prefix
	path := strings.TrimPrefix(uri, "cog://")

	// Strip incorrect .cog.md suffix if present in URI
	path = strings.TrimSuffix(path, ".cog.md")

	parts := strings.Split(path, "/")

	if len(parts) < 2 {
		return sql.NullString{Valid: false}
	}

	uriType := parts[0]

	switch uriType {
	case "mem":
		// cog://mem/semantic/insights/foo
		// → .cog/mem/semantic/insights/foo.cog.md
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	case "adr":
		// cog://adr/004 → .cog/adr/004-*.cog.md (glob pattern)
		// cog://adr/004-cogdoc-format → .cog/adr/004-cogdoc-format.cog.md (exact)
		if len(parts) < 2 {
			return sql.NullString{Valid: false}
		}
		adrID := parts[1]
		// If adrID contains hyphen, it's the full filename (exact match)
		// Otherwise it's just the number (use glob pattern)
		if strings.Contains(adrID, "-") {
			return resolveByPath(tx, filepath.Join(".cog/adr", adrID)+".cog.md")
		}
		return resolveByPattern(tx, ".cog/adr", adrID+"-*")

	case "kernel":
		// cog://kernel/path
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	case "term":
		// cog://term/thermal-time-world → .cog/ontology/vocabulary.cog.md (all terms in one file)
		// For now, try to resolve by ID (term name)
		if len(parts) < 2 {
			return sql.NullString{Valid: false}
		}
		termName := parts[1]
		return resolveByID(tx, termName)

	case "work":
		// cog://work/councils/xyz/synthesis → .cog/work/councils/xyz/synthesis.cog.md
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	default:
		// Generic: try ID lookup first, then path
		identifier := parts[len(parts)-1]
		return resolveByID(tx, identifier)
	}
}

// resolveByPath resolves a URI by path suffix match.
// Matches paths ending with the given suffix (e.g., ".cog/mem/path.cog.md")
// Handles date-prefixed filenames (e.g., 2026-01-14-name.cog.md)
func resolveByPath(tx *sql.Tx, path string) sql.NullString {
	var id string
	// First try exact suffix match
	likePattern := "%" + path
	err := tx.QueryRow("SELECT id FROM documents WHERE path LIKE ? LIMIT 1", likePattern).Scan(&id)
	if err == nil {
		return sql.NullString{String: id, Valid: true}
	}

	// If that fails, try with date prefix wildcard for episodic documents
	// Example: .cog/mem/episodic/handoffs/foo.cog.md
	//       → %.cog/mem/episodic/handoffs/%-foo.cog.md
	if strings.Contains(path, "/episodic/") {
		// Split path to insert wildcard before filename
		dir := filepath.Dir(path)
		filename := filepath.Base(path)
		dateWildcardPattern := "%" + dir + "/%-" + filename

		err = tx.QueryRow("SELECT id FROM documents WHERE path LIKE ? LIMIT 1", dateWildcardPattern).Scan(&id)
		if err == nil {
			return sql.NullString{String: id, Valid: true}
		}
	}

	return sql.NullString{Valid: false}
}

// resolveByPattern resolves a URI using LIKE pattern matching.
// Used for ADRs where the full filename isn't known (e.g., 004-*.cog.md).
func resolveByPattern(tx *sql.Tx, dir, pattern string) sql.NullString {
	// Convert glob pattern to SQL LIKE pattern with % prefix for absolute paths
	// Pattern: 004-* → %/.cog/adr/004-%.cog.md
	likePattern := "%" + filepath.Join(dir, pattern) + ".cog.md"
	likePattern = strings.ReplaceAll(likePattern, "*", "%")

	var id string
	err := tx.QueryRow(
		"SELECT id FROM documents WHERE path LIKE ? LIMIT 1",
		likePattern,
	).Scan(&id)
	if err == nil {
		return sql.NullString{String: id, Valid: true}
	}
	return sql.NullString{Valid: false}
}

// resolveByID resolves a URI by document ID.
func resolveByID(tx *sql.Tx, identifier string) sql.NullString {
	var id string
	err := tx.QueryRow("SELECT id FROM documents WHERE id = ?", identifier).Scan(&id)
	if err == nil {
		return sql.NullString{String: id, Valid: true}
	}
	return sql.NullString{Valid: false}
}

// resolveUnresolvedRefs performs a second pass to resolve references that
// failed during initial indexing (due to target documents not being indexed yet).
func (c *Constellation) resolveUnresolvedRefs() (int, error) {
	// Query all unresolved references
	rows, err := c.db.Query(`
		SELECT source_id, target_uri
		FROM doc_references
		WHERE target_id IS NULL AND target_uri LIKE 'cog://%'
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	// Collect unresolved refs
	type UnresolvedRef struct {
		SourceID  string
		TargetURI string
	}
	var unresolvedRefs []UnresolvedRef
	for rows.Next() {
		var ref UnresolvedRef
		if err := rows.Scan(&ref.SourceID, &ref.TargetURI); err != nil {
			return 0, err
		}
		unresolvedRefs = append(unresolvedRefs, ref)
	}

	if len(unresolvedRefs) == 0 {
		return 0, nil
	}

	// Start transaction for updates
	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "Warning: transaction rollback failed: %v\n", err)
		}
	}()

	resolved := 0
	for _, ref := range unresolvedRefs {
		// Try to resolve now that all documents are indexed
		targetID := resolveURIToID(tx, ref.TargetURI)

		if targetID.Valid {
			// Update the reference with resolved target_id
			_, err := tx.Exec(`
				UPDATE doc_references
				SET target_id = ?
				WHERE source_id = ? AND target_uri = ?
			`, targetID.String, ref.SourceID, ref.TargetURI)

			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update reference: %v\n", err)
				continue
			}

			// Manually create backlink (trigger only fires on INSERT, not UPDATE)
			_, err = tx.Exec(`
				INSERT OR IGNORE INTO backlinks(target_id, source_id, relation)
				VALUES (?, ?, ?)
			`, targetID.String, ref.SourceID, "refs")

			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to create backlink: %v\n", err)
			}

			resolved++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return resolved, nil
}

// rebuildFTS manually populates the FTS index with current documents and aggregated tags.
// This is called after indexing completes to sync tags into the FTS table.
// Nuclear fix: We don't use triggers anymore - manual population after all docs + tags indexed.
func (c *Constellation) rebuildFTS() error {
	// Clear existing FTS data
	if _, err := c.db.Exec("DELETE FROM documents_fts"); err != nil {
		return fmt.Errorf("failed to clear FTS: %w", err)
	}

	// Manually populate FTS with all documents and their aggregated tags
	_, err := c.db.Exec(`
		INSERT INTO documents_fts(id, title, content, tags, sector, type)
		SELECT
			d.id,
			d.title,
			d.content,
			COALESCE((SELECT group_concat(tag, ' ') FROM tags WHERE document_id = d.id), ''),
			d.sector,
			d.type
		FROM documents d
	`)

	if err != nil {
		return fmt.Errorf("failed to populate FTS: %w", err)
	}

	return nil
}
