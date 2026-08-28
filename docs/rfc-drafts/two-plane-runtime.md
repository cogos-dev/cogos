---
type: rfc-draft
title: "Two-plane runtime: machine-plane service + user-plane session agent"
status: draft
closes: "myrgic/cogos#586"
relates:
  - "myrgic/cogos#101"
  - "myrgic/cogos#551"
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
revision: "third draft, post-council + post-adversarial-re-review"
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

> **Revision note.** This is the third draft. The second incorporated a
> five-lens adversarial review (Windows mechanics, security, substrate
> coherence, cross-platform parity, operations) and an operator ruling on the
> state question the review could not settle; three first-draft claims were
> **retracted**, not softened: the "exactly one door" invariant (§4.1), the
> inheritance-from-#101 supervisor-seam claim (§3.3), and the single-ledger
> reading of the state partition (§5).
>
> This third draft answers a second, adversarial re-review by two fresh lenses
> (security; corpus coherence) that did **not** clear the blocker gate. Four
> further claims are **retracted or corrected against code**:
>
> 1. **`kernel.yaml` was on the wrong side of the partition.** It is loaded from
>    `CogDir/config/kernel.yaml` (`internal/engine/config.go`), i.e. inside
>    `.cog/` — seat(local)-written, git-settled, BEP-replicated — and it is where
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

Two further items belong on the net-new list, both surfaced by the second
adversarial re-review and both previously described as existing seams:

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
| The HTTP listener | loopback TCP | bearer-token verification in-process | CLI, MCP clients, harnesses, any local process |

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
     directly — the dashboard, a running harness hook session, THESEUS, all
     named in `boot_node_root_grant.go`'s own header — acquire the credential
     from the agent instead. **This is a breaking change for those consumers
     and is called out as such**, sequenced at rung 3e (§7.1), where the agent
     is already being demoted to session duties.
4. **The auxiliary-endpoint lint survives the retraction**, narrowed to what is
   actually checkable: no IPC endpoint in the kernel's namespace other than the
   declared door (§6.4).

All four items above are lintable and all four appear in §6.4. The second
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
| **machine plane** (service-written) | node identity + **BEP cert dir**; peer table; BEP membership; machine ledger strand (§5.2); **machine config** (§5.1a); **per-seat(local) capability envelopes** (§4.6); kernel binary + self-update staging; machine-root service definitions | **not empty — enumerated, not incidental** (§5.1b): daemon lifecycle state; projection-reconciler outputs |
| **user plane** (seat(local)-written) | per-user OAuth / token stores; DPAPI / keychain material; mapped drives; notification state | seat(local) ledger strand (§5.2); cogdoc corpus / memory; seat(local) config; harness homes |

**The second draft declared the machine × portable cell "empty by
construction." That is retracted: it is false against shipped code and against
accepted decisions the draft never cited — ADR-065, ADR-094/ADR-095, and
ADR-097/ADR-098** (§5.1b). What the ruling's structural content actually
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
| Projection-reconciler outputs (§5.1b) | machine | portable | `.cog/mem/semantic/lineage/projections/` and the ADR-097 / ADR-098 projection targets | machine identity write, seat(local) read | cross-plane |
| seat(local) ledger strand | user | portable (often gitignored — see §5.2) | `.cog/ledger/` | seat(local) identity only | n/a (append-only) |
| Cogdoc corpus / memory (authored) | user | portable | `.cog/mem/` | seat(local) identity write, machine **read grant** | cross-plane |
| seat(local) config | user | portable | `.cog/config/` (§5.1a: seat(local) keys only) | seat(local) identity | cross-plane |

### 5.1a The kernel config file is split

The second draft placed `kernel.yaml` nowhere and thereby left it where it is,
which the re-review correctly called placing it on the wrong side of the
partition. Verified against code: `internal/engine/config.go` sets
`CogDir = WorkspaceRoot/.cog` and loads the kernel config from
`CogDir/config/kernel.yaml`. That file is seat(local)-written, git-settled, and
BEP-replicated — and it is where `EnableServiceControl`, `EnableSkillExec`,
`EnableConfigMutation`, `EnableReconcileControl`, `BindAddr` and
`WriteRouteGrantAuthDisabled` are set.

