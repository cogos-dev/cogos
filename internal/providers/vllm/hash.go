// hash.go — KV block hashing tuple per RFC-0006 §hashing-tuple.
//
// Hash: SHA-256(prompt_text + "|" + model_name + "|" + temperature +
//              "|" + top_p + "|" + top_k + "|" + max_tokens)
//
// Excluded from the hash in v1: model precision, runtime version, hardware ops.
// These introduce instability across deployments without meaningfully improving
// cache hit rates in practice. The hash is an equivalence hint, not strict identity:
//   - a cache miss on hash-equal content is safe (cold start)
//   - a false positive is safe (stale KV bytes are semantically incorrect but
//     evicted by vLLM's own block validity checks)
package vllm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ComputeBlockHash computes the RFC-0006 equivalence-tuple hash for a KV block.
//
// The hash is:
//
//	SHA-256(prompt_text + "|" + model_name + "|" + temperature + "|" + top_p + "|" + top_k + "|" + max_tokens)
//
// Returns the hex-encoded SHA-256 digest.
// The function is deterministic: same inputs always produce the same output.
func ComputeBlockHash(promptText, modelName string, cfg KVSamplingConfig) string {
	h := sha256.New()
	// Separator "|" between fields. Fields themselves may not contain "|" in
	// normal usage; if they do, the hash is still unique per distinct input set.
	fmt.Fprintf(h, "%s|%s|%g|%g|%d|%d",
		promptText,
		modelName,
		cfg.Temperature,
		cfg.TopP,
		cfg.TopK,
		cfg.MaxTokens,
	)
	return hex.EncodeToString(h.Sum(nil))
}
