# CogOS

**Your AI agents forget everything between sessions. CogOS doesn't.**

CogOS is a daemon that runs alongside your AI coding agents — Claude Code, Cursor, Gemini CLI, whatever you use — and gives them persistent memory, intelligent context assembly, and multi-provider inference routing. It's not a framework. It's not a library. It's a kernel that runs continuously and maintains cognitive state while agents come and go.

```sh
# Install and initialize a workspace
make build
./cogos init --workspace ~/my-project
./cogos serve --workspace ~/my-project

# That's it. Your agents now have memory.
curl http://localhost:5200/health
```

## What it actually does

**Without CogOS:** Each agent session starts from zero. You re-explain context. The agent re-reads files it already understood yesterday. It can't remember what you decided last week.

**With CogOS:** The daemon maintains a continuous attentional field over your workspace. It knows which files matter right now (git-derived salience scoring), what the agent said last time (persistent memory), and which model should handle this request (local-first routing with cloud fallback). When a new session starts, the context engine assembles exactly the right context — scored, ranked, and budget-fitted — without the agent having to ask.

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
│  │          │  │              │  │                    │  │
│  │ Identity │  │  Active      │  │  Zone 0: Nucleus   │  │
│  │ context  │  │  Receptive   │  │  Zone 1: Knowledge │  │
│  │ (always  │  │  Consolidate │  │  Zone 2: History   │  │
│  │  loaded) │  │  Dormant     │  │  Zone 3: Current   │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │  Ledger  │  │   Router     │  │    Salience        │  │
│  │  (hash-  │  │  (local-first│  │  (git-derived      │  │
│  │  chained)│  │   routing)   │  │   attention)       │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │Coherence │  │  Blob Store  │  │   MCP Server       │  │
│  └──────────┘  └──────────────┘  └───────────────────┘  │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │              Inference Providers                   │    │
│  │    Anthropic · Ollama · Claude Code · Codex       │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

## Quick start

```sh
# Clone and build
git clone https://github.com/cogos-dev/cogos.git
cd cogos
make build

# Initialize a workspace (creates .cog/ with config, memory dirs, identity)
./cogos init --workspace ~/my-project

# Start the daemon
./cogos serve --workspace ~/my-project

# Verify
curl -s http://localhost:5200/health | jq .
```

### Docker

```sh
make e2e          # Build + run full cold-start test in a container
make image        # Build production image
make run          # Run in Docker with workspace mount
```

## How it's different

CogOS is not LangChain, CrewAI, or another agent framework. Here's why:

| | Agent frameworks | CogOS |
|---|---|---|
| **Lifecycle** | Runs when called, dies when done | Continuous daemon with internal tickers |
| **Memory** | Bolted on (vector DB, external store) | Native hierarchical memory with consolidation |
| **Context** | Naive stuffing or basic RAG | Foveated assembly with stability zones and KV cache optimization |
| **Models** | One provider, hardcoded | Multi-provider routing with sovereignty gradient |
| **Trust** | None | Hash-chained ledger (SHA-256, RFC 8785) for every decision |
| **Integration** | Replace your agent | Sits *behind* any agent (Claude Code, Cursor, etc.) |

### The continuous process

Most agent systems are request-triggered — nothing happens until you send a message. CogOS has a four-state process loop that runs regardless:

- **Active** — processing an external request
- **Receptive** — idle, listening, maintaining the attentional field
- **Consolidating** — running internal maintenance (memory consolidation, coherence checks, salience updates)
- **Dormant** — minimal activity, heartbeat only

This means the daemon is always aware of what changed in your workspace, even between sessions.

### Foveated context assembly

Instead of dumping everything into the context window, CogOS scores every available piece of context and arranges it into stability zones:

- **Zone 0 (Nucleus):** Identity — always present, never evicted
- **Zone 1 (Knowledge):** CogDocs and workspace knowledge — shifts slowly, high cache hit rate
- **Zone 2 (History):** Conversation history — scored by relevance + recency, evictable
- **Zone 3 (Current):** The current message — always present

