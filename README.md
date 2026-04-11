# CogOS

A cognitive daemon for AI agents. Written in Go. Runs locally. Gives Claude Code, Cursor, and other AI tools persistent memory, scored context, and workspace continuity.

```sh
make build && ./cogos serve --workspace ~/my-project
# http://localhost:6931/health
```

---

## What it does

- **Foveated context assembly** -- A live hook (`UserPromptSubmit`) fires on every Claude Code prompt, scores all available documents by relevance, and injects a focused context window. No manual `@`-file selection; the system decides what matters.

- **Learned retrieval via TRM** -- A 2.3M-parameter Mamba SSM (Tiny Recursive Model) trained to 0.878 mean NDCG@10 (0.900 peak) through 500+ tracked experiments. Scores documents by temporal salience, edit recency, and semantic relevance. Runs inference locally in ~6KB of state. See [docs/EVALUATION.md](docs/EVALUATION.md) for full methodology.

- **Persistent memory** -- Hierarchical memory system with salience scoring and temporal attention. Your workspace remembers across sessions, models, and tools. Switch from Claude Code to Cursor and back -- same memory, same context.

- **Multi-provider routing** -- OpenAI-compatible and Anthropic Messages-compatible HTTP API. Works with Ollama, LM Studio, Claude, and any OpenAI-compatible endpoint. Local models preferred by default.

- **Content-addressed storage** -- CogBlock protocol for content-addressed, hash-chained records. Every routing decision, context assembly, and state transition is recorded in an append-only ledger (SHA-256, RFC 8785).

---

## Architecture

CogOS runs as a single Go binary daemon. The kernel has three layers:

```
┌──────────────────────────────────────────────────────────┐
│  Membrane          HTTP API · MCP Server · Provider Router│
├──────────────────────────────────────────────────────────┤
│  Workspace          Context Engine · Memory · Ledger      │
│                     Salience Scorer · Blob Store          │
├──────────────────────────────────────────────────────────┤
│  Nucleus            Process Loop · Identity · State FSM   │
└──────────────────────────────────────────────────────────┘
```

**Membrane** -- The API surface. Serves OpenAI and Anthropic-compatible chat endpoints, an MCP server, and foveated context assembly. Routes inference requests to local or cloud providers.

**Workspace** -- Where state lives. The context engine scores documents and arranges them into stability zones optimized for KV cache reuse. The ledger is append-only and hash-chained. Memory persists across sessions.

**Nucleus** -- The process loop. Runs continuously through four states (Active, Receptive, Consolidating, Dormant). Manages identity, consolidation, and workspace lifecycle.

### How foveated context works

When you submit a prompt in Claude Code, the `UserPromptSubmit` hook fires and calls the CogOS daemon. The context engine:

1. Scores all workspace documents using TRM + git-derived salience
2. Ranks by a composite signal (edit recency, semantic match, structural importance)
3. Assembles a context window organized into stability zones:

| Zone | Contents | Behavior |
|------|----------|----------|
| 0 -- Nucleus | Identity, system config | Always present, never evicted |
| 1 -- Knowledge | Workspace docs, indexed memory | Shifts slowly, high cache hit rate |
| 2 -- History | Conversation turns | Scored by relevance, evictable |
| 3 -- Current | The current message | Always present |

4. Injects the assembled context into the prompt before it reaches the model

The model sees a pre-focused window instead of everything-or-nothing.

---

## Getting started

### Requirements

- Go 1.24+
- macOS or Linux

### Build and run

```sh
git clone https://github.com/cogos-dev/cogos.git
cd cogos
make build

# Initialize a workspace
./cogos init --workspace ~/my-project

# Start the daemon
./cogos serve --workspace ~/my-project

# Verify it's running
curl -s http://localhost:6931/health | jq .
```

### Developer setup

```sh
./scripts/setup-dev.sh    # Build, install to ~/.cogos/bin, configure PATH
```

### Docker

```sh
make image        # Build production image
make run          # Run with workspace volume mount
make e2e          # Build + run full cold-start test in a container
```

---

## API

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic Messages-compatible chat |
| `POST /v1/context/foveated` | Foveated context assembly |
| `GET /v1/context` | Current attentional field |
| `GET /health` | Liveness probe (identity, state, trust) |
| `POST /mcp` | MCP Streamable HTTP endpoint |

### Providers

Ships with adapters for Anthropic, Ollama, Claude Code, and Codex. New providers implement [six methods](docs/writing-a-provider.md).

---

## Testing

```sh
make test         # Unit tests (with -race)
make e2e-local    # Full cold-start lifecycle test
make e2e          # Containerized e2e (Docker)
```

The kernel has 64 source files and 40 test files in `internal/engine/`.

---

## Project layout

```
cmd/cogos/              Entry point (thin — delegates to internal/engine)
internal/engine/        Kernel: 30K LOC, 64 source files, 40 test files
docs/                   Specs, architecture docs, provider guide
scripts/                Setup, CLI wrapper, e2e tests, experiment harnesses
```

---

## Status

**v3 kernel** -- Ground-up rewrite after a year of daily use across Claude Code, Cursor, and custom agent harnesses.

### Working

- Continuous process daemon with four-state FSM
- Foveated context assembly with Mamba TRM (0.878 mean NDCG@10)
- Hash-chained append-only ledger
- Multi-provider routing (Ollama, Anthropic, Claude Code, Codex)
- MCP server (Streamable HTTP)
- Content-addressed blob store
- Git-derived salience scoring
- Tool-call hallucination gate
- Digestion pipeline (Claude Code + OpenClaw adapters)
- Memory consolidation
- OpenAI and Anthropic API compatibility
- Workspace scaffolding and lifecycle management
- End-to-end test suite
- OpenTelemetry instrumentation

### Next

- Wire digestion tailers into process loop
- Constellation library integration (multi-node sync)
- Multi-agent process management
- `cog` CLI

---

## Ecosystem

CogOS is one piece of a larger system. Each component is its own repo with independent releases:

| Repo | Purpose |
|------|---------|
| **[cogos](https://github.com/cogos-dev/cogos)** | The daemon -- this repo |
| [constellation](https://github.com/cogos-dev/constellation) | Distributed identity and workspace sync (BEP-based) |
| [mod3](https://github.com/cogos-dev/mod3) | Modality bus -- voice I/O, TTS, channel multiplexing |
| [skills](https://github.com/cogos-dev/skills) | Agent skill library (Claude Code compatible) |
| [charts](https://github.com/cogos-dev/charts) | Helm charts and Docker Compose for deployment |
| [desktop](https://github.com/cogos-dev/desktop) | macOS app -- kernel management and dashboard |

---

## Design documents

- [System Specification](docs/SYSTEM-SPEC.md) -- Multi-level spec from ontology to deployment
- [Architectural Principles](docs/architecture/principles.md) -- Core engineering constraints
- [Writing a Provider](docs/writing-a-provider.md) -- How to add a new inference provider
- [MCP Specification](docs/MCP-SPEC.md) -- MCP server contract
- [Provider Specification](docs/PROVIDER-SPEC.md) -- Provider interface contract
- [Architecture Diagrams](docs/architecture-diagram-source.md) -- Cell model, topology views
- [Cognitive GitOps](docs/architecture/cognitive-gitops.md) -- Substrate-coordinated repo model
- [E2E Test Plan](docs/E2E-TEST-PLAN.md) -- End-to-end test strategy

---

## License

[MIT](LICENSE) -- Copyright (c) 2025-2026 Chaz Dinkle
