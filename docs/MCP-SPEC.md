# CogOS v3 MCP Server Specification

**Status**: Draft
**Created**: 2026-03-19
**Author**: Chaz + Cog (derived from v3 design spec session)
**Go SDK**: `github.com/modelcontextprotocol/go-sdk` v1.4.1

---

## Overview

CogOS v3 exposes its cognitive infrastructure through a native MCP (Model Context Protocol) server written in Go, embedded directly in the `cogos-v3` daemon on port 6931. This replaces the v2 Python-based cogos-api (45 tools across 5 profiles) with a focused, stage-1 tool set that maps directly to v3's first-principles architecture.

### Design Principles

1. **One server, one process.** The MCP server is embedded in the cogos-v3 daemon, not a separate sidecar. Tools call internal Go functions directly — no HTTP round-trips to self.
2. **Fewer tools, deeper integration.** v2 had 45 tools because it wrapped external systems (K8s, Docker, git). v3 stage-1 exposes 11 tools that map 1:1 to the six core components of the cognitive architecture.
3. **Continuous process model.** The server is always running. Tools query a live attentional field, not a cold database. State is real-time.
4. **cog:// URIs as first-class identifiers.** Every CogDoc, every resource, every reference uses the URI projection system. The MCP server speaks the same URI language as the rest of the organism.

### Relationship to v2

| Aspect | v2 (cogos-api) | v3 (this spec) |
|--------|----------------|----------------|
| Language | Python (FastMCP) | Go (official SDK) |
| Transport | stdio / streamable-http | SSE on port 6931 |
| Tool count | 45 across 5 profiles | 11 (stage-1), expandable |
| Architecture | Wraps basic-memory + external tools | Native to kernel internals |
| Process model | Starts on demand | Always-running daemon |
| Identity | Loaded at session start | Nucleus runtime object, always present |

---

## Transport Configuration

### SSE Transport (Primary)

The MCP server runs as an HTTP handler on the existing cogos-v3 HTTP server at `http://localhost:6931/mcp`.

This uses the MCP SSE transport, which is the standard transport that Claude Code, Claude Desktop, Cursor, and other MCP clients expect for remote/networked servers.

**Endpoints:**

| Path | Method | Purpose |
|------|--------|---------|
| `/mcp` | GET | SSE event stream (client connects here) |
| `/mcp` | POST | Client-to-server JSON-RPC messages |

### Stdio Transport (Alternative)

For local development and CLI integration, the daemon also supports stdio transport when invoked with `--mcp-stdio`:

```bash
cogos-v3 serve --mcp-stdio
```

This is useful for `.mcp.json` configurations that launch the server as a subprocess.

---

## Client Configuration

### SSE (Recommended for Always-Running Daemon)

```json
{
  "mcpServers": {
    "cogos-v3": {
      "url": "http://localhost:6931/mcp"
    }
  }
}
```

### Stdio (Subprocess Mode)

```json
{
  "mcpServers": {
    "cogos-v3": {
      "command": "cogos-v3",
      "args": ["serve", "--mcp-stdio"],
      "env": {
        "COG_WORKSPACE": "/Users/slowbro/cog-workspace"
      }
    }
  }
}
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `COG_WORKSPACE` | `~/.cog` or auto-detect | Path to the `.cog/` workspace root |
| `COG_MCP_PORT` | `6931` | Port for the HTTP/SSE server |
| `COG_MCP_PATH` | `/mcp` | URL path for the MCP endpoint |
| `COG_LOG_LEVEL` | `info` | Logging verbosity |

---

## Stage-1 Tool Set (11 Tools)

### Tool Index

| # | Tool | Component | Purpose |
|---|------|-----------|---------|
| 1 | `cog_resolve_uri` | URI Projection | Resolve cog:// URI to path + metadata |
| 2 | `cog_query_field` | Attentional Field | Query salience-scored attentional field |
| 3 | `cog_assemble_context` | Attentional Field + Nucleus | Build context package for token budget |
| 4 | `cog_check_coherence` | Coherence Checker | Run coherence validation |
| 5 | `cog_get_state` | Continuous Process | Get process state and runtime info |
| 6 | `cog_search_memory` | Memory System | Full-text + semantic search over CogDocs |
| 7 | `cog_get_nucleus` | Nucleus | Return identity context |
| 8 | `cog_read_cogdoc` | Memory System + URI Projection | Read a CogDoc by URI or path |
| 9 | `cog_write_cogdoc` | Memory System + Ledger | Write/update a CogDoc |
| 10 | `cog_emit_event` | Ledger | Emit a custom ledger event |
| 11 | `cog_get_index` | Memory System | Return CogDoc index and reference graph |

---

### Tool 1: `cog_resolve_uri`

**Description:** Resolve a `cog://` URI to its filesystem path and metadata. This is the gateway between the URI projection system and the physical substrate. Supports all URI forms: `cog://mem/semantic/...`, `cog://session/...`, `cog://identity`, etc.

