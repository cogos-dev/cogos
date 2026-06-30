// mcp_stubs.go — Internal API stubs for MCP tools
//
// These functions bridge MCP tool calls to the v3 kernel internals.
// Some delegate to existing functionality, others are stubs awaiting
// full implementation. Each stub documents what it should eventually do.
package engine

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// pkgFTSRepairIndexer is the package-level ConstellationIndexer handle used
// by the free-function lazy drift-repair path (searchMemoryFTSDriftRepair).
// Set by MCPServer.SetConstellationIndexer when the constellation handle is
// wired in from the root package at daemon boot.  Nil means drift repair is
// disabled (degraded mode; safe for tests and CLI paths).
//
// Package-boundary note: this var holds an engine-local interface value
// (ConstellationIndexer, declared in cogdoc_service.go) so internal/engine
// never imports sdk/constellation directly.
var pkgFTSRepairIndexer ConstellationIndexer

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

// searchMemoryFTSDriftRepair samples up to 100 indexed documents, compares their
// stored file_mtime to the on-disk mtime, and calls IndexFile for any drifted
// paths.  Total repair time is capped at ~200ms; if drift is widespread the
// function logs once and returns without repairing the bulk.  This is the
// "idempotent lazy re-index" path: correct regardless of write path.
//
// The constellation handle is accessed via pkgFTSRepairIndexer (a
// ConstellationIndexer set by MCPServer.SetConstellationIndexer at daemon
// boot).  If the handle is nil — e.g. in tests or CLI paths where the daemon
// wiring did not run — drift repair is silently skipped (safe degraded mode).
// This keeps internal/engine free of any import of sdk/constellation
// (package-boundary guard #2 in cogdoc_service.go:22).
func searchMemoryFTSDriftRepair(workspaceRoot string) {
	// No indexer wired — skip repair silently.
	indexer := pkgFTSRepairIndexer
	if indexer == nil {
		return
	}

	const (
		sampleLimit = 100
		budgetMs    = 200
		wideThresh  = 10 // more than this many drifted rows → skip inline repair
	)

	dbPath := filepath.Join(workspaceRoot, ".cog", ".state", "constellation.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=3000")
	if err != nil {
		return
	}
	defer db.Close()

	// Sample recent documents, fetching path + stored mtime.
	rows, err := db.Query(
		`SELECT path, file_mtime FROM documents
		 WHERE path NOT LIKE '%.state/%'
		 ORDER BY indexed_at DESC LIMIT ?`, sampleLimit,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type entry struct {
		path  string
		mtime string
	}
	var drifted []entry
	for rows.Next() {
		var path, storedMtime string
		if err := rows.Scan(&path, &storedMtime); err != nil {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue // file removed — not a drift case for FTS
		}
		diskMtime := info.ModTime().Format(time.RFC3339)
		if diskMtime != storedMtime {
			drifted = append(drifted, entry{path: path, mtime: diskMtime})
		}
	}
	_ = rows.Close()

	if len(drifted) == 0 {
		return
	}
	if len(drifted) > wideThresh {
		slog.Warn("constellation: widespread FTS drift detected; run `cogos reindex` to repair",
			"drifted_sample", len(drifted))
		return
	}

	// Repair drifted entries via the wired indexer handle.
	// Budget: ~200ms total across all repairs; abort remaining on timeout.
	deadline := time.Now().Add(budgetMs * time.Millisecond)
	for _, e := range drifted {
		if time.Now().After(deadline) {
			break
		}
		if err := indexer.IndexFile(e.path); err != nil {
			slog.Warn("constellation: drift repair failed", "path", e.path, "err", err)
		}
	}
}

// searchMemoryFTS queries the constellation SQLite FTS5 index for matching documents.
func searchMemoryFTS(dbPath, workspaceRoot, query string, limit int, sector string) (map[string]any, error) {
	// Lazy drift repair: sample recent rows and repair any whose on-disk mtime
	// differs from the stored mtime.  Capped at ~200ms and ≤10 rows so hot
	// search paths are not materially impacted.
	searchMemoryFTSDriftRepair(workspaceRoot)

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
			FROM documents_fts f
			JOIN documents d ON d.id = f.id
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
			FROM documents_fts f
			JOIN documents d ON d.id = f.id
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

		// Derive a cog: URI from the filesystem path (bare form per ADR-067).
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
// All terms are double-quoted to prevent FTS5 syntax errors from special
// characters like leading '-' or 'type:' prefixes.
// Multi-word queries become OR-joined quoted terms so each word matches
// independently, matching the constellation SDK behaviour.
func buildFTSQuery(raw string) string {
	words := strings.Fields(strings.TrimSpace(raw))
	if len(words) == 0 {
		return raw
	}
	// Single-word: pass through unquoted (token match, broader results).
	// Strip FTS5 special chars that would cause parse errors (leading dash,
	// column-filter colon, double-quotes).
	if len(words) == 1 {
		w := words[0]
		w = strings.ReplaceAll(w, `"`, "")
		w = strings.TrimLeft(w, "-")
		w = strings.ReplaceAll(w, ":", "")
		return w
	}
	// Multi-word: phrase-quote each term and join with OR.
	parts := make([]string, len(words))
	for i, w := range words {
		w = strings.ReplaceAll(w, `"`, "")
		parts[i] = `"` + w + `"`
	}
	return strings.Join(parts, " OR ")
}

// pathToMemURI converts an absolute filesystem path to a cog: URI.
// Projection references use the bare form (no //) per ADR-067.
// Non-memory paths are returned as cog://workspace/ URIs.
func pathToMemURI(workspaceRoot, path string) string {
	rel, err := filepath.Rel(workspaceRoot, path)
	if err != nil {
		return "cog://workspace/" + filepath.Base(path)
	}
	prefixes := [][2]string{
		{".cog/mem/", "cog:mem/"},
		{".cog/docs/", "cog:docs/"},
		{".cog/adr/", "cog:adr/"},
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
		// Caller-supplied sector becomes a path component; contain it so a value
		// like "../../etc" can't walk the grep fallback outside the memory root.
		contained, err := containedJoin(memDir, sector)
		if err != nil {
			return nil, fmt.Errorf("invalid sector %q: %w", sector, err)
		}
		memDir = contained
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
			"uri":   "cog:mem/" + rel,
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
//
// Historical shape: pre-PR this helper wrote to a flat
// .cog/ledger/events.jsonl outside the hash chain and outside any session
// subdirectory (cogos#10). Post-refactor it builds a proper EventEnvelope
// and routes through AppendEvent, which:
//
//   - chains the event by prior_hash / seq,
//   - files it under .cog/ledger/<session_id>/events.jsonl,
//   - fans out to live subscribers via the in-process EventBroker.
//
// Callers that have a live *Process should prefer (*Process).EmitEvent for
// accurate session attribution. This helper exists for the paths that only
// hold *Config (CogDocService.emitLedgerEvent, EmitIngestEvent, tests).
//
// Source (envelope.Metadata.Source): pulled from event["source"] if set,
// otherwise defaults to "mcp-client". Type: event["type"]. All other keys
// become envelope.data.
func EmitLedgerEvent(cfg *Config, event map[string]any) error {
	if cfg == nil {
		return fmt.Errorf("emit ledger: nil config")
	}
	eventType, _ := event["type"].(string)
	if eventType == "" {
		return fmt.Errorf("emit ledger: event missing 'type'")
	}
	source, _ := event["source"].(string)
	if source == "" {
		source = "mcp-client"
	}

	// Build data by stripping the control fields that landed in the
	// envelope header.
	data := make(map[string]any, len(event))
	for k, v := range event {
		switch k {
		case "type", "source", "timestamp":
			continue
		}
		data[k] = v
	}

	// Callers that hold only *Config don't have access to the live process
	// session id. Use a predictable bucket so events are still chained and
	// live in a per-session directory — no more orphan flat file. Paths
	// that want accurate session attribution should use
	// (*Process).EmitEvent directly.
	const sessionID = "mcp-client"

	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      eventType,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			SessionID: sessionID,
			Data:      data,
		},
		Metadata: EventMetadata{Source: source},
	}
	return AppendEvent(cfg.WorkspaceRoot, sessionID, env)
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
		// Contain the caller-supplied sector — see searchMemoryGrep.
		contained, err := containedJoin(memDir, sector)
		if err != nil {
			return nil, fmt.Errorf("invalid sector %q: %w", sector, err)
		}
		memDir = contained
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
