---
type: rfc-draft
title: "mod3 subscribes to bus_sessions for cross-session voice presence"
status: draft
closes: "myrgic/cogos#156"
depends_on: "myrgic/cogos#154"
scope: "Design only — no implementation in this document"
---

# RFC Draft: mod3 subscribes to bus_sessions for cross-session voice presence

---

## 1. Motivation

Two Claude Code sessions can be active at the same time in the same workspace,
both registered on `bus_sessions`, both independently registered as channel
participants in mod3. Right now the substrate cannot express the fact that they
are in the same voice room. The dual-rail problem:

- The kernel writes `session.register`, `session.heartbeat`, and `session.end`
  events to `bus_sessions` (`internal/engine/sessions.go:47-52`). These events
  carry workspace, role, hostname, and session identity — everything needed to
  reason about co-presence.

- Mod3 receives channel-participant registrations forwarded by the kernel over
  plain HTTP (`internal/engine/serve_sessions_channel.go:57`, `POST
  /v1/channel-sessions/register`). Mod3 uses this registration to assign a
  voice and a per-session output queue.

The two paths share a kernel-minted `session_id` (the `cs-<hex>` short form
minted in `serve_sessions_channel.go:574`), but no consumer reads across them.
Mod3 cannot answer "who else is in this channel right now as an agent session"
because it only knows about channel-participant registrations, not about the
agent-session lifecycle events landing on `bus_sessions`. The peer-awareness
query (`internal/engine/peer_awareness_query.go`) has no section for voice
co-presence.

The concrete symptom: two agents running concurrently both speak into the same
voice channel with different voices (correctly serialized by mod3's round-robin
policy), but neither agent's peer-awareness packet reflects that the other
agent is present and active on the same channel. The substrate has the raw
material for cross-session presence; no wire connects it to mod3.

---

## 2. Status Quo

### Data flow today

```
Claude Code session A                    Claude Code session B
       |                                        |
       | session.register                       | session.register
       v                                        v
  bus_sessions (kernel)              bus_sessions (kernel)
       |                                        |
  (no consumer in mod3)               (no consumer in mod3)

       |                                        |
       | POST /v1/channel-sessions/register     | POST /v1/channel-sessions/register
       v                                        v
  mod3 SessionRegistry                mod3 SessionRegistry
  (assigns voice bm_lewis)            (assigns voice af_heart)
  (knows: participant_id, session_id) (knows: participant_id, session_id)

  mod3 cannot see session A           mod3 cannot see session B
  from session B's perspective        from session A's perspective
```

### Kernel side: bus_sessions event types

`internal/engine/sessions.go:47-52`:

```
BusSessions  = "bus_sessions"

EvtSessionRegister  = "session.register"
EvtSessionHeartbeat = "session.heartbeat"
EvtSessionEnd       = "session.end"
```

A `session.register` payload carries: `session_id`, `workspace`, `role`,
`task`, `model`, `hostname`, `status`, and freeform `extras`. It does not
carry `channel_id` or voice assignment — those live exclusively in mod3's
`SessionChannel` dataclass (`session_registry.py:249-275`).

### Kernel side: channel-session forward path

`internal/engine/serve_sessions_channel.go:57` registers four routes.
`RegisterChannelSession` (line 254) is the shared entry point: it mints a
`cs-<hex>` session ID, forwards a JSON body to mod3's `/v1/sessions/register`,
and on success stores a `ChannelSessionRecord` in the kernel's in-memory
`channelSessionRegistry`. The record includes `participant_id`,
`participant_type`, `preferred_voice`, `preferred_output_device`, `priority`,
`kinds`, and `metadata`.

The forward body does **not** include any `bus_sessions` session ID because the
agent-session (`bus_sessions`) and the channel-participant session
(`channelSessionRegistry`) are two separate registrations today. They happen to
share a workspace, but mod3 has no field to correlate them.

### Mod3 side: session registry

`mod3/session_registry.py:618`. Mod3 maintains an in-memory `SessionChannel`
per registered participant. The registry does not subscribe to any kernel bus;
it has no async reader. Its inputs are: HTTP POSTs from the kernel's channel-
sessions forward path, and MCP tool calls routed through `{Mod3URL}/mcp`.

