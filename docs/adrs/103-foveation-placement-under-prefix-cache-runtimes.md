# ADR-103: Foveation Placement Under Prefix-Cache Runtimes — Amendment to ADR-066 / ADR-071

| Field   | Value |
|---------|-------|
| Status  | Accepted — implemented on `feat/foveation-cache-aware-placement` (kernel `internal/engine`); pre-merge output-quality A/B pending (operator) |
| Author  | @chazmaniandinkle |
| Created | 2026-07-07 |
| Layer   | Module (engine / context assembly) |
| Amends  | ADR-066 (Foveated Context Assembly — CogBlock Context Containers), ADR-071 (Unified Foveated Proxy — LoRO as Live Observer) |
| Refs    | ADR-069 (Distributed KV Entanglement Mesh — the never-built KV mesh this amendment reacts to); Fable design-intent review 2026-07-07 |

---

## Context

ADR-066 specified a stability-ordered token stream for KV-cache optimization, with a
zone diagram that places the **volatile knowledge zone (foveal CogDoc manifest) at the
FRONT** of the stream, ahead of conversation history. That placement is only correct
**given a KV-cache manager** that can invalidate at the layer/block level and hold
"nearly permanent" context across a conversation — ADR-066 Phase 5 (KV manager) and
ADR-069 (distributed KV-block entanglement mesh). Those two pieces were **never built**.
The cheap half — per-turn re-scoring of the foveal manifest — shipped; the cache-cheap
half did not.

Under a **plain prefix cache** (llama.cpp / LM Studio / most OpenAI-compat runtimes), the
cache keys on the longest common **token prefix** and has no layer-level invalidation.
With the volatile foveal manifest at the front of the stream, its per-turn churn changes
the prefix, so **everything behind it — the entire conversation — re-prefills every turn.**
The kernel's accepted design was, in this runtime class, prefill-cache-**hostile**. This
was drift from intent, not intent.

Two compounding sources of gratuitous churn were found:

1. **Placement** — the manifest led the token stream (leading system prompt).
2. **Live salience float** — `renderWorkspaceManifest` embedded `[salience: %.2f]` per
   line. Salience is re-scored every turn, so even an **identical doc selection rendered
   different bytes** turn to turn.

Separately, the per-conversation LoRO light cone (ADR-071 Phase 2) was keyed by
`WithConversationID(creq.Metadata.RequestID)` — a **fresh per-request UUID**. The cone was
therefore always `Get()` on a key that had never been `Set()` (the feature was silently
dead), and `LightConeManager` accreted one orphaned entry per request with no TTL (an
unbounded memory leak for the process lifetime).

The Claude Code hook path (`serve_foveated.go`, `additionalContext`) already did the
cache-friendly thing — volatile foveated content is delivered **trailing**. The OpenAI /
Anthropic proxy path did not. This amendment converges the proxy path onto the hook path's
placement.

## Decision

**Under a plain prefix-cache runtime, the volatile foveal block renders TRAILING — after
the stable conversation prefix — not leading.** "Most-stable-first" for a prefix cache
means the churning block must come LAST. Concretely, in `ContextPackage.FormatForProvider`
(`internal/engine/context_assembly.go`):

- `systemPrompt` = nucleus → `# Client Context` **only** (the session-stable content).
- `messages` = conversation history (chronological) → the **final user message** with the
  foveal block (workspace manifest + `<previous-turn-speculative>`) folded in as a
  **trailing injection after the user's own text**.

The block **content** is unchanged (same headers, same manifest render, same speculative
wrapper); only its **position** moved from leading system to trailing user.

**Wire safety.** The block is folded INTO the final user message (not appended as a new
message), so:
- the sequence still ends on a **user turn** → satisfies the Anthropic normalizer's I7
  trailing-user guard (`anthropic_normalize.go`);
- it introduces no `tool_result` blocks → the I4 block-order pass never reorders it;
- it is written to `ProviderMessage.Content` (used by the OpenAI-compat provider and the
  non-image Anthropic path) **and**, when the message carries image parts, appended as a
  trailing text `ContentPart` (the Anthropic multimodal path renders from `ContentParts`
  and ignores `Content`).

**Byte-stability.** The live `[salience: %.2f]` float is **removed** from the rendered
manifest. Salience still ranks and evicts docs upstream (`FovealDoc.Salience`); it is
simply not rendered into the model-visible text. Identical selections now render identical
bytes.

**Session-scoped foveation key.** The foveation / light-cone key is derived from a
**stable per-conversation identity** (`foveation_session_key.go`), never the per-request
UUID and never `Process.SessionID()` (which is process-wide — using it would collapse every
concurrent conversation onto one light cone, a cross-conversation / cross-user state bleed).
Precedence: `X-Cogos-Session-Id` header → client `user` / `metadata.user_id` field →
**stable SHA-256 hash of the conversation's leading turns** (system prompt + first user
message). The leading-turns hash is stable within a conversation and distinct across
conversations, so a raw OpenAI-compat client (Hermes) with no session id still gets a safe,
non-bleeding key.

**Bounded light-cone memory.** `LightConeManager` gains a TTL and a background sweeper
(`NewLightConeManagerWithTTL`, default 30-minute TTL, 5-minute sweep). Stale cones are
evicted regardless of key granularity, closing the leak even if the key stays coarse.
`SetTRM` now `Close()`s a prior manager before replacing it so the sweeper goroutine does
not leak.

### Acceptance criterion (met)

For two consecutive turns with the **same** foveal doc selection, the rendered prefix
(system + all-but-last message) is **byte-identical** (test
`TestPrefixByteStableAcrossTurnsSameSelection`), so a prefix cache reuses it and only the
final exchange + foveal block re-prefill. A **changed** selection alters only the trailing
block, leaving the conversation prefix stable (`TestPrefixStableWhenOnlySelectionChanges`).

## Consequences

- Prefix-cache hit rate on the proxy path rises from near-zero (whole conversation
  re-prefilled each turn) toward reuse-of-everything-but-the-tail.
- The model now sees the foveal manifest **after** the current user message rather than in
  the system preamble. This is a placement change to model-visible context; **output-quality
  is expected to shift** and must be A/B'd against a fixed prompt set **before merge** — that
  gate is out of scope for the structural change and is run by the operator.
- The light cone (ADR-071 Phase 2) can now actually persist across turns of a conversation,
  because the key is stable. Behavioral validation of the revived cone is separate follow-up.

## Out of scope (deferred — clear TODOs)

The following are the "Core + hysteresis" / "Full retool" tiers deliberately deferred; each
leaves a TODO referencing this ADR:

- **Hysteresis / session-stable re-render** of the doc selection (avoid re-selecting docs
  every turn when the query barely moved).
- **Contiguous-oldest eviction** of conversation history (keep the evictable region a
  suffix, not a hole, so eviction doesn't fracture the prefix).
- **Tiered content loading** (summary → full on demand within the trailing block).
- **`cache_control` breakpoints** for the Anthropic path (explicit prefix cache markers).
- **KV manager / KV-block mesh** (ADR-066 Phase 5 / ADR-069) — the original premise of the
  leading placement. If ever built, the leading-vs-trailing decision should be re-derived
  per runtime class rather than assumed.
