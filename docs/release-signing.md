# Release signing

Starting with the release that ships this document, every GitHub release of
`cogos` carries a [Sigstore](https://www.sigstore.dev/) keyless signature over
`checksums.txt`, alongside the existing SHA-256 checksums.

This closes the gap noted in the v1.0 alignment ledger (`L12-RELEASE-INTEGRITY`):
the GitHub release channel is the sole trust path onto boundary-isolated
consumer nodes, and the kernel's `cogos self-update` command downloads and
installs binaries from that channel onto a running system. A checksum alone
protects against corruption in transit; it does not prove the artifact came
from this repository's CI rather than a compromised or spoofed release.

## What is signed

- **`checksums.txt`** — the SHA-256 digest of every released binary
  (`cogos-<os>-<arch>[.exe]`), one line per artifact.
- The signature is **not** applied to each binary individually. Signing the
  checksums file is sufficient: the file's own content already binds every
  binary's digest, so one signature transitively covers every artifact listed
  in it. A binary that doesn't match its line in a validly-signed
  `checksums.txt` fails verification just as surely as if it had been signed
  directly.

Two new files are uploaded alongside `checksums.txt` on every release:

- **`checksums.txt.sig`** — the Sigstore signature (base64-encoded).
- **`checksums.txt.pem`** — the short-lived X.509 signing certificate issued
  by Sigstore's Fulcio CA for that specific CI run.

## How a consumer verifies

Requires [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
(`brew install cosign`, or download from the
[cosign releases page](https://github.com/sigstore/cosign/releases)).

```sh
# From the same release, download: checksums.txt, checksums.txt.sig,
# checksums.txt.pem, and the binary you want to run.

cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp "^https://github.com/myrgic/cogos/.github/workflows/release.yml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
```

A successful run prints `Verified OK`. This proves `checksums.txt` was signed
by a GitHub Actions run of *this repository's* `release.yml` workflow — not
by an arbitrary developer key, and not by a workflow in a fork.

Only after that verification passes, check the binary against the
now-trusted checksums file:

```sh
# macOS / Linux
sha256sum -c --ignore-missing checksums.txt

# or, to check one specific artifact by hand:
shasum -a 256 cogos-darwin-arm64
grep cogos-darwin-arm64 checksums.txt
```

```powershell
# Windows
Get-FileHash -Algorithm SHA256 cogos-windows-amd64.exe
# Compare the hash against the matching line in checksums.txt
```

If either step fails — signature verification or the checksum comparison —
do not run the binary.

### Why `--certificate-identity-regexp` and not a plain string

The identity Fulcio embeds in the certificate is the full workflow ref,
including the commit/tag it ran at (e.g.
`https://github.com/myrgic/cogos/.github/workflows/release.yml@refs/tags/v0.17.0`).
The regexp pins the repository and workflow path while allowing the ref
suffix to vary release to release, so the same verify command works
unmodified for every future tag.

## Trust model

- **`checksums.txt` protects against corruption and tampering in the
  distribution channel** (a bit-flipped download, a proxy that silently
  swaps a byte). This existed before this change and is unaffected by it.
- **The Sigstore signature binds `checksums.txt` to this repository's CI
  identity** — specifically, to a run of `.github/workflows/release.yml` in
  `myrgic/cogos`, authenticated via that job's short-lived GitHub OIDC token
  (`id-token: write` permission scoped to the `release` job only). It proves
  *provenance* (this file was produced by our release workflow, not planted
  by someone else), which a checksum alone cannot do.
- **Keyless means there is no long-lived private key to protect, rotate, or
  leak.** Each signing operation requests a short-lived certificate from
  Sigstore's public Fulcio certificate authority, scoped to the exact
  workflow identity for that single CI run, and records the signing event in
  Sigstore's public Rekor transparency log. No secret is stored in this
  repository's GitHub Actions configuration for this purpose.
- **The `cogos self-update` code path verifies this signature** before applying
  an update; see the section below for the posture, the pins, and the residual.
- **What this does *not* cover:** compromise of the GitHub Actions runner
  itself during a release build (a signature only attests "this identity
  signed this content," not "this identity's build environment was
  uncompromised"), and compromise of the Sigstore public-good infrastructure
  itself (Fulcio/Rekor), both of which are out of scope for a repository-level
  control.
- **Windows binaries remain unsigned by a traditional code-signing
  certificate** (see `docs/RELEASING.md`'s SmartScreen note) — Sigstore
  signing is a separate, additional control and does not change that
  SmartScreen will still warn on first run.

## `cogos self-update` verifies this signature (closes #454)

The updater performs the same verification as the manual steps above, before
it trusts any digest in `checksums.txt`.

### What runs, and in what order

`internal/engine/cli_selfupdate_unix.go`'s `runApply` sequence is:

```
resolve → fetch checksums.txt → GATE L0 verify(signature)
        → download binary → GATE L verify(sha256) → verify(version)
        → backup → atomic swap → kickstart → health poll
```

GATE L0 (`internal/engine/cli_selfupdate_provenance.go`) fetches
`checksums.txt.sig` and `checksums.txt.pem` and verifies them against a pinned
trust root. **The ordering is the security property.** `checksums.txt` is
fetched over the same unauthenticated channel as the binary, so an attacker who
substitutes both produces a self-consistent pair that GATE L accepts by
construction. Only the signature separates them, and only if it is checked
before the digest is read — the write-ahead ledger records that digest into
`kernel.toml` immediately afterwards, so verifying later would mean the ledger
had already been written from unverified bytes.

The verifier hands back the checksum text as its *return value*; the digest is
parsed from that, never from the originally fetched string. That makes
"parse before verify" a visible mistake rather than a silent one.

### What is pinned

| Pin | Value | Where |
|---|---|---|
| Trust root | Sigstore Fulcio root + intermediate (`O=sigstore.dev`), valid to 2031-10-05 | `internal/providers/selfupdate/provenance/fulcio_roots.pem`, `go:embed`ed |
| Identity | `https://github.com/<repo>/.github/workflows/release.yml@refs/tags/<exact target tag>` | derived at runtime from config `repo` + resolved tag |
| OIDC issuer | `https://token.actions.githubusercontent.com` | `provenance.GitHubOIDCIssuer` |
| Source repository | cert extension `1.3.6.1.4.1.57264.1.5` must equal config `repo` | defence in depth behind the identity pin |

The updater pins the **exact tag**, which is stronger than the
`--certificate-identity-regexp` this document gives human verifiers. A human
does not know which tag they *should* be looking at, so the docs must use a
regexp; the updater always knows the tag it resolved, so it can refuse a
signature that is genuine but belongs to a *different* release. That closes a
cross-release replay in which an attacker answers a request for `vX` with
release `vY`'s real material.

The trust root ships **inside** the binary. Fetching it at update time would
defeat the purpose: anyone able to substitute the release could substitute the
root and mint their own chain.

### Certificate validity, and what is deliberately not checked

Fulcio issues certificates valid for **ten minutes**, so every release older
than that has an expired leaf. Verification is therefore anchored at the
certificate's own `NotBefore` rather than at wall-clock time. `cosign` instead
anchors at a timestamp from the Rekor transparency log; this verifier does not
consult Rekor.

That is a deliberate, bounded trade:

- The expiry window does not gate forgery. No attacker can obtain a leaf
  carrying our SAN under the pinned root at *any* timestamp without a GitHub
  OIDC token for this repository's `release.yml`. Chain plus identity are what
  stop the attack in the threat model above.
- What the short window buys is limiting the damage of a *leaked ephemeral
  signing key* — which requires compromising a GitHub-hosted runner during a
  ten-minute window, already out of scope above.

Two cheap guards narrow the residual: a certificate claiming issuance more than
24h in the future is refused (clock-skew), and a leaf whose lifetime exceeds 24h
is refused (it is not a Fulcio ephemeral cert even if it chains).

**Not verified:** Rekor inclusion, and the embedded SCT. Closing that without
adding a dependency means emitting a Sigstore *bundle* from the release job
(`cosign sign-blob --bundle`), which embeds the Rekor entry and its inclusion
proof for offline verification. That is the natural follow-up; it needs a
release-side change and a compatibility window, so it is out of scope here.

### Why not the cosign libraries, and why not a cosign binary

Measured on this tree (go 1.25, darwin/arm64):

| Option | Modules in `go.sum` | Probe binary |
|---|---|---|
| current kernel tree | 113 | 28.2 MB (shipped) |
| `+ github.com/sigstore/cosign/v2` | 246 | 19.5 MB |
| `+ github.com/sigstore/sigstore-go` | 185 | 17.1 MB |
| this implementation | **113** (unchanged) | ~0 |

`cosign`'s tree carries the AWS, Azure, GCP-KMS, Vault and Kubernetes SDKs — 42
modules of cloud/container SDK alone, all of which exist to support *signing*
with a cloud KMS, a capability a verifier never uses. Roughly doubling the
dependency surface of the binary we are trying to harden, to close a
supply-chain hole, is a poor trade. `sigstore-go` is the purpose-built
verification-only library and is the right answer *if* full transparency-log
verification becomes a requirement; it is the documented upgrade path.

Shelling out to a `cosign` binary was rejected separately: it is an external
runtime dependency, and under a fail-closed default a host without `cosign`
installed simply stops receiving updates (`cosign` is not installed on the
reference darwin host). It also begs the question — establishing the
authenticity of the verifier is the same problem one level down.

So the verifier is ~200 lines over `crypto/x509`, `crypto/ecdsa` and
`encoding/asn1`. No primitive is hand-rolled; only the composition is ours, and
it is auditable in one sitting.

## Failure posture and the rollout

`require_signature` in the self-update config of the workspace **the daemon is
actually running against**:

```yaml
require_signature: enforce   # enforce | warn | off
```

> **Edit the right file.** The config that governs the reconcile path lives at
> `<daemon workspace root>/.cog/config/self-update.yaml`, where the workspace
> root is the one the running kernel was started with — *not* necessarily
> `~/.cog/config/self-update.yaml`, which is inert for the reconcile provider if
> the daemon's root is elsewhere. The updater's log is likewise
> `<workspace root>/.cog/run/selfupdate.log`. If unsure which root is live,
> check which of the candidate `.cog/run/selfupdate.log` files exists, or read
> `require_signature` back out of the reconcile state attributes (below).

| Value | Behaviour |
|---|---|
| `enforce` | **Fail closed.** An absent, invalid, or wrong-identity signature aborts the update. Nothing is downloaded, `kernel.toml` is not written, the running binary is untouched. |
| `warn` | Verification runs and the outcome is logged loudly; failure does not block. Transitional. |
| `off` | Verification skipped. Escape hatch; logs on every cycle so it cannot be set once and forgotten. |

**Recommended: set `enforce` now.**

### Why the default is staged rather than immediate

Flipping straight to `enforce` on upgrade is a live risk for a node running
`auto_apply: true`, because several *legitimate* conditions produce an
unverifiable-but-honest update: a release published before signing existed, a
pipeline hiccup that drops the signing step, or a missed Sigstore root rotation.
None is an attack, and none should be discovered by an operator noticing months
later that their node silently stopped updating.

So the rollout is staged, inferred from the config file rather than announced:

- **Stage 1 (this change)** — `require_signature` **absent** from an existing
  config resolves to `warn`. Verification runs on every update and every result
  is logged; nothing is blocked. This buys real telemetry from live nodes at
  zero brick risk. A config file that does not exist at all is a *fresh* install
  with no legacy to protect, and gets `enforce` immediately.
- **Stage 2 (next minor)** — the absent-key default flips to `enforce`, a
  one-line change to `defaultRequireSignature`. By then Stage 1's warnings have
  surfaced any release that would have broken.

An explicit `warn` or `off` is honoured verbatim and is never treated as unset,
so a deliberate choice does not change under the operator at Stage 2. An
unrecognised value is a hard config error, and an unreadable config resolves to
`enforce` — an updater that cannot establish what it is permitted to do must not
assume it may skip verification.

Stage 2 cannot arrive unannounced: while `require_signature` is unset, the
provider logs a deprecation notice once per `check_interval` naming both the
current posture and the one it will become. This is a hard requirement of the
staging, not a nicety — staging is only safer than an immediate flip if
operators are actually warned first.

### Seeing the posture and the last verdict

Both are projected into the `self-update.cogos` reconcile state attributes:

| Attribute | Meaning |
|---|---|
| `require_signature` | the posture in force |
| `require_signature_explicit` | `false` while the staged default applies |
| `signature_identity_repo` | present only when `signature_repo` overrides the canonical pin |
| `last_provenance` | `{tag, result, blocked, at, message}` from the last update attempt |

`result` is one of `ok`, `unsigned`, `invalid`, `transport_error`, `skipped`.
A **blocked** outcome also drives provider health to `Degraded` with the
refusal reason, so a refused update reads as a refusal rather than as a node
that is perpetually "updating to \<tag\>". Without that, a real attack under
`enforce` would present as a spinner: the detached updater is fire-and-forget,
so its verdict has to be written somewhere the daemon can read it.

### The identity pin is not the download source

`repo:` says where bytes are downloaded from. It deliberately does **not**
decide whose signature is acceptable — that is pinned to the compile-time
canonical repository. Were the two the same knob, redirecting `repo:` at a fork
would make that fork's own entirely genuine keyless signature verify, and the
updater would log `provenance OK` over it. An affirmative green light on an
attacker's binary is worse than no verification at all.

Retargeting the identity requires a separate, explicit key, and is announced on
every verification when set:

```yaml
repo: someone/cogos-fork            # where bytes come from
signature_repo: someone/cogos-fork  # whose signature is accepted (separate opt-in)
```

### Transport failures are not verification failures

A CDN 5xx, a DNS failure, or a VPN that blackholes `github.com` means the
signature material could not be *retrieved* — nothing was learned about
authenticity. The fetch is retried, and the outcome is reported as a
connectivity condition (`transport_error`), never in the language of
compromise. Under `enforce` it still stops the update, correctly, and the next
reconcile cycle retries.

### Releases published before signing existed

Tags at or before `provenance.FirstSignedRelease` carry no `.sig`/`.pem` assets
at all. The updater distinguishes a **404 on the signature asset** (unsigned
release) from a **verification failure** (signature present and wrong), because
the safe responses differ. Under `enforce`, an unsigned release is refused with
an actionable message naming both the boundary tag and the override.

The override is manual-only:

```sh
cogos self-update --to v0.15.0 --allow-unsigned
```

`--allow-unsigned` covers **only** a missing signature — never an invalid one —
and the reconcile provider never sets it. Automatic updates can never apply an
unverifiable release under `enforce`.

It is also **bounded by the boundary tag**: the flag applies only to releases at
or before `FirstSignedRelease`. For a newer tag a missing signature is
indistinguishable from one that was stripped, so the flag does not apply and the
update is refused. Otherwise an attacker who simply 404s the `.sig` and `.pem`
assets would convert the flag into a blanket provenance bypass for any operator
who had typed it once for an unrelated reason.

### Rotating the pinned root

The pin rotates by shipping a new binary, and the ordering matters because the
update path itself consumes the pin:

1. Sigstore announces a new root or intermediate.
2. **Append** it to `fulcio_roots.pem` — do not replace. The pool accepts every
   root it contains, so old and new verify simultaneously.
3. Release. Nodes on the previous binary verify that release against the old
   root (still valid), install it, and thereafter accept both.
4. Only once the fleet has moved may a retired root be removed, in a later
   release.

Appending before removing keeps a rotation from wedging the channel that
delivers it. If a rotation is ever missed and updates stall with `does not chain
to the pinned Sigstore root`, set `require_signature: warn`, let the update
through, then set it back to `enforce`.
