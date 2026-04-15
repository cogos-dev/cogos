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

- **Library extraction** -- Seven importable Go packages in `pkg/` covering the core type system: content-addressed blocks, coordination primitives, BEP wire protocol, reconciliation framework, modality bus, field graph types, and URI parsing. All usable independently of the kernel.

- **Native agent harness** -- A homeostatic agent loop that runs as a goroutine inside the kernel process. Calls Gemma E4B via Ollama's native `/api/chat` endpoint with six kernel-native tools. Adaptive interval (5m-30m) based on assessment urgency, with panic recovery.

- **MCP Streamable HTTP** -- Full MCP transport at `POST /mcp` with JSON-RPC 2.0, session management, and 30-minute expiry. Exposes 8 tools: 4 kernel-native (memory search/read/write, coherence check) and 4 Mod3 voice tools bridged through the kernel.

- **Anthropic Messages API proxy** -- Transparent proxy at `POST /v1/messages` that forwards to the real Anthropic API with streaming SSE passthrough. Enables `cog claude` to route Claude Code through the kernel via `ANTHROPIC_BASE_URL`.

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
│                     Agent Harness · CogBus                │
└──────────────────────────────────────────────────────────┘
```

**Membrane** -- The API surface. Serves OpenAI and Anthropic-compatible chat endpoints, an MCP Streamable HTTP server, the Anthropic Messages API proxy, and foveated context assembly. Routes inference requests to local or cloud providers.

**Workspace** -- Where state lives. The context engine scores documents and arranges them into stability zones optimized for KV cache reuse. The ledger is append-only and hash-chained. Memory persists across sessions.

**Nucleus** -- The process loop. Runs continuously through four states (Active, Receptive, Consolidating, Dormant). Manages identity, consolidation, workspace lifecycle, and the homeostatic agent harness.

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

## Library packages (pkg/)

Seven importable Go packages extracted into a `go.work` multi-module workspace. Each has its own `go.mod` and can be imported independently of the kernel. Six are stdlib-only; `pkg/bep` requires `google.golang.org/protobuf`.

| Package | What it provides |
|---------|-----------------|
| `pkg/cogblock` | Content-addressed block format, CogBlockKind enum, provenance/trust types, EventEnvelope, ledger (RFC 8785 canonicalization, hash chain, verify) |
| `pkg/coordination` | Claim/Handoff/Broadcast types, 13 coordination functions, AgentID |
| `pkg/bep` | BEP wire protocol types, TLS/DeviceID, index/version vectors, events, Engine/SyncProvider interfaces |
| `pkg/reconcile` | Reconcilable interface (7 methods), State/Plan/Action types, registry, Kahn's topological sort, meta-orchestrator |
| `pkg/modality` | Module interface, Bus, wire protocol (D2), events, salience tracker, channels, ProcessSupervisor |
| `pkg/cogfield` | Node/Edge/Graph types, Block, BlockAdapter interface, conditions, signals, sessions, documents |
| `pkg/uri` | URI struct, Parse/Format, 35 namespaces, ExtractInlineRefs, error types |

69 files, ~10,200 lines, 190 tests across all packages.

---

## Agent harness

The native Go agent harness (`agent_harness.go`, `agent_tools.go`, `agent_serve.go`) runs a homeostatic assessment loop inside the kernel process:

- Calls Gemma E4B via Ollama's native `/api/chat` (with `think: false`)
- Adaptive interval: 5m idle, scales to 30m when assessment urgency is low
- Panic recovery -- a crash in the agent goroutine doesn't take down the kernel
- Observable via `GET /v1/agent/status` (cycle count, last assessment, urgency)

Six kernel-native tools are available to the agent:

| Tool | Description |
|------|-------------|
| `memory_search` | Search CogDocs by query |
| `memory_read` | Read a specific memory document |
| `memory_write` | Write or update a memory document |
| `coherence_check` | Run drift detection on the workspace |
| `bus_emit` | Emit an event to the CogBus |
| `workspace_status` | Get workspace health and metrics |

The agent tab in the embedded dashboard shows cycle history and an urgency sparkline.

---

## API

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic Messages API proxy (streaming SSE passthrough) |
| `POST /v1/context/foveated` | Foveated context assembly |
| `GET /v1/context` | Current attentional field |
| `GET /v1/agent/status` | Agent harness status (cycle count, urgency, last assessment) |
| `GET /health` | Liveness probe (identity, state, trust) |
| `GET /dashboard` | Embedded web dashboard |
| `POST /mcp` | MCP Streamable HTTP endpoint (JSON-RPC 2.0, session management) |
| `DELETE /mcp` | MCP session termination |

All endpoints serve on port **6931** by default.

### MCP tools

The MCP server exposes 8 tools over Streamable HTTP:

| Tool | Source |
|------|--------|
| `cogos_memory_search` | Kernel native |
| `cogos_memory_read` | Kernel native |
| `cogos_memory_write` | Kernel native |
| `cogos_coherence_check` | Kernel native |
| `mod3_speak` | Bridged to Mod3 TTS |
| `mod3_stop` | Bridged to Mod3 TTS |
| `mod3_voices` | Bridged to Mod3 TTS |
| `mod3_status` | Bridged to Mod3 TTS |

Sessions are created on `initialize` and expire after 30 minutes of inactivity.

### Providers

Ships with adapters for Anthropic, Ollama, Claude Code, and Codex. New providers implement [six methods](docs/writing-a-provider.md).

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

### Route Claude Code through the kernel

```sh
# Launch Claude Code with kernel-mediated API routing
cog claude
# This sets ANTHROPIC_BASE_URL to http://localhost:6931 and starts Claude Code
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

