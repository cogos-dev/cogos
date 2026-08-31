---
type: rfc-draft
title: "Two-plane runtime: machine-plane service + user-plane session agent"
status: draft
relates:
  - "myrgic/cogos#586"   # anchor issue — REFERENCED, NOT CLOSED: the issue tracks the feature through implementation; this RFC is design only
  - "myrgic/cogos#101"
  - "myrgic/cogos#551"
  - "myrgic/cogos#591"   # write-path inventory + golden-diff gate — the generated enumeration §5.1b/§5.1c/§6.4 now defer to
  - "ADR-001"   # workspace membrane (substrate corpus)
  - "ADR-065"   # container-native daemon lifecycle (substrate corpus)
  - "ADR-074"   # nested sovereignty / scope inheritance (substrate corpus)
  - "ADR-093"   # managed-session processes for attachment
  - "ADR-094"   # lineage observatory
  - "ADR-095"   # daemon reconcile-loop driver
  - "ADR-097"   # memory projection reconciler
  - "ADR-098"   # skill projection reconciler
  - "ADR-099"   # node identity layering
  - "ADR-102"   # operator as reconcilable
  - "ADR-121"   # single-binary consolidation
  - "RFC-033"
  - "RFC-036"
scope: "Design only — no implementation in this document"
revision: "fifth draft — fourth repair pass; hand-authored write-path enumerations retired in favour of the generated inventory landed by #591"
sections:
  - "1. Summary"
  - "2. Motivation"
  - "3. Design overview"
  - "4. The boundary"
  - "5. State: two strands and a shared library"
  - "6. Install-time story and doctor surface"
  - "7. Migration ladder"
  - "8. The general form"
  - "9. Open questions"
  - "10. References"
---

# RFC Draft: Two-plane runtime

> **Revision note — the standard this draft is measured against.** This is the
> **fourth draft**, produced by the **third repair pass** against adversarial
> re-review. The three passes were judged under progressively sharper standards;
> this one was measured under the **design-draft standard**, stated here so a
> reader can apply it rather than infer it:
>
> > This document's declared scope is **design only**. It passes iff **(a)** it
> > makes no claim that is false against the kernel code or the substrate
> > corpus; **(b)** every surface, mechanism, and write path the design touches
> > is **named** — no undeclared doors, writers, or execution paths; and
> > **(c)** every unresolved design point is **explicitly declared** as an open
> > question or as a named conflict with a stated resolution path. An
> > unresolved-but-declared question is a pass. A false claim, an undeclared
> > surface, or a question silently assumed answered is a fail.
>
> That standard is why several items below are *declared open* rather than
> decided. It is also why, as of this pass, the write-path enumerations in
> §5.1b / §5.1c / §6.4 are **no longer hand-authored closed lists**.
>
> ---
>
> **What the fourth pass changed: the enumeration asymptote, closed by a
> generated inventory.** The council's standing objection to the third draft was
> that a hand-authored table of write paths is an **enumeration asymptote** — it
> converges on completeness without ever reaching it, and each pass had in fact
> found writers the previous pass missed (the third pass alone added two). A
> closed list a human maintains is not an instrument; it is a recollection with
> a table around it.
>
> That objection is now answered by code rather than by another sweep.
> **myrgic/cogos#591 merged to `main`** (squash `6c2f634`), landing
> `internal/writepathaudit` — a **code-derived** inventory of every filesystem
> write site in the repository, with a **golden-diff gate**
> (`internal/writepathaudit/testdata/inventory.golden.{json,md}`) that fails CI
> when a write path appears or disappears without the golden being
> re-declared. Its own `scan_test.go` names this RFC as the reason it exists:
> *"the two-plane RFC's section 6.4 lint made real: the declared set is
> generated, not hand-authored — a human only ever approves a diff."*
>
> Accordingly, in this pass:
>
> 1. **§5.1b and §5.1c now DEFER to the generated inventory** as the
>    authoritative enumeration, and their tables are demoted to **illustrative
>    excerpts, marked as such**. §6.4's declared-set lint is re-pointed at the
>    golden instead of at prose.
> 2. **The inventory immediately falsified two hand-authored claims**, which is
>    the argument for the deferral rather than an embarrassment to it. §5.1c
>    said "all three writers reach `.cog/ledger/` through `AppendEvent`
>    (`internal/engine/ledger.go`)". The inventory shows a **fourth bucket**
>    (`identity-grants`, written by the machine-plane HTTP listener at
>    `internal/engine/serve_identity_grants.go`) and a **second,
>    independently-drifted `AppendEvent`** at `pkg/cogblock/ledger.go` reaching
>    the same directory. Both are now carried.
> 3. **§5.1b row 5's citation was imprecise** and is corrected: the BEP state
>    dir is *configured* at `internal/engine/bep_engine.go:200` but the write
>    primitive is `PersistIndex` in `pkg/substrate/bep/index.go`, which the
>    inventory bins `elsewhere` as `{stateDir}` because the anchor does not
>    resolve statically. The inventory's own test records this as a named gate.
> 4. **The inventory's honesty margin is adopted as the RFC's.** 237 sites, of
>    which **84 unanchored** and **41 dynamic** — i.e. the tool reports what it
>    cannot resolve rather than dropping it, and 73 subprocess writers are
>    declared out of scope for v1. §6.4 inherits those margins explicitly rather
>    than inheriting a table that looks complete.
>
> Two items are **deliberately left as they stand**, flagged for the operator
> and not resolved by this pass: the strand-scoping reading (below) and §5.1c's
> "count of two". Both are his calls, not the pilot's.
>
> ---
>
> **What the third pass changed.** Two fresh adversarial lenses (security;
> corpus coherence) re-measured the third draft and did not clear the gate. The
> corrections in this pass, all verified against code before being written:
>
> 1. **The config HTTP write path had no disposition.** `serve_config.go`
>    registers `PATCH /v1/config` and `POST /v1/config/rollback`; the third
>    draft split the config *file* by plane and said nothing about the route
>    that rewrites it. §4.1 item 5 scopes those routes to the seat/workspace
>    `kernel.yaml` **only**; §6.4 lints it; §3.3 records the route-scoping as
>    net-new.
> 2. **Skill execution was a machine-plane code-execution path from a
>    seat-writable directory.** `POST /v1/skills/{name}/exec` executes a binary
>    resolved from `~/.claude/skills` or `<workspaceRoot>/.claude/skills`. §4.1
>    item 6 moves it off the machine plane entirely, under a new invariant in
>    §5.8.
> 3. **The minting gate's "weakens" was a closed enumeration** and §5.7's own
>    opening sentence filed two weakenings in the routine bucket. §5.7 now
>    defines weakening **non-enumeratively**.
> 4. **§5.1b's enumeration of machine-plane writes inside `.cog/` was itself
>    incomplete** — it missed the BEP agent-definitions sync directory (whose
>    content is authored by *remote peers*) and the BEP sync-state directory.
>    Both are added; the peer-sourced one is escalated to an open question (Q8).
> 5. **§5.2's replication claim was an overstatement and contradicted §5.8.**
>    "`.cog/` is BEP-replicated" is retracted: as shipped, BEP replicates
>    exactly one subtree. The ledger strands are declared **never** in the
>    replicated set, with the forgery reason stated and the inclusion list named
>    as the lintable object.
> 6. **Shipped kernel writers append inside `.cog/ledger/`.** The third draft
>    asserted the opposite as an invariant and lints it. New §5.1c enumerates
>    the three writers and dispositions them as **relocation work**, and §6.4's
>    lint is marked as a post-rung-3 invariant rather than a present-tense fact.
> 7. **ADR-097 / ADR-098 write projections into the same directories as the
>    authored corpus**, so §5.1's two rows collide over one path and the ACL has
>    no separable subtree to attach to. This RFC **cannot settle it** and does
>    not pretend to: it is declared as **Q7** with two resolution paths, and
>    §6.4's lint is scoped to the separable rows only.
> 8. **The credential-distribution loop's mechanism was implicit.** §4.1 item 3
>    now states that the agent distributes over **existing seat-owned
>    mechanisms** and opens **no new listener**, with the residual mechanism
>    choice declared as part of the rung-3e work item.
> 9. **ADR-094 is status `Draft`** and was cited as accepted authority. Marked,
>    and the lineage row now leans on ADR-095's accepted loop.
>
> ---
>
> **Prior passes, retained.** The **second** draft incorporated a five-lens
> adversarial review (Windows mechanics, security, substrate coherence,
> cross-platform parity, operations) and an operator ruling on the state
> question the review could not settle; three first-draft claims were
> **retracted**, not softened: the "exactly one door" invariant (§4.1), the
> inheritance-from-#101 supervisor-seam claim (§3.3), and the single-ledger
> reading of the state partition (§5).
>
> The **third** draft answered a second adversarial re-review by two fresh
> lenses (security; corpus coherence) that did **not** clear the blocker gate.
> Four further claims were **retracted or corrected against code**:
>
> 1. **`kernel.yaml` was on the wrong side of the partition.** It is loaded from
>    `CogDir/config/kernel.yaml` (`internal/engine/config.go`), i.e. inside
>    `.cog/` — seat(local)-written, git-settled, and inside the directory BEP
>    syncs a subtree of (§5.2) — and it is where
>    `EnableServiceControl`, `EnableSkillExec`, `EnableConfigMutation`,
>    `BindAddr` and `WriteRouteGrantAuthDisabled` are set. The machine plane's
>    entire security posture was therefore seat(local)-editable with a text
>    editor.
>    §5.1a splits the file; §6.4 lints the split.
> 2. **The machine-plane × workspace-portable cell is not empty.** Shipped code
>    writes inside `.cog/` from the machine plane (ADR-065's daemon state file;
>    the projection reconcilers of ADR-097 / ADR-098). §5.1 enumerates those
>    paths instead of asserting emptiness, and §5.2 restates the isolation
>    invariant over the **two ledger strands** rather than over every byte of
>    `.cog/`.
> 3. **The NodeID relocation seam does not exist.** `COG_NODE_DIR` moves the
>    node-id *cache*, not the BEP cert; the cert anchor is hardcoded
>    (`internal/engine/process.go`, `nodeIDCertDir`). §7.2 and §3.3 are
>    corrected: threading a cert-dir override is net-new work.
> 4. **§8's second instance does not exist yet.** BEP as shipped is Syncthing
>    block exchange with no envelope/receipt settlement; the inter-node instance
>    is restated as **proposed**.
>
> **Flagged for operator review.** Item 2's rescoping — isolation protects the
> two *ledger strands*, not all of `.cog/` — is the pilot's faithful reading of
> an operator ruling that was about **ledgers**. The shipped machine-plane
> writes inside `.cog/` were not before the operator when the ruling was made.
> If the intended reading was the stronger one (no machine-plane write lands in
> `.cog/` at all), then ADR-065's daemon state file and the ADR-097 / ADR-098
> projection reconcilers are in scope for relocation and this RFC understates
> the work. Called out rather than decided.
>
> **Second operator flag, new in this pass.** §5.1c records that shipped kernel
> code appends to `.cog/ledger/` from the machine plane today (the worktree
> reconciler, the kernel process's own session bucket, and an `mcp-client`
> bucket). Under the weak reading of the ruling those are relocation work at
> rung 3; under the strong reading they were never admissible at all. Either
> way they are **named** here rather than covered by an invariant that reads as
> already true. The disposition this draft proposes — relocate the three
> kernel-emitted buckets to the machine strand — is stated in §5.1c and listed
> as net-new in §3.3.

---

## 1. Summary

The kernel today runs as a single user-identity process kept alive by whatever
per-platform supervision is at hand: a launchd LaunchAgent on macOS, a
Scheduled Task on Windows, a systemd user unit on Linux. This RFC splits that
runtime into two cooperating planes with distinct identities and lifecycles:

- **Machine plane** — `cogos service run`: a real OS service that boots with
  the node, owns node membership, and runs regardless of who is logged in.
- **User plane** — `cogos agent run`: a thin session agent that runs in the
  operator's logon session and owns everything that genuinely requires the
  operator's identity.

The two planes communicate over a declared IPC door (§4). The service never
impersonates the user; it requests user-plane work from the agent, which
executes in its own honest identity (§4.4, "RPC inversion").

The split makes the machine/operator distinction structural rather than
inferred:

- **service up = the node is reachable and live on the constellation.** Not
  "present": node identity is machine-scoped and cert-anchored (RFC-036), and
  a node does not stop being a node when its service is down. Presence is
  derived from attention and bus events, not from one process's liveness.
- **agent up = this node can execute user-plane work in the operator's
  identity.** Not "the operator is seated": under the corpus definition of
  *seat* (ADR-102 and the multi-seat session-occupancy RFC) an operator may be
  seated from Telegram, from voice, or from a conducting surface with no logon
  session on this node at all.

### 1.1 Terminology status — unsettled, deliberately

`service`, `agent`, and the local-logon sense of *seat* used throughout this
document are **placeholders**. The naming collision is real and acknowledged:

- **`seat`** is already defined by ADR-102 as a participant attached to a
  session — ADR-102 states the session is a **2-seat** coordination object
  (operator and agent binding simultaneously to one harness instance); the
  multi-seat session-occupancy RFC (draft) is what generalizes that to N
  co-resident seats keyed `(session_id, seat_id)`. This RFC repeatedly needs a
  word for "the operator's local OS logon session," which is a *different
  referent*. Where this document writes "seat" in the local-logon sense it is
  marked **seat(local)** on first use in each section, **including where the
  word appears inside a name** — the strand name in §5.2, the axis label in
  §5.1, and the lint text in §6.4 are all marked, because a name is exactly
  where an unmarked collision does its damage.
- **`agent`** overloads one of the five named primitives in RFC-033, which
  carries its own agent-as-projection thesis.

**What "the seat(local) strand" is, concretely, today.** The re-review is right
that the name outruns the implementation, and the gap is worth stating rather
than deferring to Q5. The shipped ledger is keyed by **session**, not by seat:
`<workspaceRoot>/.cog/ledger/<sessionID>/events.jsonl`
(`internal/engine/ledger.go`). So:

