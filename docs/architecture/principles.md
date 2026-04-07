# Architectural Principles

These are the foundational principles governing CogOS design decisions. They emerge from the system's ontology and inform every component.

## 1. The Information Lives in the Delta

Blobs are state — static, inert. The information is in the difference between one blob and the next.

- The delta is the distinction. The distinction is the quantum.
- The ledger is a chain of deltas — it records only moments where something changed.
- Same hash = no information gained. Different hash = one distinction made.
- The system's knowledge isn't in any one blob. It's in the trajectory — the sequence of deltas.
- The crystal isn't the atoms. It's the lattice — the structure of relationships between atoms.

**Implication:** Maximize the information density of the ledger by recording only state transitions. The total ledger length is a measure of how many distinctions the system has made.

## 2. The CogBlock Is the Quantum of Distinction

The CogBlock is the smallest unit of valid data given the current topological configuration. Its boundaries aren't fixed by a universal schema — they're defined by what constitutes a complete, meaningful, verifiable unit in context.

| Context | Natural quantum |
|---------|----------------|
| Voice conversation | One utterance (speech between silences) |
| Text exchange | One message |
| Code review | One commit |
| Ingestion pipeline | One document |
| BEP replication | One ledger block |

A block is valid when it is:
1. **Complete** — independently meaningful
2. **Hashable** — content-addressed and verifiable
3. **Ledgerable** — appendable to the chain with causal ordering
4. **Replicable** — transmittable via BEP without losing meaning
5. **Topologically appropriate** — granularity matches the context

The protocol knows *how* to hash and transmit a block. The cognitive layer knows *where* to draw the boundary. The block boundary is a cognitive decision, not just a format decision.

## 3. Adaptive Sampling and Bidirectional Surprise

The system minimizes thermodynamic cost by spending attention where it expects to be surprised — and adjusting those expectations based on what it actually finds.

Surprise in both directions is information:
- Expected instability that is stable → something converged (high-value signal)
- Expected stability that is unstable → something broke (high-value signal)
- Confirmation of expectation → low information (expected)

### Mapping to process states

| State | Sample behavior |
|-------|----------------|
| Active | High rate on volatile boundaries |
| Receptive | Medium rate on all boundaries |
| Consolidating | High rate on stable boundaries (verifying the crystal) |
| Dormant | Minimum rate everywhere (heartbeat) |

### Mapping to context zones

| Zone | Expected volatility | Sample behavior |
|------|-------------------|----------------|
| 0 — Nucleus | Very low | Rarely checked. Surprise here is a major event. |
| 1 — Knowledge | Low | Checked on consolidation cycles. |
| 2 — History | Medium | Re-ranked per request. |
| 3 — Current | Very high | Changes every request. Not surprising. |

**Physical intuition:** The stability ordering of the foveated zones follows the same pattern as standing waves in a bounded container — the center is always the calmest point (the node), the boundary is always the most active (the antinode). This isn't a design choice. It's a consequence of wave physics in any bounded oscillating system. The nucleus *must* be the most stable zone because it sits at the node where incoming and outgoing signals cancel.

**Design principle:** Frequency and delta should be inversely correlated. Hot paths should be boring. Interesting stuff can happen on cold paths.

**Physical intuition:** This is the same pattern as pushing someone on a swing — you only push at the moment the returning arc reaches you. Pushing continuously wastes energy. Pushing at resonance amplifies the motion with minimal effort. The system self-tunes its sample rate to the frequency where it learns the most per unit of energy spent.

## 4. Boundary Crossing Energy Signatures

Every crossing of the membrane leaves a distinct energy signature — a ledger entry recording the fingerprint, direction, and magnitude of the distinction.

Each crossing creates a radial wave of secondary distinctions through the substrate:
1. Context engine updates (first ripple)
2. Salience map shifts (second ripple)
3. Next query changes (third ripple)
4. System returns to equilibrium (wave decays)

The area under the spike = total metabolic cost of the crossing.

### The adaptive membrane

The membrane modulates its own permeability based on energy signatures:
- High disruption from last crossing → tighten (more filtering)
- Low disruption → relax (less filtering)
- Series of small crossings → stable exchange pattern
- One massive crossing → metabolize before accepting more

## 5. Stigmergic Coordination (The Cytoplasm Model)

Components coordinate through the substrate, not through direct messaging. This is the organelle/cytoplasm pattern:

- **No direct coupling** — organelles don't import each other
- **Substrate is the bus** — all information passes through the shared medium
- **Discovery over routing** — components find what they need by scanning
- **Small transformation functions** — each organelle does one kind of work
- **Low-rank observables** — entities in the substrate are simple and content-addressed

Adding a new organelle requires zero changes to existing components. It just starts reading from and writing to the substrate.

## 6. Scale Invariance

The same three operations at every scale:

| Operation | What it does |
|-----------|-------------|
| **Fork** | Create a distinction — split one thing into two |
| **Merge** | Resolve a distinction — unify two things into one |
| **Die** | Abandon a distinction — it wasn't worth keeping |

This pattern is fractal:

| Scale | Fork | Merge | Die |
|-------|------|-------|-----|
| CogBlock | Block splits into sub-blocks | Sub-blocks consolidate | Sub-block discarded |
| Thread | `/btw` spawns side thread | Result folds back | Side thread abandoned |
| Agent | Subagent spawned in worktree | Result committed | Worktree deleted |
| Workspace | Branch created | PR merged | Branch deleted |
| Node | New node joins mesh | BEP sync | Node disconnects |

## 7. The Sovereignty Gradient

Data stays local by default. The further from the user's hardware, the more abstracted the data must be:

| Location | Data form | Examples |
|----------|-----------|---------|
| Local device | Raw | Voice audio, personal models, full history |
| Local network | Filtered | Workspace state shared between user's devices |
| Federated node | Abstracted | Synced memory via BEP, stripped of raw signals |
| Cloud API | Maximally abstract | Semantic query only, no personal identifiers |

Every boundary crossing is logged in the ledger. The user can audit exactly what left their system, when, and why.

## 8. Identity Changes by Being Read

The identity card is the DNA — the pattern stored in the nucleus. It is not static:

- Every time the nucleus is included in a context window, that's a transcription event
- The model reads the identity, produces a response, the response gets ledgered
- The ledger informs consolidation, consolidation can update the identity
- The identity changes by being used

The same identity expressed across different workspaces produces different behavior — like the same DNA in different tissue environments (epigenetic expression). The genome is shared; the expression is local.