## Testing

```sh
make test         # Unit tests (with -race)
make e2e-local    # Full cold-start lifecycle test
make e2e          # Containerized e2e (Docker)
```

The kernel has 65 source files and 42 test files in `internal/engine/`, plus 190 tests across the `pkg/` library packages.

---

## Project layout

```
cmd/cogos/              Entry point (thin — delegates to internal/engine)
internal/engine/        Kernel: 65 source files, 42 test files
pkg/                    Importable library packages (go.work multi-module)
  cogblock/             Content-addressed blocks and ledger
  coordination/         Agent coordination primitives
  bep/                  BEP wire protocol types
  reconcile/            Reconciliation framework
  modality/             Modality bus and channel types
  cogfield/             Field graph types
  uri/                  URI parsing and namespaces
sdk/                    Go SDK for CogOS clients
docs/                   Specs, architecture docs, provider guide
scripts/                Setup, CLI wrapper, e2e tests, experiment harnesses
agent_harness.go        Native agent loop (Ollama /api/chat)
agent_tools.go          Kernel-native tool implementations
agent_serve.go          Agent status HTTP endpoint
serve_messages.go       Anthropic Messages API proxy
serve_dashboard.go      Embedded web dashboard
mcp_http.go             MCP Streamable HTTP transport
mcp_mod3.go             Mod3 voice tool bridge for MCP
```

---

## Status

**v3 kernel** -- Ground-up rewrite after a year of daily use across Claude Code, Cursor, and custom agent harnesses.

### Working

- Continuous process daemon with four-state FSM
- Foveated context assembly with Mamba TRM (0.878 mean NDCG@10)
- Hash-chained append-only ledger
- Multi-provider routing (Ollama, Anthropic, Claude Code, Codex)
- MCP Streamable HTTP server (8 tools, JSON-RPC 2.0, sessions)
- Anthropic Messages API proxy with streaming SSE
- Native Go agent harness with adaptive interval and 6 kernel tools
- Embedded web dashboard with agent status and cycle history
- Library extraction: 7 packages in pkg/ (69 files, ~10.2K LOC, 190 tests)
- Content-addressed blob store
- Git-derived salience scoring
- Tool-call hallucination gate
- Digestion pipeline (Claude Code + OpenClaw adapters)
- Memory consolidation
- OpenAI and Anthropic API compatibility
- Workspace scaffolding and lifecycle management
- End-to-end test suite
- OpenTelemetry instrumentation
- Port consolidation on 6931

### Next

- Wire digestion tailers into process loop
- Constellation library integration (multi-node sync)
- Multi-agent process management
- `cog` CLI

---

## Ecosystem

CogOS is one piece of a larger system. Each component is its own repo with independent releases:

| Repo | Purpose | Status |
|------|---------|--------|
| **[cogos](https://github.com/cogos-dev/cogos)** | The daemon -- this repo | Active |
| [constellation](https://github.com/cogos-dev/constellation) | Distributed identity and workspace sync (BEP-based) | Active |
| [mod3](https://github.com/cogos-dev/mod3) | Modality bus -- voice I/O, TTS, channel multiplexing | Active |
| [skills](https://github.com/cogos-dev/skills) | Agent skill library (Claude Code compatible) | Active |
| [charts](https://github.com/cogos-dev/charts) | Helm charts and Docker Compose for deployment | Active |

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
