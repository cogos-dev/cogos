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
- **What this does *not* cover:** compromise of the GitHub Actions runner
  itself during a release build (a signature only attests "this identity
  signed this content," not "this identity's build environment was
  uncompromised"), and compromise of the Sigstore public-good infrastructure
  itself (Fulcio/Rekor), both of which are out of scope for a repository-level
  control. It also does not yet protect the `cogos self-update` code path at
  the point of *applying* an update — see the tracking issue below.
- **Windows binaries remain unsigned by a traditional code-signing
  certificate** (see `docs/RELEASING.md`'s SmartScreen note) — Sigstore
  signing is a separate, additional control and does not change that
  SmartScreen will still warn on first run.

## Known gap: `cogos self-update` does not verify this signature yet

This change adds signing to the release pipeline only. The kernel's
`cogos self-update` command (`internal/engine/cli_selfupdate_unix.go`)
currently verifies the downloaded binary's SHA-256 against `checksums.txt`
(`verifyChecksum` / `verifyChecksumAgainst`) but does not verify a Sigstore
signature over that checksums file before trusting it.

Tracked in a follow-up issue: **self-update: verify cosign signature before
applying update**. Closing that gap completes the trust chain end-to-end —
today, a party that could tamper with both the binary and `checksums.txt` in
transit (without access to this repo's OIDC-derived signing identity) could
still defeat the *existing* checksum-only verification in self-update, even
though a human following the manual steps above would catch the missing/
invalid signature. Manual installs are covered as of this document; the
self-update code path is not, yet.
