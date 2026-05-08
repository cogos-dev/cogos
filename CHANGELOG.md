# Changelog

> **Going forward (v0.4.0 onward):** this project does not maintain hand-curated changelog entries. PR titles and bodies are the canonical record; auto-generated GitHub release notes are the per-release projection.
>
> - **Per-release summaries:** [Releases page](https://github.com/myrgic/cogos/releases)
> - **Detailed PR list per release:** auto-generated from PR titles since the previous tag (via `gh release create --generate-notes`)
> - **Per-PR rationale:** the linked PR's description and discussion
>
> See [CONTRIBUTING.md](CONTRIBUTING.md) ("Pull request titles and bodies") for the conventions that make those notes readable.

## Versioning

Pre-1.0. Per semver convention, breaking changes can land at any minor-version bump (`0.x` → `0.(x+1)`). When the project commits to external compatibility, a `v1.0.0` will be tagged. Until then, expect changes at minor boundaries.

---

# Historical entries

The entries below predate the changelog policy above and are preserved as the historical record. For releases at v0.4.0 or later, see the [Releases page](https://github.com/myrgic/cogos/releases).

## [0.5.0] - 2026-05-08

### Changed (BREAKING)
- Go module path renamed to `github.com/myrgic/cogos` (org rename: cogos-dev -> myrgic).
  Downstream consumers must update imports.
- `github.com/myrgic/constellation` v0.2.0 dependency (module path updated alongside org rename).
- Docker images now published to `ghcr.io/myrgic/cogos`.
- User-facing install script (`scripts/setup.sh`) now points to `myrgic/cogos` releases.
- CI LDFLAGS embed the new module path; binary version strings updated.

## [Unreleased]

_(no entries — see Releases page going forward)_

## [0.3.0] - 2026-04-22 — Track 5 consolidation + Agent F gap closure

The pre-1.0 reset. This release consolidates Track 5 dead-code removal and the full 8-gap Agent F critical MCP surface buildout. The prior `v2.x` numbering was aspirational; `v0.3.0` begins a deliberate path toward `v1.0`. See [Pre-reset history](#pre-reset-history) for the prior entries.

### Added
- `POST /v1/context/build` — context engine exposed without inference (#8)
- Hash-chained ledger exposed via `cog_read_ledger` MCP + `GET /v1/ledger`, with hash-chain verification (#11)
- Config mutation API — `cog_read_config` / `cog_write_config` / `cog_rollback_config` MCP + `GET`/`PATCH /v1/config` + `POST /v1/config/rollback` (RFC 7396 merge-patch) (#14)
- Windows build targets in `Makefile` + install docs in `docs/RELEASING.md` (#15)
- Event bus — broker hooked inside `AppendEvent`, real SSE stream at `/v1/bus/:id/events/stream`, `cog_tail_events` + `cog_query_events` MCP tools (#16)
- Traces search API — `cog_search_traces` MCP + `GET /v1/traces` (`/v1/proprioceptive` preserved byte-compat via regression test) (#17)
- `docker-compose.node.yml` bridge-{primary,secondary} + tailscale-{primary,secondary} siblings for multi-session MCP substrate (#19)
- Engine-side `cogos emit` subcommand (Track 5 Phase 1); additive — root `cmdEmit` retained for compat (#23)
- Conversation turns persisted via `turn.completed` ledger events + per-session `.cog/run/turns/<sessionID>.jsonl` sidecar; `cog_read_conversation` MCP + `GET /v1/conversation` (#24)
- Tool-bridge observability — `cog_read_tool_calls` + `cog_tail_tool_calls` MCP; activates dormant `gate.go` `tool.call`/`tool.result` recognizer + `NormalizeMCPRequest` normalize-path + pending-call correlation cache (#25)
- Agent state + loop control — `cog_list_agents` / `cog_get_agent_state` / `cog_trigger_agent_loop` MCP + `/v1/agents[/...]` plural HTTP routes (preserves singular `/v1/agent/{status,traces,trigger}` byte-compat) (#27)
- Kernel slog capture — `teeHandler` fans slog to stderr + JSONL sink at `.cog/run/kernel.log.jsonl`; `cog_tail_kernel_log` MCP + `GET /v1/kernel-log` (#28)
- `--bind` flag + `bind_addr` YAML — wires long-dormant `BindAddr` config field; default remains `127.0.0.1`; CORS relaxed on non-loopback bind (closes #12) (#33)
- `cog bus send` subcommand — write symmetry for bus CLI; defaults to direct JSONL write, `--http` opt-in for kernel broadcast (closes #26) (#34)
- Kernel-native session management (hybrid) — closes last Agent F critical MCP gap. `SessionRegistry` + `HandoffRegistry` with atomic-claim semantics (bus-level first-wins, not just cache); `POST /v1/sessions/{register,heartbeat,end}` + `POST /v1/handoffs/{offer,claim,complete}` + `GET /v1/sessions/presence` + `GET /v1/handoffs` HTTP routes; 8 `cog_*_session|handoff_*` MCP tools alongside the 8 existing `cogos_*` Python bridge tools. Bus stays ground truth; registries are derived views rebuilt via seq-sorted replay on startup. `handoff.claim_rejected` observability event emitted on every rejection with reason + attempting_session + conflicting_session. (#43)

### Changed
- `make build` / `make install` now produce an engine-based binary (`go build
  -o cog ./cmd/cogos`) instead of the root package (Track 5 Phase 4). The
  Dockerfile + per-platform cross-compile targets flip to the same path. The
  installed `cogos` binary gains engine-native subcommands (`start`, `stop`,
  `restart`, `logs`, `chat`, `bench`, `blobs`, `docs`, `experiment`,
  `manifest`) and keeps parity for the three blocking root subcommands
  (`emit` — Phase 1, `mcp serve` — Phase 2, `serve` with `/v1/bus/*` +
  `/v1/sessions` — Phase 3). Root-only dormant subcommands (`run`, `tasks`,
  `cache`, `fleet`, `research`, `registry`, `plan`, `apply`, `components`,
  `snapshot`, `refresh`, `import`, `migrate`, `watch`, `reconcile`, `index`,
  `drift`, `channel`, `cluster`, `extract`, `oci`, `service`, `decompose`)
  are no longer in the installed binary — Phase 5 audits them for deletion.
- MCP server + ingestion pipeline + tool loop + cogdoc service + membrane policy
  are now always compiled into the kernel. The `mcpserver` build tag has been
  removed from all 14 gated files in `internal/engine/`. Rationale: no release
  path (Makefile, goreleaser, CI) ever set `-tags mcpserver`, so default
  binaries silently shipped without MCP — breaking the kernel's primary use
  case (LLM collaboration via MCP). See
  [docs/archival/2026-04-21-mcp-always-on.md](docs/archival/2026-04-21-mcp-always-on.md)
  for full rationale and call-graph evidence. (#9)

### Fixed
- Ledger + sync-watcher tests now stable on `-count>=2` — `resetLedgerCacheForTest` helper + sync-watcher parse-retry (closes #18) (#30)
- `syscall.Dup2` blocked linux/arm64 builds — switched to `golang.org/x/sys/unix.Dup2` (closes #13) (#31)
- `EmitLedgerEvent` orphan-file bug — was writing to flat `.cog/ledger/events.jsonl` bypassing hash-chain (closes #10) (#16)
- `RecordBlock` data-loss — chat prompt/response text was GC'd every turn (closes #20) (#24)
- Root `cogos emit` silent-drop bug — hook invocations returned success but never wrote to ledger (#23)

### Removed
- `internal/engine/serve_mcp_stub.go` — the noop `!mcpserver` fallback, no
  longer needed now that MCP is always-on. (#9)
- `turn_metrics.jsonl` fossil source — no production writers, superseded by `turn.completed` ledger events (closes #21) (#32)
- 5 unused TAA config fields (`ExtractionMethod`, `ConfidenceThreshold`, `FailureMode`, `TraceTiers`, `TraceFile` — all marked `// TODO: not yet wired`) (closes #22) (#29)

---

## Pre-reset history

The entries below describe releases under the prior aspirational `v2.x` numbering scheme. The corresponding commits exist in git history but were never tagged as `v2.x`; `v0.3.0` is the first tag in the reset series.

## [2.6.0] - 2026-04-15 — Decomposition pipeline workbench

### Added
- `cog decompose` CLI command with 4-tier foveated decomposition via E4B:
  Tier 0 (one-sentence ~15 tokens), Tier 1 (paragraph ~100 tokens),
  Tier 2 (full CogDoc with frontmatter + sections + embeddings),
  Tier 3 (raw passthrough, gated)
- `DecompositionRunner` engine using `AgentHarness.GenerateJSON()` with
  JSON mode, per-tier schema validation, and one-retry error correction
- Interactive workbench TUI (`--workbench`): Bubbletea 2x2 viewport grid
  with tier focus switching, re-run, and metrics bar
- Embedding co-generation via nomic-embed-text (128-dim + 768-dim Matryoshka)
  for Tier 0, 1, and 2 output
- Content-addressed CogDoc storage at `.cog/mem/semantic/decompositions/`
  with full YAML frontmatter, section index, and source refs
- Constellation indexing for vector + FTS5 retrieval of decomposition output
- Bus event lifecycle (`decompose.start/tier.start/tier.complete/complete/error`)
  with file-based JSONL emission for standalone CLI runs
- Quality metrics: compression ratio, cross-tier embedding fidelity (cosine
  similarity), schema conformance tracking
- Dashboard Decompose tab with recent decomposition history, per-tier
  timing bars, and compression ratio color coding
- `GenerateJSON()` method on `AgentHarness` for general-purpose JSON-mode
  LLM completions (reusable beyond decomposition)
- 52 tests (unit + integration) across 4 test files, including mock Ollama
  server tests for prompt construction, retry logic, and event sequencing

### Files
- `decompose.go` — Core engine, types, prompts, CLI, formatter (846 lines)
- `decompose_store.go` — Embedding generation, CogDoc storage (306 lines)
- `decompose_tui.go` — Bubbletea workbench TUI (351 lines)
- `decompose_test.go` — Unit tests (1,325 lines)
- `decompose_store_test.go` — Storage tests (238 lines)
- `decompose_tui_test.go` — TUI tests (97 lines)
- `decompose_integration_test.go` — E2E with live Ollama (310 lines)

## [2.5.0] - 2026-04-14 — Gemma 4 default, dashboard model selector

### Changed
- Default Ollama model switched from Qwen 3.5 / Llama 3.2 to Gemma 4 E4B across all layers (inference.go, harness, serve_providers, provider_pi, dashboard HTML)
- Dashboard model selector updated: gemma4:e4b, gemma4:e2b, gemma4:26b, llama3.2:1b
- Provider model list now reflects locally available Ollama models
- Pi provider default model uses `defaultOllamaModel` constant instead of hardcoded string
- Help text in chat and benchmark commands updated for Gemma 4 examples

## [2.4.0]

### Added
- OpenAI-compatible provider for LM Studio, vLLM, llama.cpp (1,613 LOC, 18 tests)
- Auto-discovery of inference providers on localhost
- Professional README with progressive disclosure
- CONTRIBUTING.md
- Autoresearch pipeline (extract-signals, nightly-consolidation, survey-traces)
- Experiment harness for cross-node benchmarking
- Context assembly path fix for TRM-scored documents

### Changed
- README rewritten for clarity and approachability

### Fixed
- `Available()` in OpenAI-compat provider now returns false when the configured model is not in the server's model list

## [0.0.1] - 2026-04-03 — Performance: eliminate CPU burn in continuous process

The v3 daemon was consuming 200% CPU perpetually due to compounding
inefficiencies in the consolidation loop. This release fixes all of them
and brings idle CPU to 0%.

### Root cause

`RankFilesBySalience` called `ComputeFileSalience` per file (4,637 memory
files), and each call opened the 2.4 GB git repo from scratch via
`git.PlainOpen`. This ran every 5 minutes with no caching. The field never
successfully populated (stuck at `field_size=0`, state `consolidating`).

### Fixes

**salience.go — Single-pass batch scoring**
- `RankFilesBySalience` now walks the git log once via `batchCollectStats`,
  building a file-to-stats map in a single commit walk. Complexity drops from
  O(files x commits) to O(commits x changed_files_per_commit).
- `commitChangedFiles` uses tree diffing (no line counting) instead of the
  expensive `c.Stats()` call.

**field.go — HEAD-based caching and delta updates**
- Three update modes selected automatically:
  1. HEAD unchanged + scores exist -> no-op (instant)
  2. Previous HEAD known + new commits -> delta scan (rescore only changed files)
  3. No previous state -> full scan (startup only)
- `deltaUpdate` opens the repo once and reuses the handle for both tree
  diffing and per-file scoring.

**process.go — Cached coherence and conditional index rebuild**
- Coherence report is cached after each consolidation tick and reused by
  the heartbeat (previously ran the full 4-layer validation twice per cycle).
- `BuildIndex` is skipped when HEAD has not changed since the last rebuild.

**ledger.go — In-memory last-event cache**
- `AppendEvent` now caches the last event per session in memory. Previously
  it scanned the entire JSONL ledger file from the beginning on every append,
  producing O(N^2) I/O growth over the session lifetime.

**config.go — Default consolidation interval**
- Increased from 300s (5 min) to 900s (15 min). The HEAD cache makes this
  moot when nothing has changed, but it reduces unnecessary tick overhead
  even without the cache.

**serve_foveated_test.go — Fixed pre-existing test failure**
- `TestHandleFoveatedContext` now initializes a real git repo and builds
  the CogDoc index, matching production initialization.

### Results

| Metric | Before | After |
|--------|--------|-------|
| Steady-state CPU | 200% | 0% |
| Field populated | Never (0 files) | 4,358 files |
| Process state | Stuck `consolidating` | `receptive` |
| Initial scan | Never completed | ~13s, then idle |
| Repo opens per scan | 4,637 | 1 |
| Subsequent updates | Full rescan | Delta only |
| Ledger append cost | O(N) file scan | O(1) cached |
| Tests | 1 failing | All passing |
