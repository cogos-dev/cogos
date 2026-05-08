# The Constellation

The Constellation is one substrate. What the codebase calls `constellation.db` and what the public `myrgic/constellation` repo calls "the identity protocol" are two node populations over the same architecture, not two systems. This doc formalizes that framing and names what Constellation means as a term of art inside CogOS.

The framing landed on 2026-04-22, in the same arc that produced `channels-and-buses.md` and `identity-cycle.md`. It resolves what looked like a naming collision (kernel memory-graph vs. identity-protocol repo, both called "constellation") into a single architectural claim: the primitives converged because the primitives are what the substrate actually needs. The repos stay where they are. The term stops being ambiguous.

## BLUF

The Constellation is a hash-chained, git-backed event ledger plus a peer/reference graph indexed out of it plus EMA-weighted signals over the relationships. Everything that lives in CogOS's coordination layer — cogdocs, identities, channels, sessions, agents, peer nodes — is a node population in that one substrate. Edges between populations (references, trust attestations, attendance, mentions, derivations) compose freely because they share primitives. Signals over edges (attention, trust, salience) are all exponential moving averages over the same ledger.

One substrate, many projections. The kernel's memory-graph is the cogdoc-and-reference projection. The `myrgic/constellation` reference implementation is the trust-node projection. Neither is the whole Constellation; both are views of it.

**Confidence: consensus (within this codebase and session).** The unification is verified at the primitive level (see § Shared Primitives below); the application to new populations (channels, sessions as nodes) is still a design target, not yet in code.

## Shared Primitives

Both projections converged independently on the same substrate primitives. This is the load-bearing evidence for treating them as one thing.

| Primitive | Memory-graph projection | Identity-protocol projection | Shared? |
|---|---|---|---|
| Canonicalization | RFC 8785 canonical JSON (`pkg/cogblock/ledger.go:61-91`) | RFC 8785 canonical JSON (`constellation/ledger.go`) | Yes |
| Content hashing | SHA-256 over canonical form | SHA-256 over canonical form | Yes |
| Event ledger | Git-backed, `events/{seq:08d}.json`, `prior_hash` chain, monotonic `seq` | Git-backed, `events/{seq:08d}.json`, `prior_hash` chain, monotonic `seq` | Yes — same file layout |
| State fingerprint | Tree hash of `events/` directory | Tree hash of `events/` directory | Yes |
| Validation | Hash-chain integrity + schema + temporal monotonicity | Hash-chain integrity + schema + temporal monotonicity | Yes — identical 3-layer check |
| Decay-weighted signals | Attention decay on cogdoc reads / memory access | EMA trust decay (`trust = 0.8·trust + 0.2·consistent`) | Yes — same primitive, different relationships |

Two independent implementations did not accidentally land the same six primitives. They landed them because these are the primitives a coordination substrate actually needs: deterministic content hashes for addressability, append-only ledgers for causal ordering, tree hashes for O(1) state comparison, three-layer coherence for self-verification, and decay-weighted signals for attention allocation under bounded resources.

**Confidence: textbook.** Each primitive is well-understood in its home field (canonical JSON is RFC; SHA-256 chains are standard ledger shape; EMA smoothing is textbook signal processing; git content-addressing is textbook git). The Constellation's contribution is composition, not invention.

## Node Populations

A node in the Constellation is anything the ledger can refer to by a stable identifier. Populations are open-ended; new ones are introduced by defining (a) a node type, (b) a storage convention, (c) the edge schemas that connect it to other populations. Existing and planned populations:

- **Cogdocs** — `.cog.md` files under `.cog/mem/`, indexed in `constellation.db`. The oldest and most numerous population. Already production.
- **Identities** — keyed by NodeID (SHA-256 of pubkey DER in the trust-node projection). Today's kernel tracks a subset via session records; the trust-node projection specifies the full node lifecycle.
- **Channels** — first-class node type proposed by the channel-provider-interface RFC (`${COGOS_WORKSPACE:-$HOME/workspaces/cog}/.cog/mem/semantic/designs/channel-provider-interface.cog.md`). Storage: cogdocs under `.cog/mem/procedural/channels/`. Not yet implemented.
- **Sessions** — currently tracked by kernel session records; not yet canonicalized as Constellation nodes. Candidate for promotion.
- **Agents** — partially represented (identity cards in `cog://agents/`, session records). No canonical node form yet; would compose from identity + session populations.
- **Peer nodes** — remote CogOS instances federated via the trust-node projection. Today only present in the `myrgic/constellation` reference implementation; not yet integrated into the kernel's memory-graph.

The list is extensible by design. Adding a population does not require a substrate change — only a node type, a storage convention, and edge schemas. The Constellation's invariants (hash-chain integrity, EMA decay, tree-hash fingerprinting) apply automatically.

**Confidence: consensus for cogdocs and identities (both have working implementations). Hypothesis for the unified channel and session populations — the design is specified; the code paths to back them aren't written yet.**

## Edge Schemas

Edges are how populations compose. Each edge is a typed relation between two nodes, stored either in the ledger as an event or in the indexed graph as a row.

- **References** — a cogdoc's `refs:` frontmatter pointing to another URI. Composes cogdoc ↔ cogdoc, cogdoc ↔ identity, cogdoc ↔ channel. Stored in `doc_references` / `backlinks` tables (`sdk/constellation/schema.go`).
- **Trust attestations** — per-peer trust scores derived from heartbeat consistency. Composes identity ↔ identity. Stored in the trust-node projection's peer registry; the EMA value is the edge weight.
- **Attendance** — an identity's participation in a channel over time. Composes identity ↔ channel, with an attention-EMA as the edge weight. Specified by the channel-provider RFC; not yet in code.
- **Mentions** — an event's textual reference to a named identity, channel, or cogdoc. Composes event ↔ node across populations. Partially in code (chat event content is indexed into FTS; entity extraction is not).
- **Derivations** — a cogdoc or event derived from an earlier one (summary of a conversation, consolidation of a session). Composes node ↔ node with a provenance relation. The reverse-transcription pathway in `identity-cycle.md` is the canonical gated form of this edge type.

Edges across populations compose for free because they share content addressing. A channel's attendance edge points to the same identity node an agent's session attribution points to, because both resolve through the same hash → URI → node lookup.

## Signal Types

Signals are EMAs over edges. Different populations apply the same decay primitive to different relationships.

- **Attention** — how recently and how often a node has been read or referenced. Applies to any node; surfaces as ranking weights in search, volatility estimates in adaptive sampling (principle 3), and dormancy detection for channels.
- **Trust** — per-peer EMA over heartbeat consistency. The trust-node projection's headline signal; `trust = 0.8·trust + 0.2·consistent` at each observation. Threshold bands (trusted ≥ 0.7, pending ≥ 0.4, suspect ≥ 0.2, rejected < 0.2) are policy over the signal.
- **Salience** — combination of attention and structural position in the reference graph. Semantic: "how load-bearing is this node in the current context." Partially implemented in the cogdoc projection via frontmatter `salience:` plus indexed reference counts.

All three are the same mathematical object — EMA over a stream of 0/1 observations — applied to different relationship types. The Constellation does not need three separate signal systems. It needs one signal primitive with three configurations.

**Confidence: textbook for the math. Consensus for attention and trust (implemented). Hypothesis for salience as a first-class Constellation signal — today it's a frontmatter hint, not a live EMA.**

## Live Worked Example

Trace a single event through the substrate to see populations interact.

**Scenario.** An agent session attends the live gateway channel (port 6931), reads a cogdoc to answer a user question, and emits a reply. Today, the shape is distributed across multiple subsystems; the Constellation framing names which substrate each subsystem is touching.

1. **Session registers.** The agent calls `cogos_session_register`. A session node enters the constellation (today via the kernel's session records; under the channel-provider RFC, also as a `type: channel` cogdoc's `attendance` field). A canonical-JSON event lands on the ledger; `prior_hash` chains to the previous event; the tree hash of `events/` advances.

