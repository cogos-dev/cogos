# Contributing to CogOS

Thanks for your interest in CogOS. This document covers how to set up a development environment, run tests, and submit changes.

## Development setup

```sh
git clone https://github.com/cogos-dev/cogos.git
cd cogos
make build
./scripts/setup-dev.sh
```

Requirements: Go 1.24+, macOS or Linux.

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

## Submitting changes

1. Fork the repo and create a branch from `main`
2. Make your changes
3. Run `make test` and ensure all tests pass
4. Write a clear commit message describing what changed and why
5. Open a pull request

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Tests go in `*_test.go` files alongside the code they test
- Error messages should be lowercase and not end with punctuation (Go convention)

## Reporting issues

Open an issue on GitHub. Include:

- What you expected to happen
- What actually happened
- Steps to reproduce
- Go version and OS

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
