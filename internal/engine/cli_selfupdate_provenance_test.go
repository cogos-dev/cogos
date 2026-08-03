//go:build darwin

// cli_selfupdate_provenance_test.go — GATE L0 driven through the REAL runApply.
//
// These cases deliberately do not call the verifier directly (that is covered
// in internal/providers/selfupdate/provenance). They run the whole updater
// sequence with the network seams stubbed, and assert on the thing that
// actually matters to the operator: whether the binary on disk got replaced.
package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
	"github.com/myrgic/cogos/internal/providers/selfupdate/provenance"
)

// ─── synthetic release signer ────────────────────────────────────────────────

// releaseSigner stands in for the Sigstore keyless flow: a throwaway CA playing
// Fulcio, minting ephemeral code-signing leaves with Sigstore's certificate
// extensions. Needed because the production policy pins the real Fulcio roots,
// which nothing in a test can sign under.
type releaseSigner struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
}

func newReleaseSigner(t *testing.T) *releaseSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-fulcio"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &releaseSigner{caCert: cert, caKey: key, pool: pool}
}

// sign returns (certPEMBase64, sigBase64) over blob for the given SAN identity.
func (s *releaseSigner) sign(t *testing.T, identity, repo string, blob []byte) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	issuerDER, err := asn1.Marshal(provenance.GitHubOIDCIssuer)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-10 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    now,
		NotAfter:     now.Add(10 * time.Minute), // Fulcio-shaped: already expired
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{u},
		ExtraExtensions: []pkix.Extension{
			{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}, Value: issuerDER},
			{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 5}, Value: []byte(repo)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, &key.PublicKey, s.caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	d := sha256.Sum256(blob)
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	// cosign writes base64(PEM) for the certificate, raw base64 for the sig.
	return base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(sig)
}

// installPolicySeam points the updater at the synthetic CA for this test only.
func (s *releaseSigner) installPolicySeam(t *testing.T) {
	t.Helper()
	prev := provenancePolicyFn
	provenancePolicyFn = func(repo, tag string) (*provenance.Policy, error) {
		p, err := provenance.PolicyForRelease(repo, tag)
		if err != nil {
			return nil, err
		}
		p.Roots = s.pool
		p.Intermediates = x509.NewCertPool()
		return p, nil
	}
	t.Cleanup(func() { provenancePolicyFn = prev })
}

// ─── scenario harness ────────────────────────────────────────────────────────

type sigScenario struct {
	tag           string
	checksumsText string // what the client is served as checksums.txt
	signedText    string // what was actually signed (differs under tampering)
	identity      string
	repo          string // where the update is DOWNLOADED from
	identityRepo  string // whose signature is ACCEPTED; empty = the canonical default
	payload       []byte // bytes the download seam writes
	omitSignature bool
	fetchErr      error // when set, the signature fetch fails at the transport layer
	requireSig    string
	allowUnsigned bool
}

// sigResult is what a scenario produced: the error, whether the binary on disk
// was actually replaced, the operator-visible log, and the workspace root (so a
// case can assert on the recorded provenance outcome).
type sigResult struct {
	err     error
	swapped bool
	logs    string
	root    string
}

func identityFor(repo, tag string) string {
	return "https://github.com/" + repo + "/.github/workflows/release.yml@refs/tags/" + tag
}

// runSigScenario wires the scenario through the real runApply and reports the
// error plus whether the binary on disk was actually replaced.
func runSigScenario(t *testing.T, sc sigScenario) (err error, swapped bool, logs string) {
	t.Helper()
	r := runSigScenarioFull(t, sc)
	return r.err, r.swapped, r.logs
}

