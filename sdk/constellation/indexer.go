package constellation

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ftsMu serialises all FTS mutations (rebuildFTS and upsertFTSRow).
// The underlying SQLite connection is single-writer, but multi-statement
// sequences (delete + insert) must not interleave with a concurrent full
// rebuild.
var ftsMu sync.Mutex

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
	Salience         string
	Confidence       string
	Ingested         string
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

	// CRITICAL-4: purge stale paths from a prior workspace root before re-indexing.
	// These accumulate when the workspace root changes (e.g., when the directory is
	// renamed or a symlink is replaced with a canonical path).  We delete any indexed
	// document whose absolute path does not begin with the current workspace root so
	// that the purge is portable and correct for any host or user.
	// Log but do not abort if purge fails — stale rows are cosmetic, not blocking.
	stalePrefix := c.root + string(os.PathSeparator)
	if result, err := tx.Exec("DELETE FROM documents WHERE path NOT LIKE ?", stalePrefix+"%"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: stale-path purge failed: %v\n", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		fmt.Printf("Purged %d stale out-of-workspace paths\n", n)
	}

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

// IndexFile indexes a single cogdoc file into the constellation.
// This is the public entry point for incremental indexing (e.g., after
// a decomposition stores a new CogDoc). It handles its own transaction,
// FTS rebuild for the affected document, and optional async embedding.
func (c *Constellation) IndexFile(path string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "Warning: IndexFile rollback: %v\n", err)
		}
	}()

	if err := c.indexCogdoc(tx, path); err != nil {
		return err
	}

	// Look up the doc id inside the transaction so we get the row even if it
	// was just inserted (not yet committed to the reader connection).
	var docID string
	idErr := tx.QueryRow("SELECT id FROM documents WHERE path = ?", path).Scan(&docID)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	if idErr != nil {
		// Row absent means indexCogdoc skipped (unchanged hash). FTS already
		// up-to-date for this document; nothing to do.
		return nil
	}

	// Targeted O(1) FTS upsert — avoids full table rebuild on every write.
	if err := c.upsertFTSRow(docID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: FTS upsert after IndexFile: %v\n", err)
	}

	// Async embedding if configured
	if c.embedClient != nil {
		go func() {
			indexer := NewEmbedIndexer(c, c.embedClient)
			if _, err := indexer.BackfillAll(1); err != nil {
				fmt.Fprintf(os.Stderr, "[embed-indexer] single-doc backfill error: %v\n", err)
			}
		}()
	}

	return nil
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

	// CRITICAL-6: check for ID collision before insert.
	// If another document at a different path already claims this ID, log it to
	// migration_conflicts for human review. We still proceed with INSERT OR REPLACE
	// (last-write wins) so indexing is not blocked, but the collision is recorded.
	var existingPath string
	if collErr := tx.QueryRow(
		"SELECT path FROM documents WHERE id = ? AND path != ?", doc.ID, path,
	).Scan(&existingPath); collErr == nil {
		// Collision detected: record in migration_conflicts
		_, _ = tx.Exec(
			"INSERT INTO migration_conflicts (candidate_path, existing_path, detected_at) VALUES (?, ?, ?)",
			path, existingPath, time.Now().Format(time.RFC3339),
		)
		fmt.Fprintf(os.Stderr, "Warning: ID collision for %q: %s conflicts with %s\n",
			doc.ID, path, existingPath)
	}

	// Insert or replace document (includes new canonical schema columns)
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO documents (
			id, path, type, title, created, updated, sector, status,
			content, content_hash, word_count, line_count,
			indexed_at, file_mtime,
			frontmatter_bytes, content_bytes, substance_ratio, ref_count, ref_density,
			ingested, salience, confidence
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, doc.ID, path, doc.Type, doc.Title, doc.Created, doc.Updated, doc.Sector, doc.Status,
		doc.Content, contentHash, wordCount, lineCount, time.Now().Format(time.RFC3339), mtime,
		frontmatterBytes, contentBytes, substanceRatio, refCount, refDensity,
		doc.Ingested, doc.Salience, doc.Confidence)

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
		ID           string        `yaml:"id"`
		Type         string        `yaml:"type"`
		Title        string        `yaml:"title"`
		Created      string        `yaml:"created"`
		Updated      string        `yaml:"updated"`
		Modified     string        `yaml:"modified"` // CRITICAL-1: parse-time alias for updated
		Revised      string        `yaml:"revised"`  // CRITICAL-1: parse-time alias for updated
		Sector       string        `yaml:"sector"`
		MemorySector string        `yaml:"memory_sector"` // CRITICAL-1: parse-time alias for sector
		Status       string        `yaml:"status"`
		Salience     string        `yaml:"salience"`
		Confidence   string        `yaml:"confidence"`
		Ingested     string        `yaml:"ingested"`
		Tags         []string      `yaml:"tags"`
		Refs         []interface{} `yaml:"refs"`
		Authors      []string      `yaml:"authors"`
		Author       string        `yaml:"author"` // CRITICAL-1: singular alias for authors
		// CRITICAL-2: nested cog submap for RFC/spec frontmatter (cog.type → type, cog.id → id)
		Cog struct {
			Type string `yaml:"type"`
			ID   string `yaml:"id"`
		} `yaml:"cog"`
	}

	// Fix 8: YAML Parsing Robustness
	// Keep strict parsing but provide better error context
	if err := yaml.Unmarshal([]byte(frontmatterYAML), &fm); err != nil {
		// Enhanced error message with file context
		return nil, fmt.Errorf("failed to parse frontmatter in %s: %w", filepath.Base(path), err)
	}

	// CRITICAL-1: resolve memory_sector alias → sector
	if fm.Sector == "" && fm.MemorySector != "" {
		fm.Sector = fm.MemorySector
	}

	// CRITICAL-1: resolve updated field aliases
	if fm.Updated == "" && fm.Modified != "" {
		fm.Updated = fm.Modified
	}
	if fm.Updated == "" && fm.Revised != "" {
		fm.Updated = fm.Revised
	}

	// CRITICAL-2: lift cog submap fields to top-level for nested RFC/spec frontmatter
	if fm.Type == "" && fm.Cog.Type != "" {
		fm.Type = fm.Cog.Type
	}
	if fm.ID == "" && fm.Cog.ID != "" {
		fm.ID = fm.Cog.ID
	}

	// CRITICAL-3: lowercase status at parse time (D-10: no author burden)
	fm.Status = strings.ToLower(strings.TrimSpace(fm.Status))

	// CRITICAL-5: type-aware status enum validation — emit warning, not error
	humanStatuses := map[string]bool{
		"draft": true, "active": true, "canonical": true,
		"superseded": true, "retired": true, "": true,
	}
	machineStatuses := map[string]bool{
		"raw": true, "enriched": true, "completed": true, "": true,
	}
	machineTypes := map[string]bool{
		"conversation": true, "link": true, "session": true, "working-memory": true,
	}
	if machineTypes[fm.Type] {
		if !machineStatuses[fm.Status] {
			fmt.Fprintf(os.Stderr, "Warning: %s: machine-type %q doc has non-machine status %q\n",
				filepath.Base(path), fm.Type, fm.Status)
		}
	} else if fm.Type != "" {
		if !humanStatuses[fm.Status] {
			fmt.Fprintf(os.Stderr, "Warning: %s: human-type %q doc has non-canonical status %q\n",
				filepath.Base(path), fm.Type, fm.Status)
		}
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
		Salience:         fm.Salience,
		Confidence:       fm.Confidence,
		Ingested:         fm.Ingested,
		Tags:             fm.Tags,
		Refs:             refs,
		Content:          content,
		FrontmatterBytes: len(frontmatterYAML),
	}, nil
}

