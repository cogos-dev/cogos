# CogOS

A continuous-process operating system for AI agents, written in Go.

CogOS is a daemon that sits between AI agent harnesses (Claude Code, Cursor, Gemini CLI, etc.) and gives them persistent memory, attentional context assembly, multi-provider inference routing, and a tamper-evident decision ledger — capabilities that session-based agents don't have on their own.

## Why

AI coding agents are stateless. Every session starts from zero — no memory of what happened last time, no awareness of what matters in the codebase right now, no ability to route between local and cloud models based on what the task actually needs.

CogOS fixes this by running as a background daemon that maintains continuous cognitive state across sessions, agents, and providers.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Agent Harnesses                      │
│         Claude Code · Cursor · Gemini CLI · etc.         │
└────────────────────────┬────────────────────────────────┘
                         │ OpenAI-compatible API
                         │ Anthropic Messages API
                         │ MCP (Streamable HTTP)
┌────────────────────────▼────────────────────────────────┐
│                      CogOS Daemon                        │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │ Nucleus  │  │   Process    │  │  Context Assembly  │  │
│  │ (identity│  │  State Machine│  │  (foveated engine) │  │
│  │  context)│  │              │  │                    │  │
│  │          │  │  Active      │  │  Zone 0: Nucleus   │  │
│  │  Always  │  │  Receptive   │  │  Zone 1: CogDocs   │  │
│  │  loaded  │  │  Consolidate │  │  Zone 2: History   │  │
│  │          │  │  Dormant     │  │  Zone 3: Current   │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │  Ledger  │  │   Router     │  │    Salience        │  │
│  │ (hash-   │  │  (multi-     │  │  (git-derived      │  │
│  │  chained │  │   provider,  │  │   attention        │  │
│  │  JSONL)  │  │   local-first│  │   scoring)         │  │
│  │          │  │   routing)   │  │                    │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │Coherence │  │  Blob Store  │  │   MCP Server       │  │
│  │ (4-layer │  │  (content-   │  │  (Streamable HTTP, │  │
│  │  valid.) │  │   addressed) │  │   Go SDK)          │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │              Inference Providers                   │    │
│  │   Anthropic · Ollama · Claude Code · OpenRouter   │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

## Key Components

| Component | File(s) | What it does |
|-----------|---------|-------------|
| **Nucleus** | `nucleus.go` | Always-loaded identity context that is never evicted from the context window |
| **Process** | `process.go` | Four-state cognitive loop (Active → Receptive → Consolidating → Dormant) that runs independently of requests |
| **Context Assembly** | `context_assembly.go` | Foveated context engine that scores and ranks CogDocs and conversation history into stability-ordered zones within a token budget |
| **Ledger** | `ledger.go` | Append-only, hash-chained (SHA-256) event log for tamper-evident decision auditing |
| **Router** | `router.go` | Multi-provider inference routing with local-first sovereignty gradient and capability-based selection |
| **Salience** | `salience.go` | Git-derived attention scoring — uses commit frequency, recency, and file topology to score what matters |
| **Coherence** | `coherence.go` | Four-layer validation stack for internal consistency checking |
| **Blob Store** | `blobstore.go` | Content-addressed storage for ingested documents and artifacts |
| **MCP Server** | `mcp_server.go` | Streamable HTTP MCP endpoint using the official Go SDK, exposing memory, context, and tool operations |
| **Providers** | `provider_*.go` | Anthropic, Ollama, Claude Code, and Codex provider implementations with streaming support |
| **Field** | `field.go` | Continuous salience map over the memory corpus |
| **Gate** | `gate.go` | Event routing into the attentional fovea |

## API

The daemon exposes a standard HTTP API:

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Liveness + readiness probe |
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic Messages-compatible chat |
| `POST /v1/context/foveated` | Foveated context assembly |
| `GET /v1/context` | Current attentional field |
| `POST /v1/attention` | Emit attention signal |
| `GET /v1/constellation/fovea` | Current fovea state |
| `POST /mcp` | MCP Streamable HTTP endpoint |

Any OpenAI-compatible client works transparently — the context engine intercepts the messages array and manages what the model actually sees.

## Quick Start

```sh
# Build
make build

# Run the daemon
./cogos serve --workspace /path/to/workspace --port 5200

# Health check
curl http://localhost:5200/health

# Foveated context for current state
curl http://localhost:5200/v1/context
```

### Docker

```sh
make run
```

## Design Decisions

- **Continuous process, not request-triggered.** The daemon has internal tickers that fire regardless of external input — consolidation, salience updates, and heartbeat run on their own schedule. This is the core difference from typical agent frameworks.

- **Local-first inference routing.** The router scores local providers (Ollama) higher than cloud providers by default. Cloud is a fallback, not the primary path. This keeps data local and reduces cost.

- **Hash-chained ledger.** Every significant cognitive event is recorded with RFC 8785 canonical JSON + SHA-256 chaining. This provides tamper-evidence and causal ordering without requiring a blockchain.

- **Foveated context assembly.** Instead of naively stuffing the context window, the engine scores every piece of available context (CogDocs, conversation history, identity) and renders them into stability-ordered zones optimized for KV cache reuse.

- **Provider-agnostic.** The same daemon can route to Anthropic, Ollama, Claude Code's built-in model, or OpenRouter — selected per-request based on task requirements and provider capabilities.

## Project Status

CogOS is in active development. The v3 kernel (this repo) is the current architecture — a ground-up rewrite focused on the continuous process model.

- [x] Continuous process state machine with four operational states
- [x] Foveated context assembly with stability zones
- [x] Hash-chained event ledger
- [x] Multi-provider inference routing (Anthropic, Ollama, Claude Code, Codex)
- [x] MCP server (Streamable HTTP, Go SDK)
- [x] Content-addressed blob store
- [x] Git-derived salience scoring
- [x] OpenAI and Anthropic API compatibility
- [x] Embedded web dashboard
- [x] OpenTelemetry instrumentation
- [ ] Persistent memory consolidation loop
- [ ] Multi-agent process management
- [ ] Sentinel (routing feedback) training pipeline

## Requirements

- Go 1.25+
- macOS or Linux

## License

MIT
