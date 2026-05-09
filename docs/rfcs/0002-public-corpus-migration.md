# RFC-0002: Public ADR / RFC Corpus and Mirror Workflow

| Field         | Value                                              |
|---------------|----------------------------------------------------|
| Status        | Draft                                              |
| Author        | @chazmaniandinkle                                  |
| Tracking      | (no issue)                                         |
| Target        | `v0.5.0`                                           |
| Supersedes    | none                                               |
| Mirrors       | private RFC-006 (Public-Class ADR Corpus and Mirror Workflow) |

## Summary

There is a substantial private design corpus — 89 ADRs and 25 RFCs in `~/workspaces/cog/.cog/` — that predates this repository's first public RFC and that the public source code already cites by ID. None of those documents have been ported to the public repo. Twenty-two ADRs and three RFCs are referenced from `.go`, `.md`, and `.yaml` files in this tree and resolve to nothing.

This RFC is the canonical mirror-workflow doc. It does three things:

1. **Inventory** of what exists privately, indexed by status, citation count in public code, and transitive-closure depth from cited seeds.
2. **Migration plan** in four waves, leaves first, ordered by what unblocks self-consistency.
3. **Workflow going forward** — how new design decisions get authored, when they get mirrored, what the PR conventions are.

It is the public-corpus mirror of private RFC-006 (Public-Class ADR Corpus and Mirror Workflow), adapted to the realities of this repository: a fresh public ADR/RFC sequence (`0001`, `0002`, ...) starting now, mandatory `mirrors:` frontmatter linking back to the private source, and `docs/adrs/` + `docs/rfcs/` as the home for everything.

## Relationship to PR #141

