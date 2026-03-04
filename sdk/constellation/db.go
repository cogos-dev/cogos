package constellation

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// Constellation provides access to the cogdoc knowledge graph.
type Constellation struct {
	db          *sql.DB
	root        string
	embedClient *EmbedClient // optional — set via SetEmbedClient for async embedding on write
}

// Open opens the constellation database at the workspace root.
// Creates the database and schema if it doesn't exist.
func Open(workspaceRoot string) (*Constellation, error) {
	stateDir := filepath.Join(workspaceRoot, ".cog/.state")

	// Ensure .cog/.state directory exists
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	dbPath := filepath.Join(stateDir, "constellation.db")

	// Open database with WAL mode for concurrent access
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Fix 5: SQLite Connection Pooling
	// Configure connection pool for SQLite + WAL mode
	db.SetMaxOpenConns(1)         // SQLite works best with single writer
	db.SetMaxIdleConns(1)         // Keep one connection alive
	db.SetConnMaxLifetime(0)      // Connections never expire

	// Fix 7: Enable Foreign Keys
	// SQLite requires foreign key enforcement to be enabled per connection
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	c := &Constellation{
		db:   db,
		root: workspaceRoot,
	}

	// Apply schema
	if err := c.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return c, nil
}

// initSchema applies the constellation schema to the database.
func (c *Constellation) initSchema() error {
	// Apply schema (CREATE IF NOT EXISTS will skip existing tables)
	if _, err := c.db.Exec(Schema); err != nil {
		return err
	}

	// Run migrations for existing databases
	return c.runMigrations()
}

// runMigrations applies schema migrations for existing databases.
func (c *Constellation) runMigrations() error {
	// Migration: Add substance metrics columns if they don't exist
	// SQLite doesn't support IF NOT EXISTS for ALTER TABLE, so we check first
	substanceColumns := []struct {
		name string
		def  string
	}{
		{"frontmatter_bytes", "INTEGER DEFAULT 0"},
		{"content_bytes", "INTEGER DEFAULT 0"},
		{"substance_ratio", "REAL DEFAULT 0.0"},
		{"ref_count", "INTEGER DEFAULT 0"},
		{"ref_density", "REAL DEFAULT 0.0"},
		// Embedding columns (Phase A: context engine)
		{"embedding_768", "BLOB"},
		{"embedding_128", "BLOB"},
		{"embedding_hash", "TEXT"},
	}

	for _, col := range substanceColumns {
		// Check if column exists
		var count int
		err := c.db.QueryRow(`
			SELECT COUNT(*) FROM pragma_table_info('documents') WHERE name = ?
		`, col.name).Scan(&count)
		if err != nil {
			return fmt.Errorf("failed to check column %s: %w", col.name, err)
		}

		// Add column if it doesn't exist
		if count == 0 {
			_, err := c.db.Exec(fmt.Sprintf("ALTER TABLE documents ADD COLUMN %s %s", col.name, col.def))
			if err != nil {
				return fmt.Errorf("failed to add column %s: %w", col.name, err)
			}
		}
	}

	return nil
}

// SetEmbedClient sets the embedding client for async embedding on document writes.
// When set, newly indexed documents will be embedded asynchronously.
func (c *Constellation) SetEmbedClient(client *EmbedClient) {
	c.embedClient = client
}

// DB returns the underlying sql.DB for direct queries.
func (c *Constellation) DB() *sql.DB {
	return c.db
}

// Close closes the database connection.
func (c *Constellation) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// Health returns database health information.
func (c *Constellation) Health() (map[string]interface{}, error) {
	var docCount, tagCount, refCount int

	if err := c.db.QueryRow("SELECT COUNT(*) FROM documents").Scan(&docCount); err != nil {
		return nil, err
	}
	if err := c.db.QueryRow("SELECT COUNT(*) FROM tags").Scan(&tagCount); err != nil {
		return nil, err
	}
	if err := c.db.QueryRow("SELECT COUNT(*) FROM doc_references").Scan(&refCount); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"documents":      docCount,
		"tags":           tagCount,
		"doc_references": refCount,
	}, nil
}