2. **User message arrives on the channel.** A `chat.request` block hits the gateway. `constellation_bus.go` (see `${MYRGIC_REPOS_ROOT:-$HOME/workspaces/myrgic}/cogos/constellation_bus.go`) indexes the content into the FTS-backed constellation database. Node population: event (bus block). Edges: event → session, event → user identity (by `user_id`/`user_name` payload fields). Signal update: the session's attention-EMA ticks.

3. **Agent reads a cogdoc.** The agent queries `cogos_memory_search` and reads `cog://mem/semantic/.../some-insight.cog.md`. Population: cogdoc. Edge: event → cogdoc (a "read" relation, implicit today, explicit under the attention-EMA design). Signal update: the cogdoc's attention decay bumps upward.

4. **Agent emits a reply.** A `chat.response` block lands on the same bus. `constellation_bus.go` indexes it. Edges: event → session, event → cogdoc (as provenance, if the agent cites the read). `constellation_bridge.go` exports a kernel heartbeat to the trust-node projection — the same bridge interface (`internal/engine/constellation_bridge.go`) that carries trust snapshots also carries the kernel state fingerprint (`NucleusFingerprint`, `CoherenceFingerprint`, `LedgerHead`). One bridge, multiple payload types, because it is one substrate.

5. **Peer heartbeat (hypothetical, in a federated deployment).** Every 5 seconds, each peer CogOS node broadcasts a signed heartbeat — `{node_id, tree_hash, seq, last_hash, timestamp}`. Receiving peers check signature, sequence continuity, and update the trust EMA. The tree hash in the heartbeat is the same tree-hash primitive the memory-graph uses as a state fingerprint; verifying it costs one comparison.

At each step, populations produce events; events update the ledger; the ledger indexes into the graph; the graph's edges carry EMA-weighted signals. Same substrate, three projections observing it simultaneously (memory-graph, trust-node, channel).

## Why Unified, Not Split

Three pieces of concrete evidence that this is one substrate architecturally, not two.

**1. The in-code bridge already carries both payload types.** `internal/engine/constellation_bridge.go` defines `ConstellationBridge` as a single interface with `EmitHeartbeat(KernelHeartbeatPayload)` and `TrustSnapshot()` methods. The kernel exports its own coherence fingerprint, nucleus fingerprint, and ledger head *over the same bridge* the trust-node projection uses for peer heartbeats. The tangle is not a bug. It is the substrate showing through.

**2. The kernel's `participants` table already has exactly the columns an identity-protocol node needs.** Verified against the live schema at `.cog/.state/constellation.db`:

```sql
CREATE TABLE participants (
    id TEXT PRIMARY KEY,              -- "human:<node_id>" or "agent:<session_id>"
    type TEXT NOT NULL,               -- "agent" | "human" | "service"
    name TEXT NOT NULL,               -- "cog", "claude", "sandy", "chaz"
    identity_path TEXT,               -- path to identity card if agent
    session_id TEXT,                  -- current session if active
    active INTEGER NOT NULL DEFAULT 1,
    last_seen TEXT NOT NULL,
    registered_at TEXT NOT NULL,
    node_hash TEXT                    -- links to node_identity.json
);
```

`id`, `type`, `name`, `identity_path`, `session_id`, `active`, `last_seen`, `registered_at`, `node_hash` — every field an identity-protocol node needs is already there, in the memory-graph's own database. The schema was not grown separately to serve the identity-protocol projection; it was what the memory-graph already needed to track which cognitive observers were present. Node population and session tracking converged on the same shape because the architecture requires it.

**3. The canonicalization and ledger shapes are identical across projections.** Not analogous. Identical. RFC 8785 canonical JSON. SHA-256. `events/{seq:08d}.json`. `prior_hash` chain. Tree-hash fingerprint. Two independent implementations producing byte-identical artifacts for the same logical content is not "similar design" — it is "same substrate."

Taken together: the substrate is one. Today's code treats it as two because the code was written in separate tracks. The convergence is architectural; the unification is mostly still pending in the code.

