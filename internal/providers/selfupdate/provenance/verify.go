// Package provenance verifies the Sigstore keyless signature that
// .github/workflows/release.yml produces over a release's checksums.txt.
//
// # WHY THIS EXISTS
//
// A SHA-256 checksum proves an artifact arrived intact. It does not prove the
// artifact came from this repository's CI, because the checksums file is
// fetched over the SAME unauthenticated channel as the binary it describes — an
// attacker who can substitute both produces a self-consistent pair that passes
// every checksum test by construction. Only a signature binding checksums.txt
// to the release workflow's identity proves provenance. See
// docs/release-signing.md.
//
// # WHY NOT THE COSIGN LIBRARIES, AND WHY NOT A COSIGN BINARY
//
// Measured on this tree (go 1.25, darwin/arm64):
//
//	current kernel dependency tree      113 modules      binary 28.2 MB
//	+ github.com/sigstore/cosign/v2     246 modules      probe  19.5 MB
//	+ github.com/sigstore/sigstore-go   185 modules      probe  17.1 MB
//	this package                          0 modules      ~0
//
// cosign's tree carries the AWS, Azure, GCP-KMS, Vault and Kubernetes SDKs (42
// modules of cloud/container SDK alone). Every one of those exists to support
// SIGNING with a cloud KMS — a capability a verifier never uses — and every one
// is a new supply-chain entry point into the very binary this code is meant to
// harden. Roughly doubling the kernel's dependency surface to close a
// supply-chain hole is a poor trade.
//
// Shelling out to a cosign binary was rejected for a different reason: it is an
// external runtime dependency, and on a fail-closed default a host without
// cosign installed simply stops receiving updates. (cosign is not installed on
// the reference darwin host.) It also begs the question — verifying the
// authenticity of the verifier is the same problem one level down.
//
// So this package implements exactly the checks the release pipeline's own
// `cosign verify-blob` invocation performs, against a pinned trust root, using
// only crypto/x509, crypto/ecdsa and encoding/asn1 from the standard library.
// The composition is ~200 lines and auditable in one sitting; no primitive here
// is hand-rolled.
//
// # WHAT IS PROVEN, AND WHAT IS NOT
//
// Proven: checksums.txt was signed by a key whose Fulcio-issued certificate
// chains to the pinned Sigstore root, whose SAN is exactly this repository's
// release.yml at exactly the target tag, and whose OIDC issuer is GitHub
// Actions. Combined with the caller's duty to extract the expected digest ONLY
// from verified bytes (see VerifiedChecksums), that extends transitively to the
// downloaded binary.
//
// Not proven: inclusion in the Rekor transparency log, and the embedded SCT is
// not checked. Both defend against a compromised Fulcio rather than against a
// tampered distribution channel, and docs/release-signing.md already scopes
// Sigstore-infrastructure compromise out. See the CERTIFICATE VALIDITY note on
// verifyChain for the consequence of that choice.
package provenance

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors. Callers distinguish "the release carries no signature at
// all" (a backward-compatibility condition — releases published before
// FirstSignedRelease) from "a signature is present and it did not verify" (an
// attack or a broken pipeline). The two warrant different operator messages and,
// on the manual path, different escape hatches.
var (
	// ErrNoSignature means the signature and/or certificate asset is absent
	// from the release. Unsigned-by-omission, not invalid.
	ErrNoSignature = errors.New("release carries no Sigstore signature")

	// ErrVerification means signature material was present and failed a check.
	// This is never recoverable by retry and must never be downgraded to a
	// warning on the automatic path.
	ErrVerification = errors.New("signature verification failed")
)

// FirstSignedRelease is the first tag whose release job produced
// checksums.txt.sig / checksums.txt.pem (added by PR #456, commit 39d63c2).
// Releases at or before this boundary legitimately carry no signature; the
// updater uses this to tell "old release" apart from "signature stripped".
const FirstSignedRelease = "v0.16.20"

// Sigstore certificate extension OIDs (github.com/sigstore/fulcio,
// docs/oid-info.md). Only the ones this policy pins are listed.
var (
	// oidIssuerLegacy (1.1) holds the OIDC issuer as a bare UTF-8 string.
	oidIssuerLegacy = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	// oidSourceRepo (1.5) holds "owner/name" of the repository that ran the job.
	oidSourceRepo = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 5}
	// oidIssuerV2 (1.8) is the DER-wrapped UTF8String form of the OIDC issuer.
	oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	// oidRunnerEnv (1.11) is "github-hosted" or "self-hosted".
	oidRunnerEnv = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 11}
)