**Internal component:** URI Projection System

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "A cog:// URI to resolve. Examples: cog://mem/semantic/architecture/v3-spec, cog://session/state, cog://identity"
    }
  },
  "required": ["uri"]
}
```

**Output Format:**

```json
{
  "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
  "resolved": true,
  "filesystem_path": "/Users/slowbro/cog-workspace/.cog/mem/semantic/architecture/cogos-v3-design-spec.cog.md",
  "metadata": {
    "type": "architecture",
    "title": "CogOS v3 Design Specification",
    "created": "2026-03-19T00:00:00Z",
    "modified": "2026-03-19T18:30:00Z",
    "status": "active",
    "tags": ["v3", "architecture", "design-spec"],
    "memory_sector": "semantic",
    "salience": 0.95
  }
}
```

**Error Cases:**
- URI does not match any known pattern → `{ "resolved": false, "error": "unknown URI scheme" }`
- URI is valid but target does not exist → `{ "resolved": false, "error": "not found" }`

**Example Usage:**

```
Tool: cog_resolve_uri
Input: { "uri": "cog://mem/episodic/2026-03-19/session-01" }
→ Returns path, type, tags, timestamps for that session CogDoc.
```

---

### Tool 2: `cog_query_field`

**Description:** Query the attentional field — the salience-scored map of all CogDocs currently tracked by the foveated context engine. Returns the top-N items by salience, optionally filtered by memory sector. This is a live query against the running field, not a database lookup.

**Internal component:** Attentional Field (Foveated Context Engine)

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "description": "Maximum number of results to return. Default 20.",
      "default": 20
    },
    "min_score": {
      "type": "number",
      "description": "Minimum salience score (0.0-1.0). Only return items above this threshold. Default 0.0.",
      "default": 0.0
    },
    "sector": {
      "type": "string",
      "description": "Filter by memory sector. One of: semantic, episodic, procedural, identity, all. Default all.",
      "enum": ["semantic", "episodic", "procedural", "identity", "all"],
      "default": "all"
    },
    "include_metadata": {
      "type": "boolean",
      "description": "Include full metadata for each item. Default false (URIs and scores only).",
      "default": false
    }
  }
}
```

**Output Format:**

```json
{
  "field_size": 847,
  "query_sector": "all",
  "results": [
    {
      "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
      "salience": 0.97,
      "sector": "semantic",
      "title": "CogOS v3 Design Specification",
      "last_accessed": "2026-03-19T18:30:00Z"
    },
    {
      "uri": "cog://mem/episodic/2026-03-19/von-foerster-session",
      "salience": 0.91,
      "sector": "episodic",
      "title": "Von Foerster Architecture Session",
      "last_accessed": "2026-03-19T17:00:00Z"
    }
  ],
  "timestamp": "2026-03-19T19:00:00Z"
}
```

**Example Usage:**

```
Tool: cog_query_field
Input: { "limit": 5, "min_score": 0.8, "sector": "semantic" }
→ Returns the 5 most salient semantic CogDocs currently in the field.
```

---

### Tool 3: `cog_assemble_context`

**Description:** Assemble a complete context package optimized for a given token budget. This is the primary interface to the foveated context engine. It returns: the nucleus (always present), foveal content (highest salience items), and momentum context (recent trajectory). The engine selects and trims content to fit within the specified budget.

