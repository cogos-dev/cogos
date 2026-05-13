// provider_vllm_fork.go — Fork-over-kvcache stub (RFC-0005 integration point).
//
// When cog_fork_session is called with kv_cache.inherit_parent_kv == true,
// the fork handler (RFC-0005) calls ParentKVBlockHash on the active vLLM
// provider to obtain the parent session's KV block hash. The child session's
// first completion request carries this hash as a routing hint.
//
// In v0.5.0 scaffolding, step 3 and 4 of the fork-over-kvcache flow are stubbed:
// the hash is carried through but channel scheduling logic emits a log line
// rather than actually routing to the same vLLM channel as the parent.
//
// TODO(rfc-0006): stub — full fork-over-kvcache routing requires Linux/CUDA.
// See: https://github.com/myrgic/cogos/issues/203
package vllm

import (
	"context"
	"log/slog"
)

// ForkKVCacheResult carries the fork-over-kvcache output for a single fork.
type ForkKVCacheResult struct {
	// ParentKVBlockHash is the content-addressed hash of the parent's KV state
	// at the fork point. Empty string means the parent block was unavailable
	// (evicted, runtime restarted) — the child starts cold.
	ParentKVBlockHash BlockHash

	// Degraded is true when the block hash was unavailable and the child
	// will start with a cold cache.
	Degraded bool

	// DegradedReason describes why the block was unavailable, if Degraded is true.
	DegradedReason string
}

// ForkOverKVCache implements the fork-over-kvcache flow for a child session.
// It queries the KVBlockHashProvider (the vLLM provider) for the parent's KV
// block hash and returns a ForkKVCacheResult.
//
// When the parent's block is unavailable (evicted, runtime restarted), it
// degrades gracefully: Degraded = true, ParentKVBlockHash = "".
//
// In v0.5.0 the channel scheduling step (routing the child's first request to
// the same vLLM channel as the parent for cache reuse) is stubbed with a log
// line. Full integration requires Linux/CUDA and RFC-0005 fork handler landing.
//
// TODO(rfc-0006): stub — full fork-over-kvcache routing requires Linux/CUDA.
// See: https://github.com/myrgic/cogos/issues/203
func ForkOverKVCache(ctx context.Context, provider KVBlockHashProvider, parentSessionID, atMessageID string) ForkKVCacheResult {
	hash, err := provider.ParentKVBlockHash(ctx, parentSessionID, atMessageID)
	if err != nil {
		reason := err.Error()
		slog.Info("vllm fork-over-kvcache: parent block unavailable, child starts cold",
			"operation", "fork_kvcache_degrade",
			"parent_session_id", parentSessionID,
			"at_message_id", atMessageID,
			"reason", reason,
		)
		return ForkKVCacheResult{
			ParentKVBlockHash: "",
			Degraded:          true,
			DegradedReason:    reason,
		}
	}

	if hash == "" {
		slog.Info("vllm fork-over-kvcache: empty hash returned, child starts cold",
			"operation", "fork_kvcache_degrade",
			"parent_session_id", parentSessionID,
			"at_message_id", atMessageID,
		)
		return ForkKVCacheResult{
			ParentKVBlockHash: "",
			Degraded:          true,
			DegradedReason:    "provider returned empty hash",
		}
	}

	// TODO(rfc-0006): stub — log the hash that would be used for routing.
	// Full implementation: schedule the child's first completion request on the
	// same vLLM channel as the parent, using hash as the routing hint to
	// maximize prefix cache reuse.
	slog.Info("vllm fork-over-kvcache: parent KV block hash obtained (routing stubbed)",
		"operation", "fork_kvcache_stub",
		"parent_session_id", parentSessionID,
		"at_message_id", atMessageID,
		"parent_kv_block_hash", string(hash),
		// TODO(rfc-0006/requires-cuda): use hash for cache-aware channel routing.
	)

	return ForkKVCacheResult{
		ParentKVBlockHash: hash,
		Degraded:          false,
	}
}