// GitHubOIDCIssuer is the only OIDC issuer this policy accepts.
const GitHubOIDCIssuer = "https://token.actions.githubusercontent.com"

// maxCertLifetime bounds the leaf's validity window. Fulcio issues 10-minute
// certificates; anything materially longer is not a Fulcio ephemeral cert even
// if it chains, so this is a cheap shape check against a mis-issued long-lived
// cert under the same root.
const maxCertLifetime = 24 * time.Hour

// maxClockSkew bounds how far into the future a certificate may claim to have
// been issued, so a host with a badly-wrong clock cannot be walked forward.
const maxClockSkew = 24 * time.Hour

// Policy is the pinned trust root plus the identity constraints a release
// signature must satisfy. Zero-value fields are filled by PolicyForRelease.
type Policy struct {
	// Identity is the exact SAN URI required of the signing certificate.
	//
	// Pinning the EXACT identity (including @refs/tags/<tag>) rather than the
	// repository-and-workflow regexp that docs/release-signing.md gives human
	// consumers is a deliberate strengthening. A human verifying by hand does
	// not know which tag they "should" be looking at, so the docs must use a
	// regexp. The updater always knows the tag it resolved, so it can refuse a
	// signature that is perfectly valid for a DIFFERENT release — closing a
	// cross-release replay in which an attacker serves release A's genuine
	// checksums.txt + signature in response to a request for release B.
	Identity string

	// IdentityPattern is the structural fallback used when Identity is empty
	// (no known target tag). Never both.
	IdentityPattern *regexp.Regexp

	// OIDCIssuer is the required value of the issuer extension.
	OIDCIssuer string

	// SourceRepository is the required value of extension 1.5 ("owner/name").
	// Defence in depth behind the SAN pin: if the SAN pattern is ever loosened
	// by mistake, this still refuses a cert minted by another repository.
	SourceRepository string

	// Roots / Intermediates default to the embedded Fulcio pool. Tests inject a
	// synthetic CA so that "valid signature, wrong identity" is constructible.
	Roots         *x509.CertPool
	Intermediates *x509.CertPool

	// Now supplies the current time (clock-skew bound only). Defaults to time.Now.
	Now func() time.Time
}

// PolicyForRelease builds the production policy for a given repo and tag.
// repo is "owner/name"; tag is the exact release tag (e.g. "v0.16.26").
//
// Passing an empty tag falls back to the repository-and-workflow pattern, which
// is strictly weaker; the updater always has a tag and should always pass it.
func PolicyForRelease(repo, tag string) (*Policy, error) {
	roots, intermediates, err := embeddedFulcioPools()
	if err != nil {
		return nil, err
	}
	p := &Policy{
		OIDCIssuer:       GitHubOIDCIssuer,
		SourceRepository: repo,
		Roots:            roots,
		Intermediates:    intermediates,
	}
	if tag != "" {
		p.Identity = fmt.Sprintf("https://github.com/%s/.github/workflows/release.yml@refs/tags/%s", repo, tag)
	} else {
		p.IdentityPattern = regexp.MustCompile(
			`^https://github\.com/` + regexp.QuoteMeta(repo) + `/\.github/workflows/release\.yml@.*$`)
	}
	return p, nil
}

// Attestation is what a successful verification learned from the certificate.
// It is logged so the operator can see WHICH workflow run produced the update
// that landed on their machine, not merely that some check passed.
type Attestation struct {
	Identity     string // SAN URI: repo + workflow + ref
	OIDCIssuer   string
	SourceRepo   string
	RunnerEnv    string // "github-hosted" | "self-hosted"
	SignedAt     time.Time
	CertNotAfter time.Time
}

// String renders a one-line log form.
func (a *Attestation) String() string {
	return fmt.Sprintf("identity=%s issuer=%s repo=%s runner=%s signed_at=%s",
		a.Identity, a.OIDCIssuer, a.SourceRepo, a.RunnerEnv, a.SignedAt.UTC().Format(time.RFC3339))
}

