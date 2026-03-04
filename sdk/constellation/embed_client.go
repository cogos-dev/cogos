package constellation

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"
)

// EmbedClient calls the CogOS embedding server to generate vectors.
type EmbedClient struct {
	httpURL    string        // e.g. "http://localhost:11434/embed"
	socketPath string        // e.g. "/tmp/cogos-embed.sock" — preferred if set
	timeout    time.Duration // per-request timeout
	client     *http.Client
}

// EmbedConfig holds embedding server connection parameters.
type EmbedConfig struct {
	Enabled       bool   `yaml:"enabled"`
	ServerSocket  string `yaml:"server_socket"`
	ServerHTTP    string `yaml:"server_http"`
	DimsFull      int    `yaml:"dims_full"`
	DimsCompressed int   `yaml:"dims_compressed"`
	TimeoutMs     int    `yaml:"timeout_ms"`
}

// DefaultEmbedConfig returns sensible defaults for the embedding server.
func DefaultEmbedConfig() EmbedConfig {
	return EmbedConfig{
		Enabled:        false,
		ServerSocket:   "/tmp/cogos-embed.sock",
		ServerHTTP:     "http://localhost:11434",
		DimsFull:       768,
		DimsCompressed: 128,
		TimeoutMs:      5000,
	}
}

// NewEmbedClient creates a client for the embedding server.
// Prefers Unix socket if socketPath is set and the socket exists.
func NewEmbedClient(cfg EmbedConfig) *EmbedClient {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	ec := &EmbedClient{
		httpURL:    cfg.ServerHTTP,
		socketPath: cfg.ServerSocket,
		timeout:    timeout,
	}

	// Build HTTP client — use Unix socket transport if available
	if cfg.ServerSocket != "" {
		ec.client = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", cfg.ServerSocket, timeout)
				},
			},
		}
	} else {
		ec.client = &http.Client{Timeout: timeout}
	}

	return ec
}

// embedRequest is the JSON body sent to the embed server.
type embedRequest struct {
	Texts  []string `json:"texts"`
	Prefix string   `json:"prefix"`
}

// embedResponseItem is one embedding result from the server.
type embedResponseItem struct {
	Embedding768 []float32 `json:"embedding_768"`
	Embedding128 []float32 `json:"embedding_128"`
}

// embedResponse is the full server response.
type embedResponse struct {
	Embeddings []embedResponseItem `json:"embeddings"`
	Model      string              `json:"model"`
	ElapsedMs  float64             `json:"elapsed_ms"`
}

// EmbedResult holds the computed embeddings for a single text.
type EmbedResult struct {
	Embedding768 []float32
	Embedding128 []float32
}

// Embed sends texts to the embedding server and returns vectors.
// prefix should be "search_document" for indexing or "search_query" for queries.
func (ec *EmbedClient) Embed(texts []string, prefix string) ([]EmbedResult, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := embedRequest{
		Texts:  texts,
		Prefix: prefix,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	// Build URL — when using Unix socket, the host in URL is ignored
	url := ec.httpURL + "/embed"
	if ec.socketPath != "" {
		url = "http://localhost/embed"
	}

	ctx, cancel := context.WithTimeout(context.Background(), ec.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ec.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request failed (is embed server running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed server returned %d: %s", resp.StatusCode, string(body))
	}

	var embedResp embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}

	if len(embedResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed server returned %d results for %d texts",
			len(embedResp.Embeddings), len(texts))
	}

	results := make([]EmbedResult, len(embedResp.Embeddings))
	for i, e := range embedResp.Embeddings {
		results[i] = EmbedResult{
			Embedding768: e.Embedding768,
			Embedding128: e.Embedding128,
		}
	}

	return results, nil
}

// EmbedOne is a convenience wrapper for embedding a single text.
func (ec *EmbedClient) EmbedOne(text string, prefix string) (*EmbedResult, error) {
	results, err := ec.Embed([]string{text}, prefix)
	if err != nil {
		return nil, err
	}
	return &results[0], nil
}

// Healthy checks if the embedding server is responding.
func (ec *EmbedClient) Healthy() bool {
	url := ec.httpURL + "/health"
	if ec.socketPath != "" {
		url = "http://localhost/health"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}

	resp, err := ec.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == 200
}

// ---------------------------------------------------------------------------
// Vector serialization: []float32 ↔ BLOB (little-endian IEEE 754)
// ---------------------------------------------------------------------------

// Float32ToBytes serializes a float32 slice to a little-endian byte slice.
// Used for storing embeddings as BLOBs in SQLite.
func Float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// BytesToFloat32 deserializes a little-endian byte slice back to float32s.
func BytesToFloat32(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// CosineSimilarity computes cosine similarity between two float32 vectors.
// Vectors are assumed to be L2-normalized (so this is just a dot product).
// Returns 0.0 if either vector is nil or lengths don't match.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0.0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
