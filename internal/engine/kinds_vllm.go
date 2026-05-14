// kinds_vllm.go — Kind handler registration for cache.kv_block (RFC-0006).
//
// Registers the cache.kv_block Kind handler via ADR-090's kind registry so that
// adding the new Kind requires no modification to Kind-handler switch statements.
//
// The handler is intentionally a logging-only stub in v0.5.0. Full handling
// (routing KV block events to the KVCacheProvider, updating the ledger envelope,
// applying ADR-089 pointer-envelope semantics for evictions) lands when
// Linux/CUDA hardware integration is available.
//
// TODO(rfc-0006/requires-cuda): wire real handling once vLLM cgo binding lands.
package engine

import (
	"log/slog"

	"github.com/myrgic/cogos/pkg/cogblock"
)

func init() {
	RegisterKindHandler(cogblock.KindCacheKVBlock, handleCacheKVBlock)
}

// handleCacheKVBlock is the Kind handler for cache.kv_block CogBlocks.
// In v0.5.0 scaffolding it logs receipt and returns nil (no-op routing).
// When Linux/CUDA hardware integration lands, this handler will:
//   - Decode KVBlockBody from the RawPayload
//   - Update the KVCacheProvider's block inventory
//   - Apply ADR-089 pointer-envelope semantics for evicted blocks
//   - Emit the appropriate bus event (warmed/evicted)
func handleCacheKVBlock(block *CogBlock) error {
	slog.Info("cache.kv_block received",
		"operation", "kv_block_dispatch",
		"block_id", block.ID,
		"channel_id", block.SourceChannel,
		"session_id", block.SessionID,
		"ts", block.Timestamp,
		// TODO(rfc-0006/requires-cuda): decode KVBlockBody and emit typed event.
	)
	return nil
}
