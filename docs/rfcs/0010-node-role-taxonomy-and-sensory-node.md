# RFC-0010: Constellation Node-Role Taxonomy & the Sensory-Interface Node

| Field    | Value                                                                                                                                   |
|----------|-----------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                    |
| Author   | @chazmaniandinkle                                                                                                                       |
| Tracking | [#TBD](https://github.com/myrgic/cogos/issues/)                                                                                          |
| Target   | follows the live two-node base (Phase 2, #330); the phone node is phased                                                                 |
| Relates  | ADR-062 (recursive node), ADR-063 (multi-node deployment), ADR-067 (`cog:` URI `@node`), ADR-099 (node-identity layering), [RFC-0007](0007-dispatch-provider-override.md) (dispatch provider override), [RFC-0009](0009-consolidated-mcp-transport.md) (consolidated MCP transport) |

## Summary

The constellation is live with two nodes — **Darkstar** (origin / orchestration) and **Eclipse** (local inference organ) — over an authenticated BEP cluster link (Phase 2: agent-CRD sync + `cog_dispatch_to_harness(target_node=…)`). Both are *fixed thinking-organs* — rooms with compute.

This RFC does two things:

1. **Formalizes a node-role taxonomy** so a node's *function* is a declared, first-class attribute on its identity rather than implicit in its config.
2. **Specifies the third node: a mobile sensory-interface node** (a phone — Gemma-4 E4B reflex inference + biometric/telemetric sensing + the operator's primary interface).

The phone is not "more compute." It is the constellation's first **differentiated organ**: it *senses* (afferent), it is the always-present *I/O membrane*, and it *moves*. Its arrival forces three capabilities the LAN-bound nodes never required — **dynamic/roaming transport, a sensory-stream ingest primitive, and on-device data sovereignty** — and it realizes the **reflex → deliberation → frontier** inference escalation ladder across the three nodes.

Throughout, this is the *same* node primitive (one identity/embodiment model — ADR-099, unified identity). Roles differentiate by **embodiment**, **capability**, and **function** — not by a class hierarchy.

## Background

- Phase 2 (#330) shipped the cross-node substrate: BEP engine, mutual-TLS DeviceID-pinned peering, agent-CRD BlockSync, and remote dispatch. Darkstar↔Eclipse is live and proven (`provider_used: lmstudio` round-trips).
- ADR-062 (recursive node) makes "everything is a node" the structural primitive; ADR-063 (multi-node deployment) names the LAN topology + static peers; ADR-099 layers node identity (L1 mesh crypto / L2 workspace / L3 machine record). Node identities are content-addressed eigenforms.
- **The phone is already the interface — but only the interface.** It reaches the constellation today via two thin-client surfaces (Claude Code mobile; Telegram → Hermes/Cog), both *relaying to Darkstar*. It reaches the constellation; it is not yet a member of it. The always-present I/O membrane axis is therefore already validated; what is missing is local capability, senses, and peer membership.

## Layer 1 — the node-role taxonomy (this RFC)

Add a declared **`role`** facet to the node identity (extends the ADR-099 L3 machine record / node-identity cogdoc). Three roles, descriptive (not exclusive — a node MAY hold more than one):

| Role | Function | Inference posture | Today |
|------|----------|-------------------|-------|
| **orchestration** | conducts, routes, holds the operator's frontier inference; the always-on hub | managed / frontier | Darkstar |
| **inference-organ** | local-sovereign heavy generation; the metabolic powerhouse | local, large | Eclipse (26B) |
| **sensory-interface** | afferent sensing + operator I/O membrane + reflex-tier local inference | local, small (reflex) | the phone (E4B) |

The roles are read by the escalation ladder and routing; they are not a type hierarchy. (Darkstar is orchestration today but could also be an inference-organ; the facet is composable.)

## Layer 1 (cont.) — the escalation ladder

**reflex → deliberation → frontier**, mapped onto node roles:

- **reflex** — the sensory-interface node's small always-warm model (phone E4B): sensory triage, classification, mention-detection, talk-policy, fast local turns.
- **deliberation** — the inference-organ's large local model (Eclipse 26B): real reasoning that should stay local-sovereign.
- **frontier** — the orchestration node's managed/frontier inference (Darkstar): the heavy, the novel, the cross-domain.

A decision starts at the lowest tier that can serve it and escalates when the local tier cannot — the foveal/peripheral inference split expressed as a cross-node routing policy. This generalizes RFC-0007's named-provider routing + the `harness_provider` default (which already lets a node default to its own provider) into a **tier ladder keyed on the `role` facet**.

## Layer 2 — dynamic / roaming transport (the phone forces this)

The phone is **not** always on `192.168.10.x`. Static-peer BEP (ADR-063 / the current `cluster.yaml`) breaks the moment the node leaves the LAN. This is the first node that makes the deferred discovery work load-bearing. Requirements:

- A roaming-capable transport: **Tailscale mesh** (the existing `discovery: tailscale` field) **or** a relay through the orchestration node. Sovereignty trade-off below.
- NAT traversal + reconnect-on-roam (cellular ⇄ Wi-Fi ⇄ away).
- **Identity stable across network changes** — node identity is content-addressed, so a reconnecting node re-pins the same DeviceID regardless of IP. (No re-trust ceremony on every roam.)

## Layer 3 — sensory ingest as a first-class primitive

A new **afferent stream** primitive — *not* dispatch/response. The sensory-interface node continuously ingests biometric / telemetric / sensor data. The on-device reflex model (E4B) **foveates/summarizes locally**, and the substrate ingests the *foveated read*, not raw streams. This extends the foveal context engine from scoring documents to scoring a **live sensory stream** ("what about the operator's current embodied/contextual state is salient right now?").

## Layer 4 — on-device sovereignty (non-negotiable for this node)

Intimate biometric data lives on the most-exposed, most-mobile device. Invariant:

> **Raw sensory data never leaves the node.** The on-device model pre-processes; only a salience signal, a summary, or an explicit operator-approved read crosses the boundary.

This is the topological-hole / sovereignty rule applied to the operator's own physiology, and it is the ethical floor of the personal-cognitive-prosthesis use case (externalized attention / executive-function modulation — in the embodied-cognition lineage of Clark / Hutchins / Engelbart, not a clinical framing). The sensory-interface node is where that prosthesis stops being a thesis and gains *senses*: continuous embodied signal the substrate can attend to and modulate against — kept sovereign.

## Platform — sensory-interface node embodiment

- **Daemon:** confirm `cmd/cogos` cross-compiles to `android/arm64` (CGO-free Go; same path that produced the Windows + darwin builds).
- **On-device inference:** Gemma-4 E4B (~25 t/s on a Pixel 10 Pro Fold) via an on-device runtime (llama.cpp / MediaPipe / equivalent).
- **OS integration:** Android background-service lifecycle + battery (the supervisor analog to Eclipse's Scheduled Task / Darkstar's launchd), health/sensor permissions, the app as the interface surface.
- **Already real:** the interface axis (Code mobile + Telegram → Hermes/Cog). The node turns that thin client into a member with a local capability floor.

## Migration & compatibility

- The `role` facet is additive/optional. Existing nodes default to current behavior; declaring roles is opt-in and changes nothing until the ladder consults it.
- Static-peer transport remains the LAN default; dynamic transport is added for roaming nodes, not imposed on Darkstar/Eclipse.
- Sensory ingest is opt-in per node — only sensory-interface nodes enable it; no other node grows an afferent channel.

## Tests / validation

- Node `role` facet round-trips through the node identity and is queryable.
- Escalation-ladder routing: a request is served at the lowest viable tier; escalates when the local tier cannot serve it.
- Dynamic transport: the phone joins/leaves the mesh and reconnects with a **stable** DeviceID.
- Sovereignty: raw sensory data does **not** cross the node boundary (only foveated reads do) — assertable in test.

## Acceptance criteria

- [ ] `role` taxonomy defined and declared on the Darkstar / Eclipse / phone node identities.
- [ ] Escalation-ladder routing policy specified and minimally wired (reads `role`, picks the lowest viable tier, escalates).
- [ ] Roaming-transport path chosen (Tailscale mesh vs orchestration-node relay) with reconnect-on-roam working and identity stable.
- [ ] Sensory-stream ingest primitive + on-device foveation contract defined.
- [ ] On-device sovereignty invariant enforced and tested.
- [ ] `android/arm64` daemon + on-device E4B validated on the Pixel.

## Open questions

1. **Tailscale dependency vs a substrate-native relay** through the orchestration node? (A third-party mesh vs self-hosted is itself a sovereignty call.)
2. **Sensory-stream schema** — which signals (heart rate, sleep, activity, location, ambient audio, …), at what granularity, under what consent model?
3. Does the escalation ladder live in the **dispatcher (push)** or does each node **decide-and-escalate (pull)**?
4. How does the afferent stream interact with the **foveal context engine's** salience scoring — one unified salience field, or a separate sensory channel?
5. Relationship to the **ambient-attendee use case** (hot mic, diarization, talk-policy, seat): same node, layered on top — does this RFC subsume it or feed it?