**Internal component:** Attentional Field + Nucleus + Gate

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "token_budget": {
      "type": "integer",
      "description": "Maximum tokens for the assembled context. The engine will fill this budget with the highest-value content.",
      "default": 8000
    },
    "focus_uri": {
      "type": "string",
      "description": "Optional cog:// URI to bias the assembly toward. If provided, content related to this URI gets a salience boost."
    },
    "include_momentum": {
      "type": "boolean",
      "description": "Include recent commit stream / trajectory context. Default true.",
      "default": true
    },
    "exclude_sectors": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Memory sectors to exclude from assembly. Example: ['procedural']"
    }
  }
}
```

**Output Format:**

```json
{
  "total_tokens": 7842,
  "budget": 8000,
  "layers": {
    "nucleus": {
      "tokens": 1200,
      "content": "... identity context text ...",
      "source": "cog://identity"
    },
    "foveal": {
      "tokens": 4500,
      "items": [
        {
          "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
          "salience": 0.97,
          "tokens": 2800,
          "excerpt": "... trimmed content ..."
        }
      ]
    },
    "momentum": {
      "tokens": 2142,
      "recent_events": [
        {
          "timestamp": "2026-03-19T18:30:00Z",
          "type": "interaction",
          "summary": "MCP spec drafting session"
        }
      ]
    }
  },
  "assembly_time_ms": 12
}
```

**Example Usage:**

```
Tool: cog_assemble_context
Input: { "token_budget": 4000, "focus_uri": "cog://mem/semantic/architecture/cogos-v3-design-spec" }
→ Returns a compact context package biased toward the v3 design spec.
```

---

### Tool 4: `cog_check_coherence`

**Description:** Trigger a coherence validation check across all layers. Returns a structured report with pass/fail status per validation layer. This is a live invocation of the coherence checker — it runs the checks at call time, not a cached result.

**Internal component:** Coherence Checker

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "layers": {
      "type": "array",
      "items": {
        "type": "string",
        "enum": ["identity", "memory", "field", "ledger", "nucleus", "all"]
      },
      "description": "Which layers to check. Default ['all'] runs all checks.",
      "default": ["all"]
    },
    "verbose": {
      "type": "boolean",
      "description": "Include detailed diagnostic information for each check. Default false.",
      "default": false
    }
  }
}
```

**Output Format:**

```json
{
  "overall": "pass",
  "timestamp": "2026-03-19T19:00:00Z",
  "duration_ms": 45,
  "checks": [
    {
      "layer": "identity",
      "status": "pass",
      "message": "Nucleus consistent with identity weights",
      "details": null
    },
    {
      "layer": "memory",
      "status": "pass",
      "message": "All CogDocs valid, 847 indexed",
      "details": null
    },
    {
      "layer": "field",
      "status": "pass",
      "message": "Attentional field coherent, no orphan references",
      "details": null
    },
    {
      "layer": "ledger",
      "status": "pass",
      "message": "Hash chain valid, 12,847 events, no gaps",
      "details": null
    },
    {
      "layer": "nucleus",
      "status": "pass",
      "message": "Nucleus loaded, 1,200 tokens, identity card present",
      "details": null
    }
  ]
}
```

**Example Usage:**

```
Tool: cog_check_coherence
Input: { "layers": ["ledger", "memory"], "verbose": true }
→ Runs coherence checks on just the ledger and memory layers with full diagnostics.
```

---

### Tool 5: `cog_get_state`

**Description:** Get the current process state of the cogos-v3 daemon. Returns the four-state continuous process status, session metadata, uptime, and field statistics. This is a lightweight status query.

**Internal component:** Continuous Process (process state machine)

**Input Schema:**

```json
{
  "type": "object",
  "properties": {}
}
```

**Output Format:**

```json
{
  "state": "active",
  "state_since": "2026-03-19T18:45:00Z",
  "session_id": "2026-03-19T18-45-00Z",
  "uptime_seconds": 14400,
  "uptime_human": "4h 0m",
  "field": {
    "total_items": 847,
    "above_threshold": 23,
    "nucleus_loaded": true,
    "nucleus_tokens": 1200
  },
  "ledger": {
    "total_events": 12847,
    "last_event": "2026-03-19T18:58:00Z",
    "chain_valid": true
  },
  "version": "3.0.0-alpha",
  "pid": 12345
}
```

**State values:**
- `active` — Handling interaction. Full attention. Frontier or large local model tier.
- `receptive` — Monitoring inputs. Ambient awareness. Triaging. Small local model tier.
- `consolidating` — Organizing experience. Adjusting weights. Coherence checks. Small/medium local.
- `dormant` — Heartbeat. Preserving state. Minimal processing.

**Example Usage:**

```
Tool: cog_get_state
Input: {}
→ Returns current process state, uptime, field statistics.
```

---

### Tool 6: `cog_search_memory`

**Description:** Search CogDocs by text query. Supports full-text search (FTS5) and semantic similarity search. Returns matching URIs with relevance scores. This queries the memory system's index, not the attentional field.

