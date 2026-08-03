# Architecture Diagram Source

Reference for generating visual architecture diagrams. All diagrams should follow these principles:

- **No arrows between components** — they coordinate through the substrate, not directly
- **The API Layer boundary is soft-edged** — not a hard rectangle, more like a permeable boundary
- **The substrate is a medium** — texture, dots, or subtle pattern suggesting shared space
- **Three zones are visually distinct** — API Layer (outer), Process Core (center), workspace (inner)

---

## Diagram 1: The Three-Zone Kernel Model (Primary)

The main architecture diagram. Shows CogOS as three concentric zones.

### Zone Classification

Every component belongs to exactly one zone:

| Component | Zone | Why |
|-----------|------|-----|
| Identity Core | Process Core | Defines the node. Loaded once, persists across workspace switches |
| Process Loop (4 states) | Process Core | Always running, workspace-independent |
| MCP Server | API Layer | Mediates between external MCP clients and internal state |
| HTTP API (OpenAI/Anthropic) | API Layer | Translates protocols to internal operations |
| Router | API Layer | Selects providers — node-level, not workspace-specific |
| Coherence Validator | API Layer | Cross-workspace and cross-node integrity checks |
| Context Engine | Workspace | Assembles context from *this workspace's* data |
| Salience Scorer | Workspace | Scores *this workspace's* files |
| Ledger | Workspace | *This workspace's* hash chain |
| Blob Store | Workspace | *This workspace's* content-addressed artifacts |
| Memory (HMD) | Workspace | *This workspace's* semantic/episodic/procedural/reflective sectors |

**Test:** If you switch workspaces, does this component switch too? Yes → workspace. No → Process Core or API Layer.

### ASCII Version

```
                    ╭─── External Systems ───╮
                    │  Claude Code · Cursor   │
                    │  Gemini CLI · MCP       │
                    ╰──────────┬─────────────╯
                               │
        ┏━━━━━━━━━━━━━━━━━━━━━━▼━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
        ┃  API LAYER (semipermeable — mediates inside/outside)     ┃
        ┃                                                          ┃
        ┃    ┌──────┐  ┌──────┐  ┌────────┐  ┌───────────┐       ┃
        ┃    │ MCP  │  │ HTTP │  │ Router │  │ Coherence │       ┃
        ┃    │Server│  │ API  │  │local-  │  │ cross-node│       ┃
        ┃    │      │  │      │  │ first  │  │ validator │       ┃
        ┃    └──────┘  └──────┘  └────────┘  └───────────┘       ┃
        ┃┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┃
        ┃                                                          ┃
        ┃            ╭━━━━━━━━━━━━━━━━━━━━╮                        ┃
        ┃            ┃     PROCESS CORE    ┃                        ┃
        ┃            ┃                     ┃                        ┃
        ┃            ┃  Identity Core      ┃  ← core identity      ┃
        ┃            ┃  Process Loop       ┃  ← always running     ┃
        ┃            ┃  (Active/Receptive/ ┃  ← changes by being   ┃
        ┃            ┃   Consolidating/    ┃     read (adaptive)    ┃
        ┃            ┃   Dormant)          ┃                        ┃
        ┃            ╰━━━━━━━━━━━━━━━━━━━━╯                        ┃
        ┃                                                          ┃
        ┃  ┌ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┐   ┃
        ┃  │  WORKSPACE SUBSTRATE                              │   ┃
        ┃  │                                                   │   ┃
        ┃  │   Context     Salience     Memory                 │   ┃
        ┃  │   Engine      Scorer       (HMD)                  │   ┃
        ┃  │                                                   │   ┃
        ┃  │   Ledger      Blob Store                          │   ┃
        ┃  │   ██████                                          │   ┃
        ┃  │                                                   │   ┃
        ┃  │   · · · · · on-disk state · · · · · · · · · ·    │   ┃
        ┃  │   .cog/mem · .cog/config · .cog/ledger           │   ┃
        ┃  └ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ┘   ┃
        ┃                                                          ┃
        ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
                               │
                    ╭──────────▼─────────────╮
                    │  Inference Providers    │
                    │  Anthropic · Ollama     │
                    │  Claude Code · Codex    │
                    ╰────────────────────────╯
```

### Visual direction for image generation:

- The **API Layer** should look permeable — rounded, soft-edged, with pores/gates where the API components sit
- The **Process Core** is central and visually distinct (darker, denser) — it IS the node's identity
- **Workspace components float freely** in the substrate — NO arrows between them
- The **substrate** should feel like a medium — dots, particles, texture suggesting shared space
- External systems and providers are OUTSIDE the API Layer
- **Three zones should have distinct visual treatment:**
  - API Layer: translucent outer layer
  - Process Core: solid, dense, prominent
  - Workspace: lighter, spacious, with components floating
- Color palette: warm neutrals, Process Core in a distinct accent (blue or deep indigo)

---

## Diagram 2: The Foveated Context Zones

