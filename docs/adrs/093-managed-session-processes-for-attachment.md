# ADR-093: Managed Session Processes for Persistent Agent Attachment

| Field       | Value                                                                          |
|-------------|--------------------------------------------------------------------------------|
| Status      | Accepted                                                                       |
| Author      | @chazmaniandinkle                                                              |
| Created     | 2026-05-16                                                                     |
| Layer       | Kernel (per [ADR-091](091-substrate-as-named-architectural-layer.md) §2)       |
| Refs        | [ADR-091](091-substrate-as-named-architectural-layer.md) (substrate as named architectural layer), [ADR-092](092-substrate-contracts-and-concurrency.md) §3 (crash recovery + idempotency), §7 (substrate-to-Module boundary), ADR-082 (channel architecture, mod3), ADR-094 (lineage observatory, PR #265), `pkg/reconcile/`, `internal/engine/process_manager.go`, `internal/engine/provider_claudecode.go`, `internal/engine/serve_claude_code.go`, mod3 `clients/channel_client.py`, mod3 `seats.py` |

> **Ratified 2026-05-24.** Accepted alongside cog ADR-062 (Recursive Node
> Architecture), ratified the same day, as part of the conductor-lineage
> ratification. The 2026-05-20 validation spike retired the wire-shape risk;
> Commit 3 (dashboard Resume + dispatch migration) is unblocked.
>
> **Verification backlog** — acceptance conditions on the implementation
> commits, not blockers to acceptance. The items under "What the spike does
> NOT validate" (below) are the required follow-up commits: cancellation
> mid-turn, restart-on-crash policy, heartbeat/Stalled detection,
> `--mcp-config` substrate-MCP injection, tool-call surface typing, the ACP
> `session/update` translator, ProcessManager integration (§1), and
> concurrent multi-session safety.
>
> **Cross-restart durability** (open question 3) — resolved consistently with
> ADR-062: on cogos restart, ManagedSessions adopt orphaned subprocesses if
> still alive, otherwise rebuild from the ledger. The remaining open questions
> (authorization, cost accounting, multi-kernel, channel-transport evolution,
> pool policy) stay deferred as scoped.

## Context

CogOS currently exposes `POST /v1/claude-code/spawn` (registered in
`internal/engine/serve_claude_code.go` at `handleClaudeCodeSpawn`) as the
entry point for "resume a Claude Code session" from the mod3 dashboard's
Sessions browser (`mod3/dashboard/sessions.html`). The handler calls
`ClaudeCodeProvider.SpawnBackground` (`internal/engine/provider_claudecode.go:678`)
which invokes the `claude` CLI with:

```
claude -p --output-format json --model <model> \
       --append-system-prompt <...> \
       --mcp-config <temp>.mcp.json --strict-mcp-config \
       --no-session-persistence
```

This is `claude -p` — **non-interactive print mode**, with session
persistence explicitly disabled. The subprocess runs once, produces one
response on stdout, and exits. The dashboard banner reads *"Session
resumed (process X). The Claude Code subprocess is starting — check the
Dashboard tab for the channel-client seat to appear,"* but the subprocess
terminates within seconds, the temporary channel-client seat (if it
registered at all during the brief run) dies with the process, and the
Dashboard tab shows the user's own seat with no attached Claude Code.

The dashboard UX assumes **persistent session attachment**. The backend
implements **one-shot stateless dispatch**. The semantic mismatch is the
root cause of the observed bug.

The infrastructure for persistent attachment already exists in pieces:

- `mod3/seats.py` defines `VALID_CLIENT_TYPES = {"claude-code-channel", "generic"}`
- `mod3/clients/channel_client.py` is a long-lived stdio process that
  registers a seat and proxies messages between the dashboard and a
  hosted agent process
- `/v1/sessions/main/seats` already returns three pre-existing
  `claude-code-channel` seats from prior manually-wired channel-client
  instances, demonstrating the pattern works end-to-end when properly
  invoked
- Cogos has `ProcessManager` (`internal/engine/process_manager.go`) for
  tracked, cancellable background processes

What is missing is the **bridge**: the `/v1/claude-code/spawn` path does
not invoke this channel-client-mediated pattern. It invokes `claude -p`.

### The two-primitive draft and why it was wrong

An earlier draft of this ADR proposed keeping `claude -p` for one-shot
dispatch and introducing `ManagedSession` as a parallel primitive for
attachment. The operator correctly identified this as a false dichotomy:

> *"Could we use the channel architecture (a local stdio channel that
> persists alongside the process) we just articulated as the default?
> Is there truly any reason to continue using the -p pattern now that
> we have the channel protocol available? Even for short-lived calls,
> it's not any heavier once the process has been established already."*

