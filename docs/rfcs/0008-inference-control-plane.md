# RFC-0008: Inference Control Plane via Node-State Observatory

| Field    | Value                                                                                                    |
|----------|----------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                    |
| Author   | @chazmaniandinkle                                                                                        |
| Tracking | [#TBD](https://github.com/myrgic/cogos/issues/)                                                          |
| Target   | `v0.7.0`                                                                                                 |
| Relates  | [RFC-0006 vLLM provider](0006-vllm-pagedattention-provider.md), [RFC-0007 named-provider dispatch](0007-dispatch-provider-override.md), [ADR-090 kind-dispatch-via-registry](../adrs/090-kind-dispatch-via-registry.md) |

## Summary

The kernel's inference routing is currently blind to runtime state. The router can prefer a provider by name (RFC-0007) but has no view of whether that provider's process is running, which models are resident in memory, how long a cold-start would take from the current storage tier, or whether memory pressure makes a particular channel inadvisable. This RFC defines the **NodeStateObservatory** — a new substrate primitive that continuously probes and materializes inference runtime state — and integrates it into `SimpleRouter.Route()` so routing decisions are informed by that live view.

This RFC also introduces the **Observatory** as a named substrate concept distinct from Registry and Reconciler, and declares a follow-up ADR to formalize it as a first-class primitive in the substrate taxonomy.

## 1. Motivation

### The SD-card cold-start problem

A `StorageTier` matters at routing time, not just download time. A 7B-parameter model stored on a USB-bus SD card (read bandwidth ~30 MB/s) requires roughly 230 seconds to load from cold — versus ~6 seconds from a fast internal SSD (~1.1 GB/s). A naive router that sees both channels as "available" will happily route a latency-sensitive dispatch to the SD-card channel if the SSD channel is momentarily busy. It cannot know the difference because there is no runtime-state view.

### Lack of runtime visibility in routing

`SimpleRouter.Route()` today resolves a provider name against a static config and checks a cached `lmStudioReachable` flag. That flag is a binary ping — it does not capture process state, loaded models, memory residency, or storage tier. A channel that is "reachable" might require a 4-minute cold-start to serve the request. A channel that is "unreachable" might have crashed 10 seconds ago and be about to restart.

The autonomic loop already emits process-state events; the kernel bus carries structured events; but no component synthesizes these into a queryable runtime view that the router can consult.

### Substrate-architecture mirror

RFC-0006 exposed KV blocks as substrate primitives at the inference-runtime tier. RFC-0007 gave callers named-provider dispatch. Both assume providers exist and are reachable. Neither answers: *which* available provider should handle this request given what the system looks like right now? That is a routing question that requires a live model of inference channel state — an Observatory.

## 2. Concept: Observatory

### Definition

An **Observatory** is a substrate component with these four properties:

1. **Continuous observation** of some aspect of system or world state (a runtime process, a hardware device, a remote endpoint, a model's memory residency).
2. **Materialized current view** — a cached, queryable, eventually-consistent snapshot. Callers read from the view without triggering fresh probes; probes happen on a background schedule.
3. **Bus event emission** on view changes — when the observed state transitions, the Observatory emits a structured bus event. Subscribers receive changes; they do not poll.
4. **No authority to modify** what it observes. An Observatory is read-only by design. It decouples sensing from acting.

### Contrast with Registry and Reconciler

| Primitive   | Owns    | Acts    | Source of truth           |
|-------------|---------|---------|---------------------------|
| Registry    | Named catalog of declared things | No | Config / declaration |
| Reconciler  | A resource type | Yes — converges declared → live | Its own domain |
| Observatory | A view of observed state | No | Runtime / world |

A Registry knows what providers are configured. A Reconciler knows what providers *should* be running and can restart them. An Observatory knows what providers *are* running right now and what state they are in.

The Inference Control Plane is naturally an Observatory because most of what it observes is NOT under the substrate's authority: LM Studio, Ollama, and remote API providers are externally managed. For cog-native runtimes (cog-mlx, future vLLM), a separate Reconciler takes Observatory output and acts — it may evict a stale model or restart a crashed process. The Observatory just observes; the Reconciler decides.

### Follow-up ADR

This RFC introduces the Observatory term and uses it consistently throughout. Formalizing Observatory as a first-class substrate primitive alongside Registry and Reconciler — with interface contract, event taxonomy, and composition rules — is **out of scope for RFC-0008** but warrants a dedicated ADR. RFC-0008 is the origin of the term in the kernel codebase; that ADR should reference this RFC as the motivating introduction.

## 3. NodeStateObservatory

The **NodeStateObservatory** is the specific Observatory implementation for inference runtime state on a single node.

### Identity

- **Package**: `internal/observatory`
- **Interface**: `NodeStateObservatory`
- **Scope**: local node only. Cross-node inference federation is future scope (§10).
- **Source of truth**: direct process probes, HTTP health checks, `sysctl`/`vm_stat` for memory pressure, `diskutil`/`lsblk` for storage tier classification.

### Probe loop

The Observatory runs a background goroutine that probes each known InferenceChannel on a configurable interval (default 30s). On first observation of a new channel it emits `runtime.observed`. On each subsequent probe it diffs the current view against the prior view and emits change events as needed.

New channels can also be discovered dynamically (probe-and-adopt): if a probe detects a healthy HTTP endpoint at a known port that is not yet in the channel registry, the Observatory emits `runtime.adopted` and begins tracking it.

### Interface

```go
// NodeStateObservatory materializes a live view of inference runtime state
// on the local node. All methods are read-only; the Observatory does not
// modify any runtime.
type NodeStateObservatory interface {
    // Channels returns the current materialized view of all known
    // InferenceChannels, including their model residency state.
    Channels() []InferenceChannel

    // Channel returns the current view of a single channel by ID.
    // ok=false when the channel is unknown.
    Channel(id string) (InferenceChannel, bool)

    // MemoryPressure returns the current node memory pressure level.
    MemoryPressure() MemoryPressureLevel

    // Subscribe returns a channel that receives bus events emitted by the
    // Observatory. The caller is responsible for draining the channel.
    Subscribe() <-chan ObservatoryEvent

    // Close stops the probe loop and releases resources.
    Close() error
}
```

## 4. Data Model

### InferenceChannel

```go
// InferenceChannel represents the Observable state of one inference runtime
// endpoint on the local node. It is reconstructed on each probe cycle.
type InferenceChannel struct {
    // ID is a stable identifier matching the provider name in providers.yaml.
    // e.g. "mlx-gemma", "desktop", "ollama"
    ID string

    // RuntimeKind identifies the inference runtime implementation.
    RuntimeKind RuntimeKind

    // Classification determines which code path the router uses.
    Classification InferenceClassification

    // Endpoints lists the network or stdio addresses the runtime exposes.
    Endpoints []ChannelEndpoint

    // Capabilities describes what the runtime can do.
    Capabilities ChannelCapabilities

    // State is the last observed process and resource state.
    State ChannelState

    // Models lists the models known to be loadable by this runtime,
    // with their residency information.
    Models []ModelResidency
}

// RuntimeKind enumerates known inference runtime implementations.
type RuntimeKind string

const (
    RuntimeKindMLX          RuntimeKind = "mlx-lm"
    RuntimeKindOllama       RuntimeKind = "ollama"
    RuntimeKindLMStudio     RuntimeKind = "lmstudio"
    RuntimeKindVLLM         RuntimeKind = "vllm"
    RuntimeKindClaudeCode   RuntimeKind = "claude-code"
    RuntimeKindCodex        RuntimeKind = "codex"
    RuntimeKindAnthropicAPI RuntimeKind = "anthropic-api"
    RuntimeKindOpenAIAPI    RuntimeKind = "openai-api"
)

// InferenceClassification bifurcates the routing path.
type InferenceClassification string

const (
    // ClassificationCogNative: substrate-managed runtime; block-primitive
    // available; Reconciler may act on it.
    ClassificationCogNative InferenceClassification = "cog-native"

    // ClassificationExternalOnNode: externally managed runtime running on
    // this node (LM Studio, Ollama). Observable but not modifiable.
    ClassificationExternalOnNode InferenceClassification = "external-on-node"

    // ClassificationExternalRemote: remote API endpoint (Anthropic, OpenAI,
    // desktop machine). No local process to probe.
    ClassificationExternalRemote InferenceClassification = "external-remote"
)

// ChannelEndpoint is one address on which the runtime accepts requests.
type ChannelEndpoint struct {
    Protocol   EndpointProtocol // http | mcp | grpc | stdio
    Address    string           // e.g. http://localhost:1234
    HealthPath string           // e.g. /health (empty if no dedicated path)
}

// EndpointProtocol enumerates supported transport protocols.
type EndpointProtocol string

const (
    ProtocolHTTP  EndpointProtocol = "http"
    ProtocolMCP   EndpointProtocol = "mcp"
    ProtocolGRPC  EndpointProtocol = "grpc"
    ProtocolStdio EndpointProtocol = "stdio"
)

// ChannelCapabilities describes what this runtime can do.
type ChannelCapabilities struct {
    // BlockPrimitive indicates KV-cache block access capability.
    BlockPrimitive BlockPrimitive

    // ContextWindow is the maximum context length in tokens.
    ContextWindow int

    // ToolUse indicates support for tool/function calling.
    ToolUse bool

    // Multimodal indicates support for image/audio inputs.
    Multimodal bool

    // Streaming indicates support for streamed completion responses.
    Streaming bool
}

// BlockPrimitive describes the level of block-cache access available.
type BlockPrimitive string

const (
    BlockPrimitiveNone         BlockPrimitive = "none"
    BlockPrimitivePagedAttn    BlockPrimitive = "pagedattention"
    BlockPrimitiveCogNativeMLX BlockPrimitive = "cog-native-mlx"
)

// ChannelState is the last observed process and resource state.
type ChannelState struct {
    // Process is the observed process lifecycle state.
    Process ProcessState

    // PID is the observed process ID; zero when not running.
    PID int

    // MemoryResidentGB is how much node RAM this runtime is consuming.
    MemoryResidentGB float64

    // ColdStartEstimateSec is the estimated time to first token from cold,
    // derived from StorageTier bandwidth and observed history.
    ColdStartEstimateSec float64
}

// ProcessState is the observed lifecycle state of a runtime process.
type ProcessState string

const (
    ProcessStateRunning  ProcessState = "running"
    ProcessStateNotFound ProcessState = "not_running"
    ProcessStateCrashed  ProcessState = "crashed"
    ProcessStateUnknown  ProcessState = "unknown"
)
```

### ModelResidency

```go
// ModelResidency describes the storage and memory state of one model
// as observed in a particular InferenceChannel.
type ModelResidency struct {
    // ModelID is the identifier as reported by the runtime.
    ModelID string

    // WeightSizeGB is the on-disk weight size in gigabytes.
    WeightSizeGB float64

    // StorageTier describes where the weights live on storage.
    Storage StorageTier

    // LoadedInMemory is true when the model is currently resident in VRAM/RAM.
    LoadedInMemory bool

    // LastInferenceAt is the timestamp of the last observed inference request.
    // Zero value when not yet observed.
    LastInferenceAt time.Time

    // ColdStartSeconds is the observed time to load this model from cold,
    // populated after the first complete cold-start observation.
    // Zero value until observed.
    ColdStartSeconds float64
}
```

### StorageTier

```go
// StorageTier classifies where model weights are stored and what bandwidth
// the load path provides.
type StorageTier struct {
    // Tier is the storage class.
    Tier StorageTierKind

    // ReadBandwidthMBps is the estimated or measured read bandwidth
    // for this storage device in MB/s.
    ReadBandwidthMBps int

    // DeviceID is an optional device identifier for diagnostics.
    // e.g. "/Volumes/2TBSD", "/dev/disk4"
    DeviceID string
}

// StorageTierKind enumerates the storage classes the Observatory can classify.
type StorageTierKind string

const (
    StorageTierSSD        StorageTierKind = "ssd"         // ~500-7000 MB/s
    StorageTierSDCard     StorageTierKind = "sd_card"     // ~10-90 MB/s
    StorageTierNetwork    StorageTierKind = "network"     // variable; latency-dominated
    StorageTierMemoryOnly StorageTierKind = "memory_only" // model already in RAM
)

// MemoryPressureLevel is the node-level memory pressure state.
type MemoryPressureLevel string

const (
    MemoryPressureLow    MemoryPressureLevel = "low"
    MemoryPressureMedium MemoryPressureLevel = "medium"
    MemoryPressureHigh   MemoryPressureLevel = "high"
)
```

## 5. Bus Events

The Observatory emits structured events on the kernel bus on each state transition. All event types carry a `channel_id` field identifying the InferenceChannel involved. Payloads follow the kernel's structured-log convention; new Kinds register via ADR-090 (kind-dispatch-via-registry).

### Event types and payload schemas

```go
// ObservatoryEvent is the union type for all Observatory-emitted bus events.
type ObservatoryEvent struct {
    Kind      ObservatoryEventKind
    ChannelID string
    Payload   any // one of the payload types below
}

// ObservatoryEventKind enumerates Observatory event types.
type ObservatoryEventKind string

const (
    // RuntimeObserved fires when the Observatory first observes a runtime.
    // Payload: RuntimeObservedPayload
    EventRuntimeObserved ObservatoryEventKind = "runtime.observed"

    // RuntimeStarted fires when a process transitions from not_running/unknown
    // to running.
    // Payload: RuntimeStartedPayload
    EventRuntimeStarted ObservatoryEventKind = "runtime.started"

    // RuntimeCrashed fires when a previously running process disappears
    // without an expected shutdown sequence.
    // Payload: RuntimeCrashedPayload
    EventRuntimeCrashed ObservatoryEventKind = "runtime.crashed"

    // RuntimeAdopted fires when an external runtime is newly discovered
    // via probe-and-adopt and added to the channel registry.
    // Payload: RuntimeAdoptedPayload
    EventRuntimeAdopted ObservatoryEventKind = "runtime.adopted"

    // ModelLoaded fires when a model becomes resident in a runtime's memory.
    // Payload: ModelLoadedPayload
    EventModelLoaded ObservatoryEventKind = "model.loaded"

    // ModelEvicted fires when a model is unloaded from a runtime's memory.
    // Payload: ModelEvictedPayload
    EventModelEvicted ObservatoryEventKind = "model.evicted"

    // ModelSwapped fires when a different model is loaded into the same
    // runtime slot (eviction + load in one observed transition).
    // Payload: ModelSwappedPayload
    EventModelSwapped ObservatoryEventKind = "model.swapped"

    // MemoryPressureChanged fires when node memory pressure crosses a
    // threshold (low <-> medium <-> high).
    // Payload: MemoryPressureChangedPayload
    EventMemoryPressureChanged ObservatoryEventKind = "memory.pressure_changed"

    // ChannelHealthChanged fires when an InferenceChannel transitions
    // between available and unavailable.
    // Payload: ChannelHealthChangedPayload
    EventChannelHealthChanged ObservatoryEventKind = "channel.health_changed"

    // ChannelColdStartObserved fires when a completed cold-start is observed;
    // records duration for future routing cost estimates.
    // Payload: ChannelColdStartObservedPayload
    EventChannelColdStartObserved ObservatoryEventKind = "channel.cold_start_observed"
)

// --- Payload types ---

type RuntimeObservedPayload struct {
    Channel InferenceChannel
}

type RuntimeStartedPayload struct {
    ChannelID string
    PID       int
    At        time.Time
}

type RuntimeCrashedPayload struct {
    ChannelID  string
    LastPID    int
    DetectedAt time.Time
}

type RuntimeAdoptedPayload struct {
    Channel  InferenceChannel
    Endpoint ChannelEndpoint
}

type ModelLoadedPayload struct {
    ChannelID string
    ModelID   string
    Residency ModelResidency
}

type ModelEvictedPayload struct {
    ChannelID string
    ModelID   string
}

type ModelSwappedPayload struct {
    ChannelID     string
    EvictedModel  string
    LoadedModel   string
    NewResidency  ModelResidency
}

type MemoryPressureChangedPayload struct {
    Previous MemoryPressureLevel
    Current  MemoryPressureLevel
}

type ChannelHealthChangedPayload struct {
    ChannelID string
    Available bool
}

type ChannelColdStartObservedPayload struct {
    ChannelID        string
    ModelID          string
    DurationSeconds  float64
    StorageTierAtStart StorageTier
}
```

## 6. Cog-Native vs External Path Bifurcation

### Routing diagram

```
Agent dispatch request
        │
        ▼
  InferenceChannel router
  (SimpleRouter.Route)
        │
        ├─── cog-native classification ──────────────────────────────────────────┐
        │                                                                         │
        │    context block ──► block-cache-aware primitive                        │
        │                          │                                              │
        │                          ├── KV block ref (hash match) ─► REUSE        │
        │                          └── no match ─────────────────► GENERATE      │
        │                                    │                                    │
        │                                    ▼                                    │
        │                          managed inference runtime                      │
        │                          (cog-mlx / future vLLM)                        │
        │                                    │                                    │
        │                                    ▼                                    │
        │                             response ◄──────────────────────────────────┘
        │
        └─── external classification ────────────────────────────────────────────┐
                                                                                  │
             context block ──► flatten to OpenAI messages array                   │
                                    │                                             │
                                    ▼                                             │
                             HTTP API call                                        │
                             (LM Studio / Ollama / Anthropic / OpenAI)           │
                                    │                                             │
                                    ▼                                             │
                              response ◄────────────────────────────────────────-┘
```

### Path semantics

**Cog-native path**: the agent's context block flows into a block-cache-aware substrate primitive. On a hash match, a KV block reference is returned and the existing cached computation is reused (cross-request prefix reuse). On a miss, the primitive generates and the new blocks are cached by hash. Bus events flow on cache state. The runtime is managed by the substrate — launchd plists parallel to `com.cogos.mod3`.

**External path**: the agent's context block is flattened to an OpenAI-compatible messages array and sent as an HTTP API call to the managed-external runtime (LM Studio, Ollama) or a remote endpoint (Anthropic, OpenAI). The external provider may perform its own internal caching, but the substrate is blind to it — no block primitives, no cross-request KV reuse at the substrate layer. External runtimes are not modified by the substrate.

Both paths surface as a named provider from the agent's perspective. The `InferenceClassification` field on the channel determines which code path the router selects.

## 7. Router Integration

`SimpleRouter.Route()` currently resolves a provider name against static config and checks a binary reachability flag. RFC-0008 extends it to consult the NodeStateObservatory before committing to a channel.

### Routing factors

The router weighs the following factors, in priority order:

1. **Block-primitive requirement**: if the request carries a `RequiresBlockPrimitive` flag (e.g., cog_fork_session, prefix-reuse hint), only channels where `capabilities.block_primitive != none` are eligible. Non-block channels are excluded before any other factor.

2. **Capability match**: channels that do not satisfy `context_window >= request.estimated_tokens`, `tool_use`, or `multimodal` requirements are excluded.

3. **Cold-start cost**: among eligible channels, prefer warm channels (model already resident in memory) over cold channels. When cold-start cost exceeds a configurable budget (`routing.max_acceptable_cold_start_sec`, default 30s), the channel is treated as temporarily ineligible and the fallback chain advances.

4. **Storage tier penalty**: SD-card-resident models are penalized by their estimated cold-start duration relative to SSD baseline. A model on SD card that is not currently loaded carries a high cold-start estimate (~100-230s for typical 7B models at USB bus bandwidth); this makes it ineligible when any warm SSD-resident option exists.

5. **Memory pressure**: under `MemoryPressureHigh`, channels whose load would force eviction of a currently-loaded model are deprioritized. The Observatory's `MemoryPressure()` method provides the node-level signal.

6. **Config fallback chain**: the static `routing.fallback_chain` in `routing.yaml` provides the baseline preference order. The Observatory narrows the eligible set; the fallback chain determines the winning channel within it.

### Interface extension

```go
// RoutingContext carries Observable runtime state into the router.
type RoutingContext struct {
    Observatory NodeStateObservatory
}

// Route is extended to accept an optional RoutingContext.
// When ctx.Observatory is nil, routing degrades to the existing
// config-only behavior (backward compatible).
func (r *SimpleRouter) RouteWithContext(
    req CompletionRequest,
    ctx RoutingContext,
) (ProviderConfig, error)
```

The existing `SimpleRouter.Route()` method delegates to `RouteWithContext` with a nil Observatory for backward compatibility. New call sites (MCP handler, autonomic loop) pass the live Observatory.

### Cold-start estimate derivation

The Observatory maintains an exponential moving average of observed cold-start durations per (channel, model) pair. When no observation exists, the estimate is derived from `weight_size_gb / storage_tier.read_bandwidth_mbps`. This estimate is updated when `channel.cold_start_observed` events arrive.

## 8. Lifecycle Policy

The classification of an InferenceChannel determines what the substrate is permitted to do when its state changes.

### Cog-native (substrate-managed)

- **Who manages**: the substrate, via launchd plists (same pattern as `com.cogos.mod3`).
- **Observatory role**: observe and emit events.
- **Reconciler role** (separate component, future work): receive Observatory events, apply plans (restart crashed runtime, swap model, evict stale model). The Reconciler is the *only* component permitted to modify a cog-native runtime.
- **Router behavior on crash**: immediately reroute to next eligible channel; emit `runtime.crashed`; Reconciler decides restart policy.

### External-on-node (externally managed)

Runtimes on the same hardware but outside the substrate's authority: LM Studio, Ollama, local instances of third-party tools.

- **Who manages**: the external operator (the user, a separate launchd, etc.).
- **Observatory role**: probe-and-adopt; emit `runtime.adopted` on first discovery; emit state change events.
- **Reconciler role**: none. The substrate does NOT restart, reconfigure, or modify external runtimes.
- **Router behavior on crash**: reroute to next eligible channel; emit `runtime.crashed`. Do not attempt recovery.

### External-remote (remote API)

Remote endpoints (Anthropic API, OpenAI API, desktop machine at 192.168.x.x).

- **Who manages**: the remote provider.
- **Observatory role**: health-check probes only (HTTP GET to health path or model-list endpoint); no process probing.
- **Reconciler role**: none.
- **Router behavior on unhealthy**: reroute; emit `channel.health_changed`.

**Invariant**: the substrate takes responsibility only for runtimes it created. This distinction is load-bearing for operational safety — the substrate will never modify a runtime it does not own.

## 9. Acceptance Criteria

- [ ] `NodeStateObservatory` interface defined in `internal/observatory/` (new package)
- [ ] `InferenceChannel`, `ModelResidency`, `StorageTier` as Go structs in `internal/observatory/types.go`
- [ ] 9 bus event types (`ObservatoryEventKind`) declared with payload structs in `internal/observatory/events.go`
- [ ] `InferenceClassification` enum (`cog-native` | `external-on-node` | `external-remote`) in channel config and types
- [ ] `RuntimeKind` enum covering all 8 declared runtimes
- [ ] `SimpleRouter` extended with `RouteWithContext(req, RoutingContext)` in `internal/routing/`
- [ ] Cold-start cost factored into routing decisions (warm preferred; cold-start budget configurable)
- [ ] Block-primitive requirement excludes non-block channels before other factors
- [ ] Storage-tier classification for known mount points (SSD / SD card / network) via probe at Observatory startup
- [ ] Memory pressure level (`low`/`medium`/`high`) derived from OS APIs (`vm_stat` on macOS, `/proc/meminfo` on Linux)
- [ ] Per-runtime probe loop running on 30s default interval; interval configurable in `routing.yaml`
- [ ] Probe-and-adopt: Observatory detects and tracks external runtimes at known ports not yet in config
- [ ] Existing `SimpleRouter.Route()` delegates to `RouteWithContext` with nil Observatory (backward compatible)
- [ ] Unit tests: Observatory state transitions (not_running → running → crashed → not_running)
- [ ] Unit tests: routing decisions change based on Observatory state (warm vs cold channel selection)
- [ ] Integration test: routing changes when Observatory reports a channel crashed mid-session

## 10. Future Scope

The following are explicitly on the roadmap but out of scope for this RFC:

**Cross-node inference federation**: extending the Observatory to observe InferenceChannels on peer nodes in a constellation. Today the Observatory is node-local. A multi-node extension would require the bus-peering layer (`cog://rfc/027`) and a distributed view-merge protocol. This is a natural follow-on once the local Observatory is stable.

**Predictive model preloading**: using access-pattern history from the Observatory's cold-start log to predict which model will be needed next and issue a background warm-up. The Observatory already records `LastInferenceAt` per model; a scheduler could use this to preload during idle periods.

**Automatic eviction under memory pressure**: today the router avoids loading models under high memory pressure. A Reconciler (future work) could proactively evict least-recently-used models when the Observatory emits `memory.pressure_changed` at the high threshold, freeing capacity before the next request arrives.

## 11. Out of Scope

The following are explicitly NOT in this RFC:

- **Reconciler implementations** per runtime kind (cog-mlx Reconciler, Ollama Reconciler, etc.). Reconcilers consume Observatory events and act; each is a separate implementation effort. RFC-0008 defines the Observatory that feeds them.
- **Cog-native MLX runtime** (`cog-mlx`). The Observatory is designed to observe it, but the MLX runtime itself is a separate implementation effort.
- **vLLM block-API integration**. RFC-0006 covers the vLLM provider with PagedAttention block access. RFC-0008 classifies a vLLM channel as `cog-native` with `block_primitive: pagedattention`; the block-API wiring is RFC-0006's scope.
- **ADR formalizing Observatory as a substrate primitive**. RFC-0008 introduces the term; the ADR is follow-on work.
- **Layer 2 and Layer 3 of RFC-0007** (agent-card provider preference; router-aware autonomic loop). Those layers build on top of the provider resolution work; the Observatory feeds into Layer 3 when it lands.
- **Bus-peering / cross-node events**. `cog://rfc/027` covers bus peering. Observatory events are node-local until that RFC's scope is resolved.