- The strand this RFC calls the seat(local) strand is **realized today as the
  set of session-keyed ledgers under `.cog/ledger/`**. Per-seat identity is an
  **aggregation over sessions**, not a storage key. Nothing in this RFC requires
  re-keying the ledger; §5's invariant is about *who holds the write handle on
  that directory*, which is a single answer (the operator's logon identity)
  regardless of how the directory is subdivided.
- **Off-node seats do not contradict this.** A seat in the ADR-102 sense
  attached from Telegram, from voice, or from a conducting surface holds **no
  local ACL write on the strand at all**, and is not supposed to. Only
  **seat(local)** — the operator's logon session on this node — writes the
  strand directly. Every other seat reaches it the same way any other caller
  does: **through the door**, as a request the local plane executes and records.
  That is consistent with §5.2's write isolation, not an exception to it: the
  ACL names one local principal precisely so that remote participation is
  mediated rather than direct.

The correct terms should come from the dome taxonomy. **This is an open
question awaiting an operator ruling (§9 Q5) and is a ratification gate, not a
cosmetic cleanup.** Nothing else in this RFC depends on which terms win; every
structural claim is stated in terms of *plane* and *identity*, which are not
contested.

---

## 2. Motivation

### 2.1 The incident (2026-08-28)

On a Windows node in the constellation, `cogos-kernel` and a companion service
were supervised as Scheduled Tasks with **S4U logon** — "run whether user is
logged on or not," no stored password. S4U logon has an **empirically
reproduced** side effect: it purges the user's saved Domain Password
credentials from Credential Manager around interactive logon. The operator's
persistent SMB drive mapping broke silently; the DPAPI credential blob was
rewritten in the same second as the first SMB guest-rejection event, and the
behavior reproduced on two separate tasks on that node.

The evidence is timestamp-correlated first-party observation, not a citable
vendor KB — hence "empirically reproduced" rather than "documented." The
incident record is myrgic/cogos#586. Diagnosis cost an afternoon. For an
ordinary user the experience would read: *"I installed CogOS and my network
drives stopped working."*

### 2.2 No single-process shape is correct

The interim fix — a boot task with a stored password — surfaces the underlying
constraint rather than resolving it:

| Shape | Admin? | User password? | Headless at boot? | Hazard |
|---|---|---|---|---|
| Logon task (Interactive) | no | no | no | kernel down until logon |
| Boot task (S4U) | once | no | yes | **purges saved credentials** |
| Boot task (stored password) | once | yes, typed | yes | silent breakage on password rotation; impossible on passwordless / PIN-only accounts |
| Service as SYSTEM / virtual account | once | no | yes | wrong identity — no user profile, HKCU, DPAPI, or per-user OAuth stores |

Every row fails on either **availability** (up before logon), **credentials**
(demanding a password UAC cannot collect, from accounts that may not have
one), or **identity** (running as not-the-user, unable to dispatch lanes
against per-user token stores). No single process can satisfy all three,
because "boot-time" and "the user's identity" are different currencies on
every modern OS. Windows merely charges for the difference up front; macOS and
Linux make the same distinction (LaunchDaemon vs. LaunchAgent, system unit vs.
user unit) and we have simply been living on the user-plane half everywhere.

### 2.3 Prior art

The runtimes that solved this converged on the same shape: `tailscaled` +
`tailscale` CLI/GUI over a local socket; Docker Desktop's system service +
per-user backend; Win32-OpenSSH's service spawning into user sessions;
1Password's split helper. The two-plane shape is an attractor, not a Windows
accommodation. §8 says what else sits on the same attractor.

---

## 3. Design overview

```
┌────────────────────────── machine plane ──────────────────────────┐
│ cogos service run                                                 │
│ identity: NT SERVICE\cogos / launchd daemon / systemd system unit │
│ lifecycle: boots with the node, SCM-native recovery               │
│ owns: BEP membership + heartbeat, reconcile loop, self-update,    │
│       machine-scoped node state, the machine ledger strand,       │
│       and (see §4.1) the HTTP listener                            │
└──────────────────────────────┬────────────────────────────────────┘
              declared IPC door (§4) — one of two inbound surfaces
┌──────────────────────────────┴──────────── user plane ────────────┐
│ cogos agent run                                                   │
│ identity: the operator, inside their logon session                │
│ lifecycle: starts at logon, ends at logoff                        │
│ owns: lane dispatch against per-user OAuth stores, DPAPI /        │
│       Credential Manager, HKCU, mapped drives, notifications,     │
│       and the seat(local) ledger strand                           │
└───────────────────────────────────────────────────────────────────┘
```

Both planes are the same binary (ADR-121 single-binary consolidation), role
selected by subcommand — the `tailscaled`/`tailscale` pattern.

### 3.1 Machine plane

Runs under the least-privileged identity that can do the job — on Windows a
**virtual service account** (`NT SERVICE\cogos`): created automatically at
service registration, no password exists to store or rotate, own SID for
ACLs, strictly weaker than SYSTEM. On macOS, a LaunchDaemon running as a
**dedicated `_cogos` user**; on Linux, a systemd system unit with either a
dedicated static user or `DynamicUser=` (§6.3 weighs the two).

**The second draft offered "root or a dedicated `_cogos` user" on macOS. Root
is struck.** It is not a weaker-but-acceptable option; it is a conforming-
looking choice that **voids the settlement primitive this RFC is built on**.
§5.4 defines a settled cross-plane pair as two-party attestation *because
neither identity can write the other's strand*. Root can write the seat(local)
strand. On a root LaunchDaemon every "settled" pair is self-attested and the
count of two is fiction — the doctor would report attestation it did not
receive, which is worse than reporting none. A dedicated `_cogos` user is the
only conforming macOS choice.

Generalized, and stated once here rather than per platform:

> **Privilege ceiling invariant.** The machine-plane identity must hold **no
> privilege that overrides the seat(local) strand's ACL**. Not root; not
> SYSTEM or Administrators; specifically no `SeBackupPrivilege`,
> `SeRestorePrivilege`, or `SeTakeOwnershipPrivilege` on Windows, and no
> `CAP_DAC_OVERRIDE` on Linux. An identity that can read or rewrite the other
> strand by privilege rather than by ACL is not a second party.

The invariant is restated as a partition property in §5.2 and given a
per-platform lint in §6.4. It is symmetric: §5.8's "no seat(local)-writable
path under the machine root" is the other direction of the same rule, and the
second draft lints only that one.

The service never presents UI, never reads a user profile, and never holds
user credentials. Session-0 isolation on Windows enforces the first; this RFC
adopts all three as invariants on every platform, and §6.4 gives each one a
platform-specific doctor lint so the invariants are checkable rather than
merely asserted.

**Supervision.** Recovery moves from our hand-rolled watchdog (repeating
trigger + IgnoreNew) to the platform's native mechanisms: SCM failure actions,
launchd `KeepAlive`, systemd `Restart=`. This is an upgrade **scoped to
crash/exit recovery only**. All three native mechanisms are process-exit
triggered, not liveness triggered: an internally hung but still-running kernel
is invisible to every one of them, and the `/health`-polling watchdog did
catch that case. Two consequences the design must carry:

1. `cogos service run` keeps a lightweight in-process liveness self-check that
   **exits the process** when it fails, giving native recovery something to
   react to. The hang case is covered by the service failing honestly, not by
   an external poller.
2. SCM's failure-count and reset-period are an explicit configuration choice,
   not a default. Exhausted failure actions mean SCM *stops acting* rather
   than restarting indefinitely, which is a silent-death mode.

### 3.2 User plane

The agent is the current per-user supervision shape demoted to a thin client:
logon-triggered task / LaunchAgent / systemd user unit, no elevation, no
password, registered without any prompt. It connects to the service's door on
start, announces its seat(local) principal, and executes user-plane work the
service requests.

**Concurrent agents are already reachable today**, not a future condition:
Task Scheduler logon triggers are per-user and fire per session, so two users
switched in under Fast User Switching produce two live agent instances. The
first draft's "multiple sequential logons" was wrong. What is genuinely open
is envelope scoping across those concurrent agents (§9 Q2).

### 3.3 What this RFC does *not* inherit — retraction

The first draft claimed the two planes "slot in behind the existing
`service_supervisor_stub` seam" in the services subsystem, and that the #101
driver work "already carries the supervisor seam." **Both claims are
withdrawn.** They are wrong on the facts and would mislead an implementer
picking up §7 rung 3. Accurately:

- A generic `ServiceSupervisor` interface exists
  (`internal/engine/service_supervisor.go`). It is the control-plane
  abstraction for *kernel-declared services*, routing start/stop/restart/
  enable/disable through kind-aware implementations, dispatched on
  `def.Kind.EffectiveKind()`.
- Only darwin has an implementation. `internal/engine/service_supervisor_stub.go`
  is a `//go:build !darwin` compile shim that aliases
  `LaunchctlController = ObserverSupervisor`; every mutation on every
  non-darwin platform returns `ErrNotControllable`. Its own comment names the
  intended additions as `SystemdSupervisor` and `DirectSupervisor`.
- **#101 declares Linux user-bus (`systemctl --user`) and plugin extension
  points and explicitly defers Windows.** A machine plane needs the system bus,
  which is the opposite scoping. No `SchtasksSupervisor` exists in any branch;
  the interim boot-task state on the Windows node was hand-registered by
  PowerShell.
- The interface is **lifecycle control over things a running kernel
  supervises**. Every method presupposes a live kernel. It cannot, by
  construction, bootstrap the kernel's own privileged OS registration.

Therefore: **this RFC adds a dependency on #101 rather than inheriting one** —
specifically, a system-bus / system-unit code path alongside the existing
user-bus one. And **privileged installation** (SCM `CreateService` under the
virtual account, a root-domain LaunchDaemon plist, a systemd system-unit
drop-in) is a *different privilege model* from lifecycle control and needs its
own design surface, sketched in §6.

Machine-plane registration is **net-new work on all three platforms.**

Seven further items belong on the net-new list. The first two were surfaced by
the second adversarial re-review and were previously described as existing
seams; the remaining five were surfaced by the third and are new work the
earlier drafts did not price at all:

- **A cert-dir seam for node-id anchoring.** `internal/engine/process.go`
  hardcodes `nodeIDCertDir` to `bep.ExpandCertDir("")` — the default
  `os.UserHomeDir()` + `/.cog/etc` (`pkg/substrate/bep/tls.go`). It is a
  package var only so tests can redirect it; no env or config override is
  threaded to it in production. The file's own comment records the gap as a
  **KNOWN BOUNDARY**: a node that overrides `cluster.CertDir` "would need that
  resolved dir threaded through `NewProcess` to stay consistent." §7.2 depends
  on closing exactly that. Net-new.
- **Splitting the kernel config file.** The machine plane's security posture is
  read today from `CogDir/config/kernel.yaml` (`internal/engine/config.go`),
  which is inside the workspace. §5.1a moves those keys to a machine-root file
  with its own loader and precedence rule. The load path is `CogDir`-anchored
  at a single site, which makes the change small to *start* and not small to
  *finish*: every caller that builds a `Config` without going through
  `LoadConfig` (tests, `testkernel`, programmatic boot) must keep getting the
  safe defaults, which is the same trap #551's inverted-polarity flag was
  written to avoid. Net-new.
- **Route-scoping the config-mutation surface.** `internal/engine/serve_config.go`
  registers `GET /v1/config`, `PATCH /v1/config` (RFC 7396 merge-patch, atomic,
  backed up) and `POST /v1/config/rollback`, all three gated only by
  `EnableConfigMutation`. Splitting the config *file* (§5.1a) accomplishes
  nothing if the merge-patch handler — running **inside the machine-plane
  process**, above the new DACL — can rewrite the machine file on request.
  §4.1 item 5 binds those routes to the seat/workspace `kernel.yaml` only.
  Making that true is code: a write-target parameter on
  `ReadConfigSnapshot`/the patch path, and a refusal for any key the machine
  file owns. Net-new.
- **Moving skill execution to the agent.** `POST /v1/skills/{name}/exec`
  (`internal/engine/serve_skills.go`) resolves a skill through `skillDirs()` —
  `os.UserHomeDir()/.claude/skills` and `<WorkspaceRoot>/.claude/skills` — and
  `pkg/skills`' `Exec` runs `exec.CommandContext` on the resolved binary with
  `os.Environ()` inherited. Under two-plane that route sits on the machine
  plane and executes a **seat-writable, workspace-portable** binary as the
  service account. §4.1 item 6 removes it from the machine plane and re-lands
  it as a door-mediated request kind executed by the agent. Net-new: a request
  kind, an agent-side executor, and the removal of the route from the listener.
- **Relocating the kernel-emitted ledger buckets** enumerated in §5.1c
  (`worktree-reconciler`, the kernel process's own session bucket, `mcp-client`,
  and — added in this pass, from the generated inventory — `identity-grants`)
  off the seat(local) strand and onto the machine strand. This is the concrete
  work behind §5.2's isolation invariant; without it the invariant is
  aspirational and §6.4's `.cog/ledger/` lint fails on a clean checkout.
  Net-new, and a prerequisite for rung 3c. **Scope grew by a quarter between
  drafts** on the strength of one generated inventory, which is the honest
  argument for §5.1b's deferral: this estimate was wrong in the direction of
  optimism three drafts running, and the correction came from a scanner rather
  than from a fourth read-through. Note also that the relocation must handle
  **two** append implementations, not one (`internal/engine/ledger.go` and
  `pkg/cogblock/ledger.go`), which is a second under-estimate the inventory's
  package doc surfaced.
- **Dispositioning the remaining `.cog/` write sites** the inventory anchors but
  §5.1b does not assign a plane to (**Q9**). Bounded sweep, prerequisite for
  rung 3c, and not a design question — but not done, and therefore not assumed.
- **A BEP replication-scope declaration the doctor can read.** §5.2 requires
  that the ledger strands are never in the replicated set. As shipped that is
  **vacuously satisfied**: `pkg/substrate/bep/` has no ignore/exclusion
  mechanism at all (no `stignore` equivalent; verified) and
  `internal/engine/bep_provider.go` syncs exactly one folder,
  `<root>/.cog/bin/agents/definitions/` (folder id `cogos-agent-defs`,
  `internal/engine/bep_model.go`). So the lintable object today is the
  **inclusion list**, not an ignore file. Net-new is only the declaration —
  making the inclusion list an observable the doctor reads. **Ignore support
  becomes net-new work only if the inclusion list is ever widened beyond a
  subtree that provably excludes both strands**, and this RFC does not propose
  widening it.
- **Repointing the node-root credential's local consumers at the agent**
  (§4.1 item 3, sequenced at rung 3e in §7.1). `internal/engine/boot_node_root_grant.go`'s
  own header names them: the dashboard, canvas, Claude Code hook sessions, and
  THESEUS. Each acquires the credential today either from the gate-exempt
  `GET /v1/identity/grants/current?surface=node-root` or from the 0600 vault
  file under the operator's home; both disappear under the split. Net-new per
  consumer: a client change to read the credential from the agent's existing
  seat-owned delivery instead. **No new listener is introduced** — see §4.1
  item 3 for the mechanism constraint and what remains open inside it.

What #101 does compose with, honestly: once both planes are registered, they
become *observable* through the services manifest and the doctor, using the
supervisor interface as an observer. That is a real and useful composition
without the bootstrap inversion.

---

## 4. The boundary

### 4.1 Two inbound surfaces, not one — retraction and posture

The first draft asserted "there is exactly one [door]; auxiliary channels are
prohibited by convention and lintable by the doctor." **That invariant is
withdrawn as written**, because §3 puts the HTTP listener on the machine plane
and the listener is a second, wider inbound surface with no OS-level ACL.
`internal/engine/serve.go` binds loopback TCP (`net.Listen("tcp", ...)`,
default bind `127.0.0.1`), which is reachable by *any* local process in *any*
account on the machine — a strictly weaker gate than a named pipe or unix
socket ACL.

Stated honestly, the machine plane has **two** inbound surfaces:

| Surface | Transport | Access control | Callers |
|---|---|---|---|
| The door | named pipe (Windows) / unix socket | OS ACL on the object | the session agent only |
| The HTTP listener | loopback TCP | **writes:** bearer-token verification in-process. **reads: none by default** — `internal/engine/serve_grant_auth.go` exempts `GET` on **every path except `/mcp`**, unconditionally | CLI, MCP clients, harnesses, any local process in any account |

**The read half of that cell is stated plainly because it is the driver for
item 3 below.** The gate is a *write*-route gate by construction and says so in
its own header. Consequences a reader must not have to infer: the whole read
surface of the machine plane — including `GET /v1/config?include_raw_yaml=1`,
which returns raw `kernel.yaml` bytes, and the grant listing that exposes
`grant_id`s — is reachable **with no credential at all** from any local
account. The node-root credential GET (item 3) is not an isolated exception to
an otherwise-authenticated read surface; it is the most valuable instance of
the surface-wide default. That is why item 3 deletes the route rather than
authenticating it: authenticating one exempt GET while the exemption rule
stands would move a value and leave the rule.

Both are in scope for §4. The relevant escalation shape is not hypothetical:
service-lifecycle mutations are gated behind `Config.EnableServiceControl`
(`internal/engine/serve_services.go`), which is **default false today** — but
the two-plane design requires service control, and once it is enabled the
chain is real, because `ServiceDef.Command` is a free-form command string that
the *service account* would then execute.

**Chosen posture** (from the two options the review put forward):

1. **The listener stays on the machine plane** — proxying all HTTP through the
   agent would make the node's API unavailable whenever nobody is logged in,
   which defeats the entire motivation (§2.2, availability leg).
2. **Service-lifecycle routes bind to a machine-plane-scoped surface.** The
   second draft left this as a disjunction ("presented over the door **or**
   bound to a machine-plane grant"). **Decided: both halves, and neither is
   optional.** `/v1/services/*` and #101's `POST /v1/services/register` are
   removed from the general loopback listener's route set and served **only on
   the door**, and the request additionally carries a grant whose surface is
   machine-plane-scoped — the surface-match discipline #551 already applies to
   the mint and revoke routes, applied to service lifecycle. A token that
   `VerifyAny` accepts from anywhere does not reach these routes, because the
   transport they live on does not accept anonymous local callers at all.
   Rationale for taking both: transport scoping alone would admit any process
   the door's ACL admits, and grant scoping alone would leave the route on a
   loopback socket reachable by every account on the box. The escalation these
   routes carry is real — `ServiceDef.Command` is a free-form command string
   the *service account* executes — so it gets the narrow transport and the
   narrow authority.
3. **The gate-exempt node-root-grant GET is deleted under two-plane.** The
   second draft left this as "authenticated, **or** delivered over the door."
   **Decided: delivered over the door; the exemption is removed, not
   authenticated in place.** `internal/engine/serve_grant_auth.go` today exempts
   `GET /v1/identity/grants/current?surface=node-root` from the grant gate and
   `internal/engine/serve_cors.go` returns the **raw token** on it by design;
   `internal/engine/boot_node_root_grant.go` mints in-process at boot and
   persists to `os.UserHomeDir()` + `/.cog/vault/node-root-grant`. Both halves
   of that bootstrap break under the split — the home directory becomes the
   service account's, and an unauthenticated local GET that returns a raw
   node-root token is a credential-distribution hole that the split widens
   rather than narrows. Under two-plane:
   - The service mints the node-root credential at boot, unchanged, and stores
     it under the **machine root** (§5.1), readable only by the machine-plane
     identity.
   - The **agent obtains it over the door**, after peer authentication (§4.2),
     as a declared request kind (§5.3). The agent is the credential's
     distribution point for user-plane consumers, because it is the only party
     that has authenticated to the machine plane at the OS level.
   - Existing local consumers that read the gate-exempt GET or the vault file
     directly — the dashboard, canvas, a running harness hook session, THESEUS,
     all named in `boot_node_root_grant.go`'s own header — acquire the
     credential from the agent instead. **This is a breaking change for those
     consumers and is called out as such**, sequenced at rung 3e (§7.1), where
     the agent is already being demoted to session duties. The per-consumer
     repointing is enumerated as net-new work in §3.3.
   - **The distribution mechanism, constrained rather than left implicit.** The
     third draft named the agent as "the distribution point" and stopped, which
     read as a third inbound surface with no transport, no access control, and
     no version. Constraint, decided: **the agent distributes over mechanisms
     it already owns as the seat(local) principal — the filesystem, in
     seat-owned paths, or the agent's existing local surface — and opens no new
     listener.** This is what keeps §6.4's auxiliary-IPC lint satisfiable while
     both planes are one binary (ADR-121): the credential moves *inside* the
     identity that is entitled to it, rather than across a new boundary that
     would then need its own gate. Note the interaction the reviewer caught:
     §6.4's "not readable from any seat(local)-readable path" lint is about the
     **machine plane's** copy under the machine root, not about the agent's
     delivery to a consumer running as the same operator — a distinction the
     lint text now makes explicitly. **Which of the seat-owned mechanisms is
     chosen is declared as part of the rung-3e work item (§7.1), not left
     implicit here**; what is decided here is the bound it must satisfy.
4. **The auxiliary-endpoint lint survives the retraction**, narrowed to what is
   actually checkable: no IPC endpoint in the kernel's namespace other than the
   declared door (§6.4).
5. **Config-mutation routes bind to the seat/workspace `kernel.yaml` only; the
   machine config file is not reachable over HTTP at all.** §5.1a splits the
   config file by plane, and the third draft stopped there — but the same keys
   are mutable over HTTP. `internal/engine/serve_config.go` registers
   `PATCH /v1/config` (RFC 7396 merge-patch, atomic, backed up) and
   `POST /v1/config/rollback`, both gated only by `EnableConfigMutation` —
   which §5.1a itself promotes to a machine key. Those handlers run **inside
   the machine-plane process**, so the machine root's DACL does not constrain
   them: the service would be rewriting its own posture on request from any
   local caller holding any live grant. Left undecided, the split would protect
   against a text editor and nothing else. **Decided:**
   - `PATCH /v1/config` and `POST /v1/config/rollback` have exactly one write
     target: the **seat/workspace `kernel.yaml`**. A patch naming any key the
     machine config file owns (§5.1a's list) is **refused**, not merged and not
     silently dropped.
   - `GET /v1/config` likewise reports the workspace file; the machine file's
     contents are not served on this listener in any form, `include_raw_yaml`
     included.
   - **Mutating the machine config is a machine-local operator-tier act**, not
     an HTTP call. It is delivered the way every other operator-tier change on
     this boundary is: as a **declared door kind** (§5.3) carrying operator-tier
     attestation (§5.6), or by an operator editing the machine file directly
     with the elevation the machine root's DACL requires. There is deliberately
     no remote path to it, because a remotely reachable write to the machine
     config is a remotely reachable write to every gate the machine config
     turns on.
   - §6.4 lints it as a route property: **no HTTP route on either listener
     writes the machine config file.** §3.3 records the write-target change as
     net-new.
6. **Skill execution leaves the machine plane entirely.** Under two-plane the
   HTTP listener is machine-plane, and `POST /v1/skills/{name}/exec`
   (`internal/engine/serve_skills.go`) is a code-execution route on it:
   `skillDirs()` resolves `os.UserHomeDir()/.claude/skills` and
   `<WorkspaceRoot>/.claude/skills`, and `pkg/skills`' `Exec` runs
   `exec.CommandContext` on the resolved binary with `os.Environ()` inherited.
   That is a direct seat(local) → service-account code-execution path from a
   **seat-writable, workspace-portable** directory — the identical escalation
   §5.8 already names for `ServiceDef.Command`, one level less indirect.
   §5.1a's promotion of `EnableSkillExec` to a machine key settles who may turn
   it **on** and says nothing about who **executes** or from where.
   **Decided, and as a general rule rather than a route-by-route patch:**
   - Skill execution is **user-plane work**: user binaries, from seat-writable
     directories, needing the operator's environment. It belongs to the agent
     by §4.4's RPC inversion, not to the service.
   - `POST /v1/skills/{name}/exec` is **removed from the machine plane's route
     set** and re-lands as a **declared door request kind** (§5.3) that the
     service raises and the **agent executes in its own session**. `GET
     /v1/skills` (discovery, no execution) may stay.
   - The general rule is stated as an invariant in §5.8: **the machine plane
     executes nothing resolved from a seat(local)-writable or
     workspace-portable directory.** `POST /v1/reconcile/{type}/resume`
     (`EnableReconcileControl`) is in the same unscoped bucket and is covered by
     the same rule — it controls machine-plane reconcilers, so it stays on the
     machine plane, but it is named here so it is not read as an oversight.
   - §6.4 lints it; §3.3 records the door kind and the agent-side executor as
     net-new.

All six items above are lintable and all six appear in §6.4. The second
draft carried a lint for item 4 only, which was the weakest of them.

Both surfaces are *declared*: each appears in the node's doctor surface by
name, with its access control and protocol version observable.

### 4.2 Door construction and mutual authentication

The door authenticates **both** ends. The first draft authenticated one, which
under RPC inversion (§4.4) is the wrong one to leave open: the direction that
carries the operator's credentials is service→agent, and an agent that does not
verify who it is talking to will execute a stranger's requests in the
operator's identity, attributed as the operator's own honest work. Combined
with pipe-name squatting — Windows permits additional instances of a named pipe
absent `FILE_FLAG_FIRST_PIPE_INSTANCE` — a squatted door is a *grant of the
operator's identity*.

Invariants:

- **Server side creates first-instance or refuses to run.**
  `FILE_FLAG_FIRST_PIPE_INSTANCE` on Windows; on unix, a socket in a directory
  no seat(local) principal can write. If the name is taken, the service exits
  with a declared error rather than joining an existing namespace.
- **Client side verifies the server before sending or accepting anything.**
  `GetNamedPipeServerProcessId` plus a token/SID check on that PID on Windows;
  `LOCAL_PEERCRED` / `SO_PEERCRED` on macOS and Linux.
- **Server side authenticates the peer at connect** and records the
  seat(local) principal for the life of the connection.
- The ACL grants a *SID*, not a *binary*. "Only the agent holds the door" is
  not expressible in an ACL and therefore not lintable; the threat model must
  not assume that unobservable. What is lintable is the first-instance property
  and the ACL's principal set (§6.4).

**The restart window, named as a DoS.** Fail-closed is the right polarity, but
it converts name-squatting from an identity attack into an **availability
attack**: an unprivileged local process that grabs the name during an SCM
restart gap makes the service refuse to start. That lands on the silent-death
mode §3.1 already warns about — exhausted failure actions mean SCM stops
acting. Decided mitigation, rather than accepting the trade:

- **The namespace is reserved at registration, not claimed at start.** On
  Windows the installer creates the door's private object-namespace directory
  with a DACL granting create rights to the service account only, and the pipe
  name is created inside it; on unix the socket's parent directory is created
  at registration, owned by the machine-plane identity, mode `0755` with no
  write for any seat(local) principal. A seat principal cannot claim the name
  because it cannot create objects in the container that holds the name. This
  is the same reservation shape as §5.8's break-inheritance-at-the-root rule,
  applied to the IPC namespace instead of the filesystem.
- **The refusal is a distinct observable, not generic down-ness.** §6.4 gains a
  lint that separates "refused: door name held by a foreign principal" from
  "service down," and the refusal names the holding principal where the OS
  exposes it. A DoS that is indistinguishable from a crash is a DoS that gets
  diagnosed as a crash.

### 4.3 Versioned protocol, and a floor distinct from the skew window

Self-update now updates two cooperating processes, so the wire format carries
an explicit protocol version. Two separate numbers, not one:

- **Skew window** — the service tolerates an agent one version behind. Updates
  restart the service first; agents reconnect and are told to restart when
  convenient. This covers ordinary rolling-update lag.
- **Protocol floor** — a minimum supported version below which the door refuses
  outright. **Any change to an authentication or authorization rule raises the
  floor rather than widening the skew window.** Otherwise the door is
  downgradeable, which is exactly the polarity #551 chose against when it made
  the zero value of its grant-auth flag the *refusing* one.

Two further rules:

- **Version negotiation happens after peer authentication**, never before. An
  unauthenticated peer cannot influence which rules apply to it.
- **"Degraded" means refuse-all-requests-but-stay-observable**, not
  serve-under-old-rules. A degraded door still answers the doctor; it does not
  answer work.

The restart-service-first rule is scoped to **connection and version
handling**. In-flight request continuity is governed by §9 Q3's answer
(ledger-backed queue), not by this section.

### 4.4 RPC inversion: honest identity and attributable action

The load-bearing invariant, restated as a ban on an *act* rather than on a set
of APIs:

> **The service never performs work under a borrowed token. User-plane work is
> always executed by the agent, in its own session, under its own identity.**

The first draft wrote this as "`ImpersonateNamedPipeClient` and its cousins are
rejected by design," which forbids the ordinary non-elevated primitive that
§4.5's own connect-time identity check requires. The distinction that carries
the weight is **act versus read**. Permitted: a scoped identity *read* —
`ImpersonateNamedPipeClient` → `GetTokenInformation` → immediate
`RevertToSelf`, or `GetNamedPipeClientProcessId`, or ALPC. Forbidden: doing
any work while holding that token.

**Why impersonation would not work here even if it were permitted.** The
degradation is not an inherent property of the API; it is caused by the
**session-0 / session-N boundary** the design already relies on. An
impersonation token obtained across that boundary is a partial logon session:
DPAPI master-key access and network authentication both degrade, which is the
same family of half-identity that produced the S4U incident. This is a
*stronger* argument for inversion than the first draft's, because it means **no
impersonation API would work across this boundary**, not merely that we
declined to use one.

**What inversion buys, precisely.** Attributability, not confinement. The
service enqueues a request; the agent picks it up, executes in its own session,
and returns a digest. Every user-plane action is performed by the identity that
owns it and is recorded as such. But whatever impersonation would have taken,
*asking still gets* — inversion bounds nothing by itself. Confinement lives in
the request-kind envelope (§4.6 and §5.3), which is where the RFC now places
it.

**Queue behavior is the OwnerActuator ladder, not "enqueue → execute →
digest."** The second draft named the inheritance in §5.9 and then specified a
two-step loop in the design body, which is the half of the ladder that cannot
detect its own failure. The external-credential-federation RFC (draft)
specifies **re-resolve → actuate → re-observe → alarm**, with single-flight and
nested-stale semantics. Wired into this boundary, per request kind:

| Ladder step | At the process boundary |
|---|---|
| **re-resolve** | before dispatch, the service re-reads the request's liveness preconditions — the addressed seat(local) principal is still connected, the nonce is unspent, the expiry is unpassed, the library version still admits the kind. A request that no longer resolves is not sent; it is recorded refused with a reason. |
| **actuate** | the door delivers the request; the agent executes it in its own session. Single-flight per `(kind, seat(local) principal, nonce)`: a request already in flight is never re-actuated on a retry, it is joined. |
| **re-observe** | the service does not treat the returned digest as the terminal state. It observes the **complement on the seat(local) strand** (§5.4) — the receipt the agent wrote in its own identity. The digest is the transport's answer; the paired entry is the record. A digest with no complement is not a completed request. |
| **alarm** | the unpaired-past-window observable (§5.4, §5.5) *is* the alarm rung, classified overdue vs. unexpressed, surfaced by the doctor rather than retried silently. Nested-stale: an alarm on an outer request is not cleared by an inner request settling. |

This is why §5.4's pairing is load-bearing on the request path and not only on
the audit path: re-observe has nothing to read without it.

### 4.5 The door is a privilege boundary: two obligations

Every agent→service message is a request from a lesser identity to a
machine-plane process. #551's precedent applies, and the first draft imported
only half of it. Restated as two obligations:

1. **Authenticate the peer at connect** (§4.2). Necessary and insufficient: a
   connect-time SID check is weaker than the `VerifyAny` posture #551 itself
   found inadequate, because *every* process running as the operator carries
   that SID.
2. **Authorize each request against a declared per-request-kind envelope.** The
   door admits a closed, enumerated set of request kinds; each kind declares
   the surface it requires. This wires into the existing ledger-backed
   identity-grant machinery (`internal/engine/serve_identity_grants.go`) rather
   than a parallel bespoke SID model, so the door participates in the same
   authority the HTTP surface already uses.

Handlers validate as if internet-facing: schema-validated payloads, no path or
registry values accepted verbatim.

### 4.6 The agent is a boundary too

New in this draft. Under RPC inversion the service→agent direction is the one
carrying the operator's credentials, so the agent must be a boundary in its own
right, not a compliant executor:

- **Closed request-kind allowlist.** The agent honors only kinds it declares.
  An unrecognized kind is refused and recorded, never best-effort interpreted.
  Absence is the refusing state.
- **Schema validation on every service→agent message.** Same discipline as
  §4.5 in the opposite direction.
- **Origin verification, scoped honestly to what connect-time auth gives.** The
  second draft asked for "origin verification per request, not merely per
  connection (§4.2 gives the mechanism)" — but §4.2's mechanism
  (`GetNamedPipeServerProcessId` / `SO_PEERCRED`) is a **connect-time** check,
  and over a single authenticated connection the peer is fixed. The requirement
  as written was either vacuous or unsatisfiable. Restated as two separate,
  achievable obligations: **(a)** the peer identity is established once, at
  connect, and **bound to the connection for its life** — a connection whose
  peer cannot be re-established after any reconnect is refused, not resumed;
  **(b)** what varies per request is not the origin but the **authorization**,
  which is carried by the per-request seat(local) binding in the last bullet
  below.
  Per-request re-verification of the transport peer is not claimed, because the
  transport cannot supply it.
- **Per-seat(local) capability envelope** declaring which kinds are admissible
  for this seat(local) principal — **stored on the machine plane, under the
  machine root** (§5.1), never inside `.cog/` and never in the
  seat(local) principal's own config. Its schema is the §9 Q2 question; its
  **plane, authority, and enforcement point are decided here**:
  - **Storage:** machine-plane, node-local. It is not workspace-portable, and
    it is not BEP-replicated — an envelope that travels with the corpus is an
    envelope a peer node can rewrite.
  - **Mutation authority:** **operator tier** (§5.6) — the same tier as a
    NodeID change. Not cross-plane, because cross-plane would let the two
    parties on this node widen the envelope between themselves.
  - **Enforcement point:** the **service, at the door**, before dispatch. The
    agent additionally enforces it on receipt (defense in depth), but the
    authoritative check is on the machine side, because a boundary enforced
    only by the constrained party is not a boundary.
  - **A seat(local) principal must not hold write authority over its own
    envelope.** Stated plainly because it is the whole point: if the envelope
    were seat(local) config
    (user plane, workspace-portable), the principal it constrains would be the
    principal that edits it, and it would not be an envelope — it would be a
    preference. §6.4 lints this directly.
  - **Bootstrap: the first logon of a new seat(local) principal has no
    envelope, and default-deny is the correct behavior.** Stated because it is
    a first-run property an implementer would otherwise discover rather than
    read, and because it sits in visible tension with §6.1's "default tier — no
    prompts at all." Since absence is the refusing state (§5.3, §5.4 polarity)
    and envelope mutation is operator tier, a principal with no envelope can
    raise **nothing** across the door until an operator acts. The flow:
    1. The agent starts in the new logon session and authenticates to the door
       (§4.2). The connection succeeds; the principal is recorded.
    2. Every request kind the agent raises is refused for want of an envelope,
       and each refusal is recorded with the reason `no-envelope` rather than
       a generic denial, so the doctor can distinguish "not enrolled" from
       "denied by policy."
    3. **Enrolment — minting the principal's first envelope — is an
       operator-tier act**, carrying the same attestation as any other
       operator-tier change (§5.6). It is named in the ladder as part of rung
       3e (§7.1): a node reaching 3e with N logon principals needs N
       enrolments, and an un-enrolled principal is a **declared, observable
       state**, not a broken one.
    Note the scope this does *not* touch: §6.1's default tier is
    **agent-plane-only** supervision with no service and therefore no door, so
    a default-tier install has no envelope question to answer. The bootstrap
    gap exists only on nodes that took node mode.
- **Seat-addressed requests only.** Each queued request carries the seat(local)
  principal it was raised for, a nonce, and an expiry; the agent refuses any
  request not addressed to its own principal. Without this, a request enqueued
  for operator A can be executed under operator B's credentials after a Fast
  User Switch (§9 Q3).

The request-kind allowlist here and the protocol version in §4.3 are not two
artifacts. §5.3 unifies them.

---

## 5. State: two strands and a shared library

The review split on where machine-plane state lives — inside `.cog/` with ACLs
granted to the service account, or under an OS machine root no seat(local)
principal can write. Both positions were correct about different things, and
the disagreement was invisible in the first draft because §5's single-axis
table hid it.

**Operator ruling: there is no single ledger.** The premise both positions
shared — one record with one home — is what was wrong.

### 5.1 The partition is plane × scope

Two orthogonal axes, not one. *Plane* is who holds the write handle. *Scope* is
whether the file travels with the workspace.

| | **node-local** (does not travel) | **workspace-portable** (travels with `.cog/`) |
|---|---|---|
| **machine plane** (service-written) | node identity + **BEP cert dir**; peer table; BEP membership; machine ledger strand (§5.2); **machine config** (§5.1a); **per-seat(local) capability envelopes** (§4.6); kernel binary + self-update staging; machine-root service definitions | **not empty — enumerated, not incidental** (§5.1b): daemon lifecycle state; projection-reconciler outputs; the BEP agent-definitions sync dir (peer-sourced); BEP sync state. Plus, **today and not by design**, three ledger buckets (§5.1c) |
| **user plane** (seat(local)-written) | per-user OAuth / token stores; DPAPI / keychain material; mapped drives; notification state | seat(local) ledger strand (§5.2); cogdoc corpus / memory; seat(local) config; harness homes |

**The second draft declared the machine × portable cell "empty by
construction." That is retracted: it is false against shipped code and against
accepted decisions the draft never cited — ADR-065, ADR-095, and ADR-097 /
ADR-098, all accepted; ADR-094 is status `Draft` and is cited for purpose, not
authority** (§5.1b). What the ruling's structural content actually
is, restated so it survives contact with the codebase: **the two planes do not
share a *strand*.** They do share the workspace directory, in a small, named,
lintable set of places.

Machine-plane state that is not in that named set relocates to the machine root
(`C:\ProgramData\CogOS`, `/Library/Application Support/CogOS`,
`/var/lib/cogos`). Seat(local) state stays inside `.cog/`, where ADR-001
defines the workspace membrane by the presence of that directory, and where the
corpus stays git-settled and portable. `findWorkspaceRoot`'s assumptions are
untouched.

Per-artifact, with the ACL story each one carries:

| Artifact | Plane | Scope | Home | Write ACL | Mutation tier (§5.6) |
|---|---|---|---|---|---|
| BEP cert dir + node identity | machine | node-local | machine root | machine identity only | **operator** (NodeID change) |
| Machine ledger strand | machine | node-local | machine root | machine identity only | n/a (append-only) |
| Machine config (§5.1a) | machine | node-local | machine root | machine identity only | **operator** |
| Per-seat(local) capability envelope (§4.6) | machine | node-local | machine root | machine identity only | **operator** |
| Kernel binary + self-update staging | machine | node-local | machine root | machine identity only | n/a (§5.8, Sigstore-gated) |
| Machine-root service definitions | machine | node-local | machine root | machine identity only | **operator** |
| Daemon lifecycle state (§5.1b) | machine | portable | `.cog/run/daemon/` | machine identity write, seat(local) read | cross-plane |
| Lineage projections (§5.1b) | machine | portable | `.cog/mem/semantic/lineage/projections/` — **separable subtree** | machine identity write, seat(local) read | cross-plane |
| ADR-097 / ADR-098 projection targets (§5.1b) | machine | portable | `.cog/mem/**`, `.cog/skills/**` — **not separable from the authored corpus; see §9 Q7** | **undecided — collides with the authored-corpus row below** | cross-plane |
| BEP agent-definitions sync dir (§5.1b) | machine | portable | `.cog/bin/agents/definitions/` | machine identity write, seat(local) read — **content authored by remote peers; see §9 Q8** | cross-plane |
| BEP sync state (§5.1b) | machine | portable | `.cog/.state/bep/` | machine identity write, seat(local) read | cross-plane |
| seat(local) ledger strand | user | portable (often gitignored — see §5.2) | `.cog/ledger/` | seat(local) identity only — **target state; three machine-plane writers exist today, §5.1c** | n/a (append-only) |
| Cogdoc corpus / memory (authored) | user | portable | `.cog/mem/` | seat(local) identity write, machine **read grant** — **but see the ADR-097/098 row above: the paths overlap and this cell is what Q7 has to reconcile** | cross-plane |
| seat(local) config | user | portable | `.cog/config/` (§5.1a: seat(local) keys only) | seat(local) identity | cross-plane |

### 5.1a The kernel config file is split

The second draft placed `kernel.yaml` nowhere and thereby left it where it is,
which the re-review correctly called placing it on the wrong side of the
partition. Verified against code: `internal/engine/config.go` sets
`CogDir = WorkspaceRoot/.cog` and loads the kernel config from
`CogDir/config/kernel.yaml`. That file is seat(local)-written and git-settled,
and it sits inside the directory BEP syncs a subtree of — and it is where
`EnableServiceControl`, `EnableSkillExec`, `EnableConfigMutation`,
`EnableReconcileControl`, `BindAddr` and `WriteRouteGrantAuthDisabled` are set.

The consequence is not subtle. Under two-plane as the second draft wrote it,
**the seat(local) principal configures the machine plane's entire
security posture with a text editor** — no HTTP call, no grant, no door,
defeating every gate §4.1 negotiates. §5.8 caught precisely this shape for two
lesser inputs (#101's plugin manifest overlays, `runtime-services.yaml`) and
missed the file that turns the gates off.

**The replication half of that argument is corrected, and the correction
matters in both directions.** The third draft wrote "because `.cog/` is
BEP-replicated, a peer node can push that posture onto this one." That
overstates the shipped code: BEP as shipped replicates **exactly one subtree**,
`<root>/.cog/bin/agents/definitions/` (`internal/engine/bep_provider.go`, folder
id `cogos-agent-defs` in `internal/engine/bep_model.go`), so **today a peer
cannot push `kernel.yaml`**. Retracted as a present-tense claim. It does not
weaken the split — the seat-with-a-text-editor path is real regardless, and it
is the whole of the argument this section needs. It is restated as the reason
the *lint* is written the way it is: a machine key must not be resolvable from
a path that is seat(local)-writable **or in the BEP inclusion list**, so that
widening the inclusion list later cannot quietly re-open the door. §5.2 states
the inclusion-list invariant; §6.4 lints against the list rather than against a
mental model of whole-workspace replication.

**Decided: the file splits by plane, not by convenience.**

- **Machine config** — a new file under the machine root, owned by the service
  identity, carrying every key that configures the machine plane: the `Enable*`
  gates (`EnableServiceControl`, `EnableSkillExec`, `EnableConfigMutation`,
  `EnableReconcileControl`), `BindAddr`, **`port`**, `WriteRouteGrantAuthDisabled`,
  the door's ACL principal set, the protocol/library floor, and the SCM
  failure-action policy (§3.1). Rule of thumb for future keys: **if turning it
  the wrong way weakens a boundary the machine plane enforces, it is a machine
  key.**
  - **On `port` specifically, and the asymmetry with `BindAddr`.** `port`
    (`internal/engine/config.go`, default 6931) parameterizes the *same*
    machine-plane listener as `BindAddr` and sits adjacent to it in the same
    YAML section, so leaving it on the seat side would be a visible
    inconsistency against the rule of thumb above. It is nonetheless a
    **lower-severity** key and the asymmetry is worth naming rather than
    flattening: moving `BindAddr` off loopback exposes the listener to the
    network — a boundary weakening — whereas changing `port` is an
    **availability** change, and a seat(local) principal who moves the port
    breaks its own clients rather than widening the machine plane's exposure.
    It travels with `BindAddr` for coherence, not because the two carry the
    same risk. §6.4's machine-key lint covers both, and an implementer reading
    a `port`-only failure should read it as a hygiene finding, not a breach.
- **`kernel.yaml`** keeps only seat(local)- and workspace-scoped keys — model and
  provider selection, foveation and salience parameters, digest paths,
  intervals, workspace-local service *declarations* (as opposed to the
  machine-root service *definitions* the supervisor executes).
- **Precedence is not "machine wins ties."** A machine-plane security key
  appearing in `kernel.yaml` is **not** an override to be shadowed; it is a
  **lint failure and a boot warning**. Two cases motivate refusing rather than
  shadowing: a key an operator edited into the wrong file and believes is in
  effect, and — if the BEP inclusion list is ever widened past the one subtree
  it holds today (§5.2) — a key that arrived from a peer. Ignoring either
  silently is how it stays there.
- `WriteRouteGrantAuthDisabled` keeps its inverted polarity from #551 across
  the move: the zero value means **auth enforced**, so a caller that builds a
  config without the machine file gets the safe behavior, not the exposed one.

§6.4 lints two invariants here, not one:

1. **No machine-plane security key is readable from a seat(local)-writable path
   or from any path in the BEP inclusion list.** (The file half.)
2. **No HTTP route on either listener writes the machine config file.** (The
   route half — §4.1 item 5. Without it the first lint passes while the posture
   is rewritten by merge-patch from inside the process the DACL cannot
   constrain.)

§3.3 records both code seams as net-new migration work.

### 5.1b Declared machine-plane writes inside the workspace

Accepted decisions and shipped code put machine-plane writes inside `.cog/`. The
second draft's "empty by construction" contradicted all of them and cited none
of them; the third draft enumerated three and **missed two**, both of them BEP's
— including the one whose content is written by remote peers.

**The authoritative enumeration is generated, not written here.** Three drafts
in a row, each hand-authored table was found incomplete by the next review. That
is not a sequence of careless passes; it is the signature of an **enumeration
asymptote** — a hand-maintained list of write paths converges on completeness
without reaching it, because the thing it describes changes under it and nothing
forces the two back into agreement. This section therefore **defers**:

> **Authority.** The enumeration of filesystem write sites is
> `internal/writepathaudit`, landed by **myrgic/cogos#591**. It derives the
> inventory from the source tree on every run and diffs it against
> `internal/writepathaudit/testdata/inventory.golden.{json,md}`; the diff is a
> **CI gate** — `TestInventory_MatchesGolden` runs under this repository's
> standard `go test -race -count=1 ./...` job — so a new or removed write path
> fails the build until a human re-declares the golden with
> `go test ./internal/writepathaudit/ -run TestInventory_MatchesGolden -update`.
> Where this RFC and the golden disagree, **the golden is right and this RFC is
> stale.**

At the merge of #591 the inventory reports **237 write sites** — 97 under
`.cog/`, 2 under the user home, 13 elsewhere (positively resolved to a
non-`.cog/` anchor), **84 unanchored** and **41 dynamic**, plus **73 subprocess
writers declared out of scope for v1**. Those last three numbers are the point:
the tool reports every site it *cannot* fully resolve in its own bucket rather
than dropping it, so the margin of the enumeration is itself observable. This
RFC adopts that margin as its own (§6.4) instead of presenting a table that
looks complete.

The table below is retained as an **illustrative excerpt, not a closed list** —
it carries the *dispositions*, which are this RFC's contribution and are not
derivable from a scanner. The excerpt covers the rows whose plane assignment is
contested or whose disposition is load-bearing elsewhere in this document. It is
**not** the set of machine-plane-adjacent writes inside `.cog/`; the golden
names others this excerpt does not disposition — among them
`.cog/blobs/manifest.jsonl`, `.cog/run/bus/*.cursors.jsonl`,
`.cog/state/conversations`, `.cog/observatory/quarantine/`,
`.cog/mem/episodic/experiments/`, and `.cog/docs/generated/` — each of which
needs a plane assignment before rung 3c and none of which this draft assigns:

| Path | Writer | Authority | Disposition |
|---|---|---|---|
| `<workspaceRoot>/.cog/run/daemon/state.yaml` | daemon lifecycle (`internal/engine/daemon_lifecycle.go`) | **ADR-065** §7, accepted — the runtime state file is specified there verbatim | **stays.** Node-local content in a portable directory: mode, endpoint, container name, workspace path, PID. Machine identity holds write; seat(local) holds read. Not git-settled — `.cog/run/` is runtime scratch, gitignored in observed workspace practice. **BEP disposition, corrected:** the third draft required this path be "excluded from BEP replication," which implied an exclusion mechanism that does not exist. As shipped the requirement is **vacuously satisfied** — BEP's inclusion list contains one subtree and `.cog/run/` is not in it (§5.2). The lintable object is therefore the **inclusion list**, not an ignore file, and §6.4 reads it that way. |
| `<workspaceRoot>/.cog/mem/semantic/lineage/projections/**` | projection reconcilers (`internal/engine/projection_reconciler.go`, `internal/engine/decision_lineage_reconciler.go`) | **ADR-095** (daemon reconcile-loop driver), accepted — the loop that runs the reconciler — plus shipped code. **ADR-094** (lineage observatory) is status **`Draft`** and is cited for the projection's *purpose*, not as settling authority | **stays.** The path **is separable**: `projection_reconciler.go` writes under `.cog/mem/semantic/lineage/projections/`, a subtree distinct from the authored corpus around it, so a directory ACL can be hung on it. Machine identity holds write on the `projections/` subtree only. Projections are derived, content-addressed outputs — regenerable, so a conflict is resolved by regeneration rather than by merge. |
| `<workspaceRoot>/.cog/mem/**` and `<workspaceRoot>/.cog/skills/**` projection targets | memory / skill projection reconcilers | **ADR-097**, **ADR-098**, both accepted — both specify a kernel-run reconciler writing into these trees | **PENDING Q7 — the only row in this table whose ACL story does not close.** The authority is not in doubt; the *path boundary* is. ADR-097 §3's placement table writes projections to `.cog/mem/semantic/insights/{slug}.cog.md`, `semantic/projects/`, `semantic/references/`, `episodic/profile/` — **the same directories the authored corpus lives in**. There is no `projections/` subtree here to hang an ACL on, unlike the lineage row above. So §5.1's two rows (authored corpus: seat(local) write; projection targets: machine write) are contradictory over one path, and §6.4's declared-set lint over this row would be a *provenance* predicate, not a *path* predicate — unenforceable by the ACL mechanism §5.2 insists on. **This RFC does not settle it**; see §9 Q7 for the two resolution paths. |
| `<workspaceRoot>/.cog/bin/agents/definitions/**` | the **BEP sync engine**, machine plane (`internal/engine/bep_provider.go` — watch dir bound to this path; folder id `cogos-agent-defs` in `internal/engine/bep_model.go`) | shipped code; the machine plane owns BEP membership per §3. **No ADR or RFC in the corpus declares this directory's trust properties** — that absence is the finding | **stays, and is flagged.** The content is **peer-sourced**: agent CRD files authored on *other nodes* and replicated here by the machine-plane engine. This is the only cell in the partition whose author is another machine, and the third draft's enumeration missed it entirely. It stays because cross-node agent distribution is the feature, but the trust question it raises is **escalated to §9 Q8** rather than dispositioned here: *what reads or executes these definitions, under which identity, and what admits a definition a remote peer wrote?* Cross-reference §5.8's execution invariant — whatever consumes these files, the **machine plane must not execute from this directory**, for the same reason it must not execute from `.claude/skills` (§4.1 item 6). |
| `<workspaceRoot>/.cog/.state/bep/**` | the BEP sync engine, machine plane. **Citation corrected against the generated inventory:** the state dir is *configured* at `internal/engine/bep_engine.go:200` (`filepath.Join(root, ".cog", ".state", "bep")`), but the **write primitive** is `PersistIndex` in `pkg/substrate/bep/index.go`, writing `index.json` via temp-file + rename. The inventory bins that site **`elsewhere`, as `{stateDir}`**, because the anchor is a parameter and does not resolve statically; `internal/writepathaudit/scan_test.go` records the pairing as a named gate | shipped code | **stays.** BEP index/sync bookkeeping: node-local, derived, machine-written, regenerable. Same shape as the daemon lifecycle row — node-local content in a portable directory — and it carries the same requirement: it is not in the BEP inclusion list, and it must not enter it. Seat(local) holds read. **Note the shape of this correction:** a hand-authored table named the *configuring* file and called it the writer; the generated inventory names the *primitive*. That is the difference the deferral above buys. |

The general rule these five share, **restated so that it no longer depends on
the completeness of a table in this document**: a machine-plane write inside
`.cog/` is admissible only where it is (a) **present in the generated
inventory's golden** — i.e. a human has approved its diff — and carries a
recorded disposition, (b) derived, runtime, or peer-sourced rather than locally
authored, and (c) outside both ledger strands. Anything else is a lint failure
(§6.4).

Clause (a) is the substantive change in this pass. Previously it read "named in
this table", which made the invariant only as good as the last hand sweep; it
now names a **generated** object with a CI gate behind it, so a write path that
nobody wrote down still fails the build. This is a narrower claim than "the cell
is empty" and it is the one the code and the corpus actually support — with
three honest residuals: row 3 is **pending Q7** because its paths are not
separable, row 4 is **flagged to Q8** because its content is authored off-node,
and the `.cog/` writers named above the table but not dispositioned in it are
**declared as Q9**. Rule (c) is where §5.1c comes in: shipped code violates it
today.

Note that this is the same conflict-log entry (ADR-065 §7 versus RFC-033's path
layering) that sits immediately above the machine-tier bullet §5.9 claims to
close. The second draft decided it silently. It is decided out loud here.

### 5.1c Machine-plane writers currently inside the seat(local) strand

Rule (c) above — *outside both ledger strands* — is the one §5.2's two-party
attestation actually rests on, and **shipped kernel code violates it today.**
The third draft asserted the opposite as a present-tense invariant and lints it
in §6.4, so the lint would fail on a clean checkout and the invariant reads as a
fact when it is a target. Corrected: the writers are enumerated, and the
invariant is restated as a **post-rung-3 property with named migration work**,
not as a description of the current build.

**Corrected against the generated inventory (#591).** The third draft asserted
that "all three writers reach `.cog/ledger/<bucket>/events.jsonl` through
`AppendEvent` (`internal/engine/ledger.go`)". Both halves of that sentence were
wrong, and the generated inventory is what showed it:

- **There is a fourth bucket.** `.cog/ledger/identity-grants/events.jsonl` is
  appended by the identity-grant registry —
  `internal/engine/serve_identity_grants.go` at `appendGrantEventLocked`,
  `appendExtendEventLocked`, and `appendSupersessionEventLocked` — which runs
  **inside the machine-plane HTTP listener** under §3's assignment. It is
  arguably the most consequential of the set, since it records grant issuance.
- **There is a second, independently-drifted `AppendEvent`.** `pkg/cogblock/ledger.go`
  declares its own package-level `AppendEvent` writing
  `<workspaceRoot>/.cog/ledger/<sessionID>/events.jsonl` — same directory, same
  file, different function, its own doc comment calling itself "the canonical
  write path for all events". The inventory's package doc names this
  duplication explicitly as the reason it scans **write primitives rather than
  function names**: a name-based sweep for `AppendEvent` cannot tell these
  apart, and a hand-authored table did not.

So the enumeration below is **four buckets reached through at least two distinct
append implementations**, and it is offered as the current reading of the
golden rather than as a closed set. All of them run under §3's assignment of the
reconcile loop and the HTTP listener to the machine plane:

| Bucket | Writer | Disposition |
|---|---|---|
| `.cog/ledger/worktree-reconciler/` | `FilesystemLedgerWriter` (`internal/engine/worktree_spawn.go`) emitting `worktree.*` events; registered at boot by `internal/providers/all/all.go` | **relocate to the machine strand.** It is a reconciler's own record of machine-plane work; it is on the seat strand only because `AppendEvent` is the only ledger writer that exists. |
| `.cog/ledger/<kernel session id>/` | `(*Process).EmitEvent` (`internal/engine/process.go`), called by the margin bridge's kernel event sink and by the MCP server | **split by origin.** Kernel-originated events relocate to the machine strand; events that record *user-plane* work the agent performed stay on the seat strand and are written **by the agent**, which is the identity that performed them (§4.4). This is the one row where the fix is not a path change but an authorship change. |
| `.cog/ledger/mcp-client/` | `EmitLedgerEvent` (`internal/engine/mcp_stubs.go:641`) | **relocate to the machine strand**, or be re-attributed to the calling seat where a seat is identifiable. Whichever, it does not stay a machine-plane write on the seat strand. |
| `.cog/ledger/identity-grants/` | the identity-grant registry (`internal/engine/serve_identity_grants.go:723/780/823`), running inside the machine-plane HTTP listener | **relocate to the machine strand.** Grant issuance, extension, and supersession are machine-plane acts recorded by the machine plane; this is the clearest case in the table, and it was missed by three hand sweeps. **Sequencing note:** it is also the row most likely to be load-bearing for §4.1 item 3, whose credential-distribution rework reads this same registry — the relocation and that rework should land together rather than in either order. |

Three consequences stated so nothing here is inferred:

- **§5.2's invariant is a target, and §6.4's `.cog/ledger/` lint is a
  post-rung-3 gate.** Both are marked as such. A lint that fails on today's
  build is only honest if it is declared as measuring the destination rather
  than the origin; that declaration is made here and repeated in §6.4.
- **The relocation is net-new work** and is listed in §3.3. It is a
  **prerequisite for rung 3c** (machine state migrated): migrating machine
  state while the machine plane is still appending to the seat strand would
  settle the ladder on a strand pair that is not yet isolated.
- **Until it lands, §5.4's "count of two" is not yet real on this node.** Said
  plainly rather than left as an implication of the table above. The pairing
  mechanism is sound; the ACL that makes a pair two-party is what rung 3
  installs.

**The cogdoc corpus (Q1 in the first draft) is answered here**: the *authored*
corpus is workspace-portable seat(local) state, and over it the machine plane
holds a **declared read grant** as an indexer, not ownership. That preserves the
`.cog/` mind/body framing without making the service a reader-by-default of
formerly user-private data, and it narrows the visibility-policy question
usefully: not "where does memory live" but "what does the service's read grant
admit, and is it declared." **The projection targets are the exception, and
they are ADR-097's and ADR-098's to define, not this RFC's** — see §5.1b. The
read-grant framing covers what the operator wrote; it does not cover what the
reconciler generated.

### 5.2 Two plane-native ledger strands

- **Machine strand** — under the machine root, **service-written only**. Node
  identity events, membership, reconcile, self-update, service lifecycle.
- **seat(local) strand** — inside `.cog/`, **seat(local)-written only** and
  workspace-portable. Session events, lane dispatch, corpus writes. *Portable
  is not the same as git-settled, and the second draft used the terms
  interchangeably:* observed workspace practice gitignores `.cog/ledger/*`
  (keeping only a `.gitkeep`), so the strand travels with the directory, not
  necessarily through git history. The invariants in this RFC rest on **who
  holds the write handle**, which is unaffected either way; but "git-settled"
  is a claim about durability that this RFC should not make on the ledger's
  behalf. Whether the seat(local) strand *ought* to be git-settled is a
  separate question and is not decided here.

  **"…and by BEP replication" is retracted.** The third draft said the strand
  travels with the directory *and by BEP replication*, and §5.8 simultaneously
  asserted that no machine-plane identity writes inside `.cog/ledger/`. Both
  cannot hold: the BEP engine is machine-plane by §3's own assignment, so
  anything BEP replicates into the workspace is written by the machine-plane
  identity. The contradiction is resolved by **invariant, not by wording**, and
  in the safe direction:

  > **The ledger strands are never in the BEP-replicated set.** Neither
  > `.cog/ledger/**` nor the machine strand is replicated by BEP, in any
  > configuration.

  The reason is sharper than "the machine plane would be writing the seat
  strand," which is bad enough. If the seat strand were replicated, **a remote
  peer could author a complement on this node's seat strand** — the second
  observation in §5.4's two-party pair, written by a party that is neither of
  the two. That is off-node complement forgery, and **no local privilege lint
  detects it**, because no local principal is involved: §3.1's privilege ceiling
  and §5.8's DACLs both govern principals on this box. Pairing would report
  settlement that no local identity attested.

  **What the invariant is enforced against, concretely.** BEP as shipped has
  **no ignore or exclusion mechanism** — `pkg/substrate/bep/` contains nothing
  of the kind — and replicates exactly one subtree,
  `<root>/.cog/bin/agents/definitions/` (`internal/engine/bep_provider.go`;
  folder id `cogos-agent-defs`, `internal/engine/bep_model.go`). So exclusion
  today is **by inclusion list**, and the invariant is **vacuously satisfied**.
  That is a fact about the present, not a guarantee about the future, so the
  **lintable object is the inclusion list itself**: §6.4 reads the declared BEP
  folder set and fails if it contains, or is a prefix of, `.cog/ledger/**` or
  the machine strand. Ignore-support becomes net-new work (§3.3) **only if the
  inclusion list is ever widened** to a scope that cannot exclude the strands by
  construction; this RFC does not propose widening it.

  **How the seat strand travels, then.** By **git** where the operator settles
  it, or by an **explicit move** of the workspace directory — the transplant
  case §9 Q6 is about. Not by BEP. Q6's question is unchanged by this; what
  changes is that the transplant is an operator act with an observable moment,
  which is what makes Q6's "seal at transplant" candidate answer coherent at
  all.

  The existing session-ledger layout
  (`<workspaceRoot>/.cog/ledger/<sessionID>/events.jsonl`,
  `internal/engine/ledger.go`) **realizes** this strand; it does not move.
  Precisely: the strand is the **set of session-keyed ledgers under
  `.cog/ledger/`**, and per-seat identity is an aggregation over sessions
  rather than a storage key (§1.1). The ACL is held on the directory, which is
  the level the invariant needs; nothing here requires re-keying the ledger,
  and nothing here is disturbed by a session that carries N seats. Seats
  attached from off-node (the ADR-102 sense) hold **no local ACL write on this
  strand and are not meant to** — they reach it through the door, mediated by
  the local plane, which is the mechanism §1.1 describes and not a
  counterexample to it.

**Write isolation, stated precisely.** The second draft wrote "neither identity
can write the other's strand" and then let the claim drift into "machine-plane
writes never land inside `.cog/`," which is false (§5.1b). The invariant this
RFC asserts, and the only one it needs:

> **The isolation invariant protects the two ledger strands.** No
> seat(local) principal holds write, create, or delete on the machine strand.
> No machine-plane identity holds write, create, or delete on the seat(local)
> strand — `.cog/ledger/**`. Neither strand is in the BEP-replicated set. This
> says nothing about the rest of `.cog/`, which is governed by the per-artifact
> table in §5.1 and the named exceptions in §5.1b.

**This invariant states the destination, not the present build.** Said here
rather than left for a reader to discover: **four** shipped kernel ledger
buckets are appended from the machine plane today, through at least two distinct
append implementations (§5.1c, corrected against the generated inventory —
the count was three until #591's inventory surfaced the `identity-grants`
bucket). The invariant is what rung 3 installs; the migration that installs it
is named in §3.3 and gated at rung 3c. Everything below is written accordingly.

**And the count is not asserted as final.** It is the current reading of
`internal/writepathaudit`'s golden, which is regenerated and diffed in CI. If a
fifth bucket appears the gate fails and the golden's diff names it; that is the
property this RFC is now relying on instead of on its own thoroughness.

Three properties make that invariant real rather than declared:

1. **It is enforced by ACL, not convention**, and it is enforced **in both
   directions**. §5.8 states one direction (no seat(local)-writable path under
   the machine root). The direction §5.4's attestation actually depends on is
   the other one — **no machine-plane-writable path inside `.cog/ledger/`** —
   and the second draft lints neither it nor its ceiling. §6.4 now carries both,
   the second marked as a post-rung-3 gate.
2. **The privilege ceiling from §3.1 applies here or the ACL is decorative.** An
   ACL that says "the service cannot write the seat(local) strand" means
   nothing if the service runs as root, as SYSTEM, or with `SeBackupPrivilege`
   / `SeRestorePrivilege` / `SeTakeOwnershipPrivilege` / `CAP_DAC_OVERRIDE`.
   The machine-plane identity must hold **no privilege that overrides the
   seat(local) strand's ACL**. That is why §3.1 strikes root on macOS, and it
   is the reason the Windows virtual account was chosen for being *strictly
   weaker than SYSTEM* rather than for being convenient.
3. **The ACL has to be paired with the replication invariant, because ACLs are
   local and replication is not.** A DACL denying the machine identity write on
   `.cog/ledger/` is exactly as strong as the set of writers it can see. A file
   arriving over BEP is written by the machine-plane engine and authored
   somewhere else entirely; the local ACL is the wrong instrument for it. That
   is why the strands' exclusion from the replicated set is stated as an
   invariant above rather than left to configuration.

That is what makes the next section mean anything.

### 5.3 The paired-kind library

The two strands are joined by a **shared, versioned, read-only library of entry
kinds** — a codebook shipped with the binary, readable by both planes, writable
by neither at runtime.

Kinds that cross the plane boundary are declared in **polar pairs with forced
relational positions**: `request` / `receipt`, `grant` / `exercise`, `offer` /
`claim`. Booleans with forced relational positions rather than free-standing
flags.

Each cross-boundary kind declares four things:

| Field | Meaning |
|---|---|
| **complement** | the kind that settles it, on the other strand |
| **direction** | which plane writes which half |
| **window** | how long the complement may take, in counterpart ticks (§5.5) |
| **minimum otherness tier** | how independent the second observation must be (§5.6) |

**This library subsumes two artifacts §4 introduced separately**: the door's
closed request-kind allowlist (§4.6) and the protocol version (§4.3) are the
same object. A request kind is admissible **only if** it is in the library at a
version both ends satisfy. The protocol floor is a library version floor.

The second draft wrote "iff" there, which overstates: library membership is
**necessary and not sufficient**. Admission also requires that the kind be in
the addressed seat(local) principal's capability envelope (§4.6). The library
says what the boundary *can ever* carry; the envelope says what *this
principal* may raise. Two gates, both closed by default, and the unification
claim is over the vocabulary — not over the authorization.

### 5.4 Settlement, and what a doctor can see

**A cross-plane entry is settled iff its complement exists on the other
strand.** That is the whole definition. Because neither identity can write the
other's strand, a settled pair is **two-party attestation** by construction —
one identity cannot manufacture both halves.

Absence is the refusing state, per the #551 polarity lesson: an entry with no
complement is *not settled*, and a kind absent from the library is *not
admissible*. Neither condition needs a flag to be true.

**An unpaired entry past its declared window is a first-class doctor
observable.** But the doctor must distinguish two cases that look identical in
a naive scan:

- **Overdue** — the counterpart strand *has ticked* (advanced entries, sent
  heartbeats) without producing the complement. This is real drift and warrants
  repair.
- **Unexpressed (vacancy)** — the counterpart strand has *not ticked at all*.
  No seat(local) principal was logged on; the agent was down; the machine was
  off. The entry is recorded and observed on its own strand, and will pair in
  order when the counterpart returns. **This is not drift and must not be
  reported as such.** A machine that ran all night with no operator logged in
  should produce a clean doctor run and a backlog that expresses at next logon,
  not a wall of unsettled-entry warnings.

**The classifier's blind spot, named.** As specified so far, vacancy is inferred
from the counterpart's own sequence not advancing — which means **the classifier
is decided by data the suspect writes.** A counterpart that is up but never
ticks (hung, wedged, silently refusing, or compromised into quiescence) parks
its unsettled entries in the silent bucket indefinitely, and §5.4's own rule
that unexpressed "must not be reported as drift" is what keeps them there. The
strand's silence is not evidence of absence; it is the absence of evidence.

**Therefore vacancy is never established by the strand alone.** The doctor's
vacancy classification must be **paired with an independent occupancy signal**
— the observable §6.4 already carries and never connected to the classifier:
*agent task/unit registered and healthy*, plus the door's connection state.
The truth table the doctor lints:

| Counterpart ticked? | Occupancy signal | Classification |
|---|---|---|
| yes | present | **overdue** — real drift, repair |
| no | **absent** (not registered / not connected / machine was off) | **unexpressed** — silent, expresses on return |
| no | **present** (registered, healthy, connected) | **stalled** — a third state, reported, *not* silenced |

The third row is the one the second draft folded into the second and thereby
made invisible. A counterpart that is demonstrably present and demonstrably not
advancing is the most interesting state on the boundary, and it was the one
guaranteed to be silent.

Pairing *detects*; ACLs *prevent*; the occupancy signal *bounds the silence*.
The write-isolation work in §5.2 is not optional scaffolding for §5.3 — it is
what makes the count of two real.

### 5.5 Causal windows: counterpart ticks, never wall clock

The two strands do not share a clock, and this is not a limitation to be
engineered around. Wall-clock agreement between a service account and a logon
session on the same box is a *masked observable*: the number is always
available and rarely means what a settlement check needs it to mean (suspend,
clock skew, hibernation, VM pause, timezone/DST edges).

**Settlement windows are denominated in the counterpart's observable ticks** —
entries the counterpart advanced, heartbeats it sent — never in wall-clock
duration. Concretely, with exactly two writers a vector clock degenerates to
two integers: each entry stamps `(own-seq, last-seen-counterpart-seq)`. A
window of "8 counterpart ticks" is a statement about causal distance, and it is
correct across a suspend, a reboot, and an overnight vacancy without any
special-casing. The overdue/unexpressed distinction in §5.4 falls directly out
of it: overdue means the counterpart sequence advanced past the window;
unexpressed means it did not advance at all.

Timestamps are still *recorded* — for humans, for correlation, for the incident
record. They are not *load-bearing* for settlement.

### 5.6 Otherness tiers

Not every pairing buys the same amount of assurance. A pairing's strength is
the independence of the second observer: a self-acknowledgment adds nothing
(it is a longer first observation), cross-plane adds a real second identity on
the same node, a constellation peer adds a second machine, an operator
confirmation adds a human.

The library **quantizes this at declaration**: each paired kind declares a
**minimum otherness tier**.

| Tier | Second observer | Typical kinds |
|---|---|---|
| cross-plane | the other plane on this node | routine request/receipt, lane dispatch digests, grant exercise |
| cross-node | a constellation peer | node membership changes, seals |
| operator | a human confirmation | **NodeID change**, machine-root DACL change, service re-registration under a new identity |

Identity-grade events require thicker tiers than routine receipts. A NodeID
change settled only by the same node's other plane is not settled enough — that
is precisely the failure §7 rung 3 is designed to make impossible.

Settlement stays **binary per declaration**; the doctor lints the tier. The
gradient is continuous underneath, but the decision is made once, at minting,
and sealed in the library.

### 5.7 Minting a kind is a governance act

Every change to the library is a **version bump that raises the library floor**,
and none of them is a code change that ships quietly with a refactor. The
library is the boundary's vocabulary, and minting vocabulary is a security act:
everything the door will ever admit is enumerated there.

**The third draft's opening sentence here filed "changing a complement, changing
a direction" in the routine bucket. That is retracted** — it is the same
misfiling the gate below exists to prevent, sitting in the sentence that
introduces the gate. Changing a complement or a direction is not a neutral edit
of an existing kind; it is where the kind's *pairing* lives, and pairing is the
whole primitive.

**But the library governs the tiers, so the library must govern itself.** The
second draft named minting "a security act" and then attached no authority to
it — which left the tier system strictly weaker than the thing that edits it. A
library revision needing no operator tier could lower an identity-grade kind
from **operator** to **cross-plane**, and §5.6's protection of a NodeID change
would evaporate by version bump. Named gate, decided:

> **Any library change that WEAKENS a constraint requires operator-tier
> attestation, recorded as a paired entry on the machine strand.**
>
> "Weakens" is defined **non-enumeratively**, because the third draft's closed
> list was incomplete on its second reading and a closed list here will be
> incomplete on its third:
>
> > **Any non-additive change to an existing kind requires the operator-tier
> > gate.** Non-additive means touching what the kind already declares — its
> > tier, its window, its complement, its direction, or its field requirements
> > — whether by **changing** the declared value or by **removing** it, and
> > whether or not the change looks like a tightening. Removing a kind is a
> > non-additive change to that kind.
> >
> > **Purely additive changes remain ordinary version bumps**: adding a new
> > kind, adding a new *optional* field to an existing kind.
>
> The rule is deliberately over-inclusive. A tightening that trips the gate
> costs one operator attestation; a weakening that slips it costs the
> primitive.

**Why "changing" had to join "removing," concretely.** The worked case is not
hypothetical and it is why the enumeration failed: re-point an operator-tier
kind's **complement** from a seat(local)-strand kind to a machine-strand kind,
and both halves of the pair now land on **one strand** — which voids §5.4's
"one identity cannot manufacture both halves" for that kind. Under a closed list
that only caught *removals*, that edit reads as a floor-raising bump requiring
no operator tier. Flipping `direction` does the same thing by the other route.
The pairing fields are where the two-party property is encoded, so any edit to
them is an edit to the two-party property.

**The preventive half of this gate is the signature gate.** Worth composing
explicitly rather than leaving in two sections: the gate above is **detective**
— a weakening that shipped is observable after the fact on a strand the
beneficiary cannot write. Since §5.3 ships the library **in the binary**, a
library revision arrives by self-update, so the only *preventive* control on the
path is the Sigstore gate §5.8 declares as an invariant
(`internal/engine/cli_selfupdate.go`; `AllowUnsigned` bounded by pre-signing
tags and never set by the reconcile provider). Detection on the strand,
prevention at the update — one mechanism, two halves.

Three properties this buys, none of which the intent-statement alone did:

- **It is self-applying.** The weakening gate is itself an operator-tier
  constraint, so lowering *it* is a weakening and requires operator tier.
- **It leaves a record on the right strand.** The attestation is a paired entry
  on the **machine strand** — the strand the seat(local) principal cannot write
  — so a weakening that shipped is observable after the fact, by the doctor,
  without trusting the party that benefited from it.
- **It has a direction, not just a threshold.** "Raises the floor" and "requires
  operator tier" are different axes; the second draft used the first to describe
  changes that move in the second's direction, which is how "lowering an
  otherness tier" ended up in the routine bucket.

Practical consequence: an agent and a service that disagree about the library
version disagree about what is admissible, which is why §4.3's floor is a
library floor and why negotiation happens after authentication.

### 5.8 The machine root is specified by DACL, not by ownership

"Owned by the service account" is not a security property. `C:\ProgramData`
carries permissive inheritable ACEs by default, and §5.1 places the kernel
binary and self-update staging under the machine root — so a seat(local)-writable
machine root plus a self-updating service is a direct user → service-account
code-execution path.

Invariants:

- **No seat(local) principal holds write, create, or delete on any path under
  the machine root.**
- **No machine-plane identity holds write, create, or delete inside
  `.cog/ledger/`** — the other direction, which §5.2 explains is the one
  two-party attestation actually rests on.
- **The machine-plane identity holds no privilege that overrides either ACL**
  (§3.1's privilege ceiling). An ACL is only as strong as the weakest privilege
  that bypasses it.
- **The machine plane executes nothing resolved from a seat(local)-writable or
  workspace-portable directory.** Stated as a general invariant rather than as a
  route-by-route patch, because the third draft caught one instance of the shape
  (`ServiceDef.Command`, below) and missed a more direct one. Binaries,
  scripts, skill entry points, plugin executables, service commands: if the
  machine plane runs it, it resolves from **under the machine root**, whose DACL
  §5.8 specifies. Anything the operator can edit is executed by the **agent**,
  in the operator's identity, per §4.4's inversion — which is where such work
  belonged anyway, since it is user-plane work.
  - The instance the third draft missed: `POST /v1/skills/{name}/exec`
    (`internal/engine/serve_skills.go`) resolves through `skillDirs()` —
    `os.UserHomeDir()/.claude/skills` and `<WorkspaceRoot>/.claude/skills` —
    and `pkg/skills`' `Exec` runs `exec.CommandContext` on the resolved binary
    with `os.Environ()` inherited. Under two-plane that is a seat(local) →
    service-account code-execution path through a **workspace-portable**
    directory, which means a peer node or a transplanted workspace supplies the
    binary. §4.1 item 6 moves it to the agent.
  - The instance the third draft caught, restated as the same rule:
    `ServiceDef.Command` is a free-form command string, and the plugin-manifest
    and `runtime-services.yaml` inputs below feed it.
  - The instance §5.1b flags: the BEP agent-definitions sync directory holds
    **peer-authored** content inside the workspace. Whatever consumes it, the
    machine plane does not execute from it. See §9 Q8.
- **Inheritance is broken at the root** and a fresh DACL is applied at
  registration, not inherited.
- The service binary, the self-update staging directory, **the machine config
  file (§5.1a), and the per-seat(local) capability envelopes (§4.6)** are
  explicitly covered. The last two are the newest and the most attractive: one
  turns the gates off, the other decides what may be asked.
- Service image path integrity is checked (write-ACL on the image, and the
  unquoted-service-path class of defect).

**Self-update integrity already holds, and is declared here rather than left to
luck.** §3 moves self-update to the machine plane, which makes the update path
a machine-plane code-execution path by construction. The protecting invariant
exists in shipped code: `internal/engine/cli_selfupdate.go` applies a Sigstore
signature gate, and `AllowUnsigned` relaxes it **only** for release tags
predating the first signed release — never for an invalid signature, and the
reconcile provider never sets it. Declared as an invariant of this RFC:

> The machine plane applies no update that fails signature verification. The
> pre-signing relaxation is bounded by tag and is not reachable from the
> reconcile path.

It is cited here because an undeclared good property is one refactor away from
being an accident.

Two state categories the first draft's table omitted, both of which are
*inputs* to a machine-plane supervisor and both of which live under the user
profile today:

| State | Disposition |
|---|---|
| #101 plugin manifest overlays (`~/.cogos/plugins/*/manifest.yaml`) | move to the machine root, **or** be declared inputs the machine plane refuses to read |
| `~/.cogos/runtime-services.yaml` | same |

`ServiceDef.Command` is a free-form command string. A machine-plane supervisor
reading its service definitions from user-writable files is the same
escalation as a seat(local)-writable binary, one level of indirection away.

### 5.9 What this settles in the corpus

- **It resolves the council's first disagreement** (ledger inside `.cog/` with
  ACLs *versus* ledger under an OS machine root) by rejecting the shared
  premise. Both invariants hold simultaneously because there are two records:
  the security invariant (§5.8) holds over the machine strand; ADR-001's
  membrane holds over the seat(local) strand.
- **It provides the argument to close the machine-tier path question ADR-099
  records as unresolved — it does not close it by fiat.** ADR-099's conflict log
  carries "Which machine tier owns node state — `~/.cog/` or RFC-033's
  `~/.cogos/`? Unsettled," and notes that when RFC-033 settles it, the change is
  one line in `defaultNodeIdentityDir` with `COG_NODE_DIR` as the migration
  seam. Three corrections to how the second draft claimed this:
  - **Standing.** ADR-099 names **RFC-033** as the settling authority, and
    RFC-033 is status `draft` — as is this RFC. A draft does not settle another
    draft's question. Accurately: **this RFC proposes the answer; the ADR-099
    amendment lands on acceptance.**
  - **It supersedes rather than settles.** The answer offered is a **third
    option** — RFC-033's `.cogos/` node-runtime namespace **relocated to the OS
    machine root** — not one of the two the ADR poses. It preserves RFC-033's
    `.cog/`-is-cognitive-substrate vs. `.cogos/`-is-node-runtime axis rather
    than adding a fourth path beside it, which is why it is worth proposing;
    but the honest verb is *supersede*.
  - **It is not the one-line change the ADR anticipated.** It additionally
    requires the cert-dir seam that does not exist (§3.3, §7.2) and an explicit
    disposition for ADR-065's `.cog/run/daemon/state.yaml` (§5.1b). Scope note
    recorded so an implementer is not surprised by it.
- **It decides, out loud, the conflict-log entry adjacent to that one.**
  ADR-065 §7 specifies `.cog/run/daemon/state.yaml` and RFC-033 layers the
  paths differently; §5.1b keeps the ADR-065 path and declares it as a named
  machine-plane write inside the workspace, together with the lineage
  projections and the ADR-097 / ADR-098 projection targets driven by ADR-095's
  reconcile loop. The second draft asserted a partition that contradicted all of
  them and cited none of them. **Standing note, applying §5.9's own rule to
  itself:** the lineage row leans on **ADR-095** (accepted) plus shipped code
  for its authority, not on **ADR-094**, which is status `Draft`. A draft does
  not settle another draft's question — the correction this section makes for
  ADR-099 applies one bullet away, and the third draft cited ADR-094 as though
  it were accepted.
- **It does not settle the projection-target path collision, and says so.**
  ADR-097 and ADR-098 write into the same directories as the authored corpus,
  which makes §5.1's two rows contradictory over one path and §6.4's declared-set
  lint unenforceable by ACL over that row. That is a genuine conflict between
  accepted decisions and this RFC's partition, and resolving it requires
  amending an accepted ADR or changing the write mechanism — neither of which
  this draft has standing to do unilaterally. Declared as **§9 Q7** with two
  resolution paths.
- **§4.4's request/receipt shape is the OwnerActuator posture at the process
  boundary — and §4.4 now implements the ladder rather than naming it.** The
  external-credential-federation RFC (status `draft`) specifies re-resolve →
  actuate → re-observe → alarm, with single-flight and nested-stale semantics,
  validated end-to-end **in its harness integration**, which is engineering
  validation and not ratified corpus. §4.4's table binds each rung to a
  concrete behavior at this boundary; the re-observe rung is what makes §5.4's
  pairing part of the request path.

---

## 6. Install-time story and doctor surface

### 6.1 Tiers

- **Default tier — no prompts at all.** Agent-plane registration only, in user
  context. No elevation, no password, package-manager friendly. Node is up
  while the operator is logged in. This is today's behavior, now named.
- **Node mode — one elevation prompt, zero credentials.** Opt-in for
  constellation members that must be reachable pre-logon. Elevation registers
  the service under the virtual account (Windows) or its platform equivalent.
  Nothing is typed, stored, purged, or rotated. **A password dialog exits the
  install flow permanently.**
- **Hard rule: no S4U, ever.** Encoded in the schtasks driver as a refusal and
  in the doctor as a lint (§6.4).

### 6.2 Windows registration and packaging

Node mode requires `CreateService`, which scopes it to **classic installer
packaging** — MSI or EXE, including via winget. It is **not available under
Store/MSIX distribution**: a sandboxed MSIX package cannot call `CreateService`
at all, and Desktop Bridge ties service lifecycle to package add/remove rather
than to our own registration. If Store distribution is ever wanted, it is
default-tier only.

### 6.3 macOS and Linux registration mechanisms

These were unnamed in the first draft, and the two macOS options **diverge on
exactly the property §6.1 claims** (one prompt, no credentials):

- **Legacy `.pkg` with a root `postinstall`** gives a genuine one-prompt
  equivalent: the installer's own authorization covers writing the LaunchDaemon
  plist to `/Library/LaunchDaemons`. Note the distinction §3.1 turns on — the
  *installer* runs as root; the *daemon* does not. The plist declares
  `UserName` = the dedicated `_cogos` account, and the postinstall provisions
  that account. A plist that omits `UserName` runs the daemon as root and
  silently voids §5.4's two-party attestation, so §6.4's privilege-ceiling lint
  reads the running identity rather than the installer's.
- **`SMAppService.daemon`** requires the plist embedded in a *signed, notarized
  `.app` bundle*, plus a separate manual toggle by the user in System Settings →
  Login Items. A bare Homebrew or `curl`-installed Go binary has no bundle to
  embed a plist in, so this path does not exist for our current distribution
  shape.

Recommendation: the `.pkg` postinstall path for node mode; `SMAppService` only
if and when a signed app bundle ships. Note also that at the plumbing level
`LaunchctlController`'s `plistPathForLabel` currently targets
`~/Library/LaunchAgents` unconditionally — there is **no domain-aware plist
placement today**, which is part of the net-new work §3.3 names.

On Linux, `DynamicUser=yes` with `StateDirectory=` is the closest structural
analogue of the Windows virtual account: a transient UID, no account to
provision or rotate, and an automatically-owned state root under `/var/lib`.
The tradeoff against a static dedicated user is that a dynamic UID complicates
any static ACL a doctor lint wants to assert *before* first start. This choice
is paired with §9 Q4 and should be made once, not twice.

### 6.4 Declared doctor surface

The doctor declares the whole surface as observable state. The lint set below
covers the invariants this RFC actually asserts; the first draft's list did
not, including one §4.1 explicitly promised was lintable.

| Lint | Asserts |
|---|---|
| service exists and runs under the expected identity | §3.1 |
| agent task/unit registered and healthy | §3.2 |
| **no scheduled task with `LogonType=S4U`** for the kernel or any cogos-managed service | §6.1 hard rule |
| **no legacy boot task** (S4U *or* stored-password) present once rung 3 is active | §7 |
| door present, ACL correct, **created first-instance by the service account** | §4.2 |
| **door namespace reserved at registration** — the pipe's object-namespace directory / the socket's parent directory grants create rights to the machine identity only | §4.2 |
| **door-name refusal is distinguishable from down** — "refused: name held by a foreign principal" is its own observable, naming the holder where the OS exposes it | §4.2 |
| **no auxiliary IPC endpoint** in the kernel's namespace besides the declared door. Scope note: this constrains the **kernel's** namespace — the machine plane and the agent's service-facing side. It does not forbid the agent from using mechanisms it already owns as the seat(local) principal to serve consumers running as that same principal (§4.1 item 3) | §4.1 |
| **service-lifecycle and register routes are not served on the loopback listener** — `/v1/services/*` and `POST /v1/services/register` reachable on the door only, and only against a machine-plane-scoped grant | §4.1 item 2 |
| **no gate-exempt grant read** — no route returns a node-root credential without passing the grant gate; **the machine plane's copy** of the node-root credential is not readable from any seat(local)-readable path. Scope note: this is about the machine root's copy, not about the agent's delivery of the credential to a consumer running as the same operator (§4.1 item 3) | §4.1 item 3 |
| **no HTTP route writes the machine config file** — `PATCH /v1/config` and `POST /v1/config/rollback` resolve to the seat/workspace `kernel.yaml` only; a patch naming a machine key is refused, and no route serves the machine file's contents in any form | §4.1 item 5, §5.1a |
| **the machine plane executes nothing from a seat(local)-writable or workspace-portable directory** — no machine-plane route or supervisor resolves an executable from `~/.claude/skills`, `<ws>/.claude/skills`, `.cog/bin/agents/definitions/`, `~/.cogos/plugins/`, or `~/.cogos/runtime-services.yaml`; `POST /v1/skills/{name}/exec` is not registered on the machine plane's listener | §4.1 item 6, §5.8 |
| **protocol floor satisfied** (named check, distinct from skew) | §4.3 |
| **no machine-plane security key readable from a seat(local)-writable path or from any path in the BEP inclusion list** — no `Enable*` gate, `BindAddr`, `port`, or `WriteRouteGrantAuthDisabled` resolvable from `.cog/`; a machine key found in `kernel.yaml` fails the lint rather than being shadowed. A `port`-only failure is a hygiene finding, not a breach (§5.1a) | §5.1a |
| **no seat(local)-writable path under the machine root** | §5.8 |
| **no machine-plane-writable path inside `.cog/ledger/`** — the direction two-party attestation rests on. **Post-rung-3 gate:** this measures the destination, not today's build — **four** kernel ledger buckets are appended from the machine plane now, through at least two append implementations (§5.1c), and the lint passes only once their relocation lands. The doctor reads the bucket set from `internal/writepathaudit`'s golden rather than from a list in this document | §5.2, §5.1c, §5.8 |
| **BEP inclusion list excludes both strands** — the declared BEP folder set contains no path that is, contains, or is a prefix of `.cog/ledger/**` or the machine strand. Read the **inclusion list**, not an ignore file: BEP ships no exclusion mechanism, so the list is the object | §5.2 |
| **privilege ceiling** — machine-plane identity is not root / SYSTEM / Administrators and holds no `SeBackupPrivilege`, `SeRestorePrivilege`, `SeTakeOwnershipPrivilege`, or `CAP_DAC_OVERRIDE` | §3.1, §5.2 |
| **machine-plane writes inside `.cog/` are exactly the declared set** — **the declared set is generated, not written here.** The lint reads `internal/writepathaudit/testdata/inventory.golden.json` (#591) and asserts that every `.cog/`-anchored site carries a recorded plane disposition; an undispositioned site fails. The golden itself is CI-gated by `TestInventory_MatchesGolden`, so a *new* write path fails the build before this lint ever runs — the two compose as generate-then-disposition. **Scope:** the disposition check runs over the **separable** rows only — the ADR-097 / ADR-098 targets under `.cog/mem/**` and `.cog/skills/**` are **excluded pending Q7**, because those paths are not separable from the authored corpus and the predicate over them would be provenance, not path; the sites named in §5.1b but not yet dispositioned are **open as Q9**. `.cog/run/` and `.cog/.state/` are gitignored and absent from the BEP inclusion list | §5.1b, §9 Q7, §9 Q9 |
| **capability envelopes are machine-plane-stored and not seat(local)-writable** — no envelope resolvable from `.cog/`, no seat(local) principal holding write on its own envelope | §4.6 |
| **service image path integrity** — write-ACL on the image, unquoted-path check | §5.8 |
| **self-update signature gate intact** — no unsigned-application path reachable from the reconcile provider | §5.8 |
| **NodeID byte-identical** across the rung-2 → rung-3 transition, **and BEP DeviceID / cert bytes identical** — the derived value alone goes green while the anchor forks | §7.2 |
| unpaired-past-window entries, **classified overdue vs. unexpressed vs. stalled**, with the vacancy call **paired to the occupancy signal** — never inferred from the counterpart strand's own silence | §5.4 |
| declared otherness tier satisfied per paired kind | §5.6 |
| **library weakenings carry operator-tier attestation** — every **non-additive** change to an existing kind (tier, window, complement, direction, or field requirements — changed *or* removed) has its paired attestation entry on the machine strand. Pure additions do not | §5.7 |
| **capability-envelope enrolment state is observable** — every connected seat(local) principal either holds an envelope or is reported as **un-enrolled**, and refusals for want of an envelope are distinguishable from policy denials | §4.6 |
| per-platform invariant checks: no UI surface, no user-profile read, no user credentials held — one enforceable check per platform, mirroring the S4U rule's specificity | §3.1 |

Four honesty notes attached to the surface:

- **The write-path lints inherit the generated inventory's margins, and those
  margins are wide.** At #591's merge the inventory resolves 237 sites but bins
  **84 as unanchored** (root not positively resolved) and **41 as dynamic**
  (unresolved entirely), and declares **73 subprocess writers out of scope for
  v1** — writes performed by processes the kernel spawns, plus non-Go writers
  (shell and Python scripts this repo runs). A `.cog/`-anchored lint built on
  this inventory therefore proves something about the sites the scanner *could*
  place, and proves nothing about the rest. That is a far better position than
  a hand-authored list, which has the same blind spots without reporting them —
  but it is not coverage, and the doctor must not present it as coverage. The
  honest claim is: **no unreported blind spot**, not **no blind spot**.
  Narrowing the unanchored and dynamic buckets, and bringing subprocess writers
  in scope, is follow-on work on #591's package, not on this RFC.

- **Some lints measure the destination, not today's build, and each one says so
  in its own row.** Two are marked: the `.cog/ledger/` write lint (fails until
  §5.1c's relocation lands) and the declared-set lint (scoped to the separable
  rows pending Q7). A lint that is known to fail on a clean checkout is honest
  only if it is declared as a gate rather than presented as a check; a lint
  scoped narrower than its section's claim is honest only if the exclusion is
  named. Both are named here rather than discovered by whoever first runs the
  doctor.
- **A pipe or socket ACL grants a SID, not a binary.** "Only the agent holds
  the door" is not expressible and therefore not lintable. The threat model
  must not assume that unobservable.
- **`--fix` reconciles what it can without *credentials*, which is not the same
  as without *elevation*.** The first draft's "which, in this architecture, is
  everything" conflated the two. Registration and DACL repair need elevation;
  nothing needs a password.

---

## 7. Migration ladder

Per node, doctor-driven, each rung and sub-rung a settled observable state with
its own gate predicate.

1. **Agent-only** (today's default) — user-plane supervision, no boot presence.
2. **Stored-password interim** — boot task with Password logon. Acceptable
   stopgap; known rotation caveat; doctor detects the `0x8007052E`
   logon-failure signature and prompts re-entry.
3. **Two-plane** — split into sub-rungs below.

### 7.1 Rung 3 is a sequence, not one operation

The first draft conjoined *service registered*, *machine state migrated*, *boot
task deleted*, and *agent demoted* into a single rung with no intermediate
checkpoint and no way back. An operator who lands mid-rung at 2am with a dead
service, a deleted boot task, and half-migrated state has no doctor-declared
route home. Sequenced into five sub-rungs, each a settled observable state with
its own gate predicate and its own way back:

| Sub-rung | Gate predicate | Rollback |
|---|---|---|
| **3a. Service registered, boot task retained** | service registered and running under the expected identity; boot task still present and healthy | unregister the service; rung 2 is untouched |
| **3b. Cert relocated, NodeID verified** | NodeID byte-identical to its rung-2 value **and BEP DeviceID / cert bytes identical** (§7.2) — the NodeID check alone measures the derived value, not the anchor | restore cert dir from the retained copy; both checks re-verified |
| **3c. Machine state migrated, old copies retained read-only** | machine state present and ACL-correct at the new root; old copies still readable; **dual-valid window open**; **and the §5.1c ledger-writer relocation has landed** — the machine plane no longer appends inside `.cog/ledger/`, so §6.4's write-isolation lint passes | repoint at the old root; both copies are still valid |
| **3d. Boot task deleted** | N consecutive healthy service checks passed while 3c's dual-valid window held | `--fix` re-creates the rung-2 boot task and repoints state at the old root |
| **3e. Agent demoted to session duties, credential distribution moved to the door, skill exec inverted, principals enrolled** | agent connected to the door, executing user-plane requests; the node-root credential is served from the machine root over the door and the gate-exempt GET is gone (§4.1 item 3); the **seat-owned delivery mechanism is chosen and declared** and each named consumer (dashboard, canvas, harness hook sessions, THESEUS) is repointed at it — **no new listener** (§3.3); `POST /v1/skills/{name}/exec` removed from the machine plane and re-landed as an agent-executed door kind (§4.1 item 6); **every logon principal on this node either holds a capability envelope or is reported un-enrolled** — enrolment is an operator-tier act (§4.6) and a node with N principals needs N of them | agent reverts to standalone kernel supervision, to the rung-2 credential path, and to in-process skill exec |

**Migration procedure per state category**: copy → verify → atomic rename, with
a doctor gate on "machine state present and ACL-correct at the new root"
*before* the old copy is removed or the old process stops reading it. Moving
the reconcile ledger and registry out from under a running kernel and then
flipping ownership is a flag-day cutover absent that dual-valid window.

**The doctor's `--fix` must be able to walk back down**, not only up. A ladder
with no descent is an installer script wearing a ladder's clothes.

### 7.2 Rung 3b is a cryptographic identity relocation

This is the sub-rung most likely to fail silently, and the first draft did not
name it at all.

`CertDir()` in `pkg/substrate/bep/tls.go` resolves to `os.UserHomeDir()` +
`/.cog/etc`. `internal/engine/node_identity.go` records the anchoring chain
verbatim: **BEP device cert → DeviceID → NodeID**, machine-scoped.

A machine-plane process running as `NT SERVICE\cogos`, `_cogos`, or a systemd
dedicated user resolves a **different** `os.UserHomeDir()`, finds no cert, and
**mints a fresh one**. New DeviceID, therefore new NodeID. The node
re-identifies itself on the constellation, and every CogBlock the service
stamps thereafter carries a new `SourceIdentity`.

The bitter part: `$HOME`-anchoring is exactly the property that made certs
immune to the mirror-image bug this code was written to close (a bind-mounted
child kernel cloning the host's identity — a child container with a fresh
`$HOME` finds no cert and correctly mints its own). Two-plane converts that
protection into the failure.

Therefore, in §5.1's partition table the **BEP cert dir is named explicitly**
as machine-plane node-local state, and rung 3b is specified as a **cert
relocation, not a state move**:

**Correction: no existing seam relocates the cert.** The second draft said to
use "one of the seams `node_identity.go` already provides — `COG_NODE_DIR` or
`COG_NODE_ID`." Neither reaches the anchor, and the distinction matters because
one of them makes the gate *look* green while the identity forks underneath it:

- **`COG_NODE_DIR` moves the node-id *cache*, not the cert.** It is consumed by
  `defaultNodeIdentityDir` in `internal/engine/node_identity.go` and relocates
  where the resolved id is stored.
- **The cert anchor is hardcoded.** `internal/engine/process.go` declares
  `nodeIDCertDir` as `bep.ExpandCertDir("")` — the default cert dir,
  `os.UserHomeDir()` + `/.cog/etc` (`pkg/substrate/bep/tls.go`). It is a package
  var so tests can redirect it; **no env or config override is threaded to it in
  production.** The file's own comment records this as a **KNOWN BOUNDARY**: a
  node overriding `cluster.CertDir` (the one real cert seam,
  `pkg/substrate/bep/config.go`, honored in `internal/engine/bep_engine.go`)
  "would need that resolved dir threaded through `NewProcess` to stay
  consistent."
- **Consequence.** Under the service account, `bepAnchoredNodeID()` still finds
  no cert at the hardcoded default and the BEP device identity is minted fresh —
  **a new DeviceID on the wire even when `COG_NODE_DIR` preserves the NodeID
  string.**
- **`COG_NODE_ID` is worse for this purpose.** Pinning the id bypasses the cert
  chain entirely, which makes a byte-identical-NodeID check **vacuously green by
  construction** while the wire identity forks. It is the same substitution
  hazard `node_identity.go` already warns about for the sibling variable.

**Therefore rung 3b requires net-new work** (recorded in §3.3): thread a
cert-dir override through the `nodeIDCertDir` anchor — the same resolved dir
`cluster.CertDir` supplies to the BEP engine — so that the machine plane
anchors to the *relocated* cert rather than to a missing default.

Gates on 3b:

- **Doctor check: NodeID is byte-identical before and after the transition.**
  Retained as a hard gate.
- **Doctor check: the BEP DeviceID and the cert bytes are identical before and
  after.** Added, because the NodeID check alone measures the derived value and
  not the anchor — it is exactly the check that goes green while the identity
  forks. The anchor is what rung 3b relocates, so the anchor is what the gate
  must read.
- Per §5.6 a NodeID change is an **operator-tier** paired kind: it cannot settle
  on cross-plane attestation alone.

### 7.3 First migration target

The Windows node that motivated the design is the first rung-3 target: it
currently sits on rung 2 and carries the incident record. Note explicitly that
it brings **no inherited supervisor implementation** with it (§3.3) — its rung-2
state was hand-registered by PowerShell, and the Windows machine-plane
registration path is net-new.

---

## 8. The general form

One page, then out of scope.

**Two identity domains sharing one processing substrate is a convergent
attractor, not a shape we chose.** The host operating systems each arrived at
it independently and describe it in their own vocabulary: Windows'
session-0 / session-N split, macOS' LaunchDaemon / LaunchAgent domains, systemd's
system / user managers. §2.3's runtimes converged on the same split from the
application side. The two-plane design is not an accommodation of Windows; it
is this attractor, met where it was already sitting.

**The host boundary codebooks are the paired-kind library's ancestors.**
D-Bus policy files, XPC entitlements, and COM brokering all do what §5.3 does:
declare a closed vocabulary of what may cross a privilege boundary, so that
crossing is checkable rather than negotiated. They stop one step short — they
are **permission grammars without memory**. Each decides *admissibility per
call* and keeps no durable record of whether a call's counterpart ever
happened. The strand pair adds exactly that step: durable, two-party
settlement, with an observable for the requests that never came back.

**The same operation is *proposed* one scale up — it does not exist yet.** The
second draft wrote "BEP's envelope/receipt exchange between constellation nodes
**is** the inter-node instance of the identical pairing," in the present
indicative. That is not true of the shipped code and the correction is worth
making sharply, because §8's whole argument is a count of instances. BEP here
is Syncthing's Block Exchange Protocol (`pkg/substrate/bep/`): a file-sync
protocol whose full message set (`pkg/substrate/bep/proto.go`) is ClusterConfig,
Index, IndexUpdate, Request, Response, Ping, Pong, Close, **Dispatch**, and
**DispatchResult**. It carries **no envelope, no receipt, and no durable
two-party settlement by complement** — the words do not appear in the package.

**The Dispatch / DispatchResult pair is named rather than trimmed, because it is
the nearest thing in BEP to a complement and leaving it out of this list would
be favorable editing.** It is a remote-harness dispatch request and its result —
a live **RPC pair**, and it does not count as an instance of §5.3's operation for
one specific reason: it has **no durable strand and no settlement by
complement**. A `DispatchResult` is a transport reply, correlated in flight and
gone; nothing records that a `Dispatch` went unanswered, and no observable
exists for the ones that never came back. That is precisely the step §8 says the
host boundary codebooks stop short of, and BEP stops short of it too.

Restated honestly: an inter-node strand pair — two write-isolated node records,
a shared vocabulary, settlement by complement at a **cross-node** otherness tier
with longer windows — is a **plausible and attractive future instance** of the
same operation, and BEP's transport is where it would ride. It is proposed, not
observed.

So the count here is **one built instance and one proposed one**, which is not a
generalization and this RFC does not claim it as one. What §8 does claim is
narrower and still worth saying: the host boundary codebooks above are
**structural ancestors** — the same closed-vocabulary move, one step short of
memory — and the strand pair is what that move looks like once it keeps a
record. That is a lineage argument, not an induction over instances.

If the mechanism does generalize — a paired-kind library as a reusable
cross-boundary settlement primitive, instantiated at the process boundary here
and proposed at the node boundary — **it wants its own RFC**, and that RFC's
first job is to build the second instance rather than to cite it. This one
scopes the primitive to the two-plane runtime and claims nothing beyond the one
instance it actually specifies.

---

## 9. Open questions

**Q1 — Corpus placement. ANSWERED in §5.1.** The cogdoc corpus is
workspace-portable seat(local) state, inside `.cog/`, with a declared machine-plane
*read* grant for indexing and no ownership transfer. What remains is not a
placement question but a visibility-policy question: what the service's read
grant admits, and whether that admission is declared.

**Q2 — Multi-seat semantics. NARROWED.** The first half is answered twice over:
concurrent agents are already reachable today via per-user logon triggers under
Fast User Switching (§3.2), and the multi-seat session-occupancy RFC already
establishes N simultaneously-attached seats with identity bound per-seat and a
talk-policy Reconcilable arbitrating them. **What genuinely remains is a schema
amendment**: the capability-envelope-and-policy-vocabulary RFC fixes
`scope: public | org | constellation | node | workspace | session` — an enum
with no `seat` member. Does `seat` join that enum, or do per-seat envelopes
derive from `session` scope under selective scope inheritance — the
"inner can tighten, cannot loosen" rule, which is stated in that same
**capability-envelope RFC**, not in ADR-074? (ADR-074 establishes selective
scope inheritance; the tighten/loosen phrasing belongs to the RFC and the
second draft credited it to the ADR.) Per-seat envelope scoping and §4.6's
per-request **seat(local)** binding are **one question, not two** — and per §4.6 the
envelope's *plane and write authority* are no longer part of this question:
they are decided (machine-plane storage, operator-tier mutation, enforced at
the door). What remains open is the schema.

**Q3 — Request queue durability. ANSWERED: ledger-backed** — with security
constraints that are not optional attachments. ADR-093 already resolved the
analogous durability question for managed sessions (adopt orphaned subprocesses
if alive, otherwise rebuild from the ledger), and the
external-credential-federation RFC already specifies single-flight and
idempotent re-issue. The starting position is inherited, not open. Required
constraints, all in §4.6: each queued request carries its seat(local)
principal, a nonce, and an expiry; the agent refuses requests not addressed to
its own principal; the service accepts a digest only from the addressed agent
against a live nonce; the queue has a declared depth cap and drop policy,
because a ledger-backed queue with no agent present is unbounded growth on
machine-plane storage.

**Q4 — Linux lingering. ANSWERED: keep the split** — and the reason is not
symmetry. `loginctl enable-linger` does sidestep the *identity* leg of §2.2's
three-way tension, since the user manager runs as the real operator UID. It
does **not** obviously sidestep the *credentials* leg: PAM-driven unlock of the
user's secret store (`pam_gnome_keyring` and equivalents) is triggered by
interactive login, and a lingering user manager started at boot with no
interactive PAM session generally has a **locked keyring**. That is the same
shape of hazard as the S4U/DPAPI incident this RFC exists to answer — right
identity, unavailable credential store — failing differently. The honest
residual question is not "does a gap exist" but **"how bad is the keyring gap
for the secret stores CogOS actually reads on Linux"**, which is answerable
empirically and should be, before this is called settled. Pair the answer with
the `DynamicUser=` vs. static-dedicated-user choice in §6.3.

**Q5 — Naming. OPEN. Awaiting an operator ruling from the dome taxonomy.**
This is a ratification gate. See §1.1: `service` and `agent` are placeholders,
`agent` overloads an RFC-033 primitive, and the load-bearing collision is
**`seat`**, which ADR-102 and the multi-seat session-occupancy RFC already
define as a participant attached to a session. This draft marks the local-logon
sense **seat(local)** throughout as an interim measure and does not presume the
outcome.

**Q6 — Settlement after the portable half moves. OPEN, and real.** The
seat(local) strand is workspace-portable; the machine strand is node-local. Carry
`.cog/` to a second node and an imported seat(local) strand is paired against a
machine strand that holds **none of its complements** and whose sequence space
**shares no origin** with it. Every imported cross-plane pair then reads
unsettled — and by §5.4's own rule, a counterpart that never ticked in this
node's history is classified *unexpressed* and **suppressed**, so the failure is
silent by design. The §5.4 truth table does not save it either: the occupancy
signal on the new node is *present*, which classifies the whole imported backlog
as **stalled** — a wall of alarms for entries that were correctly settled
somewhere else.

Neither reading is right, which is why this is an open question rather than a
defect with an obvious patch. The candidate answers, none chosen here:

- **Scope pairs to the node that raised them** — an imported entry whose
  counterpart strand is a different machine strand is *out of scope for this
  node's settlement check*, not unsettled. Cheap; loses the ability to notice a
  genuinely unfinished transplant.
- **Seal at transplant** — the doctor emits a settlement seal before the move
  and the importing node reads it as the origin of a fresh sequence space.
  Honest; needs an operator-tier act at exactly the moment the operator is
  busy moving.
- **Make the pair carry its machine-strand identity** — every cross-plane entry
  stamps the machine strand's identity, so an imported entry is
  self-describingly foreign. Most durable; widest change to §5.3's kind schema.

This interacts with §8: the inter-node instance proposed there is one framing of
the same problem, which is a reason to answer this question *before* that RFC,
not after. Note one thing the third pass settles about its *mechanism*: the
seat(local) strand travels by **git or an explicit move of the directory, never
by BEP** (§5.2), so a transplant has an operator-visible moment. That makes the
"seal at transplant" candidate below implementable; it does not choose it.

**Q7 — Projection targets have no path boundary. OPEN, and it is a genuine
corpus conflict this RFC cannot settle.**

§5.1b row 3 admits the ADR-097 / ADR-098 projection targets as declared
machine-plane writes, and §5.1's per-artifact table gives them "machine identity
write, seat(local) read." The row directly above gives the **authored** corpus
at `.cog/mem/` "seat(local) identity write, machine read grant." **Those are the
same paths.** ADR-097 §3's placement table writes projections to
`.cog/mem/semantic/insights/{slug}.cog.md`, `semantic/projects/`,
`semantic/references/`, and `episodic/profile/` — exactly where authored cogdocs
live. ADR-098's `.cog/skills/` is the same shape, milder.

Three consequences, stated so the conflict is not softened:

- Applied literally, the two rows **contradict each other** over one directory.
- Applied as written, the operator **loses write on their own corpus**.
- §6.4's "machine-plane writes inside `.cog/` are exactly the declared set" is
  therefore a **provenance** predicate over these paths, not a **path**
  predicate — and provenance is not enforceable by the ACL mechanism §5.2
  insists on ("enforced by ACL, not convention"). The ADR-094 lineage row does
  not have this problem: `projection_reconciler.go` writes under
  `.cog/mem/semantic/lineage/projections/`, **a separable subtree** an ACL can
  be hung on.

**Why this RFC does not decide it.** ADR-097 and ADR-098 are **accepted**. The
partition this RFC proposes is incompatible with their file layout, not with
their authority. Resolving that means changing one of the two, and this draft
has standing to propose but not to amend an accepted decision. The candidate
resolution paths, neither chosen here:

- **A separable projections subtree**, via an ADR-097 amendment (and ADR-098 in
  parallel): projections land under a dedicated path — the shape the lineage
  reconciler already uses — so the ACL has something to attach to and the lint
  becomes a path predicate. Cleanest for this RFC; costs an amendment to an
  accepted ADR and a migration of existing projections.
- **Door-mediated writes instead of a direct ACL**: the reconciler does not hold
  a filesystem write ACL on the corpus at all. It raises a declared request kind
  (§5.3) and the **agent** performs the write in the operator's identity. The
  paths need not be separable, because the machine plane never writes them.
  Costs a round trip per projection and makes projection availability depend on
  the agent being up — which §5.4's vacancy classification already models, so
  the cost is bounded and observable.

Until it resolves, §5.1b marks the row **pending Q7** and §6.4's declared-set
lint is **scoped to the separable rows only**, with the exclusion named in the
lint text. This is the honest state: an unenforceable predicate declared as
unenforceable beats an enforceable-sounding one that no ACL can back.

**Q8 — Peer-authored agent definitions. OPEN, flagged rather than dispositioned.**

`internal/engine/bep_provider.go` binds the BEP sync watch directory to
`<workspaceRoot>/.cog/bin/agents/definitions/` (folder id `cogos-agent-defs`,
`internal/engine/bep_model.go`), and it is the **only** subtree BEP replicates
as shipped. §5.1b now carries it as a declared machine-plane write. What it
does not carry is a trust story, because none exists in the corpus to import:
**the content of that directory is authored on other nodes.**

The question, stated as a trust question rather than a placement one: *a remote
peer authors an agent definition; the machine-plane engine on this node writes
it into this workspace; what then reads or executes it, under which identity,
and what admits it?* Every other cell in §5.1's partition has a local author.
This one does not, and off-node authorship is the one case neither the ACL model
(§5.8, local principals) nor the privilege ceiling (§3.1, local privileges)
reaches — the same structural gap §5.2 names for the ledger strands, where the
answer was to keep them out of the replicated set entirely. That answer is not
available here, because cross-node agent distribution is the feature.

Bound already decided, so this is a narrowed question rather than an open field:
per §5.8, **the machine plane executes nothing resolved from this directory**,
for the same reason it executes nothing from `.claude/skills` (§4.1 item 6).
What remains open is admission — signing, peer attestation, an operator
enrolment step, or a declared refusal to consume peer definitions without one.
Recorded here as future-hardening work with a named owner-shaped question rather
than folded into a disposition column that would imply it was answered.

**Q9 — Undispositioned `.cog/` write sites surfaced by the generated inventory.
OPEN, and newly visible rather than newly created.**

§5.1b's excerpt dispositions five paths. The generated inventory (#591) anchors
**97 write sites under `.cog/`**, and several that are plainly not authored by
the operator carry no plane assignment in this RFC — among them
`.cog/blobs/manifest.jsonl` (`internal/engine/blobstore.go`),
`.cog/run/bus/*.cursors.jsonl` (`internal/engine/bus_consumers.go`),
`.cog/state/conversations` (`internal/conversations/index.go`),
`.cog/observatory/quarantine/` (`internal/conversations/quarantine.go`),
`.cog/mem/episodic/experiments/` (`internal/engine/benchmark.go`), and
`.cog/docs/generated/` (`internal/engine/docs_generate.go`).

Most of these look like the daemon-state row: derived, runtime, node-local, and
therefore probably admissible under §5.1b's rule (b) with the machine plane
holding write. **"Probably" is the finding.** Under this document's own standard
a surface is either named with a disposition or declared open, and guessing a
disposition per path from its name is exactly the hand-sweep method this pass
retired. Two of them warrant more than a rubber stamp:
`.cog/observatory/quarantine/` holds content that was quarantined *because it
was untrusted*, which makes its plane assignment a security question rather than
a bookkeeping one; and `.cog/docs/generated/` is a generated tree living beside
authored docs, which is the shape of Q7's separability problem in miniature.

**Disposition of the question itself:** this is a bounded sweep, not a design
problem — walk the golden's `.cog/` section, assign each site a plane, and land
the assignments in §5.1b. It is a **prerequisite for rung 3c** for the same
reason §5.1c's relocation is: an ACL cannot be hung on a partition with
unassigned cells. It is listed separately from Q7 because Q7 is a genuine corpus
conflict requiring an ADR amendment, whereas this is work that merely has not
been done yet. Naming the difference matters — an open question that is really
just unfinished labour should not borrow the authority of one that is genuinely
unresolved.

---

## 10. References

- myrgic/cogos#586 — anchor issue (feature request, incident record)
- myrgic/cogos#591 — **merged** (squash `6c2f634`): `internal/writepathaudit`,
  the code-derived write-path inventory with a golden-diff CI gate. The
  authoritative enumeration §5.1b / §5.1c / §6.4 defer to. Golden:
  `internal/writepathaudit/testdata/inventory.golden.{json,md}`; regenerate with
  `go test ./internal/writepathaudit/ -run TestInventory_MatchesGolden -update`
- myrgic/cogos#101 — services extensibility; Linux user-bus scope, Windows
  explicitly deferred (see §3.3 for what this RFC does *not* inherit)
- myrgic/cogos#551 — kernel write-route hardening; precedent for §4.5 and for
  the refusing-zero-value polarity in §4.3 and §5.4
**Two ADR namespaces are in play and this draft distinguishes them.** ADR-065,
ADR-074 and ADR-001 are **substrate-corpus ADRs**, which carry slug filenames
with numbers in frontmatter — the standing convention there is refer-by-slug,
and their slugs are given below. ADR-093 through ADR-121 are **kernel-repo
ADRs** under `docs/adrs/`. The number `102` collides across the two namespaces
(`internal/engine/boot_node_root_grant.go` cites a different ADR-102); every
citation below names the title, not only the number.

- ADR-001 — *workspace membrane* — membrane defined by the presence of `.cog/`
- ADR-065 — *container-native daemon lifecycle* — §7 specifies
  `.cog/run/daemon/state.yaml`; the disposition is §5.1b
- ADR-074 — *nested sovereignty and reconciliation scopes* — **selective scope
  inheritance.** The "inner can tighten, cannot loosen" phrasing cited in §9 Q2
  belongs to the capability-envelope RFC draft, not to this ADR
- ADR-093 — managed-session durability: adopt-if-alive, else rebuild from ledger
- ADR-094 (**status `Draft`**) — lineage observatory — the projections the
  reconciler writes (§5.1b). Cited for the projections' *purpose*; the §5.1b
  row's authority is **ADR-095** (accepted) plus shipped code, per §5.9's own
  rule that a draft does not settle another draft's question
- ADR-095 — daemon reconcile-loop driver — the loop that runs those reconcilers
- ADR-097 — memory projection reconciler — kernel-run writer into `.cog/mem/`;
  its §3 placement table writes into the **same directories as the authored
  corpus**, which is the conflict §9 Q7 declares
- ADR-098 — skill projection reconciler — kernel-run writer into `.cog/skills/`;
  same shape as ADR-097, milder
- ADR-099 — machine-tier path conflict log; **this RFC provides the argument to
  close its recorded "unsettled" entry (§5.9); the amendment lands on
  acceptance, and the change is larger than the one line the ADR anticipated**
- ADR-102 — *Operator as Reconcilable* — the session as a **2-seat**
  coordination object (operator seat and agent seat binding to one harness
  instance). The generalization to N co-resident seats is the multi-seat
  session-occupancy RFC's, not this ADR's
- ADR-121 — single-binary consolidation
- RFC-033 (draft) — cognitive primitives: `.cog/` substrate vs. `.cogos/` node
  runtime. Named by ADR-099 as the settling authority for the machine-tier path
- RFC-036 — node = hardware, workspace = overlay; NodeID machine-scoped
- RFC (draft) — multi-seat session occupancy
- RFC (draft) — capability envelope and policy vocabulary; source of the
  `scope` enum (§9 Q2) and of the tighten/loosen rule
- RFC (draft) — external credential federation; OwnerActuator ladder, wired
  into §4.4
- Code touchpoints named in this draft: `internal/engine/serve.go`,
  `internal/engine/serve_services.go`, `internal/engine/serve_grant_auth.go`,
  `internal/engine/serve_cors.go`, `internal/engine/serve_config.go`,
  `internal/engine/serve_skills.go`,
  `internal/engine/boot_node_root_grant.go`,
  `internal/engine/serve_identity_grants.go`,
  `internal/engine/config.go`, `internal/engine/process.go`,
  `internal/engine/bep_engine.go`, `internal/engine/bep_provider.go`,
  `internal/engine/bep_model.go`, `internal/engine/cli_selfupdate.go`,
  `internal/engine/daemon_lifecycle.go`,
  `internal/engine/projection_reconciler.go`,
  `internal/engine/decision_lineage_reconciler.go`,
  `internal/engine/service_supervisor.go`,
  `internal/engine/service_supervisor_stub.go`,
  `internal/engine/node_identity.go`, `internal/engine/ledger.go`,
  `internal/engine/worktree_spawn.go`, `internal/engine/mcp_stubs.go`,
  `internal/providers/all/all.go`,
  `pkg/skills`, `pkg/substrate/bep/tls.go`, `pkg/substrate/bep/config.go`