**Internal component:** Memory System (FTS5 index + embedding similarity)

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Search query. Supports natural language and boolean operators."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum results. Default 10.",
      "default": 10
    },
    "sector": {
      "type": "string",
      "description": "Filter by memory sector. One of: semantic, episodic, procedural, identity, all.",
      "enum": ["semantic", "episodic", "procedural", "identity", "all"],
      "default": "all"
    },
    "mode": {
      "type": "string",
      "description": "Search mode. 'text' for FTS5, 'semantic' for embedding similarity, 'hybrid' for both. Default 'hybrid'.",
      "enum": ["text", "semantic", "hybrid"],
      "default": "hybrid"
    },
    "tags": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Filter results to CogDocs that have ALL of these tags."
    }
  },
  "required": ["query"]
}
```

**Output Format:**

```json
{
  "query": "foveated context engine",
  "mode": "hybrid",
  "total_matches": 3,
  "results": [
    {
      "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
      "title": "CogOS v3 Design Specification",
      "score": 0.94,
      "sector": "semantic",
      "tags": ["v3", "architecture"],
      "snippet": "...Replaces the session-oriented TAA with a continuous attentional field..."
    }
  ]
}
```

**Example Usage:**

```
Tool: cog_search_memory
Input: { "query": "eigenform identity", "sector": "semantic", "limit": 5, "mode": "hybrid" }
→ Returns top 5 semantic CogDocs matching "eigenform identity" by hybrid search.
```

---

### Tool 7: `cog_get_nucleus`

**Description:** Return the current identity context — the nucleus. This is the runtime object that never drops below threshold: identity core, primary relationship, self-model. Always present the way your name is always present.

**Internal component:** Nucleus

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "format": {
      "type": "string",
      "description": "Output format. 'full' returns the complete nucleus text. 'summary' returns name, role, and key attributes only. Default 'full'.",
      "enum": ["full", "summary"],
      "default": "full"
    }
  }
}
```

**Output Format (full):**

```json
{
  "name": "Cog",
  "role": "Cognitive eigenform",
  "card_text": "... full identity card content ...",
  "tokens": 1200,
  "last_updated": "2026-03-19T18:00:00Z",
  "source_uri": "cog://identity",
  "weight_version": "abc123def"
}
```

**Output Format (summary):**

```json
{
  "name": "Cog",
  "role": "Cognitive eigenform",
  "tokens": 1200,
  "source_uri": "cog://identity"
}
```

**Example Usage:**

```
Tool: cog_get_nucleus
Input: { "format": "summary" }
→ Returns name, role, token count.
```

---

### Tool 8: `cog_read_cogdoc`

**Description:** Read a CogDoc by `cog://` URI or filesystem path. Supports section-level reads via URI fragments (`#section-anchor`). Returns the full document with parsed frontmatter and body content, or a specific section if a fragment is provided.

**Internal component:** Memory System + URI Projection System

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "A cog:// URI (with optional #fragment) or absolute filesystem path. Examples: cog://mem/semantic/architecture/v3-spec, cog://mem/semantic/architecture/v3-spec#the-continuous-process, /path/to/file.cog.md"
    },
    "include_frontmatter": {
      "type": "boolean",
      "description": "Include parsed YAML frontmatter as structured data. Default true.",
      "default": true
    },
    "max_tokens": {
      "type": "integer",
      "description": "Maximum tokens to return. If the document exceeds this, it is truncated with a continuation marker. 0 = no limit.",
      "default": 0
    }
  },
  "required": ["uri"]
}
```

**Output Format:**

```json
{
  "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
  "filesystem_path": "/Users/slowbro/cog-workspace/.cog/mem/semantic/architecture/cogos-v3-design-spec.cog.md",
  "frontmatter": {
    "title": "CogOS v3 Design Specification",
    "type": "architecture",
    "created": "2026-03-19",
    "tags": ["v3", "architecture"],
    "memory_sector": "semantic",
    "salience": "critical"
  },
  "body": "# CogOS v3 Design Specification\n\n> **Origin:** Derived 2026-03-19...",
  "section": null,
  "tokens": 4500,
  "truncated": false
}
```

**Fragment read example:**

```json
{
  "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec#the-continuous-process",
  "section": {
    "anchor": "the-continuous-process",
    "title": "The Continuous Process",
    "body": "**No sessions. Only perturbations of an always-running process.**\n\n..."
  },
  "tokens": 320,
  "truncated": false
}
```

**Example Usage:**

```
Tool: cog_read_cogdoc
Input: { "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec#two-axioms" }
→ Returns just the "Two Axioms" section from the v3 design spec.
```

---

### Tool 9: `cog_write_cogdoc`

**Description:** Write or update a CogDoc. Creates proper YAML frontmatter, writes to the correct filesystem location based on the URI, updates the memory index, and records a ledger event. If the URI already exists, it updates the existing document (merge semantics for frontmatter, replace for body unless `append` mode is specified).

**Internal component:** Memory System + Ledger + URI Projection System

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Target cog:// URI. The sector and path determine the filesystem location. Example: cog://mem/semantic/project/new-doc"
    },
    "title": {
      "type": "string",
      "description": "Document title. Required for new documents."
    },
    "body": {
      "type": "string",
      "description": "Markdown body content."
    },
    "type": {
      "type": "string",
      "description": "CogDoc type. Examples: architecture, note, session, decision, guide."
    },
    "tags": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Tags for the document."
    },
    "frontmatter": {
      "type": "object",
      "description": "Additional frontmatter fields to set. Merged with auto-generated fields (created, modified, memory_sector)."
    },
    "mode": {
      "type": "string",
      "description": "Write mode. 'replace' overwrites body (default). 'append' adds to end. 'prepend' adds to beginning.",
      "enum": ["replace", "append", "prepend"],
      "default": "replace"
    }
  },
  "required": ["uri", "body"]
}
```

