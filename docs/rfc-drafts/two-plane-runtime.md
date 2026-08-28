---
type: rfc-draft
title: "Two-plane runtime: machine-plane service + user-plane session agent"
status: draft
closes: "myrgic/cogos#586"
relates: ["myrgic/cogos#101", "ADR-121"]
scope: "Design only — no implementation in this document"
---

# RFC Draft: Two-plane runtime

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

The two planes communicate over a single declared IPC door. The service never
impersonates the user; it requests user-plane work from the agent, which
executes in its own honest identity (§4.3, "RPC inversion").

The split makes **node vs. seat** structural rather than inferred: service up
= node present on the constellation; agent up = operator seated.

---

## 2. Motivation

### 2.1 The incident (2026-08-28, Eclipse)

On the Eclipse node (Windows), `cogos-kernel` and a companion service were
supervised as Scheduled Tasks with **S4U logon** — "run whether user is logged
on or not," no stored password. S4U logon has a documented side effect: it
purges the user's saved Domain Password credentials from Credential Manager
around interactive logon. The operator's persistent SMB drive mapping broke
silently; the DPAPI credential blob was rewritten the same second as the first
SMB guest-rejection event. Diagnosis cost an afternoon. For an ordinary user
the experience would read: *"I installed CogOS and my network drives stopped
working."*

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
accommodation.

---

## 3. Design overview

```
┌────────────────────────── machine plane ──────────────────────────┐
│ cogos service run                                                 │
│ identity: NT SERVICE\cogos / launchd daemon / systemd system unit │
│ lifecycle: boots with the node, SCM-native recovery               │
│ owns: HTTP listener, BEP membership + heartbeat, reconcile loop,  │
│       self-update, machine-scoped workspace state                 │
└──────────────────────────────┬────────────────────────────────────┘
                one declared IPC door (§4), ACL'd to the seat
┌──────────────────────────────┴──────────── user plane ────────────┐
│ cogos agent run                                                   │
│ identity: the operator, inside their logon session                │
│ lifecycle: starts at logon, ends at logoff                        │
│ owns: lane dispatch against per-user OAuth stores, DPAPI /        │
│       Credential Manager, HKCU, mapped drives, notifications      │
└───────────────────────────────────────────────────────────────────┘
```

