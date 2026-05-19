# pkg/substrate

Substrate library extraction target per [ADR-100](../../docs/adrs/100-substrate-library-extraction.md).

## Layout

Each module from the CogOS kernel is re-exported under a subdirectory:

```
pkg/substrate/
  go.mod                  # module github.com/myrgic/cogos/pkg/substrate
  reconcile/              # re-exports pkg/reconcile
  ...                     # further modules added in Steps 2b–5
```

## Design

- **Subtree path** (`github.com/myrgic/cogos/pkg/substrate`): intentional. Option 1
  from the ADR-100 decision packet. Enables incremental extraction without a repo split.
- **Re-exports use Go type aliases** (`type Foo = reconcile.Foo`) so consumers can use
  either import path without conversion. The types are identical at the language level.
- **Naming**: "substrate" only. No conversation-formed names.

## Status

Step 1 (scaffold) and Step 2a (reconcile re-export) are complete. Steps 2b–5 follow
in separate PRs per the ADR-100 extraction sequence.