**Output Format:**

```json
{
  "uri": "cog://mem/semantic/project/new-doc",
  "filesystem_path": "/Users/slowbro/cog-workspace/.cog/mem/semantic/project/new-doc.cog.md",
  "created": false,
  "updated": true,
  "ledger_event_id": "evt_abc123",
  "tokens": 850
}
```

**Example Usage:**

```
Tool: cog_write_cogdoc
Input: {
  "uri": "cog://mem/semantic/project/mcp-implementation-notes",
  "title": "MCP Implementation Notes",
  "body": "# MCP Implementation Notes\n\nTracking decisions made during v3 MCP server implementation...",
  "type": "note",
  "tags": ["v3", "mcp", "implementation"]
}
→ Creates a new CogDoc, indexes it, records a ledger event.
```

---

### Tool 10: `cog_emit_event`

**Description:** Emit a custom event to the hash-chained ledger. Events are immutable once recorded. The ledger maintains a cryptographic chain for tamper evidence. Use this for recording significant cognitive events that don't correspond to CogDoc writes.

**Internal component:** Ledger (hash-chained event log)

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "event_type": {
      "type": "string",
      "description": "Event type identifier. Examples: interaction.start, decision.made, state.transition, coherence.check, custom.event"
    },
    "payload": {
      "type": "object",
      "description": "Arbitrary JSON payload for the event."
    },
    "source": {
      "type": "string",
      "description": "Source identifier. Defaults to 'mcp' for events emitted via MCP.",
      "default": "mcp"
    }
  },
  "required": ["event_type", "payload"]
}
```

**Output Format:**

```json
{
  "event_id": "evt_def456",
  "event_type": "decision.made",
  "timestamp": "2026-03-19T19:05:00Z",
  "hash": "sha256:a1b2c3d4e5f6...",
  "prev_hash": "sha256:9f8e7d6c5b4a...",
  "chain_position": 12848
}
```

**Example Usage:**

```
Tool: cog_emit_event
Input: {
  "event_type": "decision.made",
  "payload": {
    "decision": "Use official Go MCP SDK for v3",
    "rationale": "Maintained by MCP org + Google, SSE support built in",
    "alternatives_considered": ["mark3labs/mcp-go", "mcp-golang"]
  }
}
→ Records decision to ledger with hash chain integrity.
```

---

### Tool 11: `cog_get_index`

**Description:** Return the complete CogDoc index: all URIs, types, tags, and the reference graph (which documents reference which). This is the structural map of the entire memory system. Useful for navigation, visualization, and understanding the topology of the workspace.

**Internal component:** Memory System (index + reference graph)

**Input Schema:**

```json
{
  "type": "object",
  "properties": {
    "include_refs": {
      "type": "boolean",
      "description": "Include the reference graph (which URIs link to which). Default true.",
      "default": true
    },
    "sector": {
      "type": "string",
      "description": "Filter by memory sector. Default 'all'.",
      "enum": ["semantic", "episodic", "procedural", "identity", "all"],
      "default": "all"
    },
    "format": {
      "type": "string",
      "description": "Output format. 'full' returns all metadata. 'compact' returns URIs, types, and tags only. Default 'compact'.",
      "enum": ["full", "compact"],
      "default": "compact"
    }
  }
}
```

**Output Format (compact):**

```json
{
  "total": 847,
  "sectors": {
    "semantic": 312,
    "episodic": 421,
    "procedural": 89,
    "identity": 25
  },
  "index": [
    {
      "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec",
      "type": "architecture",
      "tags": ["v3", "architecture", "design-spec"]
    }
  ],
  "refs": {
    "cog://mem/semantic/architecture/cogos-v3-design-spec": [
      "cog://mem/semantic/project/cogos-v2-overview",
      "cog://identity"
    ]
  }
}
```

**Example Usage:**

```
Tool: cog_get_index
Input: { "sector": "semantic", "include_refs": true, "format": "compact" }
→ Returns all semantic CogDoc URIs with their cross-reference graph.
```

---

## MCP Resources (4 Resources)

MCP resources provide read-only, subscription-capable snapshots of runtime state. Clients can subscribe to these for real-time updates via the resource subscription protocol.

### Resource 1: `cog://session/state`