## Relationship to Other Architecture Docs

This doc sits alongside `channels-and-buses.md` and `identity-cycle.md` and ties them to the substrate both assume.

From `channels-and-buses.md`:
- The **triad** (bus / channel / tool surface) is three shapes of edge into the Constellation. Bus events are ledger entries. Channels are node populations (proposed) with attendance edges. Tool surfaces are external endpoints reachable through the bridge interface but not themselves Constellation nodes today.
- **Git as substrate** in that doc is the Constellation's storage layer: blobs are cogdoc content, commits are bus events, refs are channels, remotes are tool surfaces. Same content-addressed primitives at a different abstraction layer.
- **Scale invariance** (principle 6) applies here: the Constellation is fractal by the same logic. A kernel has one Constellation; a repo under `.cog/` has a Constellation projection; a CogBlock carries a one-event slice of it.

From `identity-cycle.md`:
- **Replication / transcription / translation / reverse transcription** are the four phases that modify or express the Constellation's canonical state. Replication writes new ledger events. Transcription reads nodes into active context. Translation produces output from context. Reverse transcription is the **gated** pathway that promotes transient state (events on a channel, behavioral signals) into canonical identity (cogdoc edits, new identity-node attestations, updated salience).
- The Constellation is the thing being modified or read through those phases. The four cycles are *what* changes its state; this doc is *what it is*.
- The reverse-transcription security boundary applies specifically to edges that cross from event-population to canonical-population (e.g., promoting a channel message to a cogdoc edit). Consolidation gates live on that boundary.

Both neighboring docs assume a substrate. This doc names it.

## The myrgic/constellation Repo's Role

The public repo `myrgic/constellation` (at `${MYRGIC_REPOS_ROOT:-$HOME/workspaces/myrgic}/constellation/`) is the **canonical specification and reference implementation for the trust-node projection**. It stays published. It is not renamed. It is not deprecated. It is not a competitor to the kernel's memory-graph — it is specifying the trust semantics the kernel will eventually run natively over the same Constellation substrate.

Why keep it separate as a repo even though the architecture is unified:

1. **Published specification.** The repo is self-contained, has tests (4 scenarios: happy path, drift, key theft, dynamic join), documents structural isomorphism with blockchain, and can be cited as prior art. Folding it into the kernel would lose that standalone artifact.

2. **Reference implementation for the trust projection.** The kernel today uses `ConstellationBridge` as an interface with `NilBridge` as a default and a future real implementation driven by the protocol repo. Keeping the reference implementation external sharpens the contract — the interface lives in the kernel; the behavior lives in the repo.

3. **No architectural distinction lost.** Calling it "the constellation-protocol reference implementation" in prose (where disambiguation matters) preserves the naming without fragmenting the substrate concept. "The Constellation" is the substrate; the repo is a projection of it, with a spec.

The convergence — running the trust-node semantics natively inside the kernel over the same `constellation.db` — is future work. See § Next Steps.

## What This Framing Does Not Claim

To avoid overclaiming: this doc unifies the **architecture**, not the **code**.

- **The code paths are still separate.** The memory-graph's indexing (`sdk/constellation/`, writes from `cog memory write`) and the trust-node projection's heartbeat loop (in the `myrgic/constellation` repo) run in different parts of the kernel process, or entirely different processes. They share the bridge but not the event loop.
- **The 5-second heartbeat and the memory-graph's event ingestion are on different clocks today.** The trust-node projection emits a fresh heartbeat every 5 seconds by design (frequent, cheap, consistency-checking). The memory-graph writes when a cogdoc changes (on edit). Both feed the same ledger shape conceptually; operationally, they feed different ledgers today.
- **No migration is specified here.** This doc does not say "delete constellation.db and replace it with the protocol repo's ledger." It says the primitives are the same. Convergence is a design target; the migration is its own RFC.
- **Not every subsystem that touches the ledger is "in" the Constellation.** The bus layer writes ledger events; the kernel's scheduler reads them; the bridge emits fingerprints. Whether each of those is *part of* the Constellation or *adjacent to* it is an open boundary question. The substrate-vs-service distinction (see `channels-and-buses.md` § Providers per Kind — "Bus is not a reconciled resource. It is a service.") applies: the Constellation is the substrate; everything else is service.
- **This doc does not replace any ADR.** It compresses the I/O implications of ADR-011 (kernel as DNA), ADR-033 (event/signal/ledger separation), ADR-058 (inter-workspace coordination), ADR-062 (recursive node architecture), ADR-074 (nested sovereignty), and ADR-081 (homeostatic kernel loop). The ADRs are authoritative for their respective decisions.

