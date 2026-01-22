# CogOS

**CogOS** is a cognitive operating system platform that provides:

- **Hierarchical Memory Domains (HMD)** - Persistent memory across sessions
- **Inference Routing** - Multi-provider LLM access (Claude, OpenRouter, local)
- **Session Ledger** - Cryptographic event tracking
- **OpenAI-compatible API** - `cog serve` exposes standard endpoints
- **Workspace Management** - Multi-workspace support with identity

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Your Workspace (Private)                      │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │ .cog/                                                      │ │
│  │   mem/          ← Your memories (semantic, episodic, etc.) │ │
│  │   ledger/       ← Your session history                     │ │
│  │   config/       ← Your preferences                         │ │
│  └────────────────────────────────────────────────────────────┘ │
│                              ▲                                   │
│                              │ manages                           │
│                    ┌─────────┴─────────┐                        │
│                    │   cog (kernel)    │                        │
│                    │   JSON Schema API │                        │
│                    └─────────┬─────────┘                        │
│                              │                                   │
│              ┌───────────────┼───────────────┐                  │
│              ▼               ▼               ▼                  │
│         ┌────────┐    ┌───────────┐    ┌─────────┐             │
│         │ Claude │    │OpenRouter │    │  Local  │             │
│         │  (Max) │    │           │    │(Ollama) │             │
│         └────────┘    └───────────┘    └─────────┘             │
└─────────────────────────────────────────────────────────────────┘
```

## Installation

```bash
# Build from source
make build

# Or build for all platforms
make all
```

## Usage

```bash
# Core commands
cog health              # Workspace health check
cog verify              # Validate cogdocs
cog coherence check     # Check workspace coherence

# Memory operations
cog memory search "topic"
cog memory list semantic

# Inference
cog infer "prompt"
cog infer --model openrouter/anthropic/claude-3.5-sonnet "prompt"

# API Server
cog serve start --port 5100
cog serve status
cog serve stop
```

## JSON Schema API

The kernel exposes all functionality through JSON schemas, enabling:

- **Frontend agnostic** - Any UI can connect (CogCode, custom, etc.)
- **Language agnostic** - SDKs in any language
- **Protocol stability** - Schema versioning

### OpenAI-Compatible Endpoints

```
POST /v1/chat/completions    # Chat completions (streaming supported)
GET  /v1/models              # Available models
POST /v1/embeddings          # Text embeddings
```

### CogOS-Specific Endpoints

```
GET  /cog/health             # Workspace health
GET  /cog/memory/search      # Memory search
POST /cog/memory/write       # Write to memory
GET  /cog/coherence          # Coherence status
```

## Frontends

CogOS is designed to work with any frontend that speaks its JSON API:

- **[CogCode](https://github.com/cogos-dev/cogcode)** - Official desktop/CLI/web frontend
- **Custom** - Build your own using the SDK

## SDK

```go
import "github.com/cogos-dev/cogos/sdk"

// Connect to workspace
ws, err := sdk.OpenWorkspace(".")

// Search memory
results, err := ws.Memory.Search("topic")

// Run inference
resp, err := ws.Infer("What is this code doing?")
```

## License

MIT
