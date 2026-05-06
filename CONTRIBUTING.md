# Contributing to CogOS

Thanks for your interest in CogOS. This document covers how to set up a development environment, run tests, and submit changes.

## Development setup

```sh
git clone https://github.com/cogos-dev/cogos.git
cd cogos
make build
./scripts/setup-dev.sh
```

Requirements: Go 1.25+, macOS or Linux.

## Running tests

```sh
make test         # Unit tests with race detector
make e2e-local    # Full cold-start lifecycle test (requires built binary)
make e2e          # Containerized e2e (requires Docker)
```

All changes should pass `make test` before submitting.

## Project structure

The kernel lives in `internal/engine/`. The entry point at `cmd/cogos/main.go` is intentionally thin -- it delegates immediately to the engine.

Key areas:

- `internal/engine/` -- Core daemon: process loop, context engine, memory, ledger, providers, API
- `docs/` -- Specifications and architecture documents
- `scripts/` -- Build tooling, setup scripts, experiment harnesses

## Reporting issues

Use the templates in `.github/ISSUE_TEMPLATE/`. They aren't optional formality -- they encode the project's standard for what an actionable issue looks like.

The single most important rule: **the bug is in the code, not on your machine.** Your environment helped you find the defect, but your paths, PIDs, dashboard processes, and local config overrides are not the bug. Keep them in the "Local Environment" section at the bottom of the bug template, and keep them out of the Summary, Evidence, Reproduction, Impact, and Acceptance sections.

A reviewer in a different environment must be able to:

- Read the Summary and understand what defect exists in the code.
- Run the Reproduction steps on a fresh checkout (no machine-specific setup beyond the project's stated supported config).
- Use the Acceptance checklist as a definition of done.

If your reproduction depends on a specific local arrangement (a particular provider configured, a particular dashboard process running), describe the *class* of arrangement in Reproduction ("a kernel running with at least one chronically-degraded provider") and note your specific instantiation in the Local Environment section.

### Examples that meet this standard

- [#75](https://github.com/cogos-dev/cogos/issues/75) -- model advertisement staleness + routing semantics. Three sub-bugs in one issue with shared fix surface, each cited at file:line.
- [#79](https://github.com/cogos-dev/cogos/issues/79) -- kernel hang RCA. Filed with hypotheses honestly marked as hypotheses, then updated with a follow-up RCA that named the actual root cause.
- [#80](https://github.com/cogos-dev/cogos/issues/80), [#81](https://github.com/cogos-dev/cogos/issues/81) -- companion issues cross-linked so a reader entering through any one can find the others.

## Submitting changes

1. Fork the repo and create a branch from `main`
2. Make your changes
3. Run `make test` and ensure all tests pass
4. Open a pull request using `.github/PULL_REQUEST_TEMPLATE.md`

The bar for "ready for review":

- The PR links the issue it fixes.
- At least one acceptance checklist item from the linked issue is ticked, or you've said why none are yet.
- Test evidence is concrete: new tests are named, command output is pasted, CI status is linked. "Tests pass" by itself doesn't count.
- Out-of-scope is stated. Reviewers should never have to guess whether you intentionally left something for a follow-up or just forgot.

## Pull request titles and bodies

PR titles and bodies are this project's changelog. The auto-generated GitHub release notes pull every PR title since the previous tag verbatim, so PR title quality is a load-bearing concern for the project's history.

Conventions:

- **Title is a complete imperative-mood sentence with a conventional-commit prefix.** Example: `fix(chat): execute injected cog_* MCP tools server-side` -- not `Tool fix` or `various improvements`. Anyone reading the release notes should understand what the PR did from the title alone.
- **Use a conventional-commit prefix:** `feat:`, `fix:`, `refactor:`, `chore:`, `docs:`, `test:`, `perf:`. Optionally scope it: `fix(provider/ollama):`. Prefixes group release notes naturally and scan well in `git log`.
- **Body explains the why, not the what.** The diff shows the what; the body should answer "why is this the right change here?" and "what is this NOT trying to do?" -- out-of-scope is as informative as in-scope.
- **Link the issue** with `Closes #N` so the PR-issue chain is queryable. The issue's acceptance criteria become the merge bar.
- **Out-of-scope deserves a section.** If you noticed something during the work that you intentionally didn't fix, name it -- otherwise reviewers can't tell whether you missed it or chose to defer.

See [CHANGELOG.md](CHANGELOG.md) for the rationale.

## RFCs and ADRs

For substantial design changes -- new subsystems, protocol changes, breaking behavior, or anything that touches the kernel's persistence or routing -- open an RFC before opening a PR:

```sh
./scripts/cog rfc new "<title>"
```

ADRs document decisions that have been made; RFCs gather input on decisions that are still open. RFC-004 covers the workflow.

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Tests go in `*_test.go` files alongside the code they test
- Error messages should be lowercase and not end with punctuation (Go convention)
- Default to no comments. Add one only when the *why* is non-obvious.
- Don't add error-handling, fallbacks, or validation for scenarios that can't happen. Trust internal code and framework guarantees.
- Don't add abstractions for hypothetical future requirements.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