## Related Principles and ADRs

- **Principle 1 (Information in the delta)** — the ledger's append-only shape is where deltas become distinctions; each Constellation event is one.
- **Principle 2 (CogBlock is the quantum of distinction)** — the CogBlock is the ledger's natural event unit; the Constellation aggregates CogBlocks across populations.
- **Principle 3 (Adaptive sampling)** — attention-EMAs drive sample rate across the Constellation's populations; hot nodes sample more.
- **Principle 4 (Boundary crossing energy signatures)** — every event that enters or exits the substrate leaves a ledger entry; the Constellation is the membrane's interior view.
- **Principle 5 (Stigmergic coordination)** — "substrate is the bus" holds macro; the Constellation is the structure of the substrate, the bus is the always-on face of it.
- **Principle 6 (Scale invariance)** — the Constellation repeats at every scale; a repo, a CogBlock, and a full kernel each carry a projection of it.
- **Principle 8 (Identity changes by being read)** — every read is an attention-edge update; every attendance is an identity mutation.
- **ADR-011 (Kernel as Cognitive DNA)** — canonical identity is the Constellation's most-protected node population; the four-cycle cascade modifies it under gating.
- **ADR-033 (Event / signal / ledger separation)** — this doc is the event/ledger side; signals are the EMA layer on top.
- **ADR-058 (Inter-workspace coordination)** — the trust-node projection's federation story; the mechanism by which multiple CogOS nodes share one Constellation across hosts.
- **ADR-062 (Recursive node architecture)** — the recursion axis for populations; each node in the Constellation may itself host a Constellation at smaller scale.
- **ADR-074 (Nested sovereignty)** — scope boundaries across populations; determines which edges are allowed to cross which membranes.
- **ADR-081 (Homeostatic kernel loop)** — the running process that maintains the Constellation's coherence; `Receptive → Consolidating` is the reverse-transcription gate on canonical node promotions.

## Next Steps

Two concrete tracks follow from this framing.

**Near-term (design landed, code pending).** The channel-provider RFC at `${COGOS_WORKSPACE:-$HOME/workspaces/cog}/.cog/mem/semantic/designs/channel-provider-interface.cog.md` is the first adoption of this framing at the code level. It treats channels as a new node population in the Constellation, with attendance as the edge type and an attention-EMA as the signal — exactly the shape this doc makes generic. Adopting the RFC and prototyping the first `audio`-kind provider (mod3) validates whether the unified framing holds under implementation pressure.

**Medium-term (convergence).** The larger work is running the trust-node projection's semantics natively in the kernel over the same `constellation.db` the memory-graph uses. Concretely: promote the kernel's session-record surface into a first-class identity-node table (with the columns the trust-node projection specifies); wire the 5-second heartbeat loop into the kernel's process state; teach the memory-graph indexer to emit events into the same `events/{seq:08d}.json` ledger shape the trust-node projection already uses; keep the public `myrgic/constellation` repo as the specification the kernel implements. When that lands, "the Constellation" stops being an architectural claim layered over two implementations and becomes one running system. The bridge interface at `internal/engine/constellation_bridge.go` is the seam where that convergence will happen.

Neither of those tracks is this doc's scope to execute. This doc is scope-setting: naming the substrate, making the populations explicit, making the shared primitives visible, so that future design work inherits the framing rather than re-deriving it each time.