```
    ┌─────────────────────────────────────────┐
    │           Zone 0: CORE                  │  ← always present
    │           identity · self-model          │     never evicted
    ├─────────────────────────────────────────┤
    │           Zone 1: KNOWLEDGE             │  ← shifts slowly
    │           CogDocs · indexed memory       │     high cache hit
    ├─────────────────────────────────────────┤
    │           Zone 2: HISTORY               │  ← scored, evictable
    │           conversation turns             │     relevance + recency
    ├─────────────────────────────────────────┤
    │           Zone 3: CURRENT               │  ← always present
    │           the current message            │
    ├═════════════════════════════════════════┤
    │           [OUTPUT RESERVE]              │  ← reserved for
    │           model generation               │     generation
    └─────────────────────────────────────────┘

    Stable ──────────────────────────── Volatile
    (top stays in KV cache)      (bottom changes per request)

    Design principle: frequency and delta are inversely correlated.
    Hot paths (Zone 0) are boring. Interesting stuff happens on cold paths.
```

---

## Diagram 3: The Scale Invariance (Fractal)

Same three operations at every level: fork, merge, die.

```
    Scale 0: CogBlock
    ┌─────┐ fork ──→ ┌──┐┌──┐  merge ──→ ┌─────┐
    │block│          │b1││b2│             │block'│
    └─────┘          └──┘└──┘             └─────┘

    Scale 1: Conversation Thread
    ┌──────────┐ /btw ──→ ┌──────┐  fold back ──→ ┌──────────┐
    │  main    │          │ side │                 │  main'   │
    │  thread  │          │thread│                 │  thread  │
    └──────────┘          └──────┘                 └──────────┘

    Scale 2: Agent Process
    ┌──────────┐ spawn ──→ ┌────────┐  commit ──→ ┌──────────┐
    │  parent  │           │subagent│             │  parent'  │
    │  process │           │worktree│             │  process  │
    └──────────┘           └────────┘             └──────────┘

    Scale 3: Workspace
    ┌──────────┐ branch ──→ ┌────────┐  PR ──→ ┌──────────┐
    │   main   │            │feature │         │  main'   │
    └──────────┘            └────────┘         └──────────┘

    Scale 4: Node Network
    ┌──────────┐ BEP ──→ ┌────────┐  sync ──→ ┌──────────┐
    │  node A  │         │ node B │           │ coherent │
    └──────────┘         └────────┘           └──────────┘

    The CogBlock is the quantum of distinction at every scale.
    fork = create distinction | merge = resolve distinction | die = not worth keeping
```

---

## Diagram 4: Node vs Workspace Topology

### 4a: Single Node, Single Workspace (Day 1)

```
    ┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
    ┃  Node (laptop, port 6931)                      ┃
    ┃                                                ┃
    ┃  ╭━━━━━━━━━━╮  ← Process Core (shared)        ┃
    ┃  ┃ Identity ┃                                  ┃
    ┃  ┃ Process  ┃                                  ┃
    ┃  ╰━━━━━━━━━━╯                                  ┃
    ┃                                                ┃
    ┃  ┌───────────────────────────────────────────┐  ┃
    ┃  │  Workspace: "my-project"                  │  ┃
    ┃  │  Context · Salience · Ledger · Memory     │  ┃
    ┃  │  .cog/mem  .cog/config  .cog/ledger       │  ┃
    ┃  └───────────────────────────────────────────┘  ┃
    ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
```

### 4b: Single Node, Multiple Workspaces

```
    ┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
    ┃  Node (laptop, port 6931)                                   ┃
    ┃                                                             ┃
    ┃  ╭━━━━━━━━━━━━━━╮                                           ┃
    ┃  ┃   Kernel      ┃   (shared Process Core, shared API Layer)┃
    ┃  ╰━━━━━━━━━━━━━━╯                                           ┃
    ┃       │                    │                                 ┃
    ┃  ┌────▼──────────────┐  ┌──▼───────────────────┐            ┃
    ┃  │ Workspace: "home" │  │ Workspace: "work"    │            ┃
    ┃  │ Identity: Owner   │  │ Identity: Team-Infra  │            ┃
    ┃  │ Memory: personal  │  │ Memory: work docs     │            ┃
    ┃  │ Ledger: ██████    │  │ Ledger: ██████        │            ┃
    ┃  └───────────────────┘  └──────────────────────┘            ┃
    ┃                                                             ┃
    ┃  Isolated: different identity, memory, ledger per workspace ┃
    ┃  Shared: same kernel, same providers, same API Layer        ┃
    ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛
```

### 4c: Multi-Node — Workspace Spanning Nodes via Constellation + BEP

