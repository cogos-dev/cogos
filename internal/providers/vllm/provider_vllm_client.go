// provider_vllm_client.go — VLLMClient interface and supporting types.
//
// VLLMClient is the mockable interface both the cgo binding (primary) and the
// Python sidecar (fallback) implement. Unit tests use MockVLLMClient defined in
// provider_vllm_mock.go.
//
// See README-vllm.md for the cgo vs. sidecar path discussion.
package vllm

import (
	"context"
	"time"
)

// BlockHash is the content-addressed identifier for a single KV block.
// Format: hex-encoded SHA-256 of the equivalence tuple (see RFC-0006 §Hashing).
type BlockHash string

// VLLMClient is the interface the cgo binding (primary) and Python sidecar
// (fallback) both implement. Unit tests use MockVLLMClient.
type VLLMClient interface {
	// WarmBlocks pre-warms the KV cache for the given prompt prefixes.
	WarmBlocks(ctx context.Context, prefixes []WarmRequest) ([]BlockResult, error)

	// EvictBlocks removes specified block hashes from the cache.
	EvictBlocks(ctx context.Context, blockHashes []string) error

	// ListWarmBlocks returns all currently warm blocks on the channel.
	ListWarmBlocks(ctx context.Context) ([]WarmBlock, error)

	// BlockHashForPrompt computes the content-addressed hash for a prompt
	// using the equivalence tuple: prompt+model+sampling_config.
	// See RFC-0006 §hashing-tuple for the exact hash algorithm.
	BlockHashForPrompt(prompt, model string, cfg KVSamplingConfig) string

	// CacheStats returns current hit rate and fragmentation metrics.
	CacheStats(ctx context.Context) (CacheStats, error)

	// Close releases the underlying cgo handles or sidecar connection.
	Close() error
}

// ── Request/response types ───────────────────────────────────────────────────

// WarmRequest describes a single prompt prefix to pre-warm in the KV cache.
type WarmRequest struct {
	PromptPrefix   string          `json:"prompt_prefix"`
	ModelName      string          `json:"model_name"`
	SamplingConfig KVSamplingConfig `json:"sampling_config"`
}

// BlockResult describes the outcome of a single WarmBlocks request.
type BlockResult struct {
	BlockHash string `json:"block_hash"`
	Warmed    bool   `json:"warmed"`
	Error     string `json:"error,omitempty"`
}

// WarmBlock describes a currently warm block on the vLLM channel.
type WarmBlock struct {
	BlockHash      string          `json:"block_hash"`
	ModelName      string          `json:"model_name"`
	SamplingConfig KVSamplingConfig `json:"sampling_config"`
	WarmAt         time.Time       `json:"warm_at"`
	BlockSize      int             `json:"block_size"`
}

// CacheStats holds current hit rate and fragmentation metrics for the channel.
type CacheStats struct {
	HitRate       float64 `json:"hit_rate"`      // 0.0–1.0
	BlockCount    int     `json:"block_count"`
	Fragmentation float64 `json:"fragmentation"` // 0.0–1.0
}

// ── KV block types ───────────────────────────────────────────────────────────

// KVSamplingConfig captures sampling parameters included in the equivalence hash.
// Only these four fields participate in the hash; others (e.g. random seed) are
// excluded to avoid instability across equivalent requests.
type KVSamplingConfig struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	TopK        int     `json:"top_k"`
	MaxTokens   int     `json:"max_tokens"`
}

// KVBlockBody is the Payload body for a cache.kv_block CogBlock.
// Each KV block represents a single PagedAttention block in the vLLM
// block manager — a fixed-size chunk of key-value tensors addressable
// by content hash.
type KVBlockBody struct {
	// BlockHash is the content-addressed identifier for this KV block.
	// Computed from the prompt prefix that fills the block.
	// Format: hex-encoded SHA-256 of the equivalence tuple (see RFC-0006 §Hashing).
	BlockHash string `json:"block_hash"`

	// ChannelID identifies the vLLM channel this block lives on.
	ChannelID string `json:"channel_id"`

	// PromptPrefix is the prompt text whose KV computation this block holds.
	// May be truncated for large blocks; BlockHash is the canonical reference.
	PromptPrefix string `json:"prompt_prefix,omitempty"`

	// ModelName is the model identifier this block was computed for.
	ModelName string `json:"model_name"`

	// SamplingConfig captures the sampling parameters in the equivalence hash.
	SamplingConfig KVSamplingConfig `json:"sampling_config"`

	// BlockSize is the number of tokens this block covers.
	BlockSize int `json:"block_size"`

	// WarmAt is when this block was last warmed (cache hit or explicit warm).
	WarmAt time.Time `json:"warm_at"`

	// EvictedAt is set when the block is evicted from the vLLM block manager.
	// A non-zero EvictedAt means the envelope persists in the ledger but the
	// KV bytes are gone (ADR-089 pointer-envelope semantics).
	EvictedAt *time.Time `json:"evicted_at,omitempty"`
}