The consequence is not subtle. Under two-plane as the second draft wrote it,
**the seat(local) principal configures the machine plane's entire
security posture with a text editor** — no HTTP call, no grant, no door,
defeating every gate §4.1 negotiates. And because `.cog/` is BEP-replicated,
**a peer node can push that posture onto this one.** §5.8 caught precisely
this shape for two lesser inputs (#101's plugin manifest overlays,
`runtime-services.yaml`) and missed the file that turns the gates off.

**Decided: the file splits by plane, not by convenience.**

- **Machine config** — a new file under the machine root, owned by the service
  identity, carrying every key that configures the machine plane: the `Enable*`
  gates (`EnableServiceControl`, `EnableSkillExec`, `EnableConfigMutation`,
  `EnableReconcileControl`), `BindAddr`, `WriteRouteGrantAuthDisabled`, the
  door's ACL principal set, the protocol/library floor, and the SCM
  failure-action policy (§3.1). Rule of thumb for future keys: **if turning it
  the wrong way weakens a boundary the machine plane enforces, it is a machine
  key.**
- **`kernel.yaml`** keeps only seat(local)- and workspace-scoped keys — model and
  provider selection, foveation and salience parameters, digest paths,
  intervals, workspace-local service *declarations* (as opposed to the
  machine-root service *definitions* the supervisor executes).
- **Precedence is not "machine wins ties."** A machine-plane security key
  appearing in `kernel.yaml` is **not** an override to be shadowed; it is a
  **lint failure and a boot warning**, because the interesting case is a key
  that arrived by BEP replication from a peer. Ignoring it silently is how it
  stays there.
- `WriteRouteGrantAuthDisabled` keeps its inverted polarity from #551 across
  the move: the zero value means **auth enforced**, so a caller that builds a
  config without the machine file gets the safe behavior, not the exposed one.

§6.4 lints the invariant directly: **no machine-plane security key is readable
from a seat(local)-writable or BEP-replicated path.** §3.3 records the code seam as
net-new migration work.

### 5.1b Declared machine-plane writes inside the workspace

Three accepted decisions and shipped code put machine-plane writes inside
`.cog/`. The second draft's "empty by construction" contradicted all of them
and cited none of them. Enumerated, so the exception is declared rather than
discovered:

| Path | Writer | Authority | Disposition |
|---|---|---|---|
| `<workspaceRoot>/.cog/run/daemon/state.yaml` | daemon lifecycle (`internal/engine/daemon_lifecycle.go`) | **ADR-065** §7, accepted — the runtime state file is specified there verbatim | **stays.** It is node-local content in a portable directory: mode, endpoint, container name, workspace path, PID. Machine identity holds write; seat(local) holds read. Not git-settled — `.cog/run/` is runtime scratch, and this RFC requires it be excluded from BEP replication and from git, which is the property that makes a portable *location* safe for node-local *content*. |
| `<workspaceRoot>/.cog/mem/semantic/lineage/projections/**` | projection reconcilers (`internal/engine/projection_reconciler.go`, `internal/engine/decision_lineage_reconciler.go`) | **ADR-094** (lineage observatory), driven by **ADR-095**'s reconcile loop | **stays.** Machine identity holds write on the `projections/` subtree only; the authored corpus around it stays seat(local)-write. Projections are derived, content-addressed outputs — regenerable, so a conflict is resolved by regeneration rather than by merge. |
| `<workspaceRoot>/.cog/mem/**` and `<workspaceRoot>/.cog/skills/**` projection targets | memory / skill projection reconcilers | **ADR-097**, **ADR-098**, both accepted — both specify a kernel-run reconciler writing into these trees | **stays, and §5.1's "declared read grant as an indexer, not ownership" is corrected.** For the *authored* corpus the read-grant framing holds. For the **projection targets** it does not: ADR-097 and ADR-098 specify a writer, and this RFC does not get to demote them by assertion. The machine plane writes projections; it reads the authored corpus. |

The general rule these three share: **a machine-plane write inside `.cog/` is
admissible only where it is (a) named in this table, (b) derived or runtime
rather than authored, and (c) outside both ledger strands.** Anything else is a
lint failure (§6.4). This is a narrower claim than "the cell is empty" and it
is the one the code and the corpus actually support.