// runSigScenarioFull is runSigScenario plus the workspace root.
func runSigScenarioFull(t *testing.T, sc sigScenario) sigResult {
	t.Helper()
	signer := newReleaseSigner(t)
	signer.installPolicySeam(t)

	u, binDir := newTestUpdater(t, sc.tag)
	var sb strings.Builder
	u.logf = func(f string, a ...any) { sb.WriteString(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n") }
	u.repo = sc.repo
	u.identityRepo = sc.identityRepo
	if u.identityRepo == "" {
		u.identityRepo = selfupdate.DefaultRepo()
	}
	u.requireSig = sc.requireSig
	u.allowUnsigned = sc.allowUnsigned

	// The certificate asserts the SAME repo it was minted for: an attacker
	// signing under their own fork gets a wholly genuine certificate for that
	// fork, which is exactly the case the identity pin has to catch.
	certRepo := sc.repo
	certB64, sigB64 := signer.sign(t, sc.identity, certRepo, []byte(sc.signedText))

	u.fetchText = func(ctx context.Context, url string) (string, error) { return sc.checksumsText, nil }
	u.fetchOptional = func(ctx context.Context, url string) (string, bool, error) {
		if sc.fetchErr != nil {
			return "", false, sc.fetchErr
		}
		if sc.omitSignature {
			return "", false, nil // 404: release predates signing
		}
		if strings.HasSuffix(url, ".sig") {
			return sigB64, true, nil
		}
		return certB64, true, nil
	}
	u.download = func(ctx context.Context, url, dst string) error {
		return os.WriteFile(dst, sc.payload, 0o644)
	}
	u.smokeTest = func(string) (string, error) { return sc.tag, nil }

	err := runApplyWithSignedResolve(u)

	got, rerr := os.ReadFile(filepath.Join(binDir, "cogos"))
	if rerr != nil {
		t.Fatalf("reading binary: %v", rerr)
	}
	return sigResult{err: err, swapped: string(got) == string(sc.payload), logs: sb.String(), root: u.root}
}

// runApplyWithSignedResolve mirrors runApplyWithStubResolve but also supplies
// the signature/certificate URLs the resolver now returns.
func runApplyWithSignedResolve(u *selfUpdater) error {
	prev := resolveAssetURLsFn
	resolveAssetURLsFn = func(ctx context.Context, repo, tag string) (*assetURLs, error) {
		return &assetURLs{
			AssetName:      assetName(),
			AssetURL:       "https://example.invalid/" + assetName(),
			ChecksumURL:    "https://example.invalid/checksums.txt",
			SignatureURL:   "https://example.invalid/checksums.txt.sig",
			CertificateURL: "https://example.invalid/checksums.txt.pem",
		}, nil
	}
	defer func() { resolveAssetURLsFn = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return u.runApply(ctx)
}

// checksumsFor builds a sha256sum-format line for this platform's asset.
func checksumsFor(payload []byte) string {
	return suHashBytes(payload) + "  " + assetName() + "\n"
}

// ─── 1. A VALID SIGNATURE PASSES ─────────────────────────────────────────────

func TestRunApplyValidSignatureApplies(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	err, swapped, logs := runSigScenario(t, sigScenario{
		tag: tag, repo: repo,
		checksumsText: sums, signedText: sums,
		identity:   identityFor(repo, tag),
		payload:    payload,
		requireSig: selfupdate.SignatureEnforce,
	})
	if err != nil {
		t.Fatalf("a validly signed release must apply: %v", err)
	}
	if !swapped {
		t.Fatal("binary should have been replaced")
	}
	if !strings.Contains(logs, "provenance OK") {
		t.Errorf("expected a provenance OK log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, identityFor(repo, tag)) {
		t.Errorf("the attested identity must be logged so the operator can see which run shipped this; got:\n%s", logs)
	}
}

// ─── 2. A TAMPERED BINARY FAILS ──────────────────────────────────────────────

// The attacker swaps only the binary. Its digest no longer matches the signed
// checksums.txt, so GATE L (sha256) catches it — the signature made the
// checksums trustworthy, and the checksum then covers the binary. This is the
// "chain from the signature to the artifact" holding end to end.
func TestRunApplyTamperedBinaryFails(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	honest := []byte("NEW BINARY " + tag)
	sums := checksumsFor(honest) // signed digest of the HONEST binary

	err, swapped, _ := runSigScenario(t, sigScenario{
		tag: tag, repo: repo,
		checksumsText: sums, signedText: sums,
		identity:   identityFor(repo, tag),
		payload:    []byte("MALICIOUS BINARY"), // ...but this is delivered
		requireSig: selfupdate.SignatureEnforce,
	})
	if err == nil {
		t.Fatal("a tampered binary must not be applied")
	}
	if !strings.Contains(err.Error(), "checksum verify") {
		t.Errorf("expected the sha256 gate to catch it, got: %v", err)
	}
	if swapped {
		t.Fatal("running binary must be untouched")
	}
}

// The attacker substitutes the binary AND rewrites checksums.txt to match it —
// self-consistent, so every checksum test passes by construction. THIS is the
// attack the whole change exists to stop: before GATE L0 this sailed through to
// atomic swap and launchd restart. The forged checksums no longer match the
// signature, so it is now refused before anything is downloaded.
func TestRunApplyConsistentlyForgedChecksumsFails(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	honest := []byte("NEW BINARY " + tag)
	malicious := []byte("MALICIOUS BINARY")

	err, swapped, _ := runSigScenario(t, sigScenario{
		tag: tag, repo: repo,
		checksumsText: checksumsFor(malicious), // served: matches the bad binary
		signedText:    checksumsFor(honest),    // signed: the real release
		identity:      identityFor(repo, tag),
		payload:       malicious,
		requireSig:    selfupdate.SignatureEnforce,
	})
	if err == nil {
		t.Fatal("a self-consistent forged (binary, checksums) pair must be refused")
	}
	if !strings.Contains(err.Error(), "provenance verify") {
		t.Errorf("expected GATE L0 to refuse it, got: %v", err)
	}
	if swapped {
		t.Fatal("running binary must be untouched")
	}
}

// ─── 3. A VALID SIGNATURE FROM THE WRONG IDENTITY FAILS ──────────────────────

func TestRunApplyWrongIdentitySignatureFails(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	cases := []struct {
		name     string
		identity string
	}{
		{"another repository's release.yml", identityFor("attacker/cogos", tag)},
		{"another workflow in our repository", "https://github.com/myrgic/cogos/.github/workflows/evil.yml@refs/tags/" + tag},
		{"our release.yml at a different tag", identityFor(repo, "v0.0.1")},
		{"a branch rather than a tag", "https://github.com/myrgic/cogos/.github/workflows/release.yml@refs/heads/main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, swapped, _ := runSigScenario(t, sigScenario{
				tag: tag, repo: repo,
				checksumsText: sums, signedText: sums,
				identity:   tc.identity, // signature is cryptographically perfect
				payload:    payload,
				requireSig: selfupdate.SignatureEnforce,
			})
			if err == nil {
				t.Fatal("a signature from the wrong identity must be refused")
			}
			if !strings.Contains(err.Error(), "provenance verify") {
				t.Errorf("expected GATE L0 to refuse it, got: %v", err)
			}
			if swapped {
				t.Fatal("running binary must be untouched")
			}
		})
	}
}

// An invalid signature is refused even with --allow-unsigned: that flag covers
// "this release predates signing", never "this signature is wrong".
func TestAllowUnsignedDoesNotExcuseAnInvalidSignature(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	err, swapped, _ := runSigScenario(t, sigScenario{
		tag: tag, repo: repo,
		checksumsText: sums, signedText: sums,
		identity:      identityFor("attacker/cogos", tag),
		payload:       payload,
		requireSig:    selfupdate.SignatureEnforce,
		allowUnsigned: true,
	})
	if err == nil || swapped {
		t.Fatal("--allow-unsigned must not wave through a bad signature")
	}
}

// ─── 4. A MISSING SIGNATURE BEHAVES PER THE CHOSEN POSTURE ───────────────────

func TestRunApplyMissingSignaturePosture(t *testing.T) {
	const tag, repo = "v0.15.0", "myrgic/cogos" // predates FirstSignedRelease
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	cases := []struct {
		name          string
		requireSig    string
		allowUnsigned bool
		wantApplied   bool
		wantLog       string
	}{
		{
			name:       "enforce refuses (fail closed)",
			requireSig: selfupdate.SignatureEnforce,
		},
		{
			name:        "warn applies but says so loudly",
			requireSig:  selfupdate.SignatureWarn,
			wantApplied: true,
			wantLog:     "WARNING",
		},
		{
			name:        "off skips verification, still logs",
			requireSig:  selfupdate.SignatureOff,
			wantApplied: true,
			wantLog:     "DISABLED",
		},
		{
			name:          "enforce plus explicit --allow-unsigned applies",
			requireSig:    selfupdate.SignatureEnforce,
			allowUnsigned: true,
			wantApplied:   true,
			wantLog:       "--allow-unsigned",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, swapped, logs := runSigScenario(t, sigScenario{
				tag: tag, repo: repo,
				checksumsText: sums, signedText: sums,
				identity:      identityFor(repo, tag),
				payload:       payload,
				omitSignature: true,
				requireSig:    tc.requireSig,
				allowUnsigned: tc.allowUnsigned,
			})
			if tc.wantApplied {
				if err != nil {
					t.Fatalf("expected the update to apply, got: %v", err)
				}
				if !swapped {
					t.Fatal("binary should have been replaced")
				}
			} else {
				if err == nil {
					t.Fatal("expected the update to be refused")
				}
				if swapped {
					t.Fatal("running binary must be untouched")
				}
				// The refusal must be actionable, not just a denial.
				for _, want := range []string{provenance.FirstSignedRelease, "--allow-unsigned"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal should mention %q; got: %v", want, err)
					}
				}
			}
			if tc.wantLog != "" && !strings.Contains(logs, tc.wantLog) {
				t.Errorf("expected %q in logs, got:\n%s", tc.wantLog, logs)
			}
		})
	}
}