`mod3/bus_bridge.py:84` shows that mod3 already has a `KernelBusSubscriber`
that tails `/v1/events/stream` (the global kernel SSE stream) for cycle-trace
events to feed the dashboard. This bridge uses `httpx` for async SSE and
reconnects on disconnect. The subscription infrastructure exists — it is just
pointed at the wrong bus and does not update the session registry.

### Peer-awareness query: current sections

`internal/engine/peer_awareness_query.go:1-108`. The packet has four sections:

1. MY RECENT ACTIVITY — `channel.<sid>.activity` events  
2. OPEN HANDOFFS — `bus_handoffs`, filtered by session relevance  
3. PEER OVERLAP — attention-table co-focus intersection  
4. COORD CHATTER — `bus_broadcast` `coord.*` and `impl.*` events  

No section represents voice-channel co-presence. Section 3 (PEER OVERLAP)
requires both sessions to have attended the same file/URI (via the attention
log). Two agents speaking in a mod3 voice room but working on different files
produce zero overlap in section 3.

---

## 3. Proposal

### 3.1 Core idea

Mod3 subscribes to `bus_sessions` by long-polling the kernel's
`GET /v1/bus/bus_sessions/events` endpoint (the already-existing per-bus
event store route in `internal/engine/serve_bus.go:14`). On each new
`session.register`, `session.heartbeat`, and `session.end` event, mod3
updates an in-memory agent-session map keyed by `session_id`. When a channel-
participant registers via `POST /v1/sessions/register`, mod3 optionally
correlates the participant's `participant_id` with the agent-session map to
populate voice-room co-presence.

Mod3 then exposes a new query endpoint: `GET /v1/channels/{id}/peers`. The
peer-awareness query on the kernel side calls this before rendering, yielding a
new optional section 5: VOICE-CHANNEL CO-PRESENCE.

When two channel-participants are both in the same channel, mod3 also emits a
`coord.voice_room` event on `bus_broadcast` so the existing COORD CHATTER
section surfaces the overlap without any schema change to the peer-awareness
query.

### 3.2 Why long-poll (not SSE, not a new MCP tool)

Three options were considered:

**Option A: mod3 long-polls `GET /v1/bus/bus_sessions/events?since_seq=N`**  
The per-bus events endpoint already exists (`serve_bus.go:332`). Mod3 records
the highest `seq` seen and re-polls with `?since_seq=N` on a configurable
interval (default 5 s). No kernel changes required. Resilient to restarts on
either side — mod3 replays missed events by replaying from its last `seq`.
This is the recommended approach.

**Option B: mod3 subscribes to the per-bus SSE stream
`GET /v1/bus/bus_sessions/stream`**  
The SSE stream route exists (`serve_bus.go:17`). Mod3 already has async SSE
infrastructure in `bus_bridge.py`. However, this stream covers the global
event store — it is not bus-filtered at the URL today. A new per-bus SSE
endpoint would be clean, but requires a kernel change, widening scope. Leave
for a follow-up.

**Option C: new `mod3_subscribe_bus` MCP tool on the kernel**  
This would require a new MCP tool, a new transport path, and would be
synchronous (MCP tools are request/response, not streaming). Not appropriate
for a continuous subscription. Reject.

**Option A is the recommended transport.** The polling loop is structurally
identical to what `bus_bridge.py` already does for the dashboard (a
reconnecting subscriber), just using the REST events endpoint instead of the
SSE stream.

---

## 4. Protocol Detail

### 4.1 Mod3 agent-session map

Mod3 maintains a new in-memory map `_bus_session_state`:

```python
# Pseudocode — not implementation
{
    "slowbro-laptop-cogos-gap-closure": {
        "session_id": "slowbro-laptop-cogos-gap-closure",
        "workspace": "${COGOS_WORKSPACE:-$HOME/workspaces/cog}",
        "role": "claude-code",
        "status": "active",
        "last_seen": <unix epoch float>,
        "ended": False,
    },
    ...
}
```

Updated by `session.register` and `session.heartbeat` (bump `last_seen`).
On `session.end`, the entry's `ended` flag is set to `True` and `last_seen`
is updated. Entries are not deleted immediately — a configurable TTL (default
300 s after `ended=True`) governs eviction so that queries against recently
ended sessions still return meaningful context.