Note that this is the same conflict-log entry (ADR-065 §7 versus RFC-033's path
layering) that sits immediately above the machine-tier bullet §5.9 claims to
close. The second draft decided it silently. It is decided out loud here.

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
  (keeping only a `.gitkeep`), so the strand travels with the directory and by
  BEP replication, not necessarily through git history. The invariants in this
  RFC rest on **who holds the write handle**, which is unaffected either way;
  but "git-settled" is a claim about durability that this RFC should not make
  on the ledger's behalf. Whether the seat(local) strand *ought* to be
  git-settled is a separate question and is not decided here.

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
> strand — `.cog/ledger/**`. This says nothing about the rest of `.cog/`, which
> is governed by the per-artifact table in §5.1 and the named exceptions in
> §5.1b.

Two properties make that invariant real rather than declared:

1. **It is enforced by ACL, not convention**, and it is enforced **in both
   directions**. §5.8 states one direction (no seat(local)-writable path under
   the machine root). The direction §5.4's attestation actually depends on is
   the other one — **no machine-plane-writable path inside `.cog/ledger/`** —
   and the second draft lints neither it nor its ceiling. §6.4 now carries both.
2. **The privilege ceiling from §3.1 applies here or the ACL is decorative.** An
   ACL that says "the service cannot write the seat(local) strand" means
   nothing if the service runs as root, as SYSTEM, or with `SeBackupPrivilege`
   / `SeRestorePrivilege` / `SeTakeOwnershipPrivilege` / `CAP_DAC_OVERRIDE`.
   The machine-plane identity must hold **no privilege that overrides the
   seat(local) strand's ACL**. That is why §3.1 strikes root on macOS, and it
   is the reason the Windows virtual account was chosen for being *strictly
   weaker than SYSTEM* rather than for being convenient.

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

Adding a paired kind, changing a complement, changing a direction, changing a
window, or lowering an otherness tier is a **version bump that raises the
library floor**. It is not a code change that ships quietly with a refactor.
The library is the boundary's vocabulary, and minting vocabulary is a security
act: everything the door will ever admit is enumerated there.

**But the library governs the tiers, so the library must govern itself.** The
second draft named minting "a security act" and then attached no authority to
it — which left the tier system strictly weaker than the thing that edits it. A
library revision needing no operator tier could lower an identity-grade kind
from **operator** to **cross-plane**, and §5.6's protection of a NodeID change
would evaporate by version bump. Named gate, decided:

> **Any library change that WEAKENS a constraint requires operator-tier
> attestation, recorded as a paired entry on the machine strand.** Weakening
> means: lowering an otherness tier; widening a window; removing a kind's
> requirement (a complement, a direction, a declared field); or removing a kind
> whose absence would silently re-admit an interaction under a looser rule.
> Floor-raising changes — adding a kind, raising a tier, narrowing a window,
> adding a required field — remain ordinary version bumps.

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
  machine-plane write inside the workspace, together with the ADR-094 lineage
  projections and the ADR-097 / ADR-098 projection targets driven by ADR-095's
  reconcile loop. The second draft asserted a partition that contradicted all
  five and cited none of them.
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
| **no auxiliary IPC endpoint** in the kernel's namespace besides the declared door | §4.1 |
| **service-lifecycle and register routes are not served on the loopback listener** — `/v1/services/*` and `POST /v1/services/register` reachable on the door only, and only against a machine-plane-scoped grant | §4.1 item 2 |
| **no gate-exempt grant read** — no route returns a node-root credential without passing the grant gate; the node-root credential is not readable from any seat(local)-readable path | §4.1 item 3 |
| **protocol floor satisfied** (named check, distinct from skew) | §4.3 |
| **no machine-plane security key readable from a seat(local)-writable or BEP-replicated path** — no `Enable*` gate, `BindAddr`, or `WriteRouteGrantAuthDisabled` resolvable from `.cog/`; a machine key found in `kernel.yaml` fails the lint rather than being shadowed | §5.1a |
| **no seat(local)-writable path under the machine root** | §5.8 |
| **no machine-plane-writable path inside `.cog/ledger/`** — the direction two-party attestation rests on | §5.2, §5.8 |
| **privilege ceiling** — machine-plane identity is not root / SYSTEM / Administrators and holds no `SeBackupPrivilege`, `SeRestorePrivilege`, `SeTakeOwnershipPrivilege`, or `CAP_DAC_OVERRIDE` | §3.1, §5.2 |
| **machine-plane writes inside `.cog/` are exactly the declared set** — daemon lifecycle state and the declared projection targets, nothing else; and `.cog/run/` is excluded from git and from BEP replication | §5.1b |
| **capability envelopes are machine-plane-stored and not seat(local)-writable** — no envelope resolvable from `.cog/`, no seat(local) principal holding write on its own envelope | §4.6 |
| **service image path integrity** — write-ACL on the image, unquoted-path check | §5.8 |
| **self-update signature gate intact** — no unsigned-application path reachable from the reconcile provider | §5.8 |
| **NodeID byte-identical** across the rung-2 → rung-3 transition, **and BEP DeviceID / cert bytes identical** — the derived value alone goes green while the anchor forks | §7.2 |
| unpaired-past-window entries, **classified overdue vs. unexpressed vs. stalled**, with the vacancy call **paired to the occupancy signal** — never inferred from the counterpart strand's own silence | §5.4 |
| declared otherness tier satisfied per paired kind | §5.6 |
| **library weakenings carry operator-tier attestation** — every tier lowering, window widening, or requirement removal has its paired attestation entry on the machine strand | §5.7 |
| per-platform invariant checks: no UI surface, no user-profile read, no user credentials held — one enforceable check per platform, mirroring the S4U rule's specificity | §3.1 |

