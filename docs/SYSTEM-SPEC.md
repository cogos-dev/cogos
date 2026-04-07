# CogOS Multi-Level System Specification

## 0. Purpose

CogOS is a substrate-centered cognitive system — not a chat wrapper, app shell, or single-agent runtime. The workspace is the primary seat of continuity; sessions, models, channels, and interfaces are transient embodiments operating over that substrate. The system ingests perturbations, preserves verified history, builds and serves context, expresses outputs across channels, and improves its own selection behavior over time.

This specification is organized into levels so the same system can be visualized and reasoned about at multiple resolutions.

---

## 1. Ontological Foundation

### The workspace is the cognitive object

The workspace is a persistent cognitive object with continuity that survives changes in harness, model, and channel. It is described through three compatible views:

- **Blob**: the total evolving state object
- **Crystal**: the ordered persistence of accumulated distinctions
- **Furnace**: the metabolic update process that digests perturbations into future structure

### The CogBlock is the quantum of distinction

The CogBlock is the smallest unit of valid data given the current topological configuration — not the smallest possible physical unit of data. A block's boundaries are defined by what constitutes a complete, meaningful, verifiable unit in the context where it exists:

- In a voice conversation, a block might be one utterance
- In a text exchange, one message
- In a code review, one commit
- In an ingestion pipeline, one document

The information lives in the delta between blobs. Blobs are state — static, inert. The meaning is in the difference between one blob and the next. The ledger records only moments where something changed. Same hash = no information gained. Different hash = one distinction made.

### Boundary discipline

Theory can inform naming and intuition, but engineering success must not be mistaken for theory validation. Productive analogies are acceptable; direct parameter transfer from theory into software must be treated cautiously.

---

## 2. Core System Contract

### System role

CogOS is the substrate layer beneath active sessions. In the channel-native framing, Claude Code (or any agent harness) is the active session authority while CogOS provides continuity, memory, verified history, embodiment services, and attention/salience infrastructure.

### Three-zone cell architecture

The system has three zones, like a biological cell:

| Zone | Contains | Function |
|------|----------|----------|
| **Membrane** | MCP Server, HTTP API, Router, Coherence | Mediates between inside and outside. Semipermeable — controls what crosses the boundary. |
| **Nucleus** | Identity Core, Process Loop | Defines the node. Always loaded, always running. The identity changes by being read (epigenetic model). |
| **Workspace** | Context Engine, Salience, Ledger, Memory, Blob Store | The cognitive substrate. Workspace-scoped — switch workspaces and these components operate on different data. |

### Coordination model

Components coordinate through the substrate, not through direct connections. This is stigmergic coordination — organelles modify the shared medium and other organelles react to what they find. No component imports another component's code. Adding a new organelle requires zero changes to existing ones.

### Architectural principle

Adopt open/public protocols where possible. CogOS-specific semantics live at the envelope/substrate layer rather than inventing custom formats at every boundary. The CogBlock envelope serves as the sovereign wrapper around portable content.

---

## 3. Canonical Subsystems

### Substrate and persistence

The substrate provides what transient sessions do not: persistent memory, verified history, cross-session continuity, identity resolution, salience and context construction, and branch-aware workspace continuity.

### Ledger

Append-only, hash-chained (SHA-256, RFC 8785), and complete. The historical record of what occurred. Optimized for append, replay, and integrity verification. Each ledger entry is a CogBlock. Each CogBlock is a BEP block — the same hash, the same content, replicable across nodes.

### Memory (HMD — Hierarchical Memory Domains)

A materialized, indexed, salience-weighted view over accumulated substrate knowledge, organized into four sectors: semantic, episodic, procedural, and reflective. Optimized for retrieval and read-time decay, not historical completeness. Consolidation is the ETL bridge between ledger and memory.

### Context engine

Selects what matters for active work through foveated assembly. Context is arranged into stability zones:

| Zone | Contents | Stability |
|------|----------|-----------|
| 0 — Nucleus | Identity | Always present, never evicted |
| 1 — Knowledge | CogDocs, indexed memory | Shifts slowly, high cache hit rate |
| 2 — History | Conversation turns | Scored by relevance, evictable |
| 3 — Current | The current message | Always present |

Zones are ordered for KV cache reuse. The design principle: frequency and delta are inversely correlated. Hot paths are boring. Interesting stuff happens on cold paths.

### Salience

Git-derived attention scoring. Uses commit frequency, recency, and file topology to score what matters in the workspace right now.

### Coherence

Four-layer validation stack for internal consistency. Operates at both the workspace level and across nodes during BEP synchronization.

### Ingestion

Afferent pipeline that accepts raw external material and converts it into substrate-compatible artifacts. Design philosophy: deterministic first, intelligence second.

