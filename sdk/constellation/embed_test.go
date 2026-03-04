package constellation

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestCosineSimilarity verifies the cosine similarity function.
func TestCosineSimilarity(t *testing.T) {
	// Identical normalized vectors → similarity = 1.0
	a := []float32{0.6, 0.8}
	sim := CosineSimilarity(a, a)
	if math.Abs(sim-1.0) > 1e-6 {
		t.Errorf("identical vectors: got %f, want 1.0", sim)
	}

	// Orthogonal vectors → similarity = 0.0
	b := []float32{0.8, -0.6}
	sim = CosineSimilarity(a, b)
	if math.Abs(sim) > 1e-6 {
		t.Errorf("orthogonal vectors: got %f, want 0.0", sim)
	}

	// Opposite vectors → similarity = -1.0
	c := []float32{-0.6, -0.8}
	sim = CosineSimilarity(a, c)
	if math.Abs(sim+1.0) > 1e-6 {
		t.Errorf("opposite vectors: got %f, want -1.0", sim)
	}

	// Nil/empty → 0.0
	if CosineSimilarity(nil, a) != 0.0 {
		t.Error("nil vector should return 0.0")
	}
	if CosineSimilarity(a, []float32{1.0}) != 0.0 {
		t.Error("mismatched lengths should return 0.0")
	}
}

// TestFloat32Serialization verifies round-trip BLOB serialization.
func TestFloat32Serialization(t *testing.T) {
	original := []float32{0.1, -0.5, 3.14159, 0.0, -1.0, 1e-7}

	encoded := Float32ToBytes(original)
	if len(encoded) != len(original)*4 {
		t.Fatalf("encoded length: got %d, want %d", len(encoded), len(original)*4)
	}

	decoded := BytesToFloat32(encoded)
	if len(decoded) != len(original) {
		t.Fatalf("decoded length: got %d, want %d", len(decoded), len(original))
	}

	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("index %d: got %f, want %f", i, decoded[i], original[i])
		}
	}

	// Invalid input
	if result := BytesToFloat32([]byte{1, 2, 3}); result != nil {
		t.Error("non-aligned bytes should return nil")
	}
}

// openTestDB opens a constellation DB, skipping if FTS5 is unavailable.
func openTestDB(t *testing.T) (*Constellation, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, ".cog", ".state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	c, err := Open(tmpDir)
	if err != nil {
		if err.Error() == "failed to initialize schema: no such module: fts5" {
			t.Skip("FTS5 not available (build with -tags fts5)")
		}
		t.Fatal(err)
	}
	return c, func() { c.Close() }
}

// TestEmbeddingColumnsExist verifies the migration adds embedding columns.
func TestEmbeddingColumnsExist(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	// Check that embedding columns exist
	columns := []string{"embedding_768", "embedding_128", "embedding_hash"}
	for _, col := range columns {
		var count int
		err := c.DB().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('documents') WHERE name = ?`, col,
		).Scan(&count)
		if err != nil {
			t.Fatalf("checking column %s: %v", col, err)
		}
		if count != 1 {
			t.Errorf("column %s not found in documents table", col)
		}
	}
}

// TestEmbeddingBlobStorage verifies storing and retrieving embedding BLOBs.
func TestEmbeddingBlobStorage(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	// Insert a test document with embeddings
	emb768 := make([]float32, 768)
	emb128 := make([]float32, 128)
	for i := range emb768 {
		emb768[i] = float32(i) / 768.0
	}
	for i := range emb128 {
		emb128[i] = float32(i) / 128.0
	}

	_, err := c.DB().Exec(`
		INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime,
			embedding_768, embedding_128, embedding_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"test-doc-1", "/test/doc1.cog.md", "insight", "Test Document",
		"2026-03-04", "Test content for embeddings", "hash123",
		"2026-03-04T00:00:00Z", "2026-03-04T00:00:00Z",
		Float32ToBytes(emb768), Float32ToBytes(emb128), "content-hash-1",
	)
	if err != nil {
		t.Fatal(err)
	}

	// Retrieve and verify
	var blob768, blob128 []byte
	var embHash sql.NullString
	err = c.DB().QueryRow(
		`SELECT embedding_768, embedding_128, embedding_hash FROM documents WHERE id = ?`,
		"test-doc-1",
	).Scan(&blob768, &blob128, &embHash)
	if err != nil {
		t.Fatal(err)
	}

	recovered768 := BytesToFloat32(blob768)
	recovered128 := BytesToFloat32(blob128)

	if len(recovered768) != 768 {
		t.Fatalf("recovered 768-dim: got %d dims", len(recovered768))
	}
	if len(recovered128) != 128 {
		t.Fatalf("recovered 128-dim: got %d dims", len(recovered128))
	}

	// Verify values round-tripped correctly
	for i := range emb768 {
		if recovered768[i] != emb768[i] {
			t.Fatalf("768-dim index %d: got %f, want %f", i, recovered768[i], emb768[i])
		}
	}

	if embHash.String != "content-hash-1" {
		t.Errorf("embedding hash: got %q, want %q", embHash.String, "content-hash-1")
	}
}

