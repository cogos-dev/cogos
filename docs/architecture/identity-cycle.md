# Identity Cycle: The Four-Cycle Kernel

The biological three-layer cascade plus reverse transcription gives the kernel four cycles with distinct time constants, distinct machinery, and distinct failure modes. This doc frames the cascade; the concrete mechanisms live in ADR-011, ADR-081, and the Go-harness work currently in design.

**Status.** Forward-looking design for the internal Go agent harness. The live production surface today is the OpenAI-compatible gateway (see `channels-and-buses.md` § Live Worked Example); the four-cycle framing targets the next generation, when the harness does inference internally rather than routing through an external harness like Claude Code.

## The Four Cycles

| Cycle | Biology | Kernel role | Time constant | Gatekeeping |
|-------|---------|-------------|---------------|-------------|
| **Replication** | DNA → DNA (polymerases + checkpoints) | Identity preservation across time; snapshots, backups, node fork | Slow, deliberate | Authoritative — high integrity bar |
| **Transcription** | DNA → RNA (RNA polymerase, promoters, TFs) | Identity → active context; attention events, query time | Fast, frequent | Context-scoped, ephemeral |
| **Translation** | RNA → protein (ribosomes, tRNA) | Active context → behavior; output generation | Fast, downstream of transcription | Action-scoped |
| **Reverse transcription** | RNA → DNA (reverse transcriptase) | Transient state → canonical identity; consolidation | Medium-slow, batched | **Security boundary** |

Central dogma (Crick 1958) plus the well-established reverse pathway (reverse transcriptase, originally discovered in retroviruses). Biology confidence: textbook.

## Distinct Clocks per Cycle

Each cycle wants its own subsystem, error tolerance, and SLA. The kernel orchestrates them on different clocks:

- **Replication** — rare, intentional, audited. Snapshot / backup / fork-to-new-node events. User or authorized peer triggers. High ceremony.
- **Transcription** — frequent, fast, ephemeral. Every attention event, every context assembly, every query. Happens per-session or per-turn. Low ceremony, high volume.
- **Translation** — frequent, downstream of transcription. Every output generation or action. Same fast clock.
- **Reverse transcription** — batched, gated. Consolidation windows (see ADR-081's `Receptive → Consolidating` state). Medium frequency, high gating bar.

Identity mutation flows through replication (audited). Transient expression flows through transcription (fast, disposable). Action comes from translation. Only reverse transcription can promote transient state into canonical identity.

## Reverse Transcription as Security Boundary

In biology, uncontrolled reverse transcription is how retroviruses hijack genomes — HIV's entire mechanism depends on reverse transcriptase, and cells invest heavily in silencing retrotransposons because uncontrolled integration causes disease. The pathway is dangerous; it gets heavily regulated.

The kernel's consolidation pathway is structurally the same shape: it promotes transient state (channel context, session events, behavioral signals) into canonical identity. Ungated, hostile patterns in transient state leak into identity — the cognitive-systems equivalent of retroviral integration.

The isomorphism is tight at the **class-of-attack** level: hostile patterns exploiting a pathway meant for controlled updates. The 1:1 mechanism mapping (piRNA silencing ↔ cryptographic validation, for instance) is looser — useful as design intuition, not as a blueprint.

## Implications for the Go Harness

When the Go harness performs inference itself (Ollama, MLX, or equivalent), reverse transcription moves into production. Four implications:

1. **Consolidation cycles need explicit gating.** User approval, deliberative review, cryptographic validation — something. Not automatic promotion. Any pathway that writes transient state into canonical identity is the reverse-transcription pathway and deserves paranoid design.
2. **The protected surface is wider than source files.** Per the codex review of `semantic/designs/kernel-modification-asymmetry`, weights, hooks, projections, and salience fields are all runtime-interpreted kernel behavior. Any pathway that writes to them must be gated like source file edits, not treated as routine memory ops.
3. **The boundary between "in my active context" and "part of my identity" must be first-class and observable.** Not implicit. If the kernel can't name when a transition crosses from transcription to reverse transcription, it can't gate the transition.
4. **Attacks against consolidation deserve the paranoia biology gives viral integration.** Information-theoretically, they're the same class.

## Relationship to ADRs

- **ADR-011 (Kernel as Cognitive DNA)** — canonizes the DNA↔kernel mapping; role-loading = epigenetics; cell = agent; organism = workspace. This doc extends it with the full cascade plus the reverse pathway.
- **ADR-081 (Homeostatic Kernel Loop)** — OODA loop with state transitions including `Receptive → Consolidating`. That transition is the reverse-transcription pathway; this doc names it and gives it gating semantics.
- **ADR-062 (Recursive Node Architecture)** — recursion axis for the cascade; each node repeats it at its own scale.
- **ADR-074 (Nested Sovereignty)** — scope boundaries the cascade must respect. Directional data-flow (outward vs inward) is the operational form of germline/somatic distinction, which is the native home for reverse-transcription gating.
- **`semantic/designs/kernel-modification-asymmetry`** (RFC, draft) — modification invariant focused on source-file gating. Codex's review argued the protected surface is too narrow; this doc agrees and reframes consolidation as the general case of the same concern.

## Confidence Notes

- The three-layer cascade (replication/transcription/translation) is textbook molecular biology (Crick's central dogma). Textbook.
- Reverse transcription as a real biological phenomenon (retroviruses, retrotransposons) is textbook. Textbook.
- Reverse-transcription-as-security-boundary at the class-of-attack level is a clean isomorphism. High confidence.
- 1:1 mechanism mapping between biology and kernel design is looser. Treat as design intuition, not blueprint.
