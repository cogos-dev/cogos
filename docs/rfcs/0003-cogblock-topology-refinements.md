# RFC-0003: CogBlock Topology Refinements

| Field         | Value                                                          |
|---------------|----------------------------------------------------------------|
| Status        | Partially Implemented (R4 merged; R1, R2, R3, R5 pending)    |
| Author        | @chazmaniandinkle                                             |
| Tracking      | [#204](https://github.com/myrgic/cogos/issues/204), [#205](https://github.com/myrgic/cogos/issues/205), [#206](https://github.com/myrgic/cogos/issues/206), [#207](https://github.com/myrgic/cogos/issues/207), [#208](https://github.com/myrgic/cogos/issues/208) |
| Target        | `v0.6.0`                                                       |
| Council       | 2026-05-05 CogBlock topology council (7 Opus + 3 Codex seats, cross-model) |

## Summary

The 2026-05-05 council deliberation on CogBlock topology (7 Opus seats + 3 Codex seats cross-model) validated the convergence thesis — CogBlock's field topology is the terminal object in the category of persistent, content-addressed, version-evolving self-referential systems (PSRS-strong) — and produced four operational gaps that production self-referential protocols learned via production scars. This RFC ratifies those four gaps and adds a fifth (canonicalization algorithm versioning). It does not implement kernel changes; it is a design ratification record that gates implementation.

The five refinements compose cleanly. They do not alter the convergence thesis; they make the envelope more robust under parallel emission, federated delegation, cross-envelope dedup, and long-horizon algorithm evolution.

## Background

CogBlock (defined in `pkg/cogblock/block.go` and governed by ADR-059) is the fundamental interaction unit of the CogOS substrate. Every inbound interaction, bus event, and kernel action flows through the CogBlock envelope. The 2026-05-05 council evaluated the current field topology against 17 production self-referential protocols (Git, Bitcoin, OCI, IPLD, Ceramic, Hypercore, Kafka, Nostr, Macaroons, and others) and confirmed structural convergence at the field level.

The council produced five specific findings that translate directly to implementation gaps. Each gap corresponds to one GitHub issue (#204–#208) and one section of this RFC.

## Refinement 1 — Nonce field for parallel-safe uniqueness (issue #204)

### Problem

The current `CogBlock` hash is computed over canonical content including `SessionID`, `Timestamp`, and `Kind`. There is a race window: two parallel emit paths can read the same ledger state simultaneously, compute identical sequence numbers, and produce blocks whose hashes collide despite distinct emitters.

Production precedent: Bitcoin's `nNonce` field exists precisely for this reason. MCP's `id` field is mandatory per-request for dedup. Email's `Message-Id` (RFC 5322) uses domain scoping to prevent relay duplicates.

### Resolution

Add `Nonce string` to `CogBlock`, included in canonical hash computation. Callers that need parallel-safe uniqueness populate it with UUIDv7 (time-ordered, dedup-safe), a per-emitter monotonic counter, or a clock-drift token. Callers that don't need it leave it empty; existing blocks default to empty Nonce and remain valid (backward-compatible).

### Acceptance criteria

- `Nonce` field added to `CogBlock` struct (`omitempty`, type `string`, included in hash).
- `CanonicalizeEvent` updated to include Nonce when non-empty.
- Existing blocks without Nonce continue to hash correctly.
- New test: parallel emit from N goroutines; no hash collision even when Ts/SessionID/Kind match.

### References

- Issue #204
- Bitcoin `nNonce`; MCP request `id`; RFC 5322 `Message-Id`; Macaroon third-party caveats

---

## Refinement 2 — Caveats field for capability-scoping chain (issue #205)

### Problem

`TrustContext { Authenticated bool, TrustScore float64, Scope string }` is a flat capability descriptor. It cannot express chain-of-narrowing: when workspace A shares a CogBlock with federated workspace B, B needs to be able to attenuate the block's authority before passing it further without re-issuing it from A.

Production precedent: Macaroons (Birgisson et al.) implement exactly this in ~200 lines with proven security properties. SPKI/SDSI and Biscuit tokens solve the same problem. The Macaroon principle: caveats narrow scope; they can only be added, never removed; each caveat further restricts what the token can authorize.

### Resolution

Add `Caveats []Caveat` to `CogBlock`. `Caveat` schema:

```go
type Caveat struct {
    Predicate           string `json:"predicate"`
    ThirdPartyLocation  string `json:"third_party_location,omitempty"`
    ThirdPartyCaveatID  string `json:"third_party_caveat_id,omitempty"`
}
```

Caveats are included in canonical hash. Adding a caveat to a block produces a new block with a new hash; the delegation chain is preserved in the Caveats list.

### Acceptance criteria

- `Caveats []Caveat` added to `CogBlock`; `Caveat` struct defined in `pkg/cogblock`.
- Included in canonical hash.
- Integration test: issuer emits block with empty Caveats; delegator appends `{ Predicate: "action in [read]" }`; verifier accepts read, rejects write.
- Documentation: caveats narrow authority and can only be added.

### References

- Issue #205
- Macaroons (Birgisson et al., 2014); SPKI/SDSI; Biscuit token spec

---

## Refinement 3 — Separate content hash from envelope hash (issue #206)

### Problem

The current hash covers the full canonical envelope including inline payload. Two blocks with the same content but different `From`, `Timestamp`, or `SessionID` values produce different hashes, preventing cross-envelope content deduplication.

Production precedent: OCI manifests compute the manifest hash over descriptor fields (including the `digest` of the layer), not over inline layer bytes. Git distinguishes blob hash (content) from commit hash (envelope). IPLD uses CIDs for content-level addressing; descriptors chain up from CIDs.

ADR-084's `Digest` field is partially this — a content hash distinct from the envelope hash — but the integration is incomplete. The hash computation does not currently use `Digest` to enable cross-envelope dedup.

### Resolution

When `Digest` is set, the envelope hash is computed as:

```
Hash = SHA256(canonical(envelope_fields_excluding_payload + Digest))
```

rather than:

```
Hash = SHA256(canonical(envelope_with_inline_payload))
```

Backward compatibility: when `Digest` is empty (inline payload), hash computation is unchanged.

### Acceptance criteria

- Hash computation refactored in `pkg/cogblock/ledger.go`: Digest-present and Digest-absent paths.
- Backward compat: existing blocks without Digest still hash correctly.
- New test: two blocks with same `Digest` value, different `From`/`Timestamp`/`SessionID`; they are deduplicatable via shared Digest, and their envelope hashes are distinct (as expected).
- ADR-059 and ADR-084 amended (in `docs/`) to document the integration.

### References

- Issue #206
- OCI image format; Git blob vs commit objects; IPLD CID content addressing; Tahoe-LAFS caps

---

## Refinement 4 — CanonForm field for canonicalization algorithm versioning (issue #207)

### Problem

`CanonicalizeEvent` in `pkg/cogblock/ledger.go` implements RFC 8785 (JSON Canonicalization Scheme). RFC 8785 has minor edge cases (Unicode normalization, number precision, float NaN handling). If a canonicalization edge case is fixed in a future version, hashes of already-emitted blocks change, making rolling forward impossible without a version field to anchor which algorithm was used.

Production precedent: Git's SHA-1 → SHA-256 transition required a version field and coordinated migration. IPLD's `multihash` + `multicodec` are self-describing — blocks carry their own codec version. X.509 signature algorithm versioning enables PKI to evolve. DNS DNSSEC algorithm numbers enable algorithm agility.

The lesson: systems that don't version their canonicalization get trapped.

### Resolution

Add `CanonForm string` to `CogBlock`. New blocks default to `"rfc8785-v1"`. CanonForm is included in the canonical hash input so the hash depends on which algorithm was declared.

Migration: existing blocks without CanonForm are read as `"rfc8785-v1"` (no hash change for current blocks; the algorithm hasn't changed). A future `"rfc8785-v2"` would be used when a canonicalization edge case is fixed; old and new blocks coexist under different CanonForm values.

### Acceptance criteria

- `CanonForm string` added to `CogBlock` (`omitempty`, defaults to `"rfc8785-v1"` on new blocks).
- `CanonicalizeEvent` includes CanonForm in canonical input.
- Existing blocks read as `"rfc8785-v1"` on unmarshal.
- Migration plan sketched in documentation: how `"rfc8785-v2"` would be introduced and how blocks with different CanonForm values are handled at read time.

### Implementation notes (2026-05-19)

- `CanonFormRFC8785V1 = "rfc8785-v1"` constant added to `pkg/cogblock/block.go`.
- `CanonForm string` added to both `CogBlock` and `EventPayload` (omitempty).
- `NewEventEnvelope` defaults `CanonForm` to `CanonFormRFC8785V1`.
- `CanonicalizeEvent` includes `canon_form` in the canonical map when non-empty.
- Migration path: new events produced after this PR carry `"rfc8785-v1"`. Legacy
  events (empty CanonForm) hash as before — the field is absent from their
  canonical map. When `"rfc8785-v2"` is introduced, old and new events coexist
  unambiguously; a verifier reads `CanonForm` to select the correct algorithm.

### References

- Issue #207
- Git SHA-1 → SHA-256 transition; IPLD multihash; X.509 algorithm versioning; DNSSEC algorithm numbers

---

## Refinement 5 — CogBlock signature as terminal object in PSRS-strong (issue #208)

### Problem

Issue #208 frames a theorem candidate: the CogBlock categorical signature is the terminal object in PSRS-strong — the category of persistent, attributed, bounded, version-evolving, partially-composable self-referential systems. If true, this claim is publishable and provides the theoretical grounding for the convergence thesis in ADR-059.

The council confirmed the claim is **falsifiable, publishable, and supported by multi-tradition convergence** (Kauffman's reflexive domain fixed-point theorem, eigenform lineage, category theory). But the council also produced a precision refinement:

- The strict claim is "terminal in PSRS-strong" (the category with a specific joint constraint set), not "minimum topological shape of all self-reference." Systems outside PSRS-strong (pure content-addressing, unchained events, unsigned logs) hold stable self-reference with fewer fields.
- The Codex council seat (laws-of-form perspective) noted that Kauffman gives no result characterizing a minimum field topology like CogBlock; the specific field set (hashes, Merkle trees, signatures) is not a universal eigenform requirement in the published corpus. The weaker correct claim: ADR-059 is _consistent with_ Kauffman's reflexive domain framing.

### Resolution

This refinement is **research, not implementation**. The action is documentation:

1. Amend ADR-059 to replace "minimum topological shape" language with "terminal in PSRS-strong within the constraint envelope" language.
2. Add a §Theorem candidate section to ADR-059 with the falsification path (per issue #208 body).
3. Add `docs/architecture/cogblock-terminality-theorem.md` with the full theorem statement, lineage, cross-model verification record, and falsification conditions.

No Go code changes in this refinement. The theorem candidate does not gate implementation of refinements 1–4.

### Acceptance criteria

- ADR-059 (in `docs/`) amended: "terminal in PSRS-strong" language, falsification path, forward-reference to the full theorem doc.
- `docs/architecture/cogblock-terminality-theorem.md` created with: theorem statement, PSRS-strong category definition, lineage (Kauffman, eigenform, category theory), 2026-05-05 cross-model verification record, falsification conditions.
- Issue #208 closed with link to the docs.

### References

- Issue #208
- Kauffman (2009) §V, "Reflexivity and Eigenform"; eigenform lineage (von Foerster 1981, Spencer-Brown 1969); Lambek's lemma; ADR-059
- Council cogdocs: `cog://mem/semantic/council/2026-05-05/` (private; verbatim not reproduced here)

---

## Implementation order

These refinements are independent but share a natural order:

1. **Refinement 4 first** (CanonForm): adds a field to every new block. Establishes the versioning contract before refinements 1–3 change the canonicalization.
2. **Refinement 1** (Nonce): field addition only; backward-compatible.
3. **Refinement 2** (Caveats): field addition + new type; backward-compatible.
4. **Refinement 3** (content/envelope hash split): touches hash computation logic; most risk, best done after CanonForm establishes the versioning anchor.
5. **Refinement 5** (theorem docs): pure documentation, can land in parallel with any of the above.

Each refinement ships as its own PR. This RFC is the ratification record; implementation PRs reference it and are linked to their corresponding issue.

## Non-goals

- Full Macaroon verifier implementation (Refinement 2 adds the field shape; the verifier is a follow-up).
- Signature scheme changes (Sig field remains optional Ed25519 as in current code).
- Migration tooling for existing ledger files (each refinement is backward-compatible at read time; no migration needed).
- KV-cache block-mesh integration (separate design seed; this RFC is about the CogBlock envelope, not session-layer composition).
- Publishing the theorem candidate (Refinement 5 prepares the internal record; peer review is a separate track).

## Relationship to prior art

- **ADR-059** — "Five Systems, One Structure" — this RFC extends, not replaces. ADR-059 establishes the convergence thesis; this RFC adds the four operational lessons the council surfaced and refines the theorem statement.
- **ADR-084** — Content-Addressed Payload Phase 1 — Refinement 3 integrates ADR-084's Digest field into the hash computation as intended but not yet implemented.
- **Block-mesh architecture** design seed — the KV-cache-as-block-layer composition design. That design depends on this RFC's refinements being stable but is not in scope here.
- **RFC-0001** (root package refactor), **RFC-0002** (public corpus migration) — unrelated; same numbered sequence.