Two honesty notes attached to the surface:

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

### 7.1 Rung 3 is four operations, not one

The first draft conjoined *service registered*, *machine state migrated*, *boot
task deleted*, and *agent demoted* into a single rung with no intermediate
checkpoint and no way back. An operator who lands mid-rung at 2am with a dead
service, a deleted boot task, and half-migrated state has no doctor-declared
route home. Sequenced:

| Sub-rung | Gate predicate | Rollback |
|---|---|---|
| **3a. Service registered, boot task retained** | service registered and running under the expected identity; boot task still present and healthy | unregister the service; rung 2 is untouched |
| **3b. Cert relocated, NodeID verified** | NodeID byte-identical to its rung-2 value **and BEP DeviceID / cert bytes identical** (§7.2) — the NodeID check alone measures the derived value, not the anchor | restore cert dir from the retained copy; both checks re-verified |
| **3c. Machine state migrated, old copies retained read-only** | machine state present and ACL-correct at the new root; old copies still readable; **dual-valid window open** | repoint at the old root; both copies are still valid |
| **3d. Boot task deleted** | N consecutive healthy service checks passed while 3c's dual-valid window held | `--fix` re-creates the rung-2 boot task and repoints state at the old root |
| **3e. Agent demoted to session duties, credential distribution moved to the door** | agent connected to the door, executing user-plane requests; the node-root credential is served from the machine root over the door and the gate-exempt GET is gone (§4.1 item 3); named local consumers repointed at the agent | agent reverts to standalone kernel supervision and to the rung-2 credential path |

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
protocol whose message set is ClusterConfig / Index / Request / Response. It
carries **no envelope, no receipt, and no durable two-party settlement by
complement** — the words do not appear in the package.

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
per-request seat binding are **one question, not two** — and per §4.6 the
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
not after.

---

## 10. References

- myrgic/cogos#586 — anchor issue (feature request, incident record)
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
- ADR-094 — lineage observatory — the projections the reconciler writes (§5.1b)
- ADR-095 — daemon reconcile-loop driver — the loop that runs those reconcilers
- ADR-097 — memory projection reconciler — kernel-run writer into `.cog/mem/`
- ADR-098 — skill projection reconciler — kernel-run writer into `.cog/skills/`
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
  `internal/engine/serve_cors.go`,
  `internal/engine/boot_node_root_grant.go`,
  `internal/engine/serve_identity_grants.go`,
  `internal/engine/config.go`, `internal/engine/process.go`,
  `internal/engine/bep_engine.go`, `internal/engine/cli_selfupdate.go`,
  `internal/engine/daemon_lifecycle.go`,
  `internal/engine/projection_reconciler.go`,
  `internal/engine/decision_lineage_reconciler.go`,
  `internal/engine/service_supervisor.go`,
  `internal/engine/service_supervisor_stub.go`,
  `internal/engine/node_identity.go`, `internal/engine/ledger.go`,
  `pkg/substrate/bep/tls.go`, `pkg/substrate/bep/config.go`
