// cogfield_documents.go - Document content endpoint for CogField
//
// GET /api/cogfield/documents/{id} - Returns document content, metadata, refs, backlinks

package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

// DocRef represents a reference to/from another document
type DocRef struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Relation string `json:"relation"`
	Type     string `json:"type"`
	Sector   string `json:"sector"`
}

// DocumentDetail is the full response for a document content request
type DocumentDetail struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Sector    string   `json:"sector"`
	Path      string   `json:"path"`
	Created   string   `json:"created"`
	Modified  string   `json:"modified"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	Refs      []DocRef `json:"refs"`
	Backlinks []DocRef `json:"backlinks"`
}

// handleDocumentDetail handles GET /api/cogfield/documents/{id}
func (s *serveServer) handleDocumentDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract document ID from URL path
	// Path: /api/cogfield/documents/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/cogfield/documents/")
	if path == "" {
		http.Error(w, "Document ID required", http.StatusBadRequest)
		return
	}
	docID := path

	// Open constellation DB (per-request workspace or singleton fallback)
	var c *constellation.Constellation
	var err error
	if ws := workspaceFromRequest(r); ws != nil {
		c, err = getConstellationForWorkspace(ws.root)
	} else {
		c, err = getConstellation()
	}
	if err != nil {
		log.Printf("cogfield: failed to open constellation: %v", err)
		http.Error(w, "Failed to open constellation database", http.StatusInternalServerError)
		return
	}

	detail, err := buildDocumentDetail(c, docID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Document not found", http.StatusNotFound)
			return
		}
		log.Printf("cogfield: failed to build document detail: %v", err)
		http.Error(w, "Failed to load document", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(detail)
}

// buildDocumentDetail assembles the full document response from constellation DB
func buildDocumentDetail(c *constellation.Constellation, docID string) (*DocumentDetail, error) {
	db := c.DB()

	// Fetch document metadata + content
	var detail DocumentDetail
	var sector, content sql.NullString
	err := db.QueryRow(`
		SELECT id, path, title, type, COALESCE(sector, ''), content, created, updated
		FROM documents
		WHERE id = ?
	`, docID).Scan(
		&detail.ID, &detail.Path, &detail.Title, &detail.Type,
		&sector, &content, &detail.Created, &detail.Modified,
	)
	if err != nil {
		return nil, err
	}
	detail.Sector = sector.String
	detail.Content = content.String

	// Fetch tags
	tagRows, err := db.Query(`SELECT tag FROM tags WHERE doc_id = ? ORDER BY tag`, docID)
	if err != nil {
		log.Printf("cogfield: tags query failed for %s: %v", docID, err)
	} else {
		defer tagRows.Close()
		for tagRows.Next() {
			var tag string
			if err := tagRows.Scan(&tag); err == nil {
				detail.Tags = append(detail.Tags, tag)
			}
		}
	}
	if detail.Tags == nil {
		detail.Tags = []string{}
	}

	// Fetch outgoing refs (documents this doc references)
	refRows, err := db.Query(`
		SELECT d.id, d.title, r.relation, d.type, COALESCE(d.sector, '')
		FROM doc_references r
		JOIN documents d ON d.id = r.target_id
		WHERE r.source_id = ?
		ORDER BY d.title
	`, docID)
	if err != nil {
		log.Printf("cogfield: refs query failed for %s: %v", docID, err)
	} else {
		defer refRows.Close()
		for refRows.Next() {
			var ref DocRef
			var sector sql.NullString
			if err := refRows.Scan(&ref.ID, &ref.Title, &ref.Relation, &ref.Type, &sector); err == nil {
				ref.Sector = sector.String
				detail.Refs = append(detail.Refs, ref)
			}
		}
	}
	if detail.Refs == nil {
		detail.Refs = []DocRef{}
	}

	// Fetch backlinks (documents that reference this doc)
	blRows, err := db.Query(`
		SELECT d.id, d.title, COALESCE(r.relation, 'refs'), d.type, COALESCE(d.sector, '')
		FROM backlinks b
		JOIN documents d ON d.id = b.source_id
		LEFT JOIN doc_references r ON r.source_id = b.source_id AND r.target_id = b.target_id
		WHERE b.target_id = ?
		ORDER BY d.title
	`, docID)
	if err != nil {
		log.Printf("cogfield: backlinks query failed for %s: %v", docID, err)
	} else {
		defer blRows.Close()
		for blRows.Next() {
			var ref DocRef
			var sector sql.NullString
			if err := blRows.Scan(&ref.ID, &ref.Title, &ref.Relation, &ref.Type, &sector); err == nil {
				ref.Sector = sector.String
				detail.Backlinks = append(detail.Backlinks, ref)
			}
		}
	}
	if detail.Backlinks == nil {
		detail.Backlinks = []DocRef{}
	}

	return &detail, nil
}