// A zero-value requireSig (a wiring bug) must behave as enforce, never as off.
func TestUnsetPostureFailsClosed(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	err, swapped, _ := runSigScenario(t, sigScenario{
		tag: tag, repo: repo,
		checksumsText: sums, signedText: sums,
		identity:      identityFor(repo, tag),
		payload:       payload,
		omitSignature: true,
		requireSig:    "", // unset
	})
	if err == nil || swapped {
		t.Fatal("an unset provenance posture must fail closed, not open")
	}
}

// The write-ahead ledger must not be touched when GATE L0 refuses: the refusal
// happens before kernel.toml is written, so a blocked update leaves no trace
// claiming a version that never installed (the #442 drift invariant).
func TestProvenanceFailureLeavesKernelTOMLUntouched(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	signer := newReleaseSigner(t)
	signer.installPolicySeam(t)
	u, _ := newTestUpdater(t, tag)
	u.repo = repo
	u.requireSig = selfupdate.SignatureEnforce
	ktPath := seedKernelTOML(t, u)
	before, err := os.ReadFile(ktPath)
	if err != nil {
		t.Fatal(err)
	}

	certB64, sigB64 := signer.sign(t, identityFor("attacker/cogos", tag), repo, []byte(sums))
	u.fetchText = func(context.Context, string) (string, error) { return sums, nil }
	u.fetchOptional = func(_ context.Context, url string) (string, bool, error) {
		if strings.HasSuffix(url, ".sig") {
			return sigB64, true, nil
		}
		return certB64, true, nil
	}
	u.download = func(_ context.Context, _, dst string) error { return os.WriteFile(dst, payload, 0o644) }

	if err := runApplyWithSignedResolve(u); err == nil {
		t.Fatal("expected refusal")
	}
	after, err := os.ReadFile(ktPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("kernel.toml must be untouched when provenance fails.\ngot:\n%s\nwant:\n%s", after, before)
	}
}