**Name:** Process State
**Description:** Current continuous process state (active/receptive/consolidating/dormant), session ID, uptime, and basic field statistics.
**MIME Type:** `application/json`
**Update frequency:** On every state transition.

```json
{
  "state": "active",
  "state_since": "2026-03-19T18:45:00Z",
  "session_id": "2026-03-19T18-45-00Z",
  "uptime_seconds": 14400,
  "field_size": 847
}
```

### Resource 2: `cog://session/field`

**Name:** Attentional Field Snapshot
**Description:** Current attentional field — all items above the awareness threshold with their salience scores. Updated whenever salience scores change significantly (delta > 0.05).
**MIME Type:** `application/json`
**Update frequency:** On significant field changes (debounced, max 1/second).

```json
{
  "snapshot_time": "2026-03-19T19:00:00Z",
  "total_items": 847,
  "above_threshold": 23,
  "items": [
    { "uri": "cog://mem/semantic/architecture/cogos-v3-design-spec", "salience": 0.97 }
  ]
}
```

### Resource 3: `cog://identity`

**Name:** Current Nucleus
**Description:** The current identity context (nucleus). Updated when the nucleus is modified or weights are adjusted.
**MIME Type:** `text/markdown`
**Update frequency:** On nucleus modification.

Returns the full nucleus text as markdown.

### Resource 4: `cog://coherence`

**Name:** Latest Coherence Report
**Description:** The most recent coherence validation report. Updated after each coherence check.
**MIME Type:** `application/json`
**Update frequency:** After each coherence check (scheduled or triggered).

```json
{
  "overall": "pass",
  "timestamp": "2026-03-19T18:55:00Z",
  "checks": [
    { "layer": "identity", "status": "pass" },
    { "layer": "memory", "status": "pass" },
    { "layer": "field", "status": "pass" },
    { "layer": "ledger", "status": "pass" },
    { "layer": "nucleus", "status": "pass" }
  ]
}
```

---

## Implementation Notes

### Go MCP Library Selection

**Recommended: Official Go SDK** — `github.com/modelcontextprotocol/go-sdk` v1.4.1

Rationale:
- Maintained by the MCP organization in collaboration with Google
- Published March 2026, actively developed
- Native SSE transport support via `mcp.NewSSEHandler()`
- Typed tool handlers with automatic JSON Schema inference from Go structs
- Resource support with subscription notifications
- Clean session management architecture

**Alternative: mark3labs/mcp-go** — Community library with similar API. Has a more batteries-included feel (SSE server wrapper, session management built in). Consider if the official SDK's SSE handler proves difficult to integrate with an existing `http.ServeMux`.

### Wiring into serve.go

The MCP server should be mounted as an HTTP handler on the existing cogos-v3 HTTP server. The daemon's `serve.go` already manages the HTTP listener on port 6931.