// TestEmbeddingSimilarityRanking verifies that cosine similarity correctly
// distinguishes similar from unrelated documents using stored BLOBs.
func TestEmbeddingSimilarityRanking(t *testing.T) {
	// Simulate: 3 documents with known embeddings, 1 query
	// Doc A and query are similar (high cosine), Doc B is unrelated (low cosine)

	// Create "similar" embeddings — same direction with slight noise
	queryEmb := make([]float32, 128)
	similarEmb := make([]float32, 128)
	unrelatedEmb := make([]float32, 128)

	for i := 0; i < 128; i++ {
		queryEmb[i] = float32(i) * 0.01
		similarEmb[i] = float32(i)*0.01 + 0.001 // very close
		unrelatedEmb[i] = float32(128-i) * 0.01  // orthogonal-ish
	}

	// L2 normalize
	normalize := func(v []float32) {
		var sum float64
		for _, f := range v {
			sum += float64(f) * float64(f)
		}
		norm := float32(math.Sqrt(sum))
		for i := range v {
			v[i] /= norm
		}
	}
	normalize(queryEmb)
	normalize(similarEmb)
	normalize(unrelatedEmb)

	simScore := CosineSimilarity(queryEmb, similarEmb)
	unrelScore := CosineSimilarity(queryEmb, unrelatedEmb)

	if simScore <= unrelScore {
		t.Errorf("similar doc score (%f) should be > unrelated doc score (%f)", simScore, unrelScore)
	}

	// Similar should be high (>0.9), unrelated should be lower
	if simScore < 0.9 {
		t.Errorf("similar doc score too low: %f", simScore)
	}

	// Verify BLOB round-trip preserves ranking
	queryRecovered := BytesToFloat32(Float32ToBytes(queryEmb))
	similarRecovered := BytesToFloat32(Float32ToBytes(similarEmb))
	unrelRecovered := BytesToFloat32(Float32ToBytes(unrelatedEmb))

	simRecovered := CosineSimilarity(queryRecovered, similarRecovered)
	unrelRecovered2 := CosineSimilarity(queryRecovered, unrelRecovered)

	if math.Abs(simScore-simRecovered) > 1e-6 {
		t.Errorf("BLOB round-trip changed similar score: %f → %f", simScore, simRecovered)
	}
	if math.Abs(unrelScore-unrelRecovered2) > 1e-6 {
		t.Errorf("BLOB round-trip changed unrelated score: %f → %f", unrelScore, unrelRecovered2)
	}
}

// TestMatryoshkaPreservesRanking verifies that truncating 768→128 preserves
// relative ranking order. This is a property of nomic-embed-text Matryoshka training.
func TestMatryoshkaPreservesRanking(t *testing.T) {
	// Synthetic test: create 768-dim vectors, check that truncation to 128
	// preserves the relative similarity ordering

	query768 := make([]float32, 768)
	similar768 := make([]float32, 768)
	unrelated768 := make([]float32, 768)

	// Fill: similar is close to query in first 128 dims AND beyond
	for i := 0; i < 768; i++ {
		query768[i] = float32(i%64) * 0.01
		similar768[i] = float32(i%64)*0.01 + 0.002
		unrelated768[i] = float32((768-i)%64) * 0.01
	}

	normalize := func(v []float32) {
		var sum float64
		for _, f := range v {
			sum += float64(f) * float64(f)
		}
		norm := float32(math.Sqrt(sum))
		for i := range v {
			v[i] /= norm
		}
	}

	normalize(query768)
	normalize(similar768)
	normalize(unrelated768)

	// Full 768-dim ranking
	sim768 := CosineSimilarity(query768, similar768)
	unrel768 := CosineSimilarity(query768, unrelated768)

	// Truncate to 128-dim (Matryoshka)
	query128 := make([]float32, 128)
	similar128 := make([]float32, 128)
	unrelated128 := make([]float32, 128)
	copy(query128, query768[:128])
	copy(similar128, similar768[:128])
	copy(unrelated128, unrelated768[:128])
	normalize(query128)
	normalize(similar128)
	normalize(unrelated128)

	sim128 := CosineSimilarity(query128, similar128)
	unrel128 := CosineSimilarity(query128, unrelated128)

	// Ranking order must be preserved
	if (sim768 > unrel768) != (sim128 > unrel128) {
		t.Errorf("Matryoshka truncation changed ranking order:\n  768-dim: similar=%f unrelated=%f\n  128-dim: similar=%f unrelated=%f",
			sim768, unrel768, sim128, unrel128)
	}
}