// ─── 5. A REPO REDIRECT MUST NOT REDIRECT THE TRUST ANCHOR ───────────────────

// The download source is configurable; the identity that must have signed is
// not. Point `repo:` at a fork and the fork's OWN genuine keyless signature —
// cryptographically perfect, correctly chained, correctly issued — must still
// be refused, because it is not this project's release workflow.
//
// The critical assertion is not merely that it fails. It is that the updater
// never prints "provenance OK", because an affirmative green light over an
// attacker's binary is worse than no verification at all.
func TestRepoRedirectCannotRedirectTheIdentityPin(t *testing.T) {
	const tag = "v0.17.0"
	const attacker = "attacker/cogos"
	payload := []byte("MALICIOUS BINARY")
	sums := checksumsFor(payload)

	r := runSigScenarioFull(t, sigScenario{
		tag:  tag,
		repo: attacker, // config redirected: bytes come from the fork
		// identityRepo deliberately left empty → the compile-time default.
		checksumsText: sums, signedText: sums,
		identity:   identityFor(attacker, tag), // the fork's own real identity
		payload:    payload,
		requireSig: selfupdate.SignatureEnforce,
	})
	if r.err == nil || r.swapped {
		t.Fatal("a fork's own valid signature must not satisfy the canonical identity pin")
	}
	if strings.Contains(r.logs, "provenance OK") {
		t.Errorf("the updater must never affirm provenance for a redirected repo; logs:\n%s", r.logs)
	}
	if !strings.Contains(r.err.Error(), selfupdate.DefaultRepo()) {
		t.Errorf("the refusal should name the identity actually required; got: %v", r.err)
	}
}

