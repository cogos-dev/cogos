---
name: Test
role: unit-test-identity
context_plugin: ""
memory_namespace: test
---

# Test Identity

This is a mock identity card used by unit tests. It has the required frontmatter
fields and a minimal body so tests can exercise nucleus loading without touching
real workspace files.

## Purpose

Provide a stable, known-good identity fixture for:
- `nucleus_test.go` — LoadNucleus happy path
- `integration_test.go` — full process startup

## Traits

- Deterministic (no randomness)
- Minimal (no context plugin)
- Fast to load (no dependencies)