The map is keyed on `session_id` from `bus_sessions`. This is the three-
component hyphen-validated ID format (`internal/engine/sessions.go:77`),
**not** the `cs-<hex>` channel-participant ID.

### 4.2 Correlation: agent-session ↔ channel-participant

The missing link is a `participant_id`-to-`session_id` mapping. Today mod3
receives `participant_id` on channel-participant register but no agent
`session_id`. The kernel's `ChannelSessionRecord` carries `participant_id`
(`serve_sessions_channel.go:65`) and could carry an optional `agent_session_id`
field if the caller supplies it.

Proposed wire change (kernel-side, minimal): add an optional `agent_session_id`
field to `channelSessionRegisterRequest` (`serve_sessions_channel.go:186`).
When present, the kernel passes it through in the forward body to mod3.
When absent, mod3 falls back to substring matching on `participant_id` against
agent-session `session_id` components, which is a heuristic and may mis-match.

The explicit `agent_session_id` field is the right surface. The registration
caller (Claude Code's session hook) already knows both its agent `session_id`
and the channel it is registering to; supplying `agent_session_id` is a one-
line addition to the hook.

### 4.3 Channel membership index

Mod3 maintains a second index `_channel_members`:

```python
{
    "<channel_id>": {
        "<channel_session_id>": {
            "participant_id": "...",
            "agent_session_id": "...",   # optional; may be None
            "assigned_voice": "bm_lewis",
            "state": "speaking",         # from SessionChannel
            "last_seen": <float>,
        },
        ...
    }
}
```

This index is populated on channel-participant register and pruned on
deregister. The `agent_session_id` field links to `_bus_session_state`.

### 4.4 New query endpoint: `GET /v1/channels/{id}/peers`

Mod3 exposes this endpoint on the existing FastAPI app (`http_api.py`). It
returns current channel membership with joined agent-session state:

```json
{
  "channel_id": "general",
  "members": [
    {
      "channel_session_id": "cs-a1b2c3d4e5f6",
      "participant_id": "claude-code-session-A",
      "agent_session_id": "slowbro-laptop-cogos-gap-closure",
      "assigned_voice": "bm_lewis",
      "state": "speaking",
      "workspace": "${COGOS_WORKSPACE:-$HOME/workspaces/cog}",
      "role": "claude-code",
      "agent_active": true,
      "last_seen": 1746212345.678
    },
    {
      "channel_session_id": "cs-f6e5d4c3b2a1",
      "participant_id": "claude-code-session-B",
      "agent_session_id": "slowbro-laptop-eval-runner",
      "assigned_voice": "af_heart",
      "state": "idle",
      "workspace": "${MYRGIC_REPOS_ROOT:-$HOME/workspaces/myrgic}",
      "role": "claude-code",
      "agent_active": true,
      "last_seen": 1746212300.123
    }
  ],
  "ts": "2026-05-02T21:00:00Z"
}
```

`agent_active` is derived from `_bus_session_state[agent_session_id].ended`
and `last_seen` against the configured TTL. When `agent_session_id` is absent
(no correlation), `agent_active` is `null` and workspace/role are omitted.

This endpoint should return 200 with an empty `members` array when the channel
has no registered participants or does not exist — not 404. Mod3 channels are
ephemeral; no registered participants is the normal pre-registration state.

### 4.5 Kernel side: new peer-awareness section

The peer-awareness query (`internal/engine/peer_awareness_query.go`) gains an
optional section 5: VOICE-CHANNEL CO-PRESENCE. The section is rendered when:

- The requesting session (`sid`) has a `channel_id` associated with it (either
  passed as a query parameter or looked up from the kernel's
  `channelSessionRegistry`).
- `GET {Mod3URL}/v1/channels/{id}/peers` returns at least one peer other than
  the requesting session.

The section renders as:

```
VOICE-CHANNEL CO-PRESENCE:
  channel general — 2 agents
    af_heart (eval-runner, idle)  workspace: ${MYRGIC_REPOS_ROOT:-$HOME/workspaces/myrgic}
```

The kernel calls mod3's `/v1/channels/{id}/peers` with a configurable timeout
(same 8 s pattern as `defaultMod3ForwardTimeout` in
`serve_sessions_channel.go:159`). On mod3 unreachable, the section is silently
omitted — co-presence is enhancement, not a hard requirement for the packet.

Token budget: the co-presence section gets a weight of 0.10, borrowed from the
COORD CHATTER section (reducing it from 0.15 to 0.10). Total remains 1.0:

```
my_activity:   0.35  (unchanged)
handoffs:      0.20  (unchanged)
peer_activity: 0.30  (unchanged)
coord:         0.10  (was 0.15)
co_presence:   0.10  (new)
```

### 4.6 Optional: mod3 emits `coord.voice_room` on overlap

When the channel membership index transitions from 1 member to 2+ members, or
when a member's agent-session state changes (register/heartbeat arriving from
`bus_sessions` for a session that is currently a channel member), mod3 emits:

```json
{
  "bus_id": "bus_broadcast",
  "type": "coord.voice_room",
  "from": "mod3",
  "payload": {
    "channel_id": "general",
    "members": ["slowbro-laptop-cogos-gap-closure", "slowbro-laptop-eval-runner"],
    "event": "member_joined",   // "member_joined" | "member_left" | "state_changed"
    "summary": "2 agents in channel general"
  }
}
```

This event type matches the `coord.*` prefix filter in `composeCoord`
(`peer_awareness_query.go:824`), so it surfaces in section 4 (COORD CHATTER)
of the existing peer-awareness packet without any schema change. The kernel-
side section 5 (VOICE-CHANNEL CO-PRESENCE) is additive — both surfaces can be
live simultaneously.

The `coord.voice_room` emission is optional at initial implementation. It
provides immediate value for existing consumers of the peer-awareness packet
before the section 5 rendering is wired.

### 4.7 Poll loop: mod3 side

The poll loop runs as an asyncio background task spawned in `_lifespan` in
`http_api.py`, following the same pattern as `start_bridge`.

Pseudocode for the poll loop:

```python
async def _bus_sessions_poll_loop(state):
    last_seq = 0
    backoff = [5, 10, 20, 30, 60]  # seconds
    attempt = 0
    while not state.stopping:
        try:
            events = await _fetch_bus_events("bus_sessions", since_seq=last_seq)
            attempt = 0
            for evt in events:
                _apply_bus_session_event(evt)
                last_seq = max(last_seq, evt["seq"])
        except Exception as exc:
            wait = backoff[min(attempt, len(backoff)-1)]
            logger.warning("bus_sessions poll failed: %s — retry in %ds", exc, wait)
            attempt += 1
            await asyncio.sleep(wait)
            continue
        await asyncio.sleep(5)  # configurable: MOD3_BUS_SESSIONS_POLL_INTERVAL_S
```

`_fetch_bus_events` calls `GET {COGOS_ENDPOINT}/v1/bus/bus_sessions/events?since_seq={N}`.
The endpoint is the existing `handleBusEvents` handler (`serve_bus.go:332`),
which already accepts a `?since` query parameter that filters to `seq > N`.

`_apply_bus_session_event` dispatches on `evt["type"]`:

- `session.register` — upsert into `_bus_session_state`, update
  `_channel_members` if `agent_session_id` is known.
- `session.heartbeat` — bump `last_seen` in `_bus_session_state`, emit
  `coord.voice_room` `state_changed` if that session is a channel member.
- `session.end` — set `ended=True` in `_bus_session_state`, schedule TTL
  eviction, emit `coord.voice_room` `member_left` if that session is a channel
  member.

### 4.8 Restart and recovery behavior

Mod3 does not persist `_bus_session_state` across restarts. On startup the
poll loop begins at `last_seq=0` and replays all historical `bus_sessions`
events. This is acceptable for the initial implementation: the event store is
append-only and replay is fast (sequential read, no I/O fan-out).

For workspaces with large `bus_sessions` histories, a configurable
`MOD3_BUS_SESSIONS_REPLAY_WINDOW_S` can bound the replay by filtering to
events within the last N seconds. Events older than the window are skipped;
any session that hasn't heartbeated within the window is treated as inactive.
Defaulting to 3600 s (1 hour) covers all practical same-day active sessions.