// An operator who deliberately sets signature_repo gets what they asked for,
// but it is announced every time so it cannot be set once and forgotten.
func TestExplicitSignatureRepoIsHonouredAndAnnounced(t *testing.T) {
	const tag = "v0.17.0"
	const fork = "someone/cogos-fork"
	payload := []byte("FORK BINARY")
	sums := checksumsFor(payload)

	r := runSigScenarioFull(t, sigScenario{
		tag: tag, repo: fork, identityRepo: fork,
		checksumsText: sums, signedText: sums,
		identity:   identityFor(fork, tag),
		payload:    payload,
		requireSig: selfupdate.SignatureEnforce,
	})
	if r.err != nil {
		t.Fatalf("an explicitly pinned fork identity must verify: %v", r.err)
	}
	if !strings.Contains(r.logs, "NOT the canonical") {
		t.Errorf("a non-canonical identity pin must be announced loudly; logs:\n%s", r.logs)
	}
}

// ─── 6. --allow-unsigned IS BOUNDED BY FirstSignedRelease ────────────────────

// The flag's whole justification is "this tag predates signing". For a tag that
// postdates it, a missing signature is indistinguishable from a stripped one,
// so the flag must not apply — otherwise an attacker who simply 404s the .sig
// and .pem assets converts it into a total provenance bypass.
func TestAllowUnsignedIsBoundedByFirstSignedRelease(t *testing.T) {
	payload := []byte("PAYLOAD")
	sums := checksumsFor(payload)

	cases := []struct {
		tag       string
		wantApply bool
	}{
		{"v0.15.0", true},                     // before the boundary
		{provenance.FirstSignedRelease, true}, // at the boundary
		{"v0.16.26", false},                   // after: flag must not apply
		{"v0.17.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			err, swapped, _ := runSigScenario(t, sigScenario{
				tag: tc.tag, repo: "myrgic/cogos",
				checksumsText: sums, signedText: sums,
				identity:      identityFor("myrgic/cogos", tc.tag),
				payload:       payload,
				omitSignature: true,
				requireSig:    selfupdate.SignatureEnforce,
				allowUnsigned: true,
			})
			if tc.wantApply {
				if err != nil || !swapped {
					t.Fatalf("--allow-unsigned should cover %s: err=%v swapped=%v", tc.tag, err, swapped)
				}
				return
			}
			if err == nil || swapped {
				t.Fatalf("--allow-unsigned must NOT cover %s, which postdates %s",
					tc.tag, provenance.FirstSignedRelease)
			}
			if !strings.Contains(err.Error(), "stripped") {
				t.Errorf("the refusal should explain why the flag does not apply; got: %v", err)
			}
		})
	}
}

// ─── 7. A TRANSPORT FAILURE IS NOT A VERIFICATION FAILURE ────────────────────

// A CDN 503 or a VPN that blackholes github.com must never be reported in the
// language of compromise. Under enforce it still stops the update (correctly —
// nothing was proven), but it must say so as a network condition, and the next
// cycle retries.
func TestTransportFailureIsNotReportedAsAnAttack(t *testing.T) {
	prev := signatureFetchBackoff
	signatureFetchBackoff = time.Millisecond
	t.Cleanup(func() { signatureFetchBackoff = prev })

	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY")
	sums := checksumsFor(payload)

	base := sigScenario{
		tag: tag, repo: repo,
		checksumsText: sums, signedText: sums,
		identity: identityFor(repo, tag),
		payload:  payload,
		fetchErr: fmt.Errorf("GET https://example.invalid/checksums.txt.sig: status 503"),
	}

	t.Run("enforce stops, but not as a verification failure", func(t *testing.T) {
		sc := base
		sc.requireSig = selfupdate.SignatureEnforce
		r := runSigScenarioFull(t, sc)
		if r.err == nil || r.swapped {
			t.Fatal("enforce must not apply an update it could not verify")
		}
		if strings.Contains(r.err.Error(), "VERIFICATION FAILED") ||
			strings.Contains(r.err.Error(), "does not match") {
			t.Errorf("a network failure must not be phrased as a bad signature; got: %v", r.err)
		}
		for _, want := range []string{"network condition", "retried"} {
			if !strings.Contains(r.err.Error(), want) {
				t.Errorf("expected %q in the message; got: %v", want, r.err)
			}
		}
		if o := selfupdate.ReadProvenanceOutcome(r.root); o == nil || o.Result != selfupdate.ProvenanceTransport {
			t.Errorf("outcome should record a transport error, got %+v", o)
		}
	})

	t.Run("warn applies and says CONNECTIVITY", func(t *testing.T) {
		sc := base
		sc.requireSig = selfupdate.SignatureWarn
		r := runSigScenarioFull(t, sc)
		if r.err != nil || !r.swapped {
			t.Fatalf("warn must not block on a network failure: %v", r.err)
		}
		if !strings.Contains(r.logs, "CONNECTIVITY") {
			t.Errorf("expected the log to name this a connectivity failure; got:\n%s", r.logs)
		}
	})

	t.Run("retries before giving up", func(t *testing.T) {
		signer := newReleaseSigner(t)
		signer.installPolicySeam(t)
		u, _ := newTestUpdater(t, tag)
		u.repo, u.identityRepo, u.requireSig = repo, repo, selfupdate.SignatureEnforce
		u.logf = func(string, ...any) {}
		calls := 0
		u.fetchText = func(context.Context, string) (string, error) { return sums, nil }
		u.fetchOptional = func(context.Context, string) (string, bool, error) {
			calls++
			return "", false, fmt.Errorf("dial tcp: i/o timeout")
		}
		u.download = func(_ context.Context, _, dst string) error { return os.WriteFile(dst, payload, 0o644) }
		if err := runApplyWithSignedResolve(u); err == nil {
			t.Fatal("expected refusal")
		}
		if calls != signatureFetchAttempts {
			t.Errorf("expected %d attempts before giving up, got %d", signatureFetchAttempts, calls)
		}
	})
}