// VerifyBlob checks sig over blob using cert, subject to p.
//
// blob    — the exact bytes of checksums.txt as fetched.
// certRaw — contents of checksums.txt.pem.
// sigRaw  — contents of checksums.txt.sig.
//
// Every failure wraps ErrVerification. Empty cert or sig yields ErrNoSignature
// so the caller can apply the unsigned-release policy instead.
func (p *Policy) VerifyBlob(blob, certRaw, sigRaw []byte) (*Attestation, error) {
	if len(strings.TrimSpace(string(certRaw))) == 0 || len(strings.TrimSpace(string(sigRaw))) == 0 {
		return nil, ErrNoSignature
	}

	cert, err := ParseCertificate(certRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: parse certificate: %v", ErrVerification, err)
	}
	if err := p.verifyChain(cert); err != nil {
		return nil, err
	}
	att, err := p.verifyIdentity(cert)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(cert, blob, sigRaw); err != nil {
		return nil, err
	}
	return att, nil
}

// ParseCertificate decodes checksums.txt.pem.
//
// cosign's --output-certificate writes base64(PEM), NOT bare PEM — the file
// begins "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t", which is base64 of
// "-----BEGIN CERTIFICATE-----". A naive pem.Decode on the raw bytes returns
// nil. Both encodings are accepted here so the verifier keeps working if cosign
// ever changes that, and so hand-assembled fixtures behave.
func ParseCertificate(raw []byte) (*x509.Certificate, error) {
	b := raw
	if !strings.Contains(string(raw), "BEGIN CERTIFICATE") {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("certificate is neither PEM nor base64(PEM): %w", err)
		}
		b = dec
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE block in certificate file")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// verifyChain checks the leaf against the pinned Fulcio root.
//
// CERTIFICATE VALIDITY — the load-bearing subtlety.
//
// Fulcio issues certificates valid for TEN MINUTES. Every release older than
// that has an expired leaf, so verifying at time.Now() fails for every release
// the updater will ever be asked to install. (Empirically: the v0.16.26 leaf is
// valid 18:09:13–18:19:13 on 2026-07-31.) cosign resolves this by anchoring
// verification at a trusted timestamp obtained from the Rekor transparency log.
//
// This verifier instead anchors at the certificate's own NotBefore, and does
// NOT consult Rekor. The reasoning:
//
//   - The expiry window does not gate forgery. An attacker cannot produce a
//     leaf with our SAN under the pinned root at ANY timestamp, because that
//     requires Fulcio to sign it, which requires a GitHub OIDC token for
//     myrgic/cogos's release.yml. Chain + identity are what stop the attack in
//     the threat model docs/release-signing.md states.
//   - What the short window actually buys is bounding the damage of a LEAKED
//     ephemeral signing key: with a Rekor time anchor, a key exfiltrated from a
//     past runner cannot be used to sign new content later. Accepting that
//     residual risk requires compromise of a GitHub-hosted runner during a
//     ten-minute window — the same runner compromise that
//     docs/release-signing.md already declares out of scope.
//
// The residual is therefore explicit and bounded, not hidden. Two cheap
// guards below narrow it further, and issue #454's follow-up (emit a Sigstore
// bundle from the release job, verify its Rekor inclusion offline) closes it
// without any new dependency.
func (p *Policy) verifyChain(cert *x509.Certificate) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}

	// Guard 1 — a certificate must not claim to be issued in the future beyond
	// tolerable skew, so a wrong host clock cannot be exploited.
	if cert.NotBefore.After(now().Add(maxClockSkew)) {
		return fmt.Errorf("%w: certificate NotBefore %s is more than %s in the future",
			ErrVerification, cert.NotBefore.UTC().Format(time.RFC3339), maxClockSkew)
	}

	// Guard 2 — shape check. Fulcio ephemeral certs live ~10 minutes; a
	// long-lived leaf under the same root is not one and is refused.
	if lifetime := cert.NotAfter.Sub(cert.NotBefore); lifetime > maxCertLifetime {
		return fmt.Errorf("%w: certificate lifetime %s exceeds the %s Fulcio ephemeral bound",
			ErrVerification, lifetime, maxCertLifetime)
	}

	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:         p.Roots,
		Intermediates: p.Intermediates,
		CurrentTime:   cert.NotBefore, // see the doc comment above
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		return fmt.Errorf("%w: certificate does not chain to the pinned Sigstore root: %v", ErrVerification, err)
	}
	return nil
}

