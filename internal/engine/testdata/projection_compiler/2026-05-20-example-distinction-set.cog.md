---
type: reflective
slug: 2026-05-20-example-distinction-set
title: "Example distinction set — synthetic fixture for projection-compiler acceptance tests"
date: 2026-05-20
authors: [fixture-author]
preservation_directive: "regenerate freely; no content here is load-bearing outside this test"
salience: high
tags: [synthetic-fixture, distinction-boundary, projection-compiler]
composition:
  - session: synthetic-fixture-session
related:
  - cog://mem/reflective/example-precursor-note
status: live
aliases: []
---

# Example Distinction Set

A small set of synthetic, numbered "distinction" sections used only to
exercise the ProjectionCompiler's distinction-boundary extraction path (one
CogBlock event per "## Distinction N:" section). None of the text below
reflects any real research or conversation; it exists purely to give the
parser a stable, countable set of H2 boundaries.

## Distinction 1: A reconciler compares declared state to live state

The core Reconcilable contract is FetchLive → ComputePlan → ApplyPlan. Every
distinction in this fixture maps onto one CogBlock event when the compiler
runs its structural-extraction pass.

## Distinction 2: Boundaries must survive fenced code blocks

A heading-looking line inside a fenced code block must not be mistaken for a
structural boundary:

```
## Distinction 99: this is not a real boundary, it is inside a fence
```

The parser's fence-tracking is what keeps this fixture honest.

## Distinction 3: Counting is a structural rule, not a magic number

The acceptance test that reads this fixture counts "## Distinction N:"
headings independently of the compiler and asserts the two counts agree,
rather than hardcoding an expected total. That is what lets this fixture
shrink or grow without anyone needing to touch the test's assertions.

## Distinction 4: Idempotent re-runs must not re-Create

Running the compiler twice against an unmodified copy of this fixture should
produce only Skip actions on the second pass, in pointer mode.

## Distinction 5: One edit should touch exactly one block

If a future test wants a single-block-modification case against this
fixture, it should edit the body of exactly one Distinction section and
confirm every other section stays Skip.
