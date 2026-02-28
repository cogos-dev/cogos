// block_index.go - Append-only block index for hash-based block lookup.
//
// After each CogBlock is appended to a bus, an index entry is written to
// .cog/.state/block_index.jsonl. This enables any agent with workspace
// access to resolve a block hash to its full context: bus, seq, type, from.

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// BlockIndexEntry is a lightweight index record for a CogBlock.
type BlockIndexEntry struct {
	Hash    string `json:"hash"`
	Type    string `json:"type"`
	From    string `json:"from"`
	Ts      string `json:"ts"`
	BusID   string `json:"bus_id"`
	Seq     int    `json:"seq"`
	Prev    string `json:"prev,omitempty"`     // first prev hash for quick chain traversal
	TraceID string `json:"trace_id,omitempty"` // OTEL trace ID for cross-event correlation
}

// blockIndex manages the append-only block index file.
type blockIndex struct {
	mu   sync.Mutex
	root string
}

func newBlockIndex(root string) *blockIndex {
	return &blockIndex{root: root}
}

// indexPath returns the path to the block index file.
func (bi *blockIndex) indexPath() string {
	return filepath.Join(bi.root, ".cog", ".state", "block_index.jsonl")
}

// Append writes a new index entry for a CogBlock.
// Non-blocking: logs errors but does not fail the bus pipeline.
func (bi *blockIndex) Append(block *CogBlock) {
	if block == nil || block.Hash == "" {
		return
	}

	var prev string
	if len(block.Prev) > 0 {
		prev = block.Prev[0]
	} else if block.PrevHash != "" {
		prev = block.PrevHash
	}

	// Extract trace_id from payload for cross-event correlation indexing
	var traceID string
	if block.Payload != nil {
		if tid, ok := block.Payload["trace_id"].(string); ok {
			traceID = tid
		}
	}

	entry := BlockIndexEntry{
		Hash:    block.Hash,
		Type:    block.Type,
		From:    block.From,
		Ts:      block.Ts,
		BusID:   block.BusID,
		Seq:     block.Seq,
		Prev:    prev,
		TraceID: traceID,
	}

	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[block-index] marshal error: %v", err)
		return
	}

	bi.mu.Lock()
	defer bi.mu.Unlock()

	// Ensure directory exists
	dir := filepath.Dir(bi.indexPath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[block-index] mkdir error: %v", err)
		return
	}

	f, err := os.OpenFile(bi.indexPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[block-index] open error: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(string(line) + "\n"); err != nil {
		log.Printf("[block-index] write error: %v", err)
	}
}