// verifyIdentity enforces the SAN, OIDC-issuer and source-repository pins.
func (p *Policy) verifyIdentity(cert *x509.Certificate) (*Attestation, error) {
	// SAN. Fulcio puts the workflow identity in a URI SAN; Subject is empty.
	var matched string
	for _, u := range cert.URIs {
		s := u.String()
		switch {
		case p.Identity != "":
			if s == p.Identity {
				matched = s
			}
		case p.IdentityPattern != nil:
			if p.IdentityPattern.MatchString(s) {
				matched = s
			}
		}
		if matched != "" {
			break
		}
	}
	if matched == "" {
		want := p.Identity
		if want == "" && p.IdentityPattern != nil {
			want = p.IdentityPattern.String()
		}
		got := make([]string, 0, len(cert.URIs))
		for _, u := range cert.URIs {
			got = append(got, u.String())
		}
		return nil, fmt.Errorf("%w: certificate identity %v does not match required %q",
			ErrVerification, got, want)
	}

	// OIDC issuer. Prefer the DER-wrapped v2 extension, fall back to legacy.
	issuer, ok := certExtension(cert, oidIssuerV2, true)
	if !ok {
		issuer, ok = certExtension(cert, oidIssuerLegacy, false)
	}
	if !ok {
		return nil, fmt.Errorf("%w: certificate carries no Sigstore OIDC-issuer extension", ErrVerification)
	}
	if issuer != p.OIDCIssuer {
		return nil, fmt.Errorf("%w: OIDC issuer %q does not match required %q",
			ErrVerification, issuer, p.OIDCIssuer)
	}

	// Source repository (defence in depth behind the SAN pin).
	sourceRepo, _ := certExtension(cert, oidSourceRepo, false)
	if p.SourceRepository != "" && sourceRepo != p.SourceRepository {
		return nil, fmt.Errorf("%w: certificate source repository %q does not match required %q",
			ErrVerification, sourceRepo, p.SourceRepository)
	}

	runnerEnv, _ := certExtension(cert, oidRunnerEnv, true)
	return &Attestation{
		Identity:     matched,
		OIDCIssuer:   issuer,
		SourceRepo:   sourceRepo,
		RunnerEnv:    runnerEnv,
		SignedAt:     cert.NotBefore,
		CertNotAfter: cert.NotAfter,
	}, nil
}

// verifySignature checks the detached ECDSA signature over blob.
//
// cosign sign-blob produces an ASN.1 DER ECDSA signature over SHA-256 of the
// blob, base64-encoded. Fulcio keyless issues P-256 keys. A non-ECDSA key is
// refused explicitly rather than silently skipped.
func verifySignature(cert *x509.Certificate, blob, sigRaw []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil {
		return fmt.Errorf("%w: signature is not valid base64: %v", ErrVerification, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: unsupported certificate public key type %T (want ECDSA)", ErrVerification, cert.PublicKey)
	}
	digest := sha256.Sum256(blob)
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return fmt.Errorf("%w: signature does not match checksums.txt under the certified key", ErrVerification)
	}
	return nil
}

// certExtension returns the value of a certificate extension. When der is true
// the value is an ASN.1 UTF8String wrapper; otherwise it is a bare UTF-8 string.
func certExtension(cert *x509.Certificate, oid asn1.ObjectIdentifier, der bool) (string, bool) {
	for _, e := range cert.Extensions {
		if !e.Id.Equal(oid) {
			continue
		}
		if !der {
			return string(e.Value), true
		}
		var s string
		if _, err := asn1.Unmarshal(e.Value, &s); err != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

// VerifiedChecksums couples verification to consumption so the two cannot drift
// apart.
//
// This is the whole point of the exercise and the reason it is not merely
// "call VerifyBlob somewhere". The signature covers checksums.txt; the binary
// is covered only TRANSITIVELY, via the digest listed inside that file. If a
// caller verifies the signature but then parses the digest out of a separately
// held copy of the checksums text — or parses first and verifies afterwards —
// the chain is broken and the verification is decorative.
//
// Callers therefore receive the trusted bytes only as the return value of a
// successful verification, and must extract the expected digest from THOSE
// bytes. The updater has no other path to a checksum.
func (p *Policy) VerifiedChecksums(blob, certRaw, sigRaw []byte) (string, *Attestation, error) {
	att, err := p.VerifyBlob(blob, certRaw, sigRaw)
	if err != nil {
		return "", nil, err
	}
	return string(blob), att, nil
}
