// Package constellation implements a knowledge graph over cogdocs.
//
// This package indexes all *.cog.md files in the workspace and provides
// fast FTS5-powered search for cog-chat Tier 4 context retrieval.
package constellation

// Schema defines the SQLite database schema for the constellation graph.
// Based on the Database Architect's design from constellation-design council.
const Schema = `
-- Core documents table
CREATE TABLE IF NOT EXISTS documents (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    created TEXT NOT NULL,
    updated TEXT,
    sector TEXT,
    status TEXT,
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    word_count INTEGER,
    line_count INTEGER,
    indexed_at TEXT NOT NULL,
    file_mtime TEXT NOT NULL,
    -- Substance metrics (added for abstraction analysis)
    frontmatter_bytes INTEGER DEFAULT 0,
    content_bytes INTEGER DEFAULT 0,
    substance_ratio REAL DEFAULT 0.0,
    ref_count INTEGER DEFAULT 0,
    ref_density REAL DEFAULT 0.0
);

CREATE INDEX IF NOT EXISTS idx_documents_type ON documents(type);
CREATE INDEX IF NOT EXISTS idx_documents_sector ON documents(sector);
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(status);
CREATE INDEX IF NOT EXISTS idx_documents_updated ON documents(updated);
CREATE INDEX IF NOT EXISTS idx_documents_path_prefix ON documents(path);

-- Tags (many-to-many)
CREATE TABLE IF NOT EXISTS tags (
    document_id TEXT NOT NULL,
    tag TEXT NOT NULL,
    PRIMARY KEY (document_id, tag),
    FOREIGN KEY (document_id) REFERENCES documents(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag);

-- Document references (links between documents)
CREATE TABLE IF NOT EXISTS doc_references (
    source_id TEXT NOT NULL,
    target_uri TEXT NOT NULL,
    target_id TEXT,
    relation TEXT,
    PRIMARY KEY (source_id, target_uri),
    FOREIGN KEY (source_id) REFERENCES documents(id) ON DELETE CASCADE,
    FOREIGN KEY (target_id) REFERENCES documents(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_doc_references_source ON doc_references(source_id);
CREATE INDEX IF NOT EXISTS idx_doc_references_target ON doc_references(target_id);
CREATE INDEX IF NOT EXISTS idx_doc_references_relation ON doc_references(relation);
-- Fix 6: Composite index for backlinks traversal query optimization
CREATE INDEX IF NOT EXISTS idx_doc_references_source_target ON doc_references(source_id, target_id);

-- Backlinks (auto-generated reverse references)
CREATE TABLE IF NOT EXISTS backlinks (
    target_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    relation TEXT,
    PRIMARY KEY (target_id, source_id),
    FOREIGN KEY (target_id) REFERENCES documents(id) ON DELETE CASCADE,
    FOREIGN KEY (source_id) REFERENCES documents(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_backlinks_target ON backlinks(target_id);

-- FTS5 virtual table for full-text search
-- Nuclear fix: Remove content='documents' to allow manual tag population
-- Tags are aggregated from tags table and inserted manually
CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
    id UNINDEXED,
    title,
    content,
    tags,
    sector,
    type,
    tokenize='porter unicode61'
);

-- No automatic triggers - FTS is manually populated after indexing
-- This is because tags are inserted AFTER documents, so triggers would
-- insert empty tags. Manual population happens in rebuildFTS().

-- Fix 2: FTS Tag Synchronization
-- Note: Tags are inserted AFTER documents, so the document INSERT trigger
-- will have empty tags. We need to rebuild FTS after all indexing is complete.
-- The rebuild happens in Go code after the transaction commits.

-- Trigger to sync backlinks from doc_references
CREATE TRIGGER IF NOT EXISTS doc_references_ai AFTER INSERT ON doc_references
WHEN new.target_id IS NOT NULL
BEGIN
    INSERT OR IGNORE INTO backlinks(target_id, source_id, relation)
    VALUES (new.target_id, new.source_id, new.relation);
END;

CREATE TRIGGER IF NOT EXISTS doc_references_ad AFTER DELETE ON doc_references BEGIN
    DELETE FROM backlinks
    WHERE target_id = old.target_id AND source_id = old.source_id;
END;
`
