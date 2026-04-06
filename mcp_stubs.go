//go:build mcpserver

// mcp_stubs.go — Internal API stubs for MCP tools
//
// These functions bridge MCP tool calls to the v3 kernel internals.
// Some delegate to existing functionality, others are stubs awaiting
// full implementation. Each stub documents what it should eventually do.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SearchMemory searches the CogDoc corpus using the constellation FTS5 index.
// Falls back to naive filepath.Walk grep if the constellation DB is unavailable.
func SearchMemory(workspaceRoot, query string, limit int, sector string) (any, error) {
	dbPath := filepath.Join(workspaceRoot, ".cog", ".state", "constellation.db")

	if _, err := os.Stat(dbPath); err == nil {
		results, ftsErr := searchMemoryFTS(dbPath, workspaceRoot, query, limit, sector)
		if ftsErr == nil {
			return results, nil
		}
		// FTS failed (e.g. corrupt DB, schema mismatch) — fall through to grep
	}

	return searchMemoryGrep(workspaceRoot, query, limit, sector)
}

// searchMemoryFTS queries the constellation SQLite FTS5 index for matching documents.
func searchMemoryFTS(dbPath, workspaceRoot, query string, limit int, sector string) (map[string]any, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=3000")
	if err != nil {
		return nil, fmt.Errorf("open constellation db: %w", err)
	}
	defer db.Close()

	// Build the FTS5 query. Convert bare terms into an OR query so each
	// word is matched independently (matching the constellation SDK behaviour).
	ftsQuery := buildFTSQuery(query)

	// Build SQL with optional sector filter.
	var (
		sqlStr string
		args   []any
	)
	if sector != "" {
		sqlStr = `
			SELECT d.id, d.path, d.title, d.type, d.sector, d.status,
			       bm25(documents_fts) AS rank
			FROM documents_fts
			JOIN documents d ON d.id = documents_fts.id
			WHERE documents_fts MATCH ?
			  AND d.status != 'deprecated'
			  AND d.sector = ?
			ORDER BY rank
			LIMIT ?
		`
		args = []any{ftsQuery, sector, limit}
	} else {
		sqlStr = `
			SELECT d.id, d.path, d.title, d.type, d.sector, d.status,
			       bm25(documents_fts) AS rank
			FROM documents_fts
			JOIN documents d ON d.id = documents_fts.id
			WHERE documents_fts MATCH ?
			  AND d.status != 'deprecated'
			ORDER BY rank
			LIMIT ?
		`
		args = []any{ftsQuery, limit}
	}

	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, path, title, docType string
		var docSector, status sql.NullString
		var rank float64

		if err := rows.Scan(&id, &path, &title, &docType, &docSector, &status, &rank); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		// Derive a cog:// URI from the filesystem path.
		uri := pathToMemURI(workspaceRoot, path)

		// Normalise BM25 rank to a 0–1 relevance score.
		// SQLite bm25() returns negative values where closer to 0 = better match.
		score := math.Abs(rank)
		if score > 0 {
			score = 1.0 / (1.0 + score)
		} else {
			score = 1.0
		}

		results = append(results, map[string]any{
			"uri":   uri,
			"path":  path,
			"title": title,
			"score": math.Round(score*1000) / 1000, // 3 decimal places
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	}, nil
}

// buildFTSQuery converts a plain search string into an FTS5 query.
// Multi-word queries become OR-joined terms so each word matches
// independently, matching the constellation SDK behaviour.
// Single words are passed through as-is.
func buildFTSQuery(raw string) string {
	words := strings.Fields(strings.TrimSpace(raw))
	if len(words) <= 1 {
		return raw
	}
	// Quote each word to avoid FTS5 syntax issues with special characters.
	parts := make([]string, len(words))
	for i, w := range words {
		// Strip characters that could break FTS5 quoting.
		w = strings.ReplaceAll(w, `"`, "")
		parts[i] = `"` + w + `"`
	}
	return strings.Join(parts, " OR ")
}

// pathToMemURI converts an absolute filesystem path to a cog://mem/ URI.
// Non-memory paths are returned as cog://workspace/ URIs.
func pathToMemURI(workspaceRoot, path string) string {
	rel, err := filepath.Rel(workspaceRoot, path)
	if err != nil {
		return "cog://workspace/" + filepath.Base(path)
	}
	prefixes := [][2]string{
		{".cog/mem/", "cog://mem/"},
		{".cog/docs/", "cog://docs/"},
		{".cog/adr/", "cog://adr/"},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(rel, p[0]) {
			return p[1] + strings.TrimPrefix(rel, p[0])
		}
	}
	return "cog://workspace/" + rel
}

// searchMemoryGrep is the fallback search when the constellation DB is unavailable.
// It walks the memory directory and greps for the query in file contents.
func searchMemoryGrep(workspaceRoot, query string, limit int, sector string) (map[string]any, error) {
	memDir := filepath.Join(workspaceRoot, ".cog", "mem")
	if sector != "" {
		memDir = filepath.Join(memDir, sector)
	}

	var results []map[string]any
	lq := strings.ToLower(query)

	err := filepath.Walk(memDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		content := string(data)
		if !strings.Contains(strings.ToLower(content), lq) {
			return nil
		}

		rel, _ := filepath.Rel(filepath.Join(workspaceRoot, ".cog", "mem"), path)
		title := extractTitleFromFrontmatter(content)

		results = append(results, map[string]any{
			"uri":   "cog://mem/" + rel,
			"path":  path,
			"title": title,
			"score": 0.0, // no relevance scoring in grep fallback
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(results) > limit {
		results = results[:limit]
	}

	return map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	}, nil
}

// CheckCoherenceMCP runs workspace coherence validation for MCP tools.
func CheckCoherenceMCP(cfg *Config, nucleus *Nucleus) (any, error) {
	report := RunCoherence(cfg, nucleus)
	return map[string]any{
		"pass":      report.Pass,
		"results":   report.Results,
		"timestamp": report.Timestamp,
	}, nil
}

// EmitLedgerEvent appends an event to the workspace ledger.
func EmitLedgerEvent(cfg *Config, event map[string]any) error {
	event["timestamp"] = time.Now().UTC().Format(time.RFC3339)

	ledgerPath := filepath.Join(cfg.WorkspaceRoot, ".cog", "ledger", "events.jsonl")

	// Ensure ledger directory exists
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0755); err != nil {
		return fmt.Errorf("mkdir ledger: %w", err)
	}

	f, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()

	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// BuildMemoryIndex builds a lightweight index of all CogDocs.
// Prefers the constellation SQLite database for speed and richer metadata;
// falls back to naive filepath.Walk when the DB is unavailable or corrupt.
func BuildMemoryIndex(workspaceRoot, sector string) (any, error) {
	dbPath := filepath.Join(workspaceRoot, ".cog", ".state", "constellation.db")

	if _, err := os.Stat(dbPath); err == nil {
		result, dbErr := buildMemoryIndexFromDB(dbPath, workspaceRoot, sector)
		if dbErr == nil {
			return result, nil
		}
		// DB failed — fall through to filesystem walk
	}

	return buildMemoryIndexFromFS(workspaceRoot, sector)
}

// buildMemoryIndexFromDB queries the constellation SQLite database for document
// metadata, tags, refs, and attention-weighted salience scores.
func buildMemoryIndexFromDB(dbPath, workspaceRoot, sector string) (map[string]any, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=3000")
	if err != nil {
		return nil, fmt.Errorf("open constellation db: %w", err)
	}
	defer db.Close()

	// ── 1. Build salience map from recent attention signals ──────────────
	salience := make(map[string]float64)
	since := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	attnRows, err := db.Query(`
		SELECT document_id, COUNT(*) AS signal_count
		FROM attention
		WHERE document_id IS NOT NULL
		  AND occurred_at >= ?
		GROUP BY document_id
	`, since)
	if err == nil {
		defer attnRows.Close()
		var maxSignals float64
		type attnEntry struct {
			docID string
			count float64
		}
		var entries []attnEntry
		for attnRows.Next() {
			var docID string
			var count float64
			if err := attnRows.Scan(&docID, &count); err == nil {
				entries = append(entries, attnEntry{docID, count})
				if count > maxSignals {
					maxSignals = count
				}
			}
		}
		// Normalise to 0–1 range.
		for _, e := range entries {
			if maxSignals > 0 {
				salience[e.docID] = math.Round(e.count/maxSignals*1000) / 1000
			}
		}
	}

	// ── 2. Pre-load tags grouped by document ID ─────────────────────────
	tagMap := make(map[string][]string)
	tagRows, err := db.Query(`SELECT document_id, tag FROM tags ORDER BY document_id`)
	if err == nil {
		defer tagRows.Close()
		for tagRows.Next() {
			var docID, tag string
			if err := tagRows.Scan(&docID, &tag); err == nil {
				tagMap[docID] = append(tagMap[docID], tag)
			}
		}
	}

	// ── 3. Pre-load ref counts grouped by source document ───────────────
	refMap := make(map[string][]string)
	refRows, err := db.Query(`
		SELECT source_id, target_uri FROM doc_references ORDER BY source_id
	`)
	if err == nil {
		defer refRows.Close()
		for refRows.Next() {
			var sourceID, targetURI string
			if err := refRows.Scan(&sourceID, &targetURI); err == nil {
				refMap[sourceID] = append(refMap[sourceID], targetURI)
			}
		}
	}

	// ── 4. Query documents (with optional sector filter) ────────────────
	var (
		sqlStr string
		args   []any
	)
	if sector != "" {
		sqlStr = `
			SELECT id, path, title, type, COALESCE(sector, ''),
			       COALESCE(status, ''), content_bytes, file_mtime
			FROM documents
			WHERE sector = ?
			ORDER BY file_mtime DESC
		`
		args = []any{sector}
	} else {
		sqlStr = `
			SELECT id, path, title, type, COALESCE(sector, ''),
			       COALESCE(status, ''), content_bytes, file_mtime
			FROM documents
			ORDER BY file_mtime DESC
		`
	}

	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("documents query: %w", err)
	}
	defer rows.Close()

	var docs []map[string]any
	for rows.Next() {
		var id, path, title, docType, docSector, status, mtime string
		var contentBytes int64

		if err := rows.Scan(&id, &path, &title, &docType, &docSector, &status, &contentBytes, &mtime); err != nil {
			return nil, fmt.Errorf("scan document row: %w", err)
		}

		uri := pathToMemURI(workspaceRoot, path)

		doc := map[string]any{
			"uri":      uri,
			"title":    title,
			"size":     contentBytes,
			"mod":      mtime,
			"salience": salience[id], // 0.0 if no recent attention
		}

		if tags, ok := tagMap[id]; ok && len(tags) > 0 {
			doc["tags"] = tags
		}
		if refs, ok := refMap[id]; ok && len(refs) > 0 {
			doc["refs"] = refs
		}

		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document rows: %w", err)
	}

	return map[string]any{
		"count": len(docs),
		"docs":  docs,
	}, nil
}

// buildMemoryIndexFromFS is the fallback index builder that walks the filesystem.
func buildMemoryIndexFromFS(workspaceRoot, sector string) (map[string]any, error) {
	memDir := filepath.Join(workspaceRoot, ".cog", "mem")
	if sector != "" {
		memDir = filepath.Join(memDir, sector)
	}

	var docs []map[string]any
	err := filepath.Walk(memDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		uri := pathToMemURI(workspaceRoot, path)
		title := extractTitleFromFrontmatter(string(data))

		docs = append(docs, map[string]any{
			"uri":      uri,
			"title":    title,
			"size":     info.Size(),
			"mod":      info.ModTime().Format(time.RFC3339),
			"salience": 0.0,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"count": len(docs),
		"docs":  docs,
	}, nil
}

// extractTitleFromFrontmatter pulls the title from YAML frontmatter.
func extractTitleFromFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return ""
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return ""
	}
	fm := content[4 : 4+end]
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "title:") {
			title := strings.TrimPrefix(line, "title:")
			title = strings.TrimSpace(title)
			title = strings.Trim(title, `"'`)
			return title
		}
	}
	return ""
}
