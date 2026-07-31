---
type: reflective
title: "Example quote sequence — synthetic fixture for projection-compiler acceptance tests"
sector: reflective
status: active
created: 2026-05-19
updated: 2026-05-19
salience: high
tags:
  - synthetic-fixture
  - quote-boundary
  - projection-compiler
authors:
  - fixture-author
refs:
  - uri: cog://mem/reflective/example-precursor-note
    rel: precursor
    desc: "Placeholder precursor reference retained to exercise the refs-parsing path."
provenance:
  composition: synthetic-fixture
  composition_method: "hand-authored for test coverage; not a real session transcript"
  preservation_directive: "regenerate freely; no content here is load-bearing outside this test"
  source_session: 00000000-0000-0000-0000-000000000000
  source_window: "N/A"
---

Six short quotes, synthetic, used only to exercise the ProjectionCompiler's
quote-boundary extraction path (one CogBlock event per "## Quote N" section).
None of the text below reflects any real conversation; it exists purely to
give the parser six stable boundaries to walk.

## Quote 1 — Opening remark

> Every reconciler needs a live source and a declared target before it can compute a plan.

The compiler should treat this as the first quote boundary and emit one event anchored at `#quote-1`.

## Quote 2 — Structural observation

> Boundaries are cheap to detect and expensive to get wrong, so the parser should be conservative about what counts as a heading.

Second boundary; anchor `#quote-2`.

## Quote 3 — The load-bearing marker sentence

> This also means that the wave is initiated by the highest-energy distinction and all other layers are necessarily topologically lower unless the level of local energy gradient is positive.

This sentence is the fixture's mutation marker: the single-block-modification
acceptance test swaps a substring inside it to prove that editing one quote
produces exactly one Update while every other quote stays pointer-referenced.
Anchor `#quote-3`.

## Quote 4 — Idempotency note

> Re-running a reconciler on unchanged input should never produce a new Create; it should produce a Skip.

Fourth boundary; anchor `#quote-4`.

## Quote 5 — Fixture housekeeping

> A good fixture documents why it exists in the same place it lives, so a future reader isn't left guessing.

Fifth boundary; anchor `#quote-5`.

## Quote 6 — Closing remark

> Six quotes, six anchors, six events on a clean first pass — that is the whole acceptance criterion.

Sixth and final boundary; anchor `#quote-6`.
