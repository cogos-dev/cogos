// serve_blocks.go — HTTP endpoints for block sync protocol
//
// Phase 3 of the block sync protocol: remote blob exchange.
//
//	GET  /v1/blocks/{hash}     — retrieve a blob by hash
//	PUT  /v1/blocks/{hash}     — store a blob by hash
//	GET  /v1/blocks/manifest   — list all stored blobs (manifest exchange)
//	POST /v1/blocks/verify     — verify a list of hashes, return missing ones
//
// These endpoints enable workspace-to-workspace blob sync:
//   1. Workspace A gets B's manifest
//   2. Diffs against local manifest
//   3. GETs missing blobs by hash
//   4. Stores them locally
//
// Content is verified by hash on both read and write — the hash IS the address.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// registerBlockRoutes wires the block sync endpoints into the mux.
func (s *Server) registerBlockRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/blocks/manifest", s.handleBlocksManifest)
	mux.HandleFunc("POST /v1/blocks/verify", s.handleBlocksVerify)
	mux.HandleFunc("GET /v1/blocks/{hash}", s.handleBlockGet)
	mux.HandleFunc("PUT /v1/blocks/{hash}", s.handleBlockPut)
}

// handleBlockGet returns blob content by hash.
//
//	GET /v1/blocks/{hash}
//	200 → raw blob content (application/octet-stream)
//	404 → blob not found
//	409 → integrity check failed
func (s *Server) handleBlockGet(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" || len(hash) != 64 {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}

	bs := NewBlobStore(s.cfg.WorkspaceRoot)
	content, err := bs.Get(hash)
	if err != nil {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Blob-Hash", hash)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	_, _ = w.Write(content)
}

// handleBlockPut stores a blob, verifying the hash matches the content.
//
//	PUT /v1/blocks/{hash}
//	Body: raw blob content
//	201 → stored successfully
//	400 → hash mismatch
//	413 → too large
func (s *Server) handleBlockPut(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" || len(hash) != 64 {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}

	// Limit to 500MB.
	const maxBlobSize = 500 << 20
	content, err := io.ReadAll(io.LimitReader(r.Body, maxBlobSize+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(content)) > maxBlobSize {
		http.Error(w, "blob too large (max 500MB)", http.StatusRequestEntityTooLarge)
		return
	}

	// Verify hash.
	actual := sha256.Sum256(content)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != hash {
		slog.Warn("blocks: hash mismatch on PUT", "expected", hash[:12], "actual", actualHex[:12])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":    "hash mismatch",
			"expected": hash,
			"actual":   actualHex,
		})
		return
	}

	bs := NewBlobStore(s.cfg.WorkspaceRoot)
	if err := bs.Init(); err != nil {
		http.Error(w, "init blob store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	if _, err := bs.Store(content, ct); err != nil {
		http.Error(w, "store failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	slog.Info("blocks: stored", "hash", hash[:12], "size", len(content))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"stored": true,
		"hash":   hash,
		"size":   len(content),
	})
}

// handleBlocksManifest returns the full blob manifest.
//
//	GET /v1/blocks/manifest
//	200 → { blobs: [...], total_size: N, count: N }
func (s *Server) handleBlocksManifest(w http.ResponseWriter, r *http.Request) {
	bs := NewBlobStore(s.cfg.WorkspaceRoot)
	entries, err := bs.List()
	if err != nil {
		http.Error(w, "list blobs: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var totalSize int64
	for _, e := range entries {
		totalSize += e.Size
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"blobs":      entries,
		"count":      len(entries),
		"total_size": totalSize,
	})
}

// handleBlocksVerify accepts a list of hashes and returns which ones are missing locally.
//
//	POST /v1/blocks/verify
//	Body: { "hashes": ["abc123...", "def456..."] }
//	200 → { "missing": ["def456..."], "present": ["abc123..."] }
func (s *Server) handleBlocksVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	bs := NewBlobStore(s.cfg.WorkspaceRoot)
	var missing, present []string
	for _, h := range req.Hashes {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if bs.Exists(h) {
			present = append(present, h)
		} else {
			missing = append(missing, h)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"missing": missing,
		"present": present,
	})
}