### Embodiment / modality

The embodiment layer transduces between raw physical/channel signals and normalized cognitive events. [Mod³](https://github.com/cogos-dev/mod3) handles TTS with adaptive playback and speech queuing. The afferent path (STT, VAD) feeds the same attentional field.

---

## 4. Identity and Trust

### Identity as dynamical property

Identity is coherence with history — not a static credential. The [Constellation Protocol](https://github.com/cogos-dev/constellation) implements this:

- **Publicly verifiable**: any peer can check a node's state in O(1)
- **Privately irreproducible**: cannot be forged without the full hash chain
- **Temporally coupled**: trust derives from consistent behavior over time

Stolen keys cannot impersonate because trust is coupled to history.

### The trust membrane

The boundary between CogOS and external systems is a semiconductive membrane — selectively permeable based on identity, context, history, sensitivity, and direction. Some boundaries are always impermeable (private memory, cryptographic identity, ledger integrity). Some are always permeable (health status, capability advertisements). Everything in between is the learned zone.

### Sovereignty gradient

Data stays local by default. Only abstracted queries cross the membrane to remote servers, and every crossing is recorded in the ledger. Raw audio, personal models, conversation history, and memory never leave the user's hardware unless explicitly configured.

---

## 5. Runtime and Deployment

### Continuous process

The kernel runs as a continuous daemon with four states:

| State | Behavior |
|-------|----------|
| **Active** | Processing an external perturbation |
| **Receptive** | Idle, maintaining the attentional field |
| **Consolidating** | Internal maintenance (memory, coherence, salience) |
| **Dormant** | Minimal activity, heartbeat only |

The process loop isn't optional — it's constitutive. Like a microwave turntable that rotates food to prevent cold spots, the process loop cycles through the cognitive field to ensure no part of the workspace sits at a permanent dead zone. Without the loop, parts of the substrate would receive no attention and go stale. The rotation ensures every part of the workspace periodically receives active processing.

The process states map to adaptive sample rates — the system spends attention where it expects to find information.

### Node vs workspace

- **Node** = the daemon process + its membrane (one per machine)
- **Workspace** = the cognitive state (memory, identity, ledger, config)
- A node can host multiple workspaces
- A workspace can span multiple nodes via BEP + Constellation

### Ecosystem

Each subsystem is its own repo, its own release cycle, its own organelle:

| Repo | Role |
|------|------|
| [cogos-dev/cogos](https://github.com/cogos-dev/cogos) | Kernel — continuous process daemon |
| [cogos-dev/constellation](https://github.com/cogos-dev/constellation) | Identity & trust protocol |
| [cogos-dev/mod3](https://github.com/cogos-dev/mod3) | Modality server (TTS, VAD, speech queuing) |

Organelles coordinate through the substrate, not through imports. Discovery happens at runtime through capability scanning.

### Cognitive GitOps

CogOS introduces a third repo coordination model beyond monorepo and polyrepo. Each component repo is an organelle — it trusts that its output is somebody else's input and its input is somebody else's output. The workspace substrate is the coordination layer. An inference dial tunes how much intelligence is applied at any boundary, from 0 (pure automation) to 1 (full cognitive reasoning).

---

## 6. Foundational Principles

### The information lives in the delta

Blobs are state. The information is in the difference between one blob and the next. The ledger records only state transitions, never steady state.

### Adaptive sampling and bidirectional surprise

The system minimizes thermodynamic cost by spending attention where it expects to be surprised — and adjusting those expectations based on what it actually finds. Surprise in both directions is information: unexpected stability is as informative as unexpected instability.

### Boundary crossing energy signatures

Every crossing of the membrane leaves a distinct energy signature in the ledger. The crossing creates a radial wave of secondary distinctions that propagates through the substrate. The membrane learns to modulate its own permeability from the energy signatures of previous crossings.

### Scale invariance

The same three operations at every scale: fork (create distinction), merge (resolve distinction), die (distinction wasn't worth keeping). From CogBlocks to conversation threads to agent processes to workspaces to node networks — the pattern is fractal.

---

## 7. Status Model

Every subsystem should be tagged with one of four statuses:

| Status | Meaning |
|--------|---------|
| **Canonical** | Current architectural source of truth |
| **Planned** | Near-term build target, aligned with canonical design |
| **Exploratory** | Valuable research direction, not a present constraint |
| **Superseded** | Historically informative, no longer primary |

---

## 8. The One-Line Version

CogOS is a substrate-centered cognitive workspace that metabolizes external perturbations into structured, verified, retrievable state; serves context to active sessions across channels and modalities; preserves integrity and continuity beyond any one model invocation; and improves its own selection behavior through use.
