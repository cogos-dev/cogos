# Cognitive GitOps

## The Third Model

CogOS introduces a repository coordination model distinct from both monorepo and polyrepo:

| Model | Coordination | Intelligence | Coupling |
|-------|-------------|-------------|----------|
| **Monorepo** | Build system (Bazel, Nx) | None — deterministic | Tight (shared build graph) |
| **Polyrepo** | CI/CD pipelines + API contracts | None — rule-based | Loose (contract-bound) |
| **Cognitive** | Workspace substrate | Tunable — 0 to full inference | Stigmergic (substrate-mediated) |

Each component repository functions as an organelle: its own lifecycle, versioning, and release cadence. It trusts that its output is somebody else's input. It trusts that its input is somebody else's output. It does not need to know the full system graph.

## How It Works

1. **Repo A** produces artifacts (binaries, configs, schemas, events)
2. Artifacts enter the substrate as content-addressed blobs
3. **The substrate** absorbs and indexes them
4. **Repo B** discovers relevant artifacts through its own scanning loop
5. Schema compatibility is verified at the substrate layer, not at build time

No external coordinator. No docker-compose. No Helm chart. The workspace IS the coordination layer.

## The Inference Dial

At any boundary in the pipeline, adaptive strength can be tuned from 0 (pure automation) to 1 (full inference):

| Setting | Behavior |
|---------|----------|
| 0.0 | Pure automation — artifacts flow, repos sync, no intelligence |
| 0.2 | Schema validation — substrate checks compatibility |
| 0.4 | Relevance filtering — substrate decides whether a sync matters |
| 0.6 | Impact analysis — substrate predicts downstream effects |
| 0.8 | Active reasoning — substrate recommends actions |
| 1.0 | Full cognitive — substrate initiates changes proactively |

The dial setting can vary per boundary. Kernel builds: 0.0. Cross-workspace sync: 0.8. The right amount of intelligence at each point.

## The Mycelium Model

CogOS doesn't replace existing systems. It connects them — like mycelium connecting trees in a forest.

Each platform (Jenkins, GitHub Actions, GitLab, AWS) is a node in the cognitive mesh:
- Its automation capabilities become node-level capabilities
- Its events become substrate-level signals
- Its outputs become inputs available to any other node
- Its auth/trust model gets bridged through the identity module

**You don't need to migrate.** Install CogOS alongside what you already have. It absorbs existing systems without replacing them. Any event in any system can trigger any action in any other system — through the substrate.

## Current Ecosystem

```
cogos-dev/cogos          → kernel (Go, public)
cogos-dev/constellation  → identity & trust (Go, public)
cogos-dev/mod3           → modality server (Python, public)
```

Each repo stands alone. They don't import each other. The workspace discovers them at runtime through capability scanning — like Kubernetes service discovery, but through content-addressed artifacts and attentional salience instead of DNS and labels.