```
    ┏━━━━━━━━━━━━━━━━━━━━━━━┓                 ┏━━━━━━━━━━━━━━━━━━━━━━━┓
    ┃  Node A (laptop)        ┃  Constellation  ┃  Node B (server)        ┃
    ┃                         ┃  ◄──────────►  ┃                         ┃
    ┃  ┌─────────────────┐    ┃   trust +      ┃    ┌─────────────────┐  ┃
    ┃  │ Workspace: "cog"│    ┃   BEP blocks   ┃    │ Workspace: "cog"│  ┃
    ┃  │                 │    ┃                ┃    │                 │  ┃
    ┃  │ Ledger: █1█2█3  │    ┃                ┃    │ Ledger: █1█2█3  │  ┃
    ┃  │ Memory: synced  │    ┃                ┃    │ Memory: synced  │  ┃
    ┃  │ Identity: same  │    ┃                ┃    │ Identity: same  │  ┃
    ┃  └─────────────────┘    ┃                ┃    └─────────────────┘  ┃
    ┗━━━━━━━━━━━━━━━━━━━━━━━┛                 ┗━━━━━━━━━━━━━━━━━━━━━━━┛

    - Constellation Protocol validates identity and trust between nodes
    - BEP replicates ledger blocks (each ledger block IS a BEP block)
    - Same workspace appears on both nodes
    - Changes propagate asynchronously
    - Coherence validates integrity on both sides
```

### 4d: Full Topology (Multi-Node, Mixed Local/Federated)

```
    ┏━━ Node A (laptop) ━━━━━━━━━━━━┓     ┏━━ Node B (server) ━━━━━━━━━━━━┓
    ┃                                ┃     ┃                                ┃
    ┃  ┌──────────┐  ┌──────────┐   ┃     ┃   ┌──────────┐                 ┃
    ┃  │ ws:home  │  │ ws:cog   │◄──╂─BEP─╂──►│ ws:cog   │                 ┃
    ┃  │ (local   │  │ (synced) │   ┃     ┃   │ (synced) │                 ┃
    ┃  │  only)   │  └──────────┘   ┃     ┃   └──────────┘                 ┃
    ┃  └──────────┘                 ┃     ┃                                ┃
    ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛     ┃   ┌──────────┐                 ┃
                                          ┃   │ ws:api   │                 ┃
                                          ┃   │ (local)  │                 ┃
                                          ┃   └──────────┘                 ┃
                                          ┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛

    - ws:home  → Node A only (personal, never synced)
    - ws:cog   → spans both nodes (federated via Constellation + BEP)
    - ws:api   → Node B only (server workload)
    - BEP only syncs explicitly federated workspaces
```

---

## Diagram 5: Ecosystem — The Independent Repos

```
    ┌────────────────────────────────────────────────────────────┐
    │                    myrgic (org)                          │
    │                    Canonical upstream                       │
    │                                                            │
    │   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐   │
    │   │    cogos      │  │constellation │  │     mod3     │   │
    │   │   (kernel)    │  │  (identity   │  │  (modality   │   │
    │   │              │  │   & trust)   │  │   server)    │   │
    │   │  Go daemon   │  │  Go protocol │  │  Python MCP  │   │
    │   │  90 source   │  │  9 source    │  │  TTS engine  │   │
    │   │  33 tests    │  │  4 scenarios │  │  VAD + queue │   │
    │   └──────┬───────┘  └──────┬───────┘  └──────┬───────┘   │
    │          │                 │                 │            │
    └──────────┼─────────────────┼─────────────────┼────────────┘
               │                 │                 │
        ┌──────▼───────┐  ┌─────▼────────┐  ┌─────▼────────┐
        │ your fork    │  │ your fork    │  │ your fork    │
        │ (daily dev)  │  │ (daily dev)  │  │ (daily dev)  │
        └──────────────┘  └──────────────┘  └──────────────┘

    Each repo is independent: independent release, independent deploy.
    Coordination: through the workspace substrate, not through imports.
    Discovery: at runtime via capability scanning, not at build time.
```

---

## Diagram 6: Presence Register (Cross-Channel Awareness)

```
    ┌─────────────────────────────────────────────────────┐
    │              Node Presence Register                   │
    │              (substrate-level state)                  │
    ├────────────┬──────────┬─────────────┬───────────────┤
    │ Channel    │ State    │ Last Active │ Modality      │
    ├────────────┼──────────┼─────────────┼───────────────┤
    │ terminal-1 │ speaking │ 0.2s ago    │ voice (VAD)   │
    │ terminal-2 │ idle     │ 45s ago     │ text          │
    │ phone-app  │ idle     │ 2h ago      │ notification  │
    │ web-dash   │ viewing  │ 3s ago      │ read-only     │
    └────────────┴──────────┴─────────────┴───────────────┘

    Output rules (substrate-level, read by all output channels):

    User speaking on ANY channel → defer voice output on ALL channels
    User idle on target channel  → output permitted
    User active elsewhere        → queue, don't interrupt
    All channels idle > Ns       → dormant state, heartbeat only

    Every output channel reads this register independently.
    No direct coordination between channels. Stigmergic.
```