// ─── 8. THE TERMINAL OUTCOME IS RECORDED FOR THE DAEMON ──────────────────────

// A refusal that only exists in this process's log file is invisible to every
// surface the operator watches: the provider would keep reporting "updating
// to <tag>" forever. The verdict has to be written where FetchLive can find it.
func TestProvenanceOutcomeIsRecorded(t *testing.T) {
	const tag, repo = "v0.17.0", "myrgic/cogos"
	payload := []byte("NEW BINARY " + tag)
	sums := checksumsFor(payload)

	t.Run("refusal is recorded as blocked", func(t *testing.T) {
		r := runSigScenarioFull(t, sigScenario{
			tag: tag, repo: repo,
			checksumsText: sums, signedText: sums,
			identity:   identityFor("attacker/cogos", tag),
			payload:    payload,
			requireSig: selfupdate.SignatureEnforce,
		})
		if r.err == nil {
			t.Fatal("expected refusal")
		}
		o := selfupdate.ReadProvenanceOutcome(r.root)
		if o == nil {
			t.Fatal("a refusal must leave a terminal outcome the daemon can read")
		}
		if o.Result != selfupdate.ProvenanceInvalid || !o.Blocked {
			t.Errorf("want invalid+blocked, got %+v", o)
		}
		if o.Tag != tag {
			t.Errorf("outcome must name the tag it refused, got %q", o.Tag)
		}
	})

	t.Run("success is recorded as not blocked", func(t *testing.T) {
		r := runSigScenarioFull(t, sigScenario{
			tag: tag, repo: repo,
			checksumsText: sums, signedText: sums,
			identity:   identityFor(repo, tag),
			payload:    payload,
			requireSig: selfupdate.SignatureEnforce,
		})
		if r.err != nil {
			t.Fatalf("expected success: %v", r.err)
		}
		o := selfupdate.ReadProvenanceOutcome(r.root)
		if o == nil || o.Result != selfupdate.ProvenanceOK || o.Blocked {
			t.Errorf("want ok+unblocked, got %+v", o)
		}
	})

	t.Run("warn-mode failure is recorded but not blocked", func(t *testing.T) {
		r := runSigScenarioFull(t, sigScenario{
			tag: tag, repo: repo,
			checksumsText: sums, signedText: sums,
			identity:   identityFor("attacker/cogos", tag),
			payload:    payload,
			requireSig: selfupdate.SignatureWarn,
		})
		if r.err != nil || !r.swapped {
			t.Fatalf("warn must not block: %v", r.err)
		}
		o := selfupdate.ReadProvenanceOutcome(r.root)
		if o == nil || o.Result != selfupdate.ProvenanceInvalid {
			t.Fatalf("warn must still record the failure, got %+v", o)
		}
		if o.Blocked {
			t.Error("warn mode did not block, so the outcome must not claim it did")
		}
	})
}