Zones are ordered for KV cache reuse. Stable content stays in the same position across requests, so the model's cache isn't invalidated unnecessarily.

### Local-first inference routing

The router scores providers on a sovereignty gradient: local models are preferred over cloud. If Ollama is running, it gets the request. Cloud APIs are fallbacks, not defaults.

```yaml
# providers.yaml
providers:
  ollama:
    type: ollama
    enabled: true
    endpoint: "http://localhost:11434"
    model: "llama3.2"

  anthropic:
    type: anthropic
    enabled: false  # enable when needed
    api_key_env: ANTHROPIC_API_KEY
    model: "claude-sonnet-4-20250514"

routing:
  prefer_local: true
  fallback_chain: [ollama, anthropic]
```

Custom providers can be added by implementing the [Provider interface](docs/writing-a-provider.md) — six methods, same pattern as Terraform providers.

## API

Any OpenAI-compatible client works. The context engine intercepts the messages array and assembles what the model actually sees.

| Endpoint | What it does |
|----------|-------------|
| `POST /v1/chat/completions` | OpenAI-compatible chat (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic Messages-compatible chat |
| `POST /v1/context/foveated` | Foveated context assembly for harness hooks |
| `GET /v1/context` | Current attentional field state |
| `GET /health` | Liveness probe with identity, state, and trust info |
| `POST /mcp` | MCP Streamable HTTP endpoint (Go SDK) |

## Project layout

```
cmd/cogos/                 Entry point
internal/engine/           Kernel implementation (90 source files, 33 test files)
  process.go               Four-state cognitive loop
  nucleus.go               Always-loaded identity context
  context_assembly.go      Foveated context engine with stability zones
  serve.go                 HTTP API (OpenAI + Anthropic compatible)
  ledger.go                Hash-chained event log (SHA-256, RFC 8785)
  router.go                Multi-provider inference routing
  provider.go              Provider interface definition
  provider_anthropic.go    Anthropic Claude API
  provider_ollama.go       Ollama local inference
  provider_claudecode.go   Claude Code agentic subprocess spawning
  mcp_server.go            MCP Streamable HTTP server (official Go SDK)
  salience.go              Git-derived attention scoring
  coherence.go             4-layer validation stack
  blobstore.go             Content-addressed storage
  init.go                  Workspace scaffolding (cogos init)
  defaults/                Embedded default configs and identity
docs/
  writing-a-provider.md    Guide for writing custom providers
  MCP-SPEC.md              MCP server specification
  PROVIDER-SPEC.md         Provider contract specification
scripts/
  e2e-test.sh              End-to-end cold-start test (15 checks)
Dockerfile                 Production multi-stage build
Dockerfile.e2e             Containerized e2e test
```

## Testing

```sh
make test         # Unit tests
make e2e-local    # Full cold-start lifecycle test (local)
make e2e          # Same test, containerized
```

## Status

This is the v3 kernel — a ground-up rewrite after a year of iteration. The architecture has been validated through daily use across multiple agent harnesses.

What's working:
- Continuous process with four operational states
- Foveated context assembly with stability zones
- Hash-chained event ledger
- Multi-provider inference routing (Anthropic, Ollama, Claude Code, Codex)
- MCP server (Streamable HTTP)
- Content-addressed blob store
- Git-derived salience scoring
- OpenAI and Anthropic API compatibility
- Workspace scaffolding (`cogos init`)
- End-to-end containerized testing
- Embedded web dashboard
- OpenTelemetry instrumentation

What's next:
- Persistent memory consolidation loop
- Multi-agent process management
- Sentinel (routing feedback) training pipeline
- `cog` CLI as the user-facing shell

## Requirements

- Go 1.24+
- macOS or Linux
- Docker (optional, for containerized testing and deployment)

## License

MIT