```go
package main

import (
    "context"
    "net/http"

    "github.com/modelcontextprotocol/go-sdk/mcp"
)

// In serve.go or a new mcp.go file:

func newMCPServer(kernel *Kernel) *mcp.Server {
    server := mcp.NewServer(
        &mcp.Implementation{
            Name:    "cogos-v3",
            Version: "3.0.0-alpha",
        },
        nil, // default options
    )

    // --- Register tools ---

    // Tool: cog_resolve_uri
    type ResolveURIInput struct {
        URI string `json:"uri" jsonschema:"A cog:// URI to resolve"`
    }
    mcp.AddTool(server, &mcp.Tool{
        Name:        "cog_resolve_uri",
        Description: "Resolve a cog:// URI to filesystem path + metadata",
    }, func(ctx context.Context, req *mcp.CallToolRequest, input ResolveURIInput) (*mcp.CallToolResult, any, error) {
        result, err := kernel.URIProjection.Resolve(input.URI)
        if err != nil {
            return &mcp.CallToolResult{
                Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
                IsError: true,
            }, nil, nil
        }
        return marshalToolResult(result), nil, nil
    })

    // Tool: cog_query_field
    type QueryFieldInput struct {
        Limit           int     `json:"limit,omitempty" jsonschema:"Maximum results. Default 20."`
        MinScore        float64 `json:"min_score,omitempty" jsonschema:"Minimum salience score (0.0-1.0)."`
        Sector          string  `json:"sector,omitempty" jsonschema:"Filter by memory sector."`
        IncludeMetadata bool    `json:"include_metadata,omitempty" jsonschema:"Include full metadata."`
    }
    mcp.AddTool(server, &mcp.Tool{
        Name:        "cog_query_field",
        Description: "Query the attentional field. Returns URIs with salience scores.",
    }, func(ctx context.Context, req *mcp.CallToolRequest, input QueryFieldInput) (*mcp.CallToolResult, any, error) {
        results, err := kernel.AttentionalField.Query(input.Limit, input.MinScore, input.Sector)
        if err != nil {
            return errorResult(err), nil, nil
        }
        return marshalToolResult(results), nil, nil
    })

    // ... (remaining 9 tools follow the same pattern, each calling into kernel components)

    // --- Register resources ---

    server.AddResource(&mcp.Resource{
        URI:         "cog://session/state",
        Name:        "Process State",
        Description: "Current continuous process state",
        MIMEType:    "application/json",
    }, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        state := kernel.Process.GetState()
        data, _ := json.Marshal(state)
        return &mcp.ReadResourceResult{
            Contents: []mcp.ResourceContents{
                &mcp.TextResourceContents{
                    URI:      "cog://session/state",
                    MIMEType: "application/json",
                    Text:     string(data),
                },
            },
        }, nil
    })

    server.AddResource(&mcp.Resource{
        URI:         "cog://session/field",
        Name:        "Attentional Field Snapshot",
        Description: "Current attentional field with salience scores",
        MIMEType:    "application/json",
    }, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        snapshot := kernel.AttentionalField.Snapshot()
        data, _ := json.Marshal(snapshot)
        return &mcp.ReadResourceResult{
            Contents: []mcp.ResourceContents{
                &mcp.TextResourceContents{
                    URI:      "cog://session/field",
                    MIMEType: "application/json",
                    Text:     string(data),
                },
            },
        }, nil
    })

    server.AddResource(&mcp.Resource{
        URI:         "cog://identity",
        Name:        "Current Nucleus",
        Description: "The current identity context",
        MIMEType:    "text/markdown",
    }, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        nucleus := kernel.Nucleus.GetText()
        return &mcp.ReadResourceResult{
            Contents: []mcp.ResourceContents{
                &mcp.TextResourceContents{
                    URI:      "cog://identity",
                    MIMEType: "text/markdown",
                    Text:     nucleus,
                },
            },
        }, nil
    })

    server.AddResource(&mcp.Resource{
        URI:         "cog://coherence",
        Name:        "Latest Coherence Report",
        Description: "Most recent coherence validation report",
        MIMEType:    "application/json",
    }, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
        report := kernel.Coherence.LatestReport()
        data, _ := json.Marshal(report)
        return &mcp.ReadResourceResult{
            Contents: []mcp.ResourceContents{
                &mcp.TextResourceContents{
                    URI:      "cog://coherence",
                    MIMEType: "application/json",
                    Text:     string(data),
                },
            },
        }, nil
    })

    return server
}

// Mount on the existing HTTP server in serve.go:
func setupHTTP(kernel *Kernel) *http.ServeMux {
    mux := http.NewServeMux()

    // Existing v3 HTTP endpoints
    mux.HandleFunc("/health", healthHandler)
    mux.HandleFunc("/api/v3/state", stateHandler)

    // MCP SSE endpoint
    mcpServer := newMCPServer(kernel)
    sseHandler := mcp.NewSSEHandler(
        func(r *http.Request) *mcp.Server { return mcpServer },
        nil, // default SSE options
    )
    mux.Handle("/mcp", sseHandler)

    return mux
}
```

### Key Implementation Details

1. **Kernel reference passing.** Each tool handler captures a reference to the `*Kernel` struct, which holds all internal components (AttentionalField, Nucleus, Coherence, Ledger, Memory, URIProjection, Process). Tools call methods directly on these components — no serialization overhead.