[PR #141](https://github.com/cogos-dev/cogos/pull/141) opened the same day as this RFC and introduces RFC-0001 (Retire the root `package main`). It was drafted under the assumption that no prior corpus existed; that turned out to be wrong. The reconciliation:

- **PR #141 stays as RFC-0001.** Its content is correct and substantial. The `0001`-numbered public RFC is the right slot for it.
- **The "ADR-085" references in past commit messages are NOT this RFC.** They refer to private ADR-085 (CogOS Kernel Subpackage Decomposition), which is the load-bearing source document for PR #141's rules. Private ADR-085 has 5 explicit citations across this repo's code today.
- **A small amendment to PR #141 is recommended**, not required for merge: a one-paragraph "Relationship to private corpus" section noting that RFC-0001 is the public mirror of private ADR-085 and that this RFC (RFC-0002) is the corpus-wide mirror plan. If PR #141 lands first without that note, RFC-0002 names the relationship retrospectively.
- **No re-numbering.** RFC-0001 keeps its slot. This RFC is RFC-0002.

The forcing function created by PR #141 — "the first public RFC" — is the right moment to land the workflow that governs all subsequent public RFCs and ADRs. This document fills that gap.

## Inventory

### Counts

| Corpus | Location | Files | Status breakdown |
|---|---|---|---|
| Private ADRs | `~/workspaces/cog/.cog/adr/` | 89 | accepted: 56, proposed: 29, superseded: 4 |
| Private RFCs | `~/workspaces/cog/.cog/conf/spec/rfc/` | 25 | draft: 25 (all) |
| Public ADRs (today) | `docs/adrs/` (does not exist) | 0 | — |
| Public RFCs (today) | `docs/rfcs/` (PR #141 creates) | 0 → 1 | RFC-0001 draft |

ADR numbering is contiguous 001–089. RFC numbering has gaps: 1–22 plus 30, 31. All 25 RFCs are still in `draft` status — none has been formally accepted, which means the public corpus is mirroring decisions that are themselves unsettled. That distinction is preserved in `mirrors:` frontmatter (see Workflow).

### Public-code citations

A grep across `.go`, `.md`, `.yaml`, `.yml` in this repo for `(ADR|RFC)-NNN` patterns returns 22 unique ADRs and 3 unique RFCs (excluding RFC-8785, which is the IETF JCS RFC and unrelated). Five of the 22 cited ADRs are still in `proposed` status privately, and all 3 cited RFCs are still `draft`:

| Cited (private status ≠ accepted) | Status | Public citations |
|---|---|---|
| ADR-082 Mod3 Session-Aware Communication Bus | proposed | 15 |
| ADR-081 Homeostatic Kernel Loop | proposed | 9 |
| ADR-085 CogOS Kernel Subpackage Decomposition | proposed | 5 |
| ADR-062 Recursive Node Architecture | proposed | 4 |
| ADR-063 Multi-Node Deployment | proposed | 3 |
| ADR-060 Distributed Workspace OS — BEP Sync | proposed | 2 |
| ADR-040 Literate Cogdocs | proposed | 1 |
| RFC-017 Virtualized Harness Writes via Per-Dispatch Worktree | draft | 2 |
| RFC-004 ADR In-Review Status and Reshape Workflow | draft | 1 |
| RFC-003 Claude Code Hooks as Channel-Provider Membrane | draft | 1 |

This is the most surprising finding. The public source code cites a substantial number of design decisions that have not been formally ratified privately. The migration must preserve that nuance — either by mirroring the private status verbatim (so a public reader sees `status: proposed (mirrored from private)`), or by waiting on private ratification before mirroring. The recommendation below picks the first option; rationale in Workflow §Frontmatter.

### Wave assignment

Each private document is assigned to a wave by transitive-closure depth from public-code citations:

- **Wave 1** (25 docs): cited directly in `.go`/`.md`/`.yaml` source.
- **Wave 2** (60 docs): not cited in source, but referenced in a Wave-1 document's `refs:` block, transitively.
- **Wave 3** (28 docs): everything else.

Full tables in [Appendix A](#appendix-a-full-inventory).

## Decisions

These are the load-bearing choices the migration plan rests on. Each is a recommendation with rationale; the user reviewing this PR may override before accepting.

### D1. Numbering: fresh public sequence

The public corpus uses zero-padded numbering starting from `0001`. **Public IDs are NOT inherited from private IDs.** Public RFC-0001 (PR #141) is the public mirror of private ADR-085; the next public mirror gets RFC-0002 or ADR-0001, regardless of the private number it points at. Tables in `mirrors:` frontmatter and a `MIRRORS.md` index handle the mapping.

Three options were considered:

1. **Continue private numbering** (next public RFC would be RFC-026 or RFC-032 to avoid the gap). Rejected: leaks the private corpus's iteration history into the public surface, makes "what's the next number" depend on a private artifact, forces the public corpus to inherit private gaps, and conflicts with PR #141's explicit choice of `0001`.
2. **Renumber on port** (port-order = public-order, fresh `0001/0002/0003`). Recommended. Each public document is a curated artifact that earns its number when it lands; pre-allocated numbers create dead slots if a port is deferred. Matches the IETF / W3C / Rust-RFCs convention of fresh public sequence, which RFC-006 (private) already cites as precedent.
3. **Public/private offset** (e.g., RFC-100+ for public-original, port keeps original number). Rejected: split the namespace asymmetrically and adds a third number to keep track of, with no obvious benefit over a clean fresh sequence.

The downside of option 2 is the mapping table needs maintenance. That's cheap; one row per ported document, and `mirrors:` frontmatter encodes it directly on the public document so the table is a derived view.

### D2. Location: `docs/adrs/` and `docs/rfcs/`

Public RFCs go in `docs/rfcs/` (matching PR #141's choice). Public ADRs go in `docs/adrs/` (plural, matching the RFCs convention — chosen for symmetry, not because plural is unambiguously better than singular). A `docs/adrs/MIRRORS.md` and `docs/rfcs/MIRRORS.md` index file is the discovery surface; it lists each public document with title, status, version, and the private source it mirrors.

Alternatives considered:

- **`docs/architecture/`** (existing prose-style architecture docs). Rejected: existing docs there (`principles.md`, `the-constellation.md`, `channels-and-buses.md`) are reader-facing prose, not lifecycle-bound decisions. Mixing the two corpora confuses both. ADRs/RFCs link OUT to architecture docs as background; they do not replace them.
- **Separate repo** (`cogos-dev/adrs`). Rejected for now: adds an org-level repository for content that's not yet voluminous enough to warrant one, and breaks the "one repo, one CONTRIBUTING.md" convention. RFC-007 (private) imagines a future org-level substrate; if that lands later, this content moves with it.
- **Singular `docs/rfc/` + `docs/adr/`**. Rejected: PR #141 already uses `docs/rfcs/`, and rebasing the convention out from under it just to satisfy taste creates churn. Match what's landing.

### D3. Frontmatter: minimal YAML, mirror-aware

Public documents use plain `.md` (not `.cog.md`). The `.cog.md` extension is private workspace tooling convention; the public repo has no CogOS-specific Markdown processor and exposing one would conflate documentation with substrate.

Frontmatter shape:

```yaml
---
id: RFC-0002          # or ADR-0001 etc.
title: Public ADR / RFC Corpus and Mirror Workflow
status: draft         # draft | accepted | superseded | rejected
created: 2026-05-01
authors: [chazmaniandinkle]
mirrors:              # OPTIONAL; required only when this is a port
  - source: cog://rfc/006
    rel: primary-source
    private_status: draft
    note: |
      Public adaptation of private RFC-006. Public-corpus-specific decisions
      (numbering, location, file extension, mirrors-field shape) are
      inlined in §Decisions.
supersedes: []        # OPTIONAL; list of public IDs (e.g., [RFC-0005])
---
```

The `mirrors:` field replaces RFC-006's `mirrors:` schema with one adjusted for public consumption:

- **`source:`** uses the `cog://` URI form. Public readers will not be able to resolve it; that's fine. The URI is informational, identifying which private document the public version derives from. A future `MIRRORS.md` lookup can render it more usefully.
- **`rel:`** values: `primary-source`, `secondary-source`, `partial-source`, `supersedes-private`. Same semantics as RFC-006.
- **`private_status:`** captures the snapshot of the private status at port time. Required when `rel: primary-source`. This is how a public reader sees that ADR-082 (`proposed` privately) was mirrored as a public ADR despite being unratified — the field is the disclosure.
- **`note:`** captures any port-specific divergence. Required when public content materially differs from private (e.g., when a private ADR's body is split into two public ADRs, or when private references to internal-only memory are stripped).

`refs:` in private documents is NOT preserved verbatim. References to other private documents become references to their public mirrors (or are stripped if no public mirror exists). References to `cog://mem/...` paths are stripped — those are private memory cogdocs that will not have public equivalents. References to public artifacts (commit SHAs, file paths in this repo) are preserved.

### D4. Public-private status divergence

A document can be `accepted` privately and `draft` publicly while review is in flight. A document can also be `proposed` privately (unratified) but already cited in public source (the most surprising finding above), and the public mirror discloses both states. The conventions:

- **Public status starts at `draft`.** Mirror authoring is not auto-promotion. Even a private `accepted` ADR ports as `draft` and goes through public review before promoting to `accepted`.
- **`private_status:` field discloses the source state.** A reader sees both: public lifecycle, and the snapshot of private state at port time.
- **Re-syncing private status changes is manual and infrequent.** When a private `proposed` ADR is ratified to `accepted`, the public mirror's `private_status:` field gets a one-line update; the public status moves only if there's a corresponding public review.
- **No automatic enforcement.** RFC-005 (private)'s `implementation:` field is a candidate for future adoption to add structural rigor, but is out of scope here.

### D5. What stays private indefinitely

Not every private document should migrate. Marked OUT in the inventory:

- **The TEMPLATE** (`TEMPLATE.cog.md`) — replaced by a public template at `docs/rfcs/TEMPLATE.md` and `docs/adrs/TEMPLATE.md`.
- **Iterations on private workspace ergonomics** — RFC-019 (Substrate Orientation Skill), RFC-020 (Stale-Index Hygiene), RFC-002 (CogDoc Templating). These describe private tooling that has no public counterpart. Stay private unless the tooling itself becomes public.
- **Private-only roadmap docs** — RFC-001 (Zero-One Cosmology Workspace Refactoring) explicitly discusses the private workspace's structure. Stays private.
- **Archived / superseded ADRs that were never widely cited** — case-by-case during Wave 3.

The default is to migrate. The above are exceptions; each will be marked explicitly in the inventory commit when its wave reaches it. Roughly 5–10 documents are expected to stay private out of 113.

### D6. Versioning and supersession

Public IDs are stable. A material change to a public document either:

- bumps a `version:` field (additive change, status remains `accepted`), or
- creates a new public document with the next sequential ID and marks the old as `superseded` with `supersedes:` / `superseded_by:` cross-links.

This matches RFC-006's recommendation and the IETF convention. The patch/minor/major distinction from RFC-006 is overkill for the volume of public traffic expected; a simple linear `version:` integer is enough. Revisit if external citation volume grows enough to warrant semver.

## Migration plan

Four waves. Each wave has explicit goals, ordering rationale, PR shape, and a verification gate.

### Wave 0: Workflow infrastructure (this PR + 2 follow-ups)

**Goal:** establish the minimum infrastructure that lets the rest of the migration land cleanly.

**Documents to land:**

1. **RFC-0002 (this document).** The plan and the workflow.
2. **`docs/rfcs/TEMPLATE.md` and `docs/adrs/TEMPLATE.md`.** Frontmatter schema + section structure. Derived from RFC-0002 §D3.
3. **`docs/adrs/MIRRORS.md` and `docs/rfcs/MIRRORS.md`.** Initially: just the column headers and the rows for RFC-0001 (PR #141 → private ADR-085) and RFC-0002 (this RFC → private RFC-006). Grows row-by-row as Wave 1 lands.
4. **CONTRIBUTING.md amendment.** Currently cites private RFC-004 as the workflow. Replace with a pointer to RFC-0002. Add a paragraph explaining the public/private distinction.

**Recommended PR shape:**

- This RFC ships in PR #N (this PR, draft). Items 2/3/4 land in a follow-up PR after this one merges. Reason: RFC-0002 is itself a substantial doc that benefits from a focused review, and the templates depend on RFC-0002 being merged.

**Verification:** RFC-0002 merged. CONTRIBUTING.md contains an authoritative pointer to the workflow. Templates are reachable from the README or CONTRIBUTING.md.

### Wave 1: Forcing-function content (25 docs, ~6–10 PRs)

**Goal:** every ADR/RFC cited in `.go`/`.md`/`.yaml` source has a public mirror. Removes the dangling-citation problem that motivated RFC-0002.

**Documents:** the 25 in [Appendix A.1](#a1-wave-1-cited-in-public-source).

**Ordering rationale (within Wave 1):**

Wave 1 documents have internal `refs:` between them. Port leaves first (no Wave-1 refs) so that later ports can cross-link to already-public siblings. The leaf-first ordering, derived from the cross-reference graph:

```
Sub-wave 1a (no refs to other Wave-1 docs, or only refs to docs in 1a):
  ADR-001, ADR-007, ADR-018, ADR-021, ADR-033, ADR-040,
  ADR-046, ADR-052, ADR-058, ADR-060

Sub-wave 1b (refs into 1a only):
  ADR-011, ADR-059, ADR-061, ADR-072, ADR-074, ADR-083

Sub-wave 1c (refs into 1a/1b):
  ADR-062, ADR-063, ADR-081, ADR-082, ADR-084, ADR-085

Sub-wave 1d (RFCs — depend on Wave-1 ADRs):
  RFC-003, RFC-004, RFC-017
```

**Recommended PR shape:**

- **One PR per sub-wave**, batching ports that belong together. Each PR adds the documents AND the corresponding rows in `MIRRORS.md`. Sub-wave 1a is up to ~10 short documents; later sub-waves shrink as the work concentrates on fewer, more interconnected docs.
- Don't try one-PR-per-doc: most ports are short (200–600 line markdown files with light edits). 25 PRs is review-fatigue without proportional benefit.
- Don't try a single mega-PR: if anything breaks (broken cross-link, frontmatter error), the entire batch needs revision. Sub-wave granularity gives a sensible green-state cadence.

**Verification per PR:**

- New documents render correctly in GitHub Markdown (frontmatter parses, tables align).
- All `mirrors:` `source:` URIs match an existing private file (verified at port time, not enforced after).
- All inter-public refs resolve (no `[RFC-NNN](./missing.md)` links).
- `MIRRORS.md` updated.

**Wave-level verification:** every ADR/RFC ID cited in the public source has a corresponding `docs/adrs/`-or-`docs/rfcs/` entry that resolves. Add a CI check (out of scope here, follow-up): grep source for `(ADR|RFC)-[0-9]+` and verify each has a `MIRRORS.md` entry.

### Wave 2: Transitive closure (60 docs, ~10–15 PRs)

**Goal:** every public document's `mirrors:` and inter-public `refs:` resolve to a published peer. No reader hits a "private only" pointer in a Wave-1 document and dead-ends.

**Documents:** the 60 in [Appendix A.2](#a2-wave-2-transitive-closure). These are referenced from a Wave-1 document but not themselves cited in source.

**Ordering rationale:** by transitive-closure depth. Leaves first (a doc with no refs to anything in Wave 2 ports first); roots last. The cross-reference graph has more structure than Wave 1 — about half of these are foundational early ADRs (001–030 era) that the later Wave-1 docs build on. Those will tend to land first.

**Recommended PR shape:** batched by topical cluster, not by sub-wave. Examples of natural clusters from the inventory:

- **Foundation** (ADR-002, 003, 004, 010, 027): kernel interface, cogdoc format, URI scheme, agent workspace, RFC process. Logically ports together.
- **UCP / context stack** (ADR-034, 043, 067, 071, RFC-014): the context protocol family.
- **Federation / nodes** (ADR-038, 039, 044, 056, 075, 077): the multi-node and federation thread.
- **Reconcile family** (RFC-008, 009, 010, 015, 016): the reconcile machinery.

Each cluster is one PR. Lower commitment to cluster boundaries — port what reviews well together.

**Verification:** same as Wave 1, plus: after every Wave-2 PR, no Wave-1 document contains an unresolved private-only `refs:` entry (because the dependency it pointed at now has a public mirror).

### Wave 3: On-demand (~28 docs, indefinite)

**Goal:** port a document only when something in this repo's code or docs newly cites it.

**Documents:** the 28 in [Appendix A.3](#a3-wave-3-on-demand). These are private documents not cited from public source and not referenced by any Wave-1/2 mirror's `refs:`.

**Recommended PR shape:** one PR per port, triggered by a code or doc PR that newly references the private ADR/RFC. The triggering PR's body links to the port PR ("This change references private ADR-066; ported in #M"). This is the steady-state shape going forward (see Workflow).

**No bulk port.** A document that nobody cites and that nothing depends on does not need to be public. Wave 3 is open-ended; some documents may never port. That's acceptable.

### Out of migration

Approximately 5–10 documents stay private indefinitely per D5. Specific candidates (verified at port-decision time):

- `TEMPLATE.cog.md` (replaced by public template).
- RFC-001 (private workspace structure).
- RFC-002 (CogDoc templating, private tooling).
- RFC-019 (Substrate Orientation Skill, private tooling).
- RFC-020 (Stale-Index Hygiene, private tooling).
- A handful of Wave-3 docs that turn out to be workspace-internal on inspection.

Each will be flagged in the migration commit that decides it.

## Workflow

The third role of this document. Once Wave 0 lands, this section governs new design decisions and ports going forward.

### Authoring a new public ADR or RFC (no private precursor)

1. **Decide the type.** RFC = open question, gathering input. ADR = decision recorded, going into effect. CONTRIBUTING.md already has the rule: "ADRs document decisions that have been made; RFCs gather input on decisions that are still open."
2. **Allocate the next ID.** Look at `docs/rfcs/MIRRORS.md` (or `docs/adrs/MIRRORS.md`) and pick the next zero-padded sequential. RFCs and ADRs are independent sequences.
3. **Copy the template** to `docs/rfcs/NNNN-slug.md` (or `docs/adrs/NNNN-slug.md`).
4. **Fill in frontmatter.** Omit `mirrors:` (no private precursor). Set `status: draft`.
5. **Open a PR.** Title: `docs(rfc): RFC-NNNN: <one-line summary>`. Body explains the why, links any tracking issue.
6. **Review and merge.** Status flips to `accepted` either in the merging PR or in a follow-up.
7. **`MIRRORS.md` entry added in the same PR.**

### Mirroring a private precursor (Wave 1/2/3 work)

1. **Confirm wave.** Wave 1 is bulk migration. Wave 3 is on-demand: only port a document when something new cites it.
2. **Read the private source** (`~/workspaces/cog/.cog/adr/NNN-...cog.md` or RFC). Note the private status at this snapshot.
3. **Allocate the next public ID.**
4. **Copy and adapt.** Public document is a derivative, not a verbatim copy. Adaptations:
   - Strip iteration history sections (Discussion Log, Simmer criteria, anything labeled "private" or that references private memory cogdocs).
   - Rewrite in clean prose suitable for external readers — no internal vocabulary that hasn't been grounded.
   - Replace `cog://mem/...` references with explicit citations of public artifacts where possible; strip them otherwise.
   - Replace `cog://adr/NNN` and `cog://rfc/NNN` references with `[ADR-NNNN](./NNNN-slug.md)` links if the target has been ported, or remove the link entirely if not.
   - Keep `mirrors:` frontmatter pointing at the private source via its `cog://` URI. Set `private_status:` to the snapshot.
5. **Open a PR.** Title: `docs(rfc): RFC-NNNN: <title> (mirror of private RFC-NNN)` or similar.
6. **Review.** Reviewers check for: clarity to external reader, no leaked private vocabulary, accurate `mirrors:` linkage, no dangling refs.
7. **`MIRRORS.md` updated in the same PR.**

### Updating a public document when its private source changes

The private source is the canonical authority. When it changes materially (status promotion, body edit, supersession), the public mirror MAY be updated, but is not required to follow lockstep.

- **Status sync:** the public `private_status:` field updates in a small PR when the private status promotes. The public `status:` does NOT move automatically — that's a separate review.
- **Body sync:** if the private source's body changes meaningfully, a maintainer judges whether the change is significant enough to push to the public mirror. Most clarifications and typo fixes do not warrant a public update. Material decisions — additions, retractions, scope changes — do.
- **Supersession:** if the private source is superseded, the public mirror's `mirrors:` `note:` field captures that the private successor exists. The public mirror's own status moves to `superseded` only if the public corpus has agreed to follow.

### Promoting a public document from `draft` to `accepted`

PR review and merge is the ratification mechanism. The PR description should call out which acceptance criteria are met. For mirrors of private accepted ADRs, the bar is "the mirror is faithful and clear" — the decision was already ratified privately. For original public RFCs (no private precursor), the bar is the standard CogOS RFC bar (problem, alternatives, consequences, examples).

### When to NOT port

Three cases where a private document should explicitly not port:

1. **It's purely about private workspace tooling.** RFC-002 (CogDoc Templating), RFC-019 (Substrate Orientation Skill), and similar describe artifacts that have no public counterpart. Mirroring them creates a public document about a thing the public reader cannot use.
2. **Its content is too coupled to private memory.** If the document's load-bearing references are all `cog://mem/...` paths and the body cannot stand without them, it's not a public-class document.
3. **It's a half-baked draft superseded internally.** Some private RFCs are exploratory and were superseded before reaching draft-publication quality. Skip.

## Open questions

Genuinely open, not deferrals. Each is small enough to land in a follow-up amendment.

1. **Should `MIRRORS.md` be auto-generated from frontmatter?** The current plan is hand-maintained, one row per port. A small Go tool that walks `docs/adrs/` and `docs/rfcs/` and emits the MIRRORS table would eliminate drift, at the cost of a tool. Defer until drift becomes visible.
2. **Should public documents carry a `version:` field at all?** D6 recommends a linear integer. Some documents will never bump. Maybe only required when bumped. Decide on operational signal.
3. **How does private RFC-005 (`implementation:` field) interact with the public corpus?** RFC-005 (private) describes a frontmatter mechanism for tracking impl evidence on accepted ADRs. The public corpus could adopt the same field to make `accepted` a testable claim. Out of scope for this RFC; revisit in a follow-up RFC if the discipline proves valuable publicly.
4. **CI lint.** Eventually, a CI job should grep public source for `(ADR|RFC)-[0-9]+` patterns and fail if any reference an ID without a `MIRRORS.md` entry. Out of scope here; track separately.
5. **License declaration.** Public documents inherit MIT (the repo's license per CONTRIBUTING.md). RFC-006 (private) recommended Apache-2.0 or CC-BY-4.0 specifically for ADRs. The MIT default is fine for the volume expected; revisit if external citation grows.

## Appendix A: Full inventory

### A.1: Wave 1 (cited in public source)

25 documents. Status snapshots are private as of 2026-05-01. "Cites" is the count of `(ADR|RFC)-NNN` matches in `.go`, `.md`, `.yaml`, `.yml`.

| Private ID | Status | Cites | Title |
|---|---|---|---|
| ADR-001 | accepted | 1 | Workspace Membrane Geometry |
| ADR-007 | accepted | 1 | Git Worktrees for Agent Isolation |
| ADR-011 | accepted | 6 | Kernel as Cognitive DNA |
| ADR-018 | accepted | 2 | Transform Pipeline Pattern |
| ADR-021 | accepted | 2 | Holographic Workspace Coherence |
| ADR-033 | accepted | 4 | Event/Signal/Ledger Separation |
| ADR-040 | proposed | 1 | Literate Cogdocs (Build-Time Code Extraction) |
| ADR-046 | accepted | 1 | Harness Transform System |
| ADR-052 | accepted | 1 | Executable Cogdocs |
| ADR-058 | accepted | 2 | Inter-Workspace Coordination |
| ADR-059 | accepted | 10 | CogBlock — The Cognitive Block Protocol |
| ADR-060 | proposed | 2 | Distributed Workspace OS — Content-Addressable Components with BEP Sync |
| ADR-061 | accepted | 5 | MCP Streamable HTTP Transport |
| ADR-062 | proposed | 4 | Recursive Node Architecture — Unified Mesh with ACP Conductors |
| ADR-063 | proposed | 3 | Multi-Node Deployment — Containerized Nodes, Selective Sync, and Zero-Trust Secrets |
| ADR-072 | accepted | 22 | State-Transition Hooks for Node Lifecycle |
| ADR-074 | accepted | 4 | Nested Sovereignty and Reconciliation Scopes |
| ADR-081 | proposed | 9 | Homeostatic Kernel Loop |
| ADR-082 | proposed | 15 | Mod3 Session-Aware Communication Bus |
| ADR-083 | accepted | 2 | Cycle-Trace Event Schema on the Kernel Bus |
| ADR-084 | accepted | 40 | Bus Payloads as CogBlocks |
| ADR-085 | proposed | 5 | CogOS Kernel Subpackage Decomposition |
| RFC-003 | draft | 1 | Claude Code Hooks as Channel-Provider Membrane |
| RFC-004 | draft | 1 | ADR In-Review Status and Reshape Workflow |
| RFC-017 | draft | 2 | Virtualized Harness Writes via Per-Dispatch Worktree |

### A.2: Wave 2 (transitive closure)

60 documents not cited in public source but referenced from a Wave-1 document's `refs:`.

| Private ID | Status | Title |
|---|---|---|
| ADR-002 | accepted | Kernel Interface Specification |
| ADR-003 | accepted | Cogdoc Format Specification |
| ADR-004 | accepted | cog:// URI Scheme |
| ADR-006 | superseded | Two-Branch Model |
| ADR-008 | proposed | Content-Addressable State Consensus |
| ADR-009 | proposed | Merkle Tree Change Detection & Proofs |
| ADR-010 | accepted | Agent Workspace Architecture |
| ADR-012 | accepted | Role-Based View Projection |
| ADR-013 | accepted | Stigmergic Coordination |
| ADR-015 | accepted | Salience System (Git-Derived Attention) |
| ADR-017 | superseded | CQL Query Language |
| ADR-019 | accepted | Unified CLI Interface |
| ADR-020 | accepted | Shell as Universal Interface |
| ADR-022 | accepted | Projection System Architecture |
| ADR-023 | superseded | Executable Cogdocs |
| ADR-024 | accepted | Security Boundaries |
| ADR-025 | accepted | Workspace Audit System |
| ADR-026 | accepted | Agent Job Coordination |
| ADR-027 | accepted | RFC Process Adoption |
| ADR-028 | accepted | Go Migration for CogOS System Tooling |
| ADR-029 | accepted | Hybrid Handler Architecture |
| ADR-030 | accepted | Unified URI and CogDoc Reference System |
| ADR-031 | accepted | Autonomic Bookkeeping and Fail-Fast Validation |
| ADR-032 | accepted | Config Resolution System |
| ADR-034 | proposed | Universal Cognitive Protocol (UCP) |
| ADR-035 | proposed | OpenCode Integration Architecture |
| ADR-036 | accepted | Kernel Write Boundary: Eigenform Integrity Protection |
| ADR-037 | accepted | Multi-Platform UI Architecture |
| ADR-038 | accepted | Instance Branch Model |
| ADR-039 | accepted | SRC-Coherent Branching Strategy |
| ADR-041 | proposed | Substrate Coordination Protocol |
| ADR-042 | proposed | Cognitive Resource Model |
| ADR-043 | accepted | Universal Context Protocol (UCP) Integration |
| ADR-044 | accepted | Cognitive Branch Architecture |
| ADR-045 | accepted | Layered Configuration Model |
| ADR-047 | accepted | Content-Addressed Cognitive Substrate |
| ADR-048 | proposed | Branching Cognitive Substrate |
| ADR-050 | accepted | Event Unification Pipeline |
| ADR-053 | proposed | Plugin Architecture for CogOS |
| ADR-055 | superseded | Docker Sandbox Integration |
| ADR-056 | accepted | Hook-Based Node Architecture |
| ADR-057 | proposed | Eigenform Task Primitive |
| ADR-065 | accepted | Container-Native Daemon Lifecycle |
| ADR-071 | accepted | Unified Foveated Proxy — LoRO as Live Observer |
| ADR-073 | accepted | Node Control Plane, MCP Integration, and Pi Provider Architecture |
| ADR-075 | proposed | Federated Workspace Protocol |
| ADR-076 | proposed | Self-Describing Inference API |
| ADR-077 | accepted | CogOps — Cognitive GitOps for Distributed Build, Deploy, and Sync |
| ADR-078 | proposed | A2A-Inspired Model Negotiation Protocol |
| ADR-079 | proposed | Cogdoc v2 — The Unified Cognitive Document Specification |
| ADR-080 | accepted | Mod3 Discord Voice Pipeline Integration |
| ADR-086 | accepted | "Membrane" Terminology Resolution |
| ADR-089 | proposed | Pointer-Envelope Schema for External Content |
| RFC-008 | draft | Reconcilable Provider Contract and Loop Orchestration |
| RFC-009 | draft | Proposer / Actor Decomposition for the Homeostatic Loop |
| RFC-010 | draft | Peer-Awareness Query Surface |
| RFC-013 | draft | On-Node Session Substrate |
| RFC-014 | draft | Agent-Directed Context Curation |
| RFC-015 | draft | Capability Envelope and Policy Vocabulary |
| RFC-016 | draft | Named-Scope Harness Tools |

### A.3: Wave 3 (on demand)

28 documents. Port only when newly cited.

| Private ID | Status | Title |
|---|---|---|
| ADR-005 | accepted | Hierarchical Memory Domains (HMD) |
| ADR-014 | proposed | Perspectival Observer System |
| ADR-016 | proposed | Git-Native Vector Database |
| ADR-049 | accepted | Session Import Pipeline |
| ADR-051 | proposed | Kernel Migrate Command |
| ADR-054 | proposed | Config Consolidation and Schema Versioning |
| ADR-064 | proposed | Skills as Living Procedural Memory |
| ADR-066 | accepted | Foveated Context Assembly — CogBlock Context Containers |
| ADR-067 | proposed | cog: URI Scheme v2 — Multi-Scheme Resolution with Workspace Addressing |
| ADR-068 | proposed | MCP Resource-Based Ecosystem Awareness |
| ADR-069 | proposed | Distributed KV Entanglement Mesh |
| ADR-070 | accepted | TRM Index Scope Decision |
| ADR-087 | accepted | Architecture Documentation Lifecycle Skill |
| ADR-088 | accepted | LoRO Nomenclature Retirement |
| RFC-001 | draft | Zero-One Cosmology Workspace Refactoring |
| RFC-002 | draft | CogDoc Templating |
| RFC-005 | draft | ADR Implementation-Tracking Frontmatter |
| RFC-006 | draft | Public-Class ADR Corpus and Mirror Workflow |
| RFC-007 | draft | Org-Level Cognitive Substrate (cogos-dev/.cog) |
| RFC-011 | draft | Bus Stream Transport — Polling, SSE Recovery, and Envelope v1 |
| RFC-012 | draft | Node Bootstrap and Plugin-Install Flow |
| RFC-018 | draft | Modular Context Construction Pipeline |
| RFC-019 | draft | Substrate Orientation Skill |
| RFC-020 | draft | Stale-Index Hygiene and Tool-Call Error Contract |
| RFC-021 | draft | Kernel Self-Update via OCI Hot-Reload |
| RFC-022 | draft | Cognitive Primitives — Substrate, Runtime, Workspace, Node, Agent |
| RFC-030 | draft | Kernel-Issued Cogdoc Identity and Signature Contract |
| RFC-031 | draft | Display-Number Projection from Canonical_ID Ledger |

## Appendix B: Port script sketch

A starting point for sub-wave PRs. Not run in CI; a one-shot the porting maintainer adapts per batch.

```python
#!/usr/bin/env python3
"""Skeleton: read a private ADR/RFC, emit a draft public mirror."""
import re, sys, pathlib

PRIVATE_ROOT = pathlib.Path("~/workspaces/cog/.cog").expanduser()
PUBLIC_ROOT = pathlib.Path(".")  # run from repo root

def port(private_id: str, public_id: str, kind: str):
    # private_id: "ADR-082" or "RFC-004"
    # public_id: "ADR-0007" or "RFC-0003"
    # kind: "adr" | "rfc"
    if kind == "adr":
        priv_glob = list((PRIVATE_ROOT / "adr").glob(f"{int(private_id.split('-')[1]):03d}-*.cog.md"))
    else:
        priv_glob = list((PRIVATE_ROOT / "conf/spec/rfc").glob(f"{private_id}-*.cog.md"))
    assert len(priv_glob) == 1, f"expected exactly one match for {private_id}"
    src = priv_glob[0].read_text()
    # Strip frontmatter, body
    fm_end = src.find("\n---", 4)
    fm, body = src[4:fm_end], src[fm_end+4:]
    # Strip Discussion Log / Simmer criteria sections
    body = re.sub(r"\n## Discussion Log.*", "", body, flags=re.DOTALL)
    body = re.sub(r"\n### Simmer criteria.*", "", body, flags=re.DOTALL)
    # Strip cog:// references (replace with TODO comments for human review)
    body = re.sub(
        r"`?cog://(mem|run|skills|memory)/[^\s)`]+`?",
        "<!-- private memory ref stripped; verify before merge -->",
        body,
    )
    # Build public frontmatter
    public_status = "draft"
    private_status_match = re.search(r"^status:\s*(\S+)", fm, re.MULTILINE)
    private_status = private_status_match.group(1) if private_status_match else "unknown"
    title_match = re.search(r"^title:\s*\"?([^\"\n]+)\"?", fm, re.MULTILINE)
    title = title_match.group(1).strip().rstrip('"') if title_match else "TODO"
    out_fm = f"""---
id: {public_id}
title: {title}
status: {public_status}
created: 2026-05-01
authors: [chazmaniandinkle]
mirrors:
  - source: cog://{kind}/{private_id.split('-')[1].lower()}
    rel: primary-source
    private_status: {private_status}
---

"""
    slug = re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-")[:50]
    pub_dir = PUBLIC_ROOT / "docs" / (f"{kind}s")
    pub_path = pub_dir / f"{public_id.split('-')[1]}-{slug}.md"
    pub_path.write_text(out_fm + body)
    print(f"Wrote {pub_path} (review before commit)")

if __name__ == "__main__":
    port(sys.argv[1], sys.argv[2], sys.argv[3])
```

Use it as a draft-emitter; the maintainer hand-edits before opening the PR.

## References

- [PR #141](https://github.com/cogos-dev/cogos/pull/141): RFC-0001, the first public RFC, drafted under the assumption no prior corpus existed; this RFC is the corrective.
- Private RFC-006 (`cog://rfc/006`): Public-Class ADR Corpus and Mirror Workflow. The substrate ancestor of this RFC. Most decisions here adapt RFC-006's frontmatter and lifecycle conventions to the public corpus's realities.
- Private RFC-004 (`cog://rfc/004`): ADR In-Review Status and Reshape Workflow. The private workflow doc CONTRIBUTING.md currently cites; replaced for public purposes by RFC-0002.
- Private RFC-005 (`cog://rfc/005`): ADR Implementation-Tracking Frontmatter. A candidate for future public adoption (Open Question 3).
- Private ADR-027 (`cog://adr/027`): RFC Process Adoption. Defines the private RFC lifecycle this RFC mirrors.
- Private ADR-085 (`cog://adr/085`): CogOS Kernel Subpackage Decomposition. The load-bearing source for PR #141; will be ported as a public ADR in Wave 1c.
- [CONTRIBUTING.md](../../CONTRIBUTING.md): currently cites private RFC-004; needs amendment as part of Wave 0.