Cross-node mod3 instances (multiple mod3 processes on different machines
reading the same kernel's `bus_sessions`) each maintain their own
`_bus_session_state`. There is no synchronization between them. This is
correct: channel membership is local to a mod3 instance; two mod3 instances
on different machines serve different physical output devices.

---

## 5. Composition with Existing Primitives

### 5.1 Peer-awareness packet: 4 sections → 5 sections

The `PeerAwarenessRequest` struct (`peer_awareness_query.go:153`) gains an
optional `ChannelID string` field. When set, the render function calls
`GET {Mod3URL}/v1/channels/{id}/peers` and composes section 5. The section is
conditional on Mod3URL being configured — the existing `forwardMod3` helper
(`serve_sessions_channel.go:519`) handles the transport consistently.

The `peerAwarenessDeps` struct (`peer_awareness_query.go:210`) gains an
optional `channelPeers channelPeerReader` interface with a single method:
`ReadChannelPeers(ctx, channelID) ([]ChannelPeer, error)`. The Server wires
this from `NewPeerAwarenessDepsFromServer` when Mod3URL is configured.

This follows the existing dependency-injection pattern: the render function
stays a pure function over its inputs; the HTTP handler wires the live
dependencies.

### 5.2 `bus_broadcast` coord events: existing path, no change

`coord.voice_room` events land on `bus_broadcast` via the kernel's existing
`POST /v1/bus/send` route (or mod3 calling the kernel's `cog_emit_event` MCP
tool if it is registered as an MCP client). The `composeCoord` function
already picks up any `coord.*` event. No changes needed to the peer-awareness
query's section 4 logic.

### 5.3 Channel-session lifecycle: existing forward path, small extension

The only kernel-side schema change in the recommended design is the optional
`agent_session_id` field on `channelSessionRegisterRequest`. This is additive
and backward-compatible: callers that do not supply it continue to work;
mod3 logs a warning that correlation is unavailable for that participant.

### 5.4 mod3 MCP surface: no new tools in phase 1

The existing `mod3_list_sessions` MCP tool already returns the merged kernel +
mod3 session roster. A future `mod3_channel_peers` MCP tool could wrap
`GET /v1/channels/{id}/peers` for LLM-facing queries, but this is out of scope
for the initial implementation. The peer-awareness section 5 calls the HTTP
endpoint directly.

---

## 6. Open Questions

**Q1: Where does `channel_id` come from in the peer-awareness request?**

The `UserPromptSubmit` hook currently only knows its own `sid`. Two options:
Option A (recommended) — the hook caches `channel_id` from its own channel-
participant registration response and passes it as a query param. This is
available because the same hook does the registration. Option B — the kernel
looks up `channel_id` from `channelSessionRegistry` by matching
`participant_id` against `sid`, which is a heuristic and may mis-match.

**Q2: Does mod3 persist `_bus_session_state` across restarts?**

Current design does not. Full replay from seq 0 is simple and correct for the
near term. A checkpoint file (`{"last_seq": N}`) is an easy addition if replay
latency becomes a problem. Deferred.

**Q3: Cross-node mod3 (two mod3 instances, one kernel)?**

Out of scope. Each mod3 instance manages its own physical audio output;
channel membership is local. Cross-node federation requires a cross-node bus
topology that is tracked separately.

**Q4: How does mod3 write `coord.voice_room` to `bus_broadcast`?**

Direct HTTP: `POST {COGOS_ENDPOINT}/v1/bus/send`. Simpler than an MCP round-
trip; no kernel changes required.

**Q5: What if `bus_sessions` events arrive for sessions not yet in a channel?**

Mod3 updates `_bus_session_state` regardless of channel membership. Correlation
happens at query time, not at registration time — a session may join a channel
after its agent session is already live.

---

## 7. Out of Scope

- **Voice-stream routing** — the mechanism by which one session's TTS output
  is routed to another session's audio input. Requires pipeline changes to
  mod3's audio subscriber architecture (`audio_subscribers.py`). This RFC
  provides the presence substrate that makes routing meaningful; it does not
  implement routing.

- **New bus types** — this RFC reuses `bus_sessions` and `bus_broadcast`
  unchanged. No new bus names, no new event type namespaces beyond
  `coord.voice_room`.

- **Per-bus SSE stream for `bus_sessions`** — subscribing to
  `GET /v1/bus/bus_sessions/stream` instead of polling the events endpoint
  would eliminate polling latency. This requires a kernel route change
  (the current SSE handler in `serve_bus.go:473` serves the global stream,
  not a per-bus filtered stream). Worthwhile follow-up; not required for
  correctness.

- **Channel-session ↔ agent-session schema unification** — the comment in
  `serve_sessions_channel.go:20-24` notes that unifying the two session shapes
  at the MCP tool layer is "Wave 3" aspirational work. This RFC adds the
  `agent_session_id` bridge field as a minimal correlation without committing
  to schema unification.

- **Multi-workspace mod3** — a single mod3 instance serving sessions from
  multiple kernels on different workspaces. Not the current deployment model.

---

## 8. Implementation Milestones

The following sequence is ordered by dependency. An agent picking this up next
session should tackle them in order.

**Milestone 0 (prerequisite): Fix #154.**

Issue #154 documents that `GET /v1/sessions` returns empty after successful
register — the in-memory registry is not updated alongside the bus append in
`serve_sessions_mgmt.go:50,109`. Until #154 is fixed all end-to-end verification
of this RFC is blocked, though the protocol design is independent.

**Milestone 1: mod3 poll loop + `_bus_session_state`**

Add `_bus_sessions_poll_loop` as an asyncio task in `_lifespan`
(`http_api.py:57`), alongside the existing bridge tasks. Polls
`GET {COGOS_ENDPOINT}/v1/bus/bus_sessions/events?since_seq={N}`. No kernel
changes required.

**Milestone 2: `agent_session_id` on channel-participant register**

Add optional `agent_session_id` to `channelSessionRegisterRequest`
(`serve_sessions_channel.go:186`), forward it through to mod3. Update the
session hook to supply it at registration time.

**Milestone 3: `_channel_members` index + `GET /v1/channels/{id}/peers`**

Add the `_channel_members` index inside mod3, populate it from the register/
deregister handlers, and expose the new FastAPI route.

**Milestone 4: `coord.voice_room` emission**

On membership change, mod3 posts `coord.voice_room` to
`POST {COGOS_ENDPOINT}/v1/bus/send`. Surfaces immediately in the existing
COORD CHATTER section of the peer-awareness packet — no kernel change needed.

**Milestone 5: kernel peer-awareness section 5**

Add `ChannelID string` to `PeerAwarenessRequest` (`peer_awareness_query.go:153`),
add `channelPeerReader` to `peerAwarenessDeps`, add `composeCoPresence` section
composer, wire from `NewPeerAwarenessDepsFromServer`, update the
`UserPromptSubmit` hook to pass `channel_id` when known.

---

## Appendix: Key File References

| File | Lines | What |
|------|-------|------|
| `internal/engine/sessions.go` | 47-52 | `bus_sessions` constant + event type constants |
| `internal/engine/sessions.go` | 106-128 | `SessionState` struct — payload shape mod3 will parse |
| `internal/engine/serve_sessions_channel.go` | 186-195 | `channelSessionRegisterRequest` — add `agent_session_id` here |
| `internal/engine/serve_sessions_channel.go` | 254-342 | `RegisterChannelSession` — forward `agent_session_id` to mod3 |
| `internal/engine/serve_sessions_channel.go` | 519-567 | `forwardMod3` helper — pattern for section 5 mod3 call |
| `internal/engine/serve_bus.go` | 332-355 | `handleBusEvents` — poll target for mod3 loop |
| `internal/engine/peer_awareness_query.go` | 153-174 | `PeerAwarenessRequest` — add `ChannelID string` field |
| `internal/engine/peer_awareness_query.go` | 210-255 | `peerAwarenessDeps` interfaces — add `channelPeerReader` |
| `internal/engine/peer_awareness_query.go` | 808-858 | `composeCoord` — structural reference for section 5 |
| `mod3/session_registry.py` | 618-700 | `SessionRegistry` — where agent-session map lives |
| `mod3/bus_bridge.py` | 84-160 | `KernelBusSubscriber` — structural reference for poll loop |
| `mod3/http_api.py` | 57-120 | `_lifespan` — spawn poll task here |
