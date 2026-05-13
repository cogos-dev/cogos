// provider_vllm_mock.go — MockVLLMClient for unit tests.
//
// MockVLLMClient implements VLLMClient with controllable behavior for testing
// KVCacheProvider, hash stability, bus event emission, and cgo error recovery.
package vllm

import (
	"context"
	"sync"
	"time"
)

// MockVLLMClient implements VLLMClient for unit tests.
// All methods are safe for concurrent use. Override fields to control behavior:
//
//	mock := &MockVLLMClient{
//	    WarmBlocksFn: func(...) ([]BlockResult, error) { ... },
//	}
type MockVLLMClient struct {
	mu sync.Mutex

	// WarmBlocksFn overrides WarmBlocks when non-nil.
	WarmBlocksFn func(ctx context.Context, prefixes []WarmRequest) ([]BlockResult, error)
	// EvictBlocksFn overrides EvictBlocks when non-nil.
	EvictBlocksFn func(ctx context.Context, blockHashes []string) error
	// ListWarmBlocksFn overrides ListWarmBlocks when non-nil.
	ListWarmBlocksFn func(ctx context.Context) ([]WarmBlock, error)
	// BlockHashForPromptFn overrides BlockHashForPrompt when non-nil.
	BlockHashForPromptFn func(prompt, model string, cfg KVSamplingConfig) string
	// CacheStatsFn overrides CacheStats when non-nil.
	CacheStatsFn func(ctx context.Context) (CacheStats, error)
	// CloseFn overrides Close when non-nil.
	CloseFn func() error

	// Recorded calls for assertion in tests.
	WarmCalls   [][]WarmRequest
	EvictCalls  [][]string
	CloseCalled int

	// WarmBlocks defaults: populated when WarmBlocksFn is nil.
	DefaultWarmResult []BlockResult

	// Default stats returned by CacheStats when CacheStatsFn is nil.
	DefaultStats CacheStats

	// WarmBlockList is returned by ListWarmBlocks when ListWarmBlocksFn is nil.
	WarmBlockList []WarmBlock
}

// WarmBlocks implements VLLMClient.
func (m *MockVLLMClient) WarmBlocks(ctx context.Context, prefixes []WarmRequest) ([]BlockResult, error) {
	m.mu.Lock()
	m.WarmCalls = append(m.WarmCalls, prefixes)
	fn := m.WarmBlocksFn
	def := m.DefaultWarmResult
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, prefixes)
	}
	if def != nil {
		return def, nil
	}
	// Default: return a warmed result for each request using the hash tuple.
	results := make([]BlockResult, len(prefixes))
	for i, p := range prefixes {
		results[i] = BlockResult{
			BlockHash: m.BlockHashForPrompt(p.PromptPrefix, p.ModelName, p.SamplingConfig),
			Warmed:    true,
		}
	}
	return results, nil
}

// EvictBlocks implements VLLMClient.
func (m *MockVLLMClient) EvictBlocks(ctx context.Context, blockHashes []string) error {
	m.mu.Lock()
	m.EvictCalls = append(m.EvictCalls, blockHashes)
	fn := m.EvictBlocksFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, blockHashes)
	}
	return nil
}

// ListWarmBlocks implements VLLMClient.
func (m *MockVLLMClient) ListWarmBlocks(ctx context.Context) ([]WarmBlock, error) {
	m.mu.Lock()
	fn := m.ListWarmBlocksFn
	list := m.WarmBlockList
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	return list, nil
}

// BlockHashForPrompt implements VLLMClient.
// When BlockHashForPromptFn is nil it delegates to ComputeBlockHash for a
// deterministic, stable hash using the RFC-0006 equivalence tuple.
func (m *MockVLLMClient) BlockHashForPrompt(prompt, model string, cfg KVSamplingConfig) string {
	m.mu.Lock()
	fn := m.BlockHashForPromptFn
	m.mu.Unlock()

	if fn != nil {
		return fn(prompt, model, cfg)
	}
	return ComputeBlockHash(prompt, model, cfg)
}

// CacheStats implements VLLMClient.
func (m *MockVLLMClient) CacheStats(ctx context.Context) (CacheStats, error) {
	m.mu.Lock()
	fn := m.CacheStatsFn
	def := m.DefaultStats
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	return def, nil
}

// Close implements VLLMClient.
func (m *MockVLLMClient) Close() error {
	m.mu.Lock()
	m.CloseCalled++
	fn := m.CloseFn
	m.mu.Unlock()

	if fn != nil {
		return fn()
	}
	return nil
}

// newTestWarmBlock is a test helper for constructing a WarmBlock with now-time.
func newTestWarmBlock(hash, model string, cfg KVSamplingConfig, size int) WarmBlock {
	return WarmBlock{
		BlockHash:      hash,
		ModelName:      model,
		SamplingConfig: cfg,
		WarmAt:         time.Now().UTC(),
		BlockSize:      size,
	}
}
