# vLLM Provider — RFC-0006

This package implements the CogOS vLLM inference provider with direct
PagedAttention block-API access.

## Hardware requirements

**Full integration requires Linux/CUDA and a running vLLM instance.** The
v0.5.0 deliverable is scaffolding with mock-based test coverage. Hardware
integration tests are tagged `requires-cuda` and tracked as a follow-up
milestone item on [issue #203](https://github.com/myrgic/cogos/issues/203).

The following are out of scope until a Linux/CUDA environment is available:

- cgo binding to `vllm.core.block_manager`
- Integration test against a real vLLM instance
- Cache-aware dispatch routing with real block hashes
- Full fork-over-kvcache end-to-end

## Access shape

### Primary: cgo binding

The primary path calls into vLLM's internal block manager API
(`vllm.core.block_manager`) to read block hashes, warm/evict blocks, and
receive cache-hit callbacks. All cgo callouts are wrapped in `vllmCall` (see
`provider_vllm.go`) which converts panics and recoverable SIGSEGVs to typed
Go errors.

```go
// All cgo calls:
err := vllmCall(func() error {
    return cgoBinding.SomeCall(...)
})
```

`debug.SetPanicOnFault(true)` is set inside `vllmCall` and reset on return so
hardware-fault SIGSEGVs from the C/Python layer are converted to recoverable
panics rather than crashing the kernel process.

### Fallback: Python sidecar

If cgo proves brittle in practice (GIL contention, version skew between vLLM
releases), the Python-sidecar fallback path provides equivalent semantics: a
lightweight Python process that owns the vLLM session and communicates block
events to the Go kernel via a JSON-lines protocol over a Unix domain socket.

Both the cgo binding and the Python sidecar implement the `VLLMClient`
interface (`provider_vllm_client.go`). Switching between them is a constructor
argument.

## Package structure

| File | Purpose |
|------|---------|
| `doc.go` | Package documentation |
| `provider_vllm.go` | `Provider` (engine.Provider) + `vllmCall` cgo wrapper |
| `provider_vllm_client.go` | `VLLMClient` interface + request/response types |
| `provider_vllm_mock.go` | `MockVLLMClient` for tests |
| `provider_vllm_cache.go` | `KVCacheProvider` (reconcile.Reconcilable, 7-method) |
| `provider_vllm_events.go` | Bus event types, topic constants, `BusEmitter` interface |
| `provider_vllm_fork.go` | Fork-over-kvcache stub (RFC-0005 integration) |
| `kvblock_hash_provider.go` | `KVBlockHashProvider` interface (cross-RFC) |
| `hash.go` | Hashing tuple per RFC-0006 §hashing-tuple |
| `provider_vllm_test.go` | Unit tests (mock-based) |

## KV block hashing

The equivalence tuple hash used for cache hit detection:

```
SHA-256(prompt_text + "|" + model_name + "|" + temperature + "|" + top_p + "|" + top_k + "|" + max_tokens)
```

Excluded from v1 hash: model precision, runtime version, hardware ops. These
introduce instability across deployments without meaningfully improving cache
hit rates. A false positive is safe (stale KV bytes are evicted by vLLM's own
validity checks); a false negative is safe (cold start).

## Bus events

The provider emits typed bus events on cache lifecycle transitions:

| Topic | Fires when |
|-------|-----------|
| `inference.cache.warmed` | Blocks successfully warmed |
| `inference.cache.evicted` | Blocks evicted from cache |
| `inference.cache.hit_rate_changed` | Hit rate crosses ±5% threshold |
| `inference.cache.fragmentation_high` | Fragmentation > 0.8 |

All events carry structured log fields: `operation`, `channel_id`,
`block_count`, `hit_rate`, `ts` per the kernel log convention.

## RFC references

- RFC-0006: `docs/rfcs/0006-vllm-pagedattention-provider.md`
- RFC-0005: `docs/rfcs/0005-cog-fork-session.md` (fork handler consumer)
- ADR-090: `docs/adr/090-kind-dispatch-via-registry.md` (Kind registry)
- ADR-089: pointer-envelope semantics for evicted blocks
