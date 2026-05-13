# RFC-0006: vLLM PagedAttention Provider with Direct Block-API Access

| Field    | Value                                                               |
|----------|---------------------------------------------------------------------|
| Status   | Draft                                                               |
| Author   | @chazmaniandinkle                                                   |
| Tracking | [#203](https://github.com/myrgic/cogos/issues/203)                 |
| Target   | `v0.5.0`                                                            |
| Requires | RFC-0003 (CogBlock topology), RFC-0005 (cog_fork_session)           |

## Summary

This RFC specifies a `Provider` implementation for vLLM that exposes the
PagedAttention block-addressed KV cache as first-class kernel objects rather than
treating vLLM as an opaque OpenAI-shape endpoint. KV blocks become `cache.kv_block`
CogBlocks; the KV cache becomes a `Reconcilable` resource; block-level events flow
on the kernel bus. The result is that vLLM's prefix-cache layer is visible to and
schedulable by the kernel — enabling cache-aware dispatch routing and the
`KVCacheOverlay` integration point in `cog_fork_session` (RFC-0005).

## Background and Motivation

vLLM's value as a CogOS inference channel is not the chat completion endpoint — every
runtime exposes that. It is the block-addressed KV cache layer underneath. vLLM's
PagedAttention allocates KV blocks by content address; blocks shared across requests
are never duplicated. This is the same primitive as CogOS's content-addressed substrate,
expressed at the inference-runtime tier.

An OpenAI-shape adapter to vLLM throws away the block structure. Going direct exposes
the structure that makes "KV cache as substrate block" real and proves end-to-end that
the block-mesh architecture (block-mesh design seed) works for inference, not just
memory.

Prior architectural recognition (workspace memory `project_cogos_paged_attention_substrate_match`,
2026-04-24): PagedAttention's content-addressed KV blocks ARE the substrate's block
primitive at inference-runtime tier. This RFC turns that recognition into running code.

Other runtimes (mlx-engine, ollama, lms) degrade gracefully: each conversation is one
opaque "block" with no intra-block addressability. vLLM is the first runtime that earns
the full block-mesh machinery.

## Architectural calls

These are decisions, not options.

### Hardware targets

**Primary**: Linux/CUDA. vLLM's PagedAttention is CUDA-native; full hardware validation
requires a Linux/CUDA environment not available on the development machine (Apple Silicon).

**Secondary**: mlx-engine on Apple Silicon as laptop-dev substitute. The mlx-engine
provider degrades to opaque-block semantics for the KV layer; the Provider interface
and CogBlock types specified here are shared, but mlx-engine does not implement direct
block-API access. This is acceptable for local development; the full feature requires
Linux/CUDA.

All implementation and tests in v0.5.0 target the interface layer and mock-based
verification. Hardware integration tests are a follow-up milestone item tagged
`requires-cuda`.

### Block API access shape

**Primary**: cgo binding to the vLLM Python runtime. The cgo layer calls into vLLM's
internal block manager API (`vllm.core.block_manager`) to read block hashes, warm/evict
blocks, and receive cache-hit callbacks.

**Fallback**: Python sidecar over IPC (Unix domain socket). If cgo proves brittle in
practice (e.g. GIL contention, version skew between vLLM releases), the sidecar pattern
provides equivalent semantics: a lightweight Python process that owns the vLLM session
and communicates block events to the Go kernel via a simple JSON-lines protocol.

The RFC specifies the interface; both shapes satisfy it. The implementation PR targets
cgo as primary with the sidecar interface stubbed for fallback.

### KV bit-equivalence hashing tuple

Hash: `SHA-256(prompt_text + "|" + model_name + "|" + temperature + "|" + top_p + "|" + top_k + "|" + max_tokens)`

Excluded from the hash in v1: model precision, runtime version, hardware ops. These
introduce instability across deployments without meaningfully improving cache hit rates
in practice. The hash is an equivalence **hint**, not strict identity: a cache miss on
hash-equal content is safe (cold start); a false positive is safe (stale KV bytes are
semantically incorrect but evicted by vLLM's own block validity checks).

### Provider scope for v1

Feature-complete scaffolding with mock-based tests. The following land in v0.5.0:

- Provider interface implementation
- `cache.kv_block` CogBlock Kind + body schema
- `KVCacheProvider` implementing `Reconcilable`
- Bus event type definitions and payload structs
- Mock vLLM client (interface that cgo binding will implement)
- Unit tests against the mock
- Stub for fork-over-kvcache (RFC-0005 integration point)
- README for the provider directory

Hardware validation (real vLLM instance, CUDA device) is deferred and tracked in a
follow-up issue.

### Bus event payload shapes

Typed structs with cache stats. No raw maps; all fields are typed and documented.

## `cache.kv_block` CogBlock Kind

### Kind constant

```go
const KindCacheKVBlock CogBlockKind = "cache.kv_block"
```

### Body schema

```go
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

// KVSamplingConfig captures sampling parameters for the equivalence hash.
type KVSamplingConfig struct {
    Temperature float64 `json:"temperature"`
    TopP        float64 `json:"top_p"`
    TopK        int     `json:"top_k"`
    MaxTokens   int     `json:"max_tokens"`
}
```

## `KVCacheProvider`: Reconcilable implementation

```go
// KVCacheProvider manages the vLLM KV block cache as a Reconcilable resource.
// It reconciles declared hot-prefixes (system prompt cache declarations in the
// workspace config) against live warm blocks on the vLLM channel.
//
// Implements reconcile.Reconcilable.
type KVCacheProvider struct {
    client   VLLMClient    // interface; cgo or sidecar implements
    channelID string
    logger   *slog.Logger
}

// Type implements Reconcilable.
func (p *KVCacheProvider) Type() string { return "cache.kv_block" }

// LoadConfig loads declared hot-prefixes from the workspace config.
// Config shape: list of {prompt_prefix, model_name, sampling_config} entries.
func (p *KVCacheProvider) LoadConfig(root string) (any, error)

// FetchLive queries the vLLM client for currently warm blocks.
func (p *KVCacheProvider) FetchLive(ctx context.Context, config any) (any, error)

// ComputePlan diffs declared prefixes against warm blocks.
// Actions: "warm" (prefix declared but not warm), "evict" (warm but not declared),
// "skip" (in sync).
func (p *KVCacheProvider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error)

// ApplyPlan warms or evicts blocks to converge on the declared state.
func (p *KVCacheProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error)

// BuildState constructs state from live block data.
func (p *KVCacheProvider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error)

// Health returns the three-axis status: Healthy if hit rate > 0.5,
// Degraded if fragmentation > 0.8, Progressing during a warm/evict cycle.
func (p *KVCacheProvider) Health() reconcile.ResourceStatus
```

## VLLMClient interface (mock-able)

```go
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
    BlockHashForPrompt(prompt, model string, cfg KVSamplingConfig) string

    // CacheStats returns current hit rate and fragmentation metrics.
    CacheStats(ctx context.Context) (CacheStats, error)

    // Close releases the underlying cgo handles or sidecar connection.
    Close() error
}

type WarmRequest struct {
    PromptPrefix   string           `json:"prompt_prefix"`
    ModelName      string           `json:"model_name"`
    SamplingConfig KVSamplingConfig `json:"sampling_config"`
}

type BlockResult struct {
    BlockHash string `json:"block_hash"`
    Warmed    bool   `json:"warmed"`
    Error     string `json:"error,omitempty"`
}

type WarmBlock struct {
    BlockHash     string           `json:"block_hash"`
    ModelName     string           `json:"model_name"`
    SamplingConfig KVSamplingConfig `json:"sampling_config"`
    WarmAt        time.Time        `json:"warm_at"`
    BlockSize     int              `json:"block_size"`
}

type CacheStats struct {
    HitRate       float64 `json:"hit_rate"`       // 0.0–1.0
    BlockCount    int     `json:"block_count"`
    Fragmentation float64 `json:"fragmentation"`  // 0.0–1.0
}
```

## Bus event types

```go
// KVCacheWarmedEvent fires when one or more blocks are successfully warmed.
type KVCacheWarmedEvent struct {
    ChannelID  string      `json:"channel_id"`
    BlockHashes []string   `json:"block_hashes"`
    CacheStats  CacheStats `json:"cache_stats"`
}

// KVCacheEvictedEvent fires when one or more blocks are evicted.
type KVCacheEvictedEvent struct {
    ChannelID   string     `json:"channel_id"`
    BlockHashes []string   `json:"block_hashes"`
    CacheStats  CacheStats `json:"cache_stats"`
}

// KVCacheHitRateChangedEvent fires when the hit rate crosses a 5% threshold.
type KVCacheHitRateChangedEvent struct {
    ChannelID  string  `json:"channel_id"`
    PrevRate   float64 `json:"prev_rate"`
    CurrentRate float64 `json:"current_rate"`
    CacheStats CacheStats `json:"cache_stats"`
}

// KVCacheFragmentationHighEvent fires when fragmentation exceeds 0.8.
type KVCacheFragmentationHighEvent struct {
    ChannelID     string  `json:"channel_id"`
    Fragmentation float64 `json:"fragmentation"`
    CacheStats    CacheStats `json:"cache_stats"`
}

// Bus topic constants
const (
    TopicKVCacheWarmed           = "inference.cache.warmed"
    TopicKVCacheEvicted          = "inference.cache.evicted"
    TopicKVCacheHitRateChanged   = "inference.cache.hit_rate_changed"
    TopicKVCacheFragmentationHigh = "inference.cache.fragmentation_high"
)
```

## Cross-RFC integration: KVBlockHashProvider

The vLLM provider implements the `KVBlockHashProvider` interface so that the
`cog_fork_session` fork handler (RFC-0005) can obtain the parent session's KV block
hash when forking over a kvcache layer. RFC-0005 is the consumer; this RFC is the
implementing provider.

```go
// KVBlockHashProvider is implemented by inference providers that expose
// a content-addressed KV-block layer (e.g., vLLM PagedAttention).
// The fork-session handler queries this when forking over a kvcache layer
// to obtain the parent's KV block hash.
type KVBlockHashProvider interface {
    // ParentKVBlockHash returns the hash of the KV block representing
    // the session's KV state at the given message ID, or an error if
    // the block is no longer addressable (evicted, runtime restarted).
    ParentKVBlockHash(ctx context.Context, sessionID string, atMessageID string) (BlockHash, error)
}
```

The vLLM `VLLMClient` (cgo binding or Python sidecar) provides the block hash by
querying `vllm.core.block_manager` for the block covering the given message's token
range. When the block has been evicted or the runtime has restarted, `ParentKVBlockHash`
returns a typed error; the fork handler in RFC-0005 degrades gracefully to a cold start.

## Fork-over-kvcache stub (RFC-0005 integration)

The `KVCacheOverlay.InheritParentKV` field in RFC-0005 is the integration point.
When `cog_fork_session` is called with `kv_cache: {inherit_parent_kv: true}`:

1. The fork handler looks up the parent session's most-recent `cache.kv_block` CogBlock
   to find `ParentKVBlockHash`.
2. It populates `KVCacheOverlay.ParentKVBlockHash` in the `session.fork` body.
3. The child session's first completion request carries the parent KV block hash as a
   routing hint.
4. The vLLM provider's router uses the hash to schedule the child's request on the
   same channel as the parent, maximizing cache reuse.

In v0.5.0 scaffolding, step 3 and 4 are stubbed: the hash is carried through but the
channel scheduling logic emits a log line rather than actually routing. Full integration
lands when Linux/CUDA hardware is available and RFC-0005 implementation is complete.

The stub is explicitly marked:

```go
// TODO(rfc-0006): stub — full fork-over-kvcache routing requires Linux/CUDA.
// See: https://github.com/myrgic/cogos/issues/203
```

## Provider directory structure

Per RFC-0004's package convention, provider files land in `internal/providers/vllm/`
rather than `internal/engine/`. If RFC-0004 lands with a different convention, this
section rebases to match.

```
internal/providers/vllm/
├── provider_vllm.go          # Provider interface implementation
├── provider_vllm_test.go     # Unit tests against MockVLLMClient
├── provider_vllm_client.go   # VLLMClient interface + WarmRequest/etc types
├── provider_vllm_mock.go     # MockVLLMClient for tests
├── provider_vllm_cache.go    # KVCacheProvider (Reconcilable)
├── provider_vllm_events.go   # Bus event types + topic constants
└── README-vllm.md            # cgo/sidecar path docs, hardware requirements
```

## cgo failure isolation

All cgo callouts into the vLLM Python runtime are wrapped in a deferred recover that
converts panics (and Go-recoverable SIGSEGV via `debug.SetPanicOnFault(true)` from
the `runtime/debug` package) into typed Go errors. cgo failure never propagates to
crash the kernel process. The wrapper:

```go
import "runtime/debug"

func vllmCall(fn func() error) (err error) {
    defer func() {
        if r := recover(); r != nil {
            err = fmt.Errorf("vllm cgo failure (recovered): %v", r)
        }
    }()
    debug.SetPanicOnFault(true)
    defer debug.SetPanicOnFault(false)
    return fn()
}
```

If SIGSEGV cannot be recovered in Go's runtime (some C library failures bypass the
panic-handler), the Python-sidecar IPC fallback path becomes the primary; the cgo path
is then opt-in for trusted vLLM versions only.

## Hardware blocker

**Full integration requires Linux/CUDA and a running vLLM instance.** The v0.5.0
deliverable is scaffolding with mock-based test coverage. The following are explicitly
out of scope until a Linux/CUDA environment is available:

- cgo binding to `vllm.core.block_manager`
- Integration test against a real vLLM instance
- Cache-aware dispatch routing with real block hashes
- Full fork-over-kvcache end-to-end

Hardware integration tests are tagged `requires-cuda` and tracked as a follow-up
milestone item. Implementation PRs must state this blocker prominently.

## mlx-engine path (Apple Silicon)

mlx-engine on Apple Silicon substitutes for development iteration. The mlx-engine
provider does not expose block-level APIs; the `VLLMClient` interface degrades to
opaque-block semantics:

- `ListWarmBlocks` returns a single synthetic block per conversation (no intra-block
  addressability).
- `CacheHint` always returns 0.0 (cold) — mlx-engine has no shared prefix cache.
- `WarmBlocks` / `EvictBlocks` are no-ops.

The `KVCacheProvider` Reconcilable works correctly against this degraded client; it
simply has nothing to warm or evict. This allows local development of the Reconcilable
lifecycle without CUDA hardware.

## Compose-fits

- **RFC-0003** (CogBlock topology): `cache.kv_block` is a new Kind in the topology;
  the envelope is unchanged.
- **RFC-0005** (cog_fork_session): `KVCacheOverlay` is the integration point;
  fork-over-kvcache is stubbed here and completed in RFC-0005 implementation.
- **ADR-059** (CogBlock envelope): `cache.kv_block` uses the standard envelope;
  KV bytes are pointed-to, not inlined.
- **ADR-089** (pointer-envelope): `EvictedAt` marks the eviction state; the envelope
  persists in the ledger after eviction per ADR-089 semantics.
- **reconcile.Reconcilable** (private RFC-008): `KVCacheProvider` implements the
  seven-method contract exactly.

## Acceptance criteria

- [ ] RFC merged (this document).
- [ ] `cache.kv_block` Kind constant + `KVBlockBody` + `KVSamplingConfig` types
      committed to `internal/engine/`.
- [ ] Prerequisite (separate sub-issue before implementing `cache.kv_block` Kind):
      Refactor existing Kind dispatch from switch-based to registry pattern so that
      adding the `cache.kv_block` Kind requires no modification to Kind-handler switch
      statements; the Kind infrastructure dispatches via registry, not switch.
- [ ] `KVCacheProvider` implementing `Reconcilable` — all seven methods — committed.
- [ ] `VLLMClient` interface + `MockVLLMClient` committed.
- [ ] Bus event types (`KVCacheWarmedEvent` etc.) + topic constants committed.
- [ ] Fork-over-kvcache stub committed with `TODO(rfc-0006)` marker.
- [ ] Unit tests covering: mock warm/evict cycle, Health() transitions, CacheStats
      propagation to bus events, hash computation for equivalence tuple.
- [ ] `internal/engine/README-vllm.md` committed documenting cgo and sidecar paths.
- [ ] Implementation PR body explicitly states hardware blocker (Linux/CUDA required).
- [ ] `go build ./...` and `go test ./...` green.
- [ ] All `KVCacheProvider` Reconcilable operations and bus event emissions emit
      structured logs per the kernel log convention with fields: `operation` (one of
      `warm`, `evict`, `hit_rate_change`, `frag_high`), `channel_id`, `block_count`,
      `hit_rate` (0-1 float), `ts`.

## Future scope (post-v0.5.0)

Cache-aware routing deferred until Linux/CUDA hardware available to validate routing
benefits empirically; a follow-up RFC introduces `CacheHintProvider` and the
dispatch-side hint consumer after v0.5.0 scaffolding ships and hardware is available.

```go
// CacheHintProvider is an optional interface providers may implement to
// participate in cache-aware dispatch routing.
type CacheHintProvider interface {
    Provider
    // CacheHint returns a [0,1] score indicating how warm this provider's
    // cache is for the given request. 0 = cold start, 1 = full prefix hit.
    CacheHint(req *CompletionRequest) float64
}
```

The cache-aware dispatch routing section (router calling `CacheHint(req)` on all
providers and routing to the highest-scoring provider) is also deferred to the same
follow-up RFC.

## Out of scope

- cgo binding to `vllm.core.block_manager` (hardware-blocked; follow-up milestone).
- Python sidecar implementation (architecture specified; implementation deferred).
- Cross-node KV federation (speculative; not designing for it but not precluding it).
- Replacing other inference channels (vLLM joins the channel registry, doesn't displace
  mlx-engine/ollama/anthropic/etc).
- Block mesh manifest schema RFC (separate work; covers all layer kinds).
- mlx-engine block-level API promotion (mlx-engine's architecture differs; separate
  discussion if the mlx team exposes block APIs).