This is correct. One-shot and persistent attachment differ only in
*duration of use*, not in *shape*. The substrate metaphysics names this
explicitly (see [[reconciliation-is-the-process]] and §"Why one
primitive, not two" below): categorical lines drawn across the same
operation with different rate/duration parameters are false dichotomies.
Once a managed channel-mediated process exists, even short-lived calls
are not heavier once warm. Two primitives where one suffices is the
failure mode the foundation operational mode names explicitly.

## Decision

**ManagedSession with channel-mediated I/O is the universal pattern** for
substrate agent processes. `claude -p` is deprecated as a substrate
primitive entirely. External callers (outside the substrate) may continue
to invoke the CLI directly; that use is not the substrate's concern.

### §1 — All substrate agent processes use managed channel-mediated sessions

`claude -p` is **deprecated as a substrate primitive.** It is not
preserved for any substrate-internal use case: not for one-shot dispatch,
not for background research, not for evaluations. All substrate spawn
paths migrate to `ManagedSession`.

The complete list of substrate callers that migrate:

- `cog_dispatch_to_harness` MCP tool (single-turn task to local harness)
- Background research / evaluation tasks
- Sub-agent spawns where a parent expects one response
- Dashboard Resume of a stored Claude Code session

The migration shape for callers that previously treated `claude -p` as
one-shot is: `session = mgr.New(opts); session.Send(prompt); resp =
session.Receive(); session.Detach(); return resp`. The external caller API
does not change. The internals change. The substrate's job is to make this
fast — cold-start cost is paid once per session rather than on every call.

External (non-substrate) use of `claude -p` is unaffected. The CLI
remains a valid entry point for external callers; this ADR does not touch
that surface.

### §2 — Introduce ManagedSession

A new type, **`ManagedSession`**, is introduced in
`internal/engine/managed_session.go` (target path). Its contract:

```go
// ManagedSession owns a long-lived agent subprocess, mediates I/O through
// a channel interface, and is the substrate's unit of agent attachment.
type ManagedSession interface {
    // Resume attaches to an existing on-disk session.
    Resume(ctx context.Context, sessionID string, projectDir string, opts SessionOpts) (*Session, error)

    // New creates a fresh session and attaches to it.
    New(ctx context.Context, projectDir string, opts SessionOpts) (*Session, error)

    // Get returns the live session for the given session ID, or nil if not attached.
    Get(sessionID string) *Session

    // Detach stops the managed process for a session cleanly (does not delete the
    // session's on-disk state). When Detach is called after a short-lived use,
    // the underlying process may remain in the warm pool for reuse per pool policy.
    Detach(sessionID string) error
}

type Session struct {
    SessionID  string
    ProjectDir string
    SeatID     string        // the channel-client seat ID registered with mod3
    Process    *ManagedProcess
    StartedAt  time.Time
    Channel    SessionChannel // bidirectional I/O
}
```

The implementation invokes the `claude` CLI **without** `-p` and
**without** `--no-session-persistence`. Concretely:

```
claude --resume <session_id> --model <model> \
       --mcp-config <temp>.mcp.json --strict-mcp-config
```

Stdin and stdout are piped through `SessionChannel`. The subprocess is
tracked by `ProcessManager` with a long-lived lifecycle (not a one-shot
timeout). The temporary `.mcp.json` includes the same channel-client
config the current spawn already writes.

### §3 — Channel interface contract

`SessionChannel` is the substrate-level interface between the kernel and
a managed session process. It is **not new**: it formalizes the pattern
that already exists between mod3 and `channel_client.py`. Required
operations:

| Direction              | Operation     | Semantics                                            |
|------------------------|---------------|------------------------------------------------------|
| Kernel → process       | `Send`        | Deliver a user message or system event to the agent  |
| Process → kernel       | `Receive`     | Stream of model output chunks, tool calls, state events |
| Kernel → process       | `Cancel`      | Interrupt in-flight response (barge-in / disconnect) |
| Process → kernel       | `Heartbeat`   | Liveness signal at bounded interval                  |
| Kernel → process       | `Detach`      | Graceful shutdown signal                             |

`SessionChannel` is transport-agnostic: the initial implementation uses
mod3's seat registry + `channel_client.py`'s stdio pipes, but the
interface allows future transports (gRPC, WebSocket) without changing
caller code.

### §4 — Lifecycle, health, and restart

ManagedSession processes have explicit lifecycle states:

- **Starting** — subprocess invoked, not yet registered as a seat
- **Live** — seat registered with mod3, last heartbeat within bound
- **Stalled** — seat registered but no heartbeat for `> 2 * heartbeat_interval`
- **Detached** — clean shutdown completed
- **Crashed** — subprocess exited unexpectedly

Restart policy:

- **On crash within `crash_window`** (default 60s) of start: do not
  restart automatically; surface the error to the caller and the
  dashboard. Crashing-on-startup is a configuration or auth problem;
  retrying would mask the root cause.
- **On crash after `crash_window`**: restart with exponential backoff
  up to `max_restarts` (default 3); after exhaustion, surface the
  error and require operator intervention.
- **On stall**: send heartbeat probe; if no response within
  `probe_timeout` (default 5s), terminate and follow crash-after-window
  logic.

Detach is initiated by: (a) explicit operator action, (b) session
abandonment per kernel policy (no activity for `idle_timeout`, default
1 hour), or (c) substrate shutdown.

Memory pressure from long-running sessions is mitigated by `idle_timeout`.
State isolation between unrelated calls is achieved by spawning a
new session per task — session lifetime is a per-call parameter.

### §5 — Pooling as a substrate concern

When `Detach` is called after a short-lived use (the one-shot dispatch
shape), the underlying process can either exit immediately OR remain in
a warm pool for reuse. Pool policy is a substrate tuning knob:

- **Pool-off (default):** Detach triggers immediate subprocess exit.
  Cold-start cost is paid on the next `New()`.
- **Pool-on:** Detach parks the subprocess in a warm pool keyed by
  `(model, mcp_config_hash)`. The next `New()` with matching opts
  acquires a warm session rather than starting cold.

Pool policy is configured at the substrate level, not by callers. Callers
use `New()` / `Detach()` regardless; the pool is transparent. This is the
substrate's mechanism for making channel-mediated dispatch as fast as
`claude -p` was for one-shot callers — first call pays the cold start,
subsequent calls do not.

### §6 — Idempotency

Per [ADR-092 §3](092-substrate-contracts-and-concurrency.md#§3---crash-recovery-during-reconcile),
the reconcile loop provides at-least-once semantics. ManagedSession's
operations MUST therefore be idempotent:

- **`Resume(sessionID)` against an already-live session**: return the
  existing live `Session`, do not start a second subprocess.
- **`New()` race**: two concurrent `New()` calls for the same workspace
  may create two distinct sessions; this is acceptable because sessions
  have unique IDs and there is no shared resource being contested.
- **`Detach(sessionID)` against an already-detached session**: succeed
  (no-op).
- **Heartbeat for an unregistered seat**: ignored.

Idempotency is enforced by a process-local map keyed on session ID.
Cross-process (federation) idempotency is out of scope; cogos remains
single-kernel per substrate per [ADR-092 §6](092-substrate-contracts-and-concurrency.md#§6---multi-kernel-on-one-substrate-todays-status).

### §7 — Substrate-to-Module boundary

Per [ADR-092 §7](092-substrate-contracts-and-concurrency.md#§7---substrate-to-module-boundary),
Modules consume substrate primitives via kernel-mediated surfaces.
ManagedSession is a **kernel-layer** concern that:

- Reads substrate primitives (ledger for session metadata, capability
  resolver for authorization, identity for the spawned process's
  observer-shape).
- Writes substrate state (registers a seat via mod3's HTTP API; appends
  a `session.attached` ledger event when the seat goes live).
- Exposes a public surface (`POST /v1/managed-sessions/{id}/resume`,
  `GET /v1/managed-sessions`, `DELETE /v1/managed-sessions/{id}`) for
  Modules (the dashboard, MCP-based external clients) to invoke.

The dashboard's "Resume" button is rewired to call the new endpoint
instead of `/v1/claude-code/spawn`. The `/v1/claude-code/spawn` endpoint
continues to exist for backward compatibility but emits a deprecation
warning in its response headers (`X-Deprecated: use POST /v1/managed-sessions/{id}/resume`)
and is removed in a future ADR after callers migrate.

### §8 — Migration plan

Four commits, each independently shippable:

**Commit 1 — Mark `SpawnBackground` as deprecated for all substrate use.**
Add doc-comments on `SpawnBackground` clarifying it is deprecated as a
substrate primitive and that all spawn callers migrate to `ManagedSession`.
Add an `audit_callers.go` test that flags any caller still using
`SpawnBackground` from within the kernel.

**Commit 2 — Introduce ManagedSession scaffolding.**
Add `internal/engine/managed_session.go` with the type definitions
above and a stub implementation. Add the public HTTP surface. Mark all
methods `Status: Proposed — not yet wired`. CI green; nothing else
changes.

**Commit 3 — Wire dashboard Resume and cog_dispatch_to_harness.**
Update `mod3/dashboard/sessions.html` to call the new endpoint. Update
`cog_dispatch_to_harness` internals: old shape was `spawn claude -p,
wait, return`; new shape is `session = mgr.New(opts); session.Send(prompt);
resp = session.Receive(); session.Detach(); return resp`. External API
of `cog_dispatch_to_harness` does not change. Verify end-to-end via
the actual dashboard flow and a dispatch round-trip.

**Commit 4 — Migrate remaining spawn callers and remove the old path.**
Migrate background research, evaluation tasks, and any remaining callers
from `SpawnBackground` to `ManagedSession`. Remove the `claude -p` spawn
path from substrate internals. The `/v1/claude-code/spawn` HTTP endpoint
remains with its deprecation header through at least one minor release,
then is removed by a follow-up ADR.

## Rationale

### Why one primitive, not two

The earlier two-primitive framing (keep `claude -p` for one-shot,
`ManagedSession` for attachment) embedded a false category boundary.
The substrate metaphysics rejects categorical lines where the operation
is the same with different rate/duration parameters.

Per [[reconciliation-is-the-process]]: there is no "two primitives for
two use-shapes" any more than there are two different kernels for fast
and slow reconciliation rates. The operation is the same; the
parameters differ. Unifying to one primitive is not a loss of
expressiveness — it is recognizing that the dichotomy was an artifact
of the implementation that patched attachment on top of one-shot
semantics (the current bug), not a structural property of the domain.

Per [[foundation-operational-mode]] (articulated 2026-05-16): the
pre-commitment test at this architectural decision point is "does the
substrate metaphysics permit a more unified form?" The candidate reasons
for preserving two primitives — performance, migration cost, backward
compatibility — each dissolve on inspection. Performance is addressed
by the warm pool (§5). Migration cost is bounded (§8). Backward
compatibility for external callers is preserved without preserving the
internal primitive (§1). No reason survives. The unified form is the
correct form.

### Why now

Three conditions are simultaneously true:

1. The substrate metaphysics is articulated (ADR-091, ADR-092) so the
   ManagedSession contract can cite Layer, idempotency, and
   substrate-to-Module boundary commitments by reference rather than
   re-litigating them.
2. The channel infrastructure exists (`channel_client.py`, `seats.py`,
   `ProcessManager`). This is wiring, not greenfield.
3. The dashboard Resume bug is reproducible and concretely blocks an
   actually-useful UX. The forcing function is real.

### Why this fits the Substrate / Kernel / Module trichotomy

Per ADR-091 §2:

- **Substrate primitives consumed:** ledger (append `session.attached`),
  capability resolver (authorize the spawn), reconcile (lifecycle as a
  reconcile loop), identity (observer-shape registration).
- **Kernel concern:** the ManagedSession orchestration itself — agent-
  loop execution requires a live process; ManagedSession is what keeps
  that process alive and addressable.
- **Module participation:** mod3 hosts the seat registry and runs
  `channel_client.py` subprocesses; the dashboard issues Resume actions.

ManagedSession is therefore a Kernel-layer concern that bridges
substrate primitives to Module surfaces. This is the canonical shape
ADR-091 §3 anticipated.

### Why not just fix the bug in-place

The current `/v1/claude-code/spawn` could be patched to drop `-p` and
`--no-session-persistence` and wire stdin/stdout to a channel — but
this would conflate one-shot dispatch and persistent attachment behind
the same endpoint shape, and would not give the rest of the substrate
a named, citable primitive for the attachment pattern. The patch would
work; the architecture would not improve. The deeper move is to name
the pattern, document the contract, and refactor incrementally.

## What this ADR is not

This ADR does not:

- Implement ManagedSession. Implementation is the work of Commits 1–4
  in §8.
- Affect external (non-substrate) use of `claude -p`. External callers
  may continue to use the CLI directly; this ADR does not touch that
  surface.
- Define the SessionChannel transport. The interface is named here;
  transport selection is implementation work and may begin with stdio
  pipes (matching today's `channel_client.py`).
- Replace mod3's seat registry, `channel_client.py`, or ADR-082's
  channel architecture. It uses them.
- Remove `/v1/claude-code/spawn`. The endpoint stays for backward
  compatibility through at least one minor release after Commit 4.

## Consequences

### Positive

- Dashboard Resume actually attaches to a live, addressable Claude Code
  session instead of firing a one-shot subprocess that exits.
- All substrate agent processes share one citable primitive with a
  documented contract. No parallel paths for similar operations.
- Channel-mediated managed processes become the canonical pattern for
  all agent processes. Future real-time-voice work, multi-agent
  collaboration work, and external-channel work (Discord, Telegram)
  inherit the same lifecycle and idempotency contract.
- Lifecycle states are explicit and observable. Operators can see which
  sessions are Starting / Live / Stalled / Detached / Crashed without
  reading subprocess `ps` output.
- First-call latency for short-lived dispatch matches `claude -p` after
  warm pool is established. Cold-start cost is paid once per pool slot,
  not on every call.
- The Reconcilable shape from ADR-091 §2 and ADR-092 §3 gets a concrete
  consumer: ManagedSession itself is reconcilable (target: session X
  has a live seat; live: registry state; reconcile: spawn / restart /
  detach).

### Negative

- A new type, a new HTTP surface, and a new lifecycle state machine
  enter the kernel. More code, more tests, more surface for bugs.
- All callers of the `claude -p` spawn path must migrate. During the
  migration window (Commits 1–4), two code paths exist for similar-
  looking behavior, which will confuse new contributors until the
  deprecated path is removed.
- Restart policy parameters (`crash_window`, `max_restarts`,
  `probe_timeout`, `idle_timeout`) are tuning knobs the substrate did
  not previously expose. Default values must be chosen carefully and
  may need adjustment after operational experience.
- Pool policy (pool-on/pool-off, pool size, eviction) is a new substrate
  configuration surface. Operational decisions about when to enable
  pooling are deferred; pool-off is the safe default.

### Neutral

- The channel-mediated pattern is general. Applying it to Claude Code
  is the first instance; voice channels (mod3) already use a
  structurally similar pattern. Future channels (Discord, Telegram,
  mobile clients) will instantiate the same abstraction. This is
  design intent.

## Open questions

- **Authorization for ManagedSession operations.** Who is allowed to
  resume which session? Today there is no per-session authorization;
  any caller of `/v1/claude-code/spawn` can resume any session.
  Whether the capability envelope from `capability_resolver.go` should
  gate Resume, and how, is deferred to a future ADR.
- **Cost accounting.** ManagedSessions consume tokens, API quota, and
  local compute. Whether the substrate tracks per-session cost and
  enforces budgets is deferred. Today's `MaxBudgetUSD` field on
  `BackgroundTaskOpts` is one-shot-shaped; the equivalent for
  persistent sessions has different semantics.
- **Cross-restart durability.** If cogos restarts, do ManagedSessions
  reconnect to their previously-attached subprocesses (if those
  subprocesses survived)? Or does cogos rediscover orphaned
  subprocesses and adopt them? This is a real question for substrate
  restart semantics and connects to ADR-092 §2 (boot order and
  replay).
- **Multi-kernel-on-one-substrate.** Per ADR-092 §6, multi-kernel is
  not supported today. If two kernels were attached to the same
  substrate, ManagedSession's process-local idempotency map would
  diverge. Resolution waits for the broader multi-kernel work.
- **Channel transport evolution.** The initial implementation uses
  mod3 seats + `channel_client.py` stdio. Whether to evolve toward
  in-process Go channels (for same-binary modules), gRPC (for
  cross-process), or WebSocket (for browser-direct) is deferred.
- **Pool policy tuning.** Default is pool-off. When pool-on becomes
  the operational default (e.g., when dispatch latency is a measured
  problem), a follow-up ADR specifies pool size, eviction policy, and
  the metric that triggers pool-on promotion.

## Implementation pointers

Implementation begins with Commit 1 in §8. The work is bounded:

- `internal/engine/managed_session.go` (new, ~200 lines)
- `internal/engine/serve_managed_sessions.go` (new, ~150 lines for
  HTTP surface)
- `internal/engine/provider_claudecode.go` (deprecate `SpawnBackground`;
  add channel-mediated variant)
- `internal/engine/agent_dispatch.go` (migrate `cog_dispatch_to_harness`
  internals to ManagedSession)
- `mod3/dashboard/sessions.html` (rewire Resume button; ~20 lines
  changed)

Test coverage requirements:

- Unit tests for lifecycle state transitions
- Integration test: spawn → seat appears → message round-trip → detach
  → seat removed
- Idempotency test: two concurrent Resume calls for the same session
  ID result in one live process
- Restart test: kill the subprocess mid-conversation; verify restart
  within policy and seat re-registration
- Pool test (when pool-on): Detach + New with matching opts acquires
  warm session rather than cold-starting

## §10 — Empirical findings from the validation spike (2026-05-20)

A focused ~480-LOC spike (PR #306, branch `spike/acp-bridge-claude-cli`)
exercised the central wire-shape assumption end-to-end against the real
`claude` binary before the ADR's four-commit implementation begins. The
findings shift two design decisions and confirm the rest.

### Setting

The spike adds `internal/acp/` containing:

- `streamjson.go` — typed frames for the `claude --output-format stream-json`
  NDJSON wire (`system`, `assistant`, `stream_event`, `result`,
  catch-all `unknown`), `PromptInput` constructors for
  `--input-format stream-json`, and a `ParseLine` dispatcher
- `claudecli.go` — `Subprocess` driver: `Spawn` / `Send` / `Events` /
  `CloseInput` / `Wait`. Serialized stdin writer; 1 MB scanner buffer
  for fat assistant frames; subprocess parented through
  `exec.CommandContext` (no `ProcessManager` integration yet)
- `spike_test.go` — one-prompt round-trip
- `multiturn_test.go` — two-prompt context-retention proof, with a
  fresh UUID per test run

Both tests auto-skip when `claude` is not on `PATH`. They live in the
spike branch and are not part of the migration plan in §8.

### Finding 1 — stream-json input IS multi-message. (Design unblocked.)

A prior research pass had concluded that `claude --print --input-format
stream-json` reads one input object, processes one turn, and exits —
i.e., that multi-turn over one subprocess was not supported. That would
have forced ADR-093's `Send` to spawn a fresh `claude --resume`
per call, with all the cold-start and credential-rebinding cost that
implies.

The spike refutes this directly. With one long-lived subprocess:

```
turn 1 result: "OK"        session=2550852d-c5df-19b7-d289-a13e09818a37
turn 2 result: "GLOWWORM"  session=2550852d-c5df-19b7-d289-a13e09818a37
```

Turn 2 retrieved a secret word that was only present in turn 1's
prompt. Both turns reported the same `session_id` on their `result`
frames. The CLI is reading NDJSON from stdin as a streaming protocol —
each object on stdin triggers a turn, the session JSONL appends, the
subprocess stays alive until stdin closes.

**This is the load-bearing claim of ADR-093.** `ManagedSession.Send`
maps cleanly onto a single `Encode(promptInput)` against the
subprocess's stdin pipe. No pool of `--resume` subprocesses-per-turn
required. The §4 lifecycle (Starting → Live → Stalled →
Detached/Crashed) is correct as drafted.

### Finding 2 — required flag combination

For `--print --input-format stream-json --output-format stream-json` to
work, `--verbose` must also be passed. Without it the CLI rejects the
combination. The spike's `claudecli.go` bakes this in:

```go
args := []string{
    "--print",
    "--verbose",
    "--output-format", "stream-json",
    "--input-format",  "stream-json",
}
```

§2's `claude --resume <session_id> --model <m> --mcp-config <path>
--strict-mcp-config` invocation in this ADR must add `--print
--verbose --input-format stream-json --output-format stream-json` to
be wire-compatible with the channel mechanism described.

### Finding 3 — `--session-id` is create-only

`claude --session-id <uuid>` pins a *new* session to a caller-chosen
UUID. It refuses to bind to a session_id that already exists on disk
(observed when the spike test re-ran with a hardcoded UUID; the
subprocess exited silently and the event channel closed before any
`result` frame). To resume an existing session use `--resume <id>`.
The two flags are not interchangeable.

`SpawnOpts.ResumeExisting bool` in the spike encodes this distinction.
Implementation in §8 should mirror the same discriminator on
`ManagedSession.New` vs `ManagedSession.Resume`.

### Finding 4 — frame catalogue is wider than §3 suggests

The stream-json output stream emits more frame types than the §3
operation table implies. Observed during the two-test run:

- `system` with `subtype` in {`init`, `hook_started`, `hook_response`}
  — only `init` carries the canonical session_id; the hook frames are
  emitted at startup before the operator-provided prompt is processed
- `stream_event` — Anthropic SSE deltas pulled out of the streaming
  API and re-emitted as NDJSON lines. Inner `event` carries content
  block deltas, message stops, tool-use deltas, etc.
- `assistant` — a complete assistant message at end-of-turn
  (authoritative for the turn's text)
- `result` — terminal frame for each turn; carries `result` text,
  `duration_ms`, `num_turns`, `is_error`
- `rate_limit_event` — periodic quota status; observed during the
  one-prompt test

The §3 channel contract is unchanged (`Receive` is still "stream of
model output chunks, tool calls, state events"), but the translator
that maps these onto ACP `session/update` notifications needs to
handle each frame type. Estimated translator size revised from §8's
implicit "small" to ~300 LOC — still bounded but worth budgeting.

The spike's `streamjson.go` types are deliberately subset-minimal;
they decode just enough to validate the wire. The implementation in
§8 should expand the type set as new frame kinds are observed.

### Finding 5 — initial implementation library decision

ADR-093 §7 mandates that channel transport stay behind the
`SessionChannel` interface, transport-agnostic. The spike validates
that a 280-LOC Go-only implementation can drive the wire without any
foreign-runtime dependency (no Node, no Python). The follow-up
implementation in §8 will likely import `github.com/coder/acp-go-sdk`
for ACP wire types and JSON-RPC 2.0 framing (~990 LOC eliminated from
our maintenance burden, per the comparison in PR #306's body) — but
that is a library choice for the boundary surface that mediates
between `SessionChannel` and ACP clients, not for the
`SessionChannel` ↔ claude-CLI wire itself. The claude-CLI driver
stays as authored in the spike.

The substrate explicitly does **not** vendor
`@agentclientprotocol/claude-agent-acp` (the Node adapter Zed uses).
That path was evaluated and rejected: it would have introduced a
Node ≥20 runtime + npm supply chain into CogOS deploys, in exchange
for ~200 LOC saved on the kernel side. The spike confirms that the
Go-native path is small enough that the LOC saving does not justify
the topological hole. (Cf.
`feedback_topological_hole_sovereignty_diagnostic.md`.)

### What the spike does NOT validate

These remain unverified and need explicit follow-up commits:

- Cancellation via SIGINT or stdin-close mid-turn (does claude stop
  cleanly? do trailing frames arrive?)
- Restart-on-crash policy (§4) — no failure-injection coverage
- Heartbeat semantics — claude has no native heartbeat; the §4
  Stalled detection has to come from elsewhere (output-frame
  inactivity timer, or kernel-side health probe)
- `--mcp-config` injection of substrate MCP servers (substrate tools
  reachable from inside the spawned claude)
- `--strict-mcp-config` interaction with the generated config
- Tool-call surfaces inside `stream_event` payloads (claude calling
  Bash / Read / mod3_speak) — observed shape not yet typed
- ACP `session/update` translator (a separate concern from the
  subprocess driver)
- ProcessManager integration — the spike's `exec.CommandContext`
  bypasses §1's "all substrate agent processes use managed
  channel-mediated sessions" requirement
- Concurrent multi-session safety — only one `Subprocess` per test;
  no shared-state stress

### Implication for §8 migration plan

Commit 2 ("Introduce ManagedSession scaffolding") should adopt the
spike's `internal/acp/` types where they fit, replacing rather than
duplicating. Specifically:

- `acp.Subprocess` is the concrete claude-CLI implementation of
  `SessionChannel`'s kernel-side surface — keep it; wrap it.
- `acp.Event` is a substrate-internal representation; the public
  `SessionChannel.Receive` contract should emit a richer type that
  the translator constructs from `acp.Event`.
- `acp.SpawnOpts.{SessionID, ResumeExisting}` should appear on
  `ManagedSession.New` / `.Resume` with the same semantics.

Commit 3 (dashboard Resume + dispatch migration) is unblocked. The
wire-shape risk that justified hedging the migration plan has been
retired.

### References

- PR #306 — https://github.com/myrgic/cogos/pull/306 (draft)
- Spike branch — `spike/acp-bridge-claude-cli` on `upstream`
- Test logs reproducible via `go test -v ./internal/acp/ -timeout 180s`
  (auto-skip when `claude` is not on `PATH`)
- Companion finding files in operator memory:
  `reference_claude_code_mcp_env_substitution.md` (related: env
  substitution pitfalls from PR #103-#109 cascade)