Both planes are the same binary (ADR-121 single-binary consolidation), role
selected by subcommand — the `tailscaled`/`tailscale` pattern. The
ServiceSupervisor implementation slots in behind the existing
`service_supervisor_stub` seam in the services subsystem (#101), so drivers
and the doctor observe both planes through the supervisor interface they
already speak.

### 3.1 Machine plane

Runs under the least-privileged identity that can do the job — on Windows a
**virtual service account** (`NT SERVICE\cogos`): created automatically at
service registration, no password exists to store or rotate, own SID for
ACLs, strictly weaker than SYSTEM. On macOS, a LaunchDaemon (root or a
dedicated `_cogos` user); on Linux, a systemd system unit with a dedicated
user.

The service never presents UI, never reads a user profile, and never holds
user credentials. Session-0 isolation on Windows enforces the first; this RFC
adopts all three as invariants on every platform.

Supervision moves from our hand-rolled watchdog (repeating trigger +
IgnoreNew) to the platform's native recovery: SCM failure actions, launchd
KeepAlive, systemd `Restart=`. Strict upgrade; the watchdog pattern is
retired where the service plane exists.

### 3.2 User plane

The agent is the current per-user supervision shape demoted to a thin client:
logon-triggered task / LaunchAgent / systemd user unit, no elevation, no
password, registered without any prompt. It connects to the service's door on
start, announces the seat, and executes user-plane work the service requests.
Multiple sequential logons produce one agent per session; the seat is
occupied while at least one live agent holds the door.

---

## 4. The boundary

### 4.1 One declared door

A single named IPC endpoint per node — named pipe on Windows, unix socket
elsewhere — with an ACL granting connect rights to the operator's SID/uid
only. The door is *declared*: it appears in the node's doctor surface by name,
with its ACL and protocol version observable. There is exactly one; auxiliary
channels are prohibited by convention and lintable by the doctor.

### 4.2 Versioned protocol

Self-update now updates two cooperating processes, so the wire format carries
an explicit protocol version. Rule: the service must tolerate an agent one
version behind (updates restart the service first, agents reconnect and are
told to restart when convenient). A version mismatch beyond the skew window is
a doctor-visible degraded state, not a crash.

### 4.3 RPC inversion, not impersonation

The load-bearing invariant. When the kernel needs user-plane work — dispatch
a lane against the operator's OAuth tokens, read a DPAPI-protected secret,
touch a mapped drive — it does **not** borrow the user's token.
`ImpersonateNamedPipeClient` and its cousins are rejected by design:
impersonation tokens are partial logon sessions (DPAPI and network
authentication degrade — the same family of half-identity that produced the
S4U incident), and an identity borrowed invisibly is precisely what the
declared-door discipline exists to prevent.

Instead the service **enqueues a request**; the agent picks it up, executes
in its own session with its own identity, and returns a digest. The agent is
the seat's hands. If no agent is connected, user-plane requests queue or fail
fast per request policy — "lanes only dispatch while the operator is seated"
becomes a structural property rather than a policy check.

### 4.4 The door is a privilege boundary

Every agent→service message is a request from a lesser identity to a
machine-plane process. Message handlers validate as if internet-facing:
authenticated peer (SID check on connect), schema-validated payloads, no
path/registry values accepted verbatim. The kernel write-route CSRF hardening
is the precedent; its lesson applies to this surface from day one.

---

## 5. State partition

The workspace tree today lives entirely under the user profile. The split
requires deciding, file by file, which state is **body** (machine plane) and
which is **seat** (user plane):

| State | Plane | Rationale |
|---|---|---|
| node identity, BEP membership, peer table | machine | exists when nobody is seated |
| reconcile ledger, registry, engine state | machine | the reconcile loop runs on the service |
| kernel binary + self-update state | machine | updates must not require logon |
| per-user OAuth / token stores | user | DPAPI/keychain-bound, identity-scoped |
| seat config, session state, harness homes | user | meaningful only with an operator |
| cogdoc corpus / memory | **open** | see §8 Q1 |

Machine-plane state moves to a machine-scoped root (`C:\ProgramData\CogOS`,
`/Library/Application Support/CogOS`, `/var/lib/cogos`) owned by the service
account, with read grants to seats as the visibility policy dictates. This is
a real migration with ACL design, staged per node by the doctor (§7).

---

## 6. Install-time story

- **Default tier — no prompts at all.** Agent-plane registration only, in
  user context. No UAC, no password, package-manager friendly. Node is up
  while the operator is logged in. This is today's behavior, now named.
- **Node mode — one UAC prompt, zero credentials.** Opt-in for constellation
  members that must be reachable pre-logon. Elevation registers the service
  under the virtual account. Nothing is typed, stored, purged, or rotated.
  The password dialog exits the install flow permanently.
- **Hard rule: no S4U, ever.** Encoded in the schtasks driver as a refusal,
  and in the doctor as a lint: `no scheduled task with LogonType=S4U exists
  for the kernel or any cogos-managed service`.

The doctor declares the whole surface as observable state: service exists and
running under the expected identity; agent task registered and healthy; door
present with correct ACL and protocol version; no S4U tasks. `--fix`
reconciles what it can without credentials — which, in this architecture, is
everything.

---

## 7. Migration ladder

Per node, doctor-driven, each rung a settled observable state:

1. **Agent-only** (today's default) — user-plane supervision, no boot
   presence.
2. **Stored-password interim** (Eclipse today) — boot task with Password
   logon. Acceptable stopgap; known rotation caveat; doctor detects the
   `0x8007052E` logon-failure signature and prompts re-entry.
3. **Two-plane** — service registered, machine state migrated, boot task
   deleted, agent demoted to session duties.

Eclipse is the first migration target for rung 3: it is the node that
motivated the design, it currently sits on rung 2, and its schtasks driver
work (#101) already carries the supervisor seam.

---

## 8. Open questions

1. **Corpus placement.** Is the cogdoc corpus / memory machine state served
   to seats, or seat state the service indexes on request? The `.cog`
   mind/body boundary suggests memory is seat-invariant — which argues
   machine-plane storage with seat-scoped visibility — but that makes the
   service a reader of formerly user-private data and needs the visibility
   policy to say so explicitly.
2. **Multi-seat semantics.** One service, N agents: does a node admit
   multiple simultaneously-seated operators, and is the capability envelope
   per-seat or per-node? (Fast User Switching makes this reachable on a
   family machine even before true multi-user nodes.)
3. **Request queue durability.** Do user-plane requests survive a service
   restart (ledger-backed queue) or is the queue ephemeral with idempotent
   re-issue? Ledger-first suggests the former.
4. **Linux lingering.** systemd's `loginctl enable-linger` gives boot-time
   user services natively — does the Linux backend still split, for symmetry
   and least privilege, or is linger an accepted single-plane mode there?
5. **Naming.** "service"/"agent" are placeholders. The constellation
   vocabulary (node/seat/body) may want these named from the dome taxonomy
   instead.

---

## 9. References

- myrgic/cogos#586 — anchor issue (feature request, incident record)
- myrgic/cogos#101 — services extensibility; `service_supervisor_stub` seam
- ADR-121 — single-binary consolidation
- Kernel write-route CSRF hardening (#551) — precedent for §4.4
- Incident notes: S4U credential purge, Eclipse, 2026-08-28
