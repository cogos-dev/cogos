# Distributed Presence, Learned Boundaries, and Trust

*Design vision document — April 2026*

## The Core Claim

CogOS is not an agent framework. It is an intelligent trust boundary between cognitive systems — human and artificial — that learns the shape of each relationship through use.

## Multi-Device Presence Awareness

A single CogOS node serves as the backend for multiple interfaces simultaneously. A user on their laptop and another user on their phone are both connected to the same kernel, the same attentional field, the same identity-aware substrate.

This means the kernel has cross-channel awareness:

- **Who is speaking** — VAD signals from each channel feed the same attentional field
- **Who is idle** — absence of input is itself an attention signal
- **What channel is appropriate** — voice reply to a speaking user, push notification to an idle one

### The Barge-In Problem, Solved Across Channels

Traditional voice systems solve barge-in detection locally: one mic, one speaker, one threshold. CogOS solves it across distributed devices and multiple users:

- If User A is actively speaking on their laptop, the kernel gates its own voice output on that channel
- If User B's phone is idle and User A asks the kernel to remind User B of something, the kernel routes that as a notification — not a voice interruption
- The kernel makes its own judgment calls about whether a user has finished speaking, whether it's appropriate to respond, and which channel to respond through

### Learned Conversational Boundaries

Every barge-in attempt generates a feedback signal:

- **Successful interruption** — the user accepted it, the timing was right
- **Failed interruption** — the user pushed back ("wait, I'm not done"), the timing was wrong
- **Missed window** — the user waited in silence for a response that didn't come

These signals are identity-keyed. The kernel's model of when to speak to User A evolves independently from its model for User B. Over time, the conversational boundary between each user and the system defines itself through accumulated interaction.

This is what the system spec means by "improves its own selection behavior over time" — applied not just to context retrieval, but to the fundamental rhythm of conversation.

### Portable Interaction Models

Because identity lives in the substrate, a user's learned interaction model travels with them. If User A connects to a different CogOS node (a work node, a friend's node, a public service), their interaction preferences — conversational rhythm, channel preferences, trust boundaries — can be projected into that new context.

The interface shapes itself to the user, not the other way around.

## The Trust Boundary

### Trust as Relationship, Not Configuration

CogOS cannot succeed as a hyper-personal AI interface if trust is implemented as:
- Security theater (audit logs nobody reads)
- Permission gates (approve every action manually)
- Opacity ("trust us, it's encrypted")

Trust must work the way it works between humans: gradually earned through consistent behavior, transparently communicated, and specific to the relationship.

**The right analogy:** Learning to trust a new roommate or coworker.

- At first, you check everything. You watch what they do. You don't leave valuables out.
- Over time, they demonstrate reliability. They do what they say. They respect boundaries. They tell you when something goes wrong.
- Eventually, you're comfortable leaving them alone in the house. Not because you've verified every possible action, but because you've built a model of their behavior that predicts trustworthiness.

CogOS must support this arc. A new user's node should start constrained and become more autonomous as trust is established through demonstrated reliability.

### A Selectively Permeable Boundary

The boundary between CogOS and the systems it mediates is not a wall (fully impermeable) or an open door (fully permeable). It is a **trust boundary** — selectively permeable based on:

- **Identity** — who is making the request
- **Context** — what is the current state of the interaction
- **History** — what has this entity done before, and was it trustworthy
- **Sensitivity** — how personal or consequential is the data being accessed
- **Direction** — inbound requests are filtered differently than outbound actions

Some boundaries are always impermeable:
- Private memory is never shared without explicit consent
- Cryptographic identity cannot be spoofed
- The ledger cannot be retroactively modified

Some boundaries are always permeable:
- Health status is always queryable
- Public capability advertisements are always available
- The user can always inspect what the system is doing

Everything in between is the learned zone — the boundary that becomes more or less permeable based on accumulated trust.

### Trust for AI Systems Too

If CogOS is a universal modality translation layer, it must be trustworthy not just to humans but to the AI systems integrating through it. An AI agent connecting via MCP needs to know:

- That the context it receives is accurate (not hallucinated, not stale)
- That its outputs will be handled faithfully (not silently dropped, not modified)
- That the ledger records what actually happened (not what someone wished happened)

The hash-chained ledger, content-addressed blob store, and coherence validation stack exist precisely for this. Trust is not a feature bolted on top — it is structural, verified at every layer.

## Relationship to Existing Architecture

| Vision Concept | Existing Subsystem | Status |
|---|---|---|
| Cross-channel presence | Modality bus (receptor model) | Planned |
| Barge-in detection | Mod³ VAD | Canonical (local only) |
| Identity-keyed learning | Substrate identity + ledger | Canonical (substrate), Planned (learning) |
| Conversational boundary model | Attention signals as training data | Planned |
| Trust boundary | Workspace membrane (ADR-001) | Canonical (concept), Planned (adaptive) |
| Trust arc | Hash-chained ledger + coherence | Canonical (verification), Exploratory (learned trust) |
| Portable interaction models | FWP / federated workspace | Exploratory |

## The One-Line Version

CogOS is a substrate-centered cognitive workspace that serves as an intelligent, trust-bearing boundary between humans, AI systems, and devices — learning the shape of each relationship through use.