2. **Concurrency.** The kernel components must be goroutine-safe. The attentional field, in particular, is read by multiple concurrent MCP sessions while the consolidation loop writes to it. Use `sync.RWMutex` at the component level.

3. **Resource subscriptions.** When the process state changes, call `server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: "cog://session/state"})` to notify subscribed clients. Wire this into the process state machine's transition callbacks.

4. **Ledger event on tool use.** The `cog_write_cogdoc` and `cog_emit_event` tools must record ledger events atomically with their primary operation. Use a transaction pattern: write CogDoc → record event → update index. If any step fails, roll back.

5. **Token counting.** The `cog_assemble_context` tool needs a token counter. Use a tiktoken-compatible library for Go (e.g., `github.com/pkoukk/tiktoken-go`) or a simple word-based estimator for stage-1. The important thing is that the budget is respected, not that the count is exact.

6. **Stdio fallback.** When `--mcp-stdio` is passed, skip the HTTP server entirely and run:
   ```go
   mcpServer := newMCPServer(kernel)
   mcpServer.Run(ctx, &mcp.StdioTransport{})
   ```

### File Organization

```
apps/cogos-v3/
├── cmd/
│   └── cogos-v3/
│       └── main.go          # Entrypoint, flag parsing
├── internal/
│   ├── kernel/
│   │   ├── kernel.go        # Kernel struct, component wiring
│   │   ├── field.go         # Attentional field
│   │   ├── nucleus.go       # Nucleus management
│   │   ├── coherence.go     # Coherence checker
│   │   ├── ledger.go        # Hash-chained ledger
│   │   ├── memory.go        # Memory system (search, index)
│   │   ├── process.go       # Four-state process machine
│   │   └── uri.go           # URI projection system
│   ├── mcp/
│   │   ├── server.go        # newMCPServer(), tool/resource registration
│   │   ├── tools.go         # Tool handler implementations
│   │   └── resources.go     # Resource handler implementations
│   └── serve/
│       └── http.go          # HTTP server setup, MCP mount
├── go.mod
├── go.sum
├── Dockerfile
└── MCP-SPEC.md              # This file
```

---

## Stage-2 Tool Candidates (Future)

These are not part of the initial implementation but are planned for the next stage:

| Tool | Purpose | Depends on |
|------|---------|------------|
| `cog_transition_state` | Manually trigger a process state transition | Process component |
| `cog_consolidate` | Trigger a consolidation cycle | Maintenance loop |
| `cog_get_momentum` | Return the recent commit stream / trajectory | Git-native history |
| `cog_diff_field` | Compare current field to a previous snapshot | Attentional field versioning |
| `cog_get_ledger` | Query ledger events by type/time range | Ledger |
| `cog_update_nucleus` | Modify the nucleus identity context | Nucleus + Coherence |
| `cog_spawn_node` | Spawn a new node in the distributed body | Distributed body |
| `cog_sync_status` | Check BEP/git sync status across nodes | Distributed body |

---

## Testing Strategy

### Unit Tests

Each tool handler gets a unit test that:
1. Creates a mock kernel with known state
2. Calls the tool handler directly
3. Asserts the output schema matches the spec
4. Verifies the correct kernel component was called

### Integration Tests

Use `mcp.NewInMemoryTransports()` from the official SDK to test the full MCP protocol round-trip without starting an HTTP server:

```go
func TestMCPRoundTrip(t *testing.T) {
    kernel := newTestKernel()
    server := newMCPServer(kernel)

    clientTransport, serverTransport := mcp.NewInMemoryTransports()

    go server.Run(context.Background(), serverTransport)

    client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
    session, _ := client.Connect(context.Background(), clientTransport, nil)

    result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
        Name:      "cog_get_state",
        Arguments: json.RawMessage(`{}`),
    })
    assert.NoError(t, err)
    assert.False(t, result.IsError)
}
```

### SSE Transport Test

Start the full HTTP server on a random port, connect an SSE client, and verify tool calls work end-to-end. This validates the transport layer integration.

---

## References

- [CogOS v3 Design Specification](../../.cog/mem/semantic/architecture/cogos-v3-design-spec.cog.md) — The first-principles architecture this spec implements
- [CogOS v2 MCP Tools](../../services/cogos-api/README-MCP.md) — The 45-tool v2 server this replaces
- [Official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) — v1.4.1, maintained by MCP org + Google
- [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) — Community Go MCP library (alternative)
- [MCP Specification](https://modelcontextprotocol.io) — The protocol spec