// Fix 1: Implement URI Resolution
// resolveURIToID attempts to resolve a cog: URI to a document ID.
// Accepts both bare (cog:X/Y) and legacy authority form (cog://X/Y).
//
// Supported URI patterns:
//   - cog:mem/semantic/path/to/doc → .cog/mem/semantic/path/to/doc.cog.md
//   - cog:adr/004 → .cog/adr/004-*.cog.md (glob pattern)
//   - cog:kernel/path → .cog/kernel/path.cog.md
//   - cog:type/identifier → ID lookup
func resolveURIToID(tx *sql.Tx, uri string) sql.NullString {
	if !strings.HasPrefix(uri, "cog://") {
		return sql.NullString{Valid: false}
	}

	// PREREQ-2: normalize legacy cog://memory/ → cog://mem/ (D-14 canonical prefix)
	uri = strings.Replace(uri, "cog://memory/", "cog://mem/", 1)

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
		// cog:mem/semantic/insights/foo
		// → .cog/mem/semantic/insights/foo.cog.md
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	case "adr":
		// cog:adr/004 → .cog/adr/004-*.cog.md (glob pattern)
		// cog:adr/004-cogdoc-format → .cog/adr/004-cogdoc-format.cog.md (exact)
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
		// cog:kernel/path
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	case "term":
		// cog:term/thermal-time-world → .cog/ontology/vocabulary.cog.md (all terms in one file)
		// For now, try to resolve by ID (term name)
		if len(parts) < 2 {
			return sql.NullString{Valid: false}
		}
		termName := parts[1]
		return resolveByID(tx, termName)

	case "work":
		// cog:work/councils/xyz/synthesis → .cog/work/councils/xyz/synthesis.cog.md
		return resolveByPath(tx, filepath.Join(".cog", path)+".cog.md")

	case "rfc":
		// PREREQ-2: cog:rfc/NNN → .cog/conf/spec/rfc/RFC-NNN-*.cog.md (glob pattern)
		// cog:rfc/030 → .cog/conf/spec/rfc/RFC-030-*.cog.md
		// cog:rfc/30 → .cog/conf/spec/rfc/RFC-030-*.cog.md (zero-padded)
		if len(parts) < 2 {
			return sql.NullString{Valid: false}
		}
		rfcNum := parts[1]
		// Attempt numeric zero-pad to 3 digits; fall back to literal if not numeric
		var rfcPrefix string
		var n int
		if _, parseErr := fmt.Sscanf(rfcNum, "%d", &n); parseErr == nil {
			rfcPrefix = fmt.Sprintf("RFC-%03d", n)
		} else {
			rfcPrefix = "RFC-" + strings.ToUpper(rfcNum)
		}
		return resolveByPattern(tx, ".cog/conf/spec/rfc", rfcPrefix+"-*")

	case "conf":
		// cog:conf/spec/foo → .cog/conf/spec/foo.cog.md
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

// upsertFTSRow performs a targeted FTS upsert for a single document.
// It deletes the existing FTS row (if any) then inserts a fresh one that
// joins the latest tags from the tags table. The two-statement sequence is
// wrapped in a savepoint so a crash between delete and insert leaves the
// FTS index intact rather than missing the row. ftsMu is held for the
// duration so a concurrent rebuildFTS cannot interleave.
func (c *Constellation) upsertFTSRow(docID string) error {
	ftsMu.Lock()
	defer ftsMu.Unlock()

	// Use a savepoint for crash-consistency within the single-writer connection.
	if _, err := c.db.Exec("SAVEPOINT fts_upsert"); err != nil {
		return fmt.Errorf("fts_upsert savepoint: %w", err)
	}

	if _, err := c.db.Exec("DELETE FROM documents_fts WHERE id = ?", docID); err != nil {
		_, _ = c.db.Exec("ROLLBACK TO SAVEPOINT fts_upsert")
		_, _ = c.db.Exec("RELEASE SAVEPOINT fts_upsert")
		return fmt.Errorf("fts_upsert delete: %w", err)
	}

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
		WHERE d.id = ?
	`, docID)
	if err != nil {
		_, _ = c.db.Exec("ROLLBACK TO SAVEPOINT fts_upsert")
		_, _ = c.db.Exec("RELEASE SAVEPOINT fts_upsert")
		return fmt.Errorf("fts_upsert insert: %w", err)
	}

	if _, err := c.db.Exec("RELEASE SAVEPOINT fts_upsert"); err != nil {
		return fmt.Errorf("fts_upsert release: %w", err)
	}
	return nil
}

// rebuildFTS manually populates the FTS index with current documents and aggregated tags.
// This is called after indexing completes to sync tags into the FTS table.
// Nuclear fix: We don't use triggers anymore - manual population after all docs + tags indexed.
func (c *Constellation) rebuildFTS() error {
	ftsMu.Lock()
	defer ftsMu.Unlock()

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
