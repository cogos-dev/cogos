package provenance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// ─── golden: the REAL published release ──────────────────────────────────────
//
// testdata/ holds the genuine checksums.txt / .sig / .pem from myrgic/cogos
// v0.16.26. This is the test that proves the verifier agrees with what the
// release pipeline actually emits, rather than only with fixtures this package
// generated for itself. If cosign's output encoding, the Fulcio chain, or the
// certificate extension layout ever changes, this test is what notices.

const goldenTag = "v0.16.26"

func goldenMaterial(t *testing.T) (blob, cert, sig []byte) {
	t.Helper()
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		return b
	}
	return read("checksums.txt"), read("checksums.txt.pem"), read("checksums.txt.sig")
}

// A VALID SIGNATURE PASSES.
func TestGoldenValidSignatureVerifies(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, err := PolicyForRelease("myrgic/cogos", goldenTag)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	att, err := p.VerifyBlob(blob, cert, sig)
	if err != nil {
		t.Fatalf("real v0.16.26 signature must verify, got: %v", err)
	}
	want := "https://github.com/myrgic/cogos/.github/workflows/release.yml@refs/tags/v0.16.26"
	if att.Identity != want {
		t.Errorf("identity = %q, want %q", att.Identity, want)
	}
	if att.OIDCIssuer != GitHubOIDCIssuer {
		t.Errorf("issuer = %q, want %q", att.OIDCIssuer, GitHubOIDCIssuer)
	}
	if att.SourceRepo != "myrgic/cogos" {
		t.Errorf("source repo = %q", att.SourceRepo)
	}
	if att.RunnerEnv != "github-hosted" {
		t.Errorf("runner env = %q, want github-hosted", att.RunnerEnv)
	}
}

// The whole reason verifyChain anchors at NotBefore: this certificate is long
// expired by wall-clock and must still verify. If someone "fixes" the anchor to
// time.Now(), every release older than ten minutes stops installing and this
// test catches it immediately.
func TestGoldenCertIsExpiredByWallClockYetVerifies(t *testing.T) {
	_, certRaw, _ := goldenMaterial(t)
	cert, err := ParseCertificate(certRaw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !time.Now().After(cert.NotAfter) {
		t.Skip("golden certificate has not expired yet; the anchoring invariant is untestable here")
	}
	blob, certRaw, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("myrgic/cogos", goldenTag)
	if _, err := p.VerifyBlob(blob, certRaw, sig); err != nil {
		t.Fatalf("expired-by-wall-clock certificate must still verify (anchored at NotBefore): %v", err)
	}
}

// A TAMPERED ARTIFACT FAILS. Flipping any byte of checksums.txt breaks the
// signature — which is exactly how tampering with a listed BINARY is caught,
// since changing a binary forces its digest line to change too.
func TestGoldenTamperedChecksumsFails(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("myrgic/cogos", goldenTag)

	tampered := append([]byte{}, blob...)
	// Substitute the darwin-arm64 digest for an attacker-controlled one.
	tampered[0] = 'f'
	tampered[1] = 'f'
	if _, err := p.VerifyBlob(tampered, cert, sig); !errors.Is(err, ErrVerification) {
		t.Fatalf("tampered checksums.txt must fail with ErrVerification, got: %v", err)
	}
}

// CROSS-RELEASE REPLAY. The signature is genuine and the identity is genuinely
// ours — but it belongs to a different tag. An attacker who serves release
// v0.16.26's real material in response to a request for v0.16.27 must not pass.
// This is caught only because the policy pins the exact tag, which is strictly
// stronger than the repo+workflow regexp docs/release-signing.md gives humans.
func TestGoldenSignatureFromDifferentReleaseFails(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("myrgic/cogos", "v0.16.27")
	_, err := p.VerifyBlob(blob, cert, sig)
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("signature for a different tag must fail, got: %v", err)
	}
}

// A signature from a DIFFERENT REPOSITORY's release.yml must fail even though
// it chains to the same real Fulcio root.
func TestGoldenWrongRepositoryFails(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("attacker/cogos", goldenTag)
	if _, err := p.VerifyBlob(blob, cert, sig); !errors.Is(err, ErrVerification) {
		t.Fatalf("certificate from another repository must fail, got: %v", err)
	}
}

// A MISSING SIGNATURE is reported as ErrNoSignature, distinctly from a failed
// one, because the two get different operator handling.
func TestMissingSignatureMaterialIsDistinguished(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("myrgic/cogos", goldenTag)

	for _, tc := range []struct {
		name      string
		cert, sig []byte
	}{
		{"both absent", nil, nil},
		{"signature absent", cert, nil},
		{"certificate absent", nil, sig},
		{"whitespace only", []byte("  \n"), []byte("\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.VerifyBlob(blob, tc.cert, tc.sig)
			if !errors.Is(err, ErrNoSignature) {
				t.Fatalf("want ErrNoSignature, got: %v", err)
			}
			if errors.Is(err, ErrVerification) {
				t.Fatal("absent material must not be reported as a verification failure")
			}
		})
	}
}

func TestPinnedFulcioRootsParse(t *testing.T) {
	roots, inters, err := embeddedFulcioPools()
	if err != nil {
		t.Fatalf("embedded Fulcio bundle must parse: %v", err)
	}
	if roots == nil || inters == nil {
		t.Fatal("both pools must be non-nil")
	}
}

// ─── synthetic CA: constructs cases the real material cannot ─────────────────
//
// "A valid signature from the WRONG identity" needs a certificate that is
// cryptographically sound and chains correctly, but carries an identity we
// reject. That cannot be built from the real Fulcio material, so these tests
// stand up a throwaway CA and inject it into the policy.

type fakeCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"test.invalid"}, CommonName: "test-root"},
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
	return &fakeCA{cert: cert, key: key, pool: pool}
}

type leafOpts struct {
	sanURI     string
	issuer     string
	sourceRepo string
	notBefore  time.Time
	lifetime   time.Duration
}

// issueLeaf mints a Fulcio-shaped ephemeral signing certificate.
func (ca *fakeCA) issueLeaf(t *testing.T, o leafOpts) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if o.notBefore.IsZero() {
		o.notBefore = time.Now().Add(-time.Hour)
	}
	if o.lifetime == 0 {
		o.lifetime = 10 * time.Minute
	}
	u, err := url.Parse(o.sanURI)
	if err != nil {
		t.Fatal(err)
	}
	issuerDER, err := asn1.Marshal(o.issuer)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    o.notBefore,
		NotAfter:     o.notBefore.Add(o.lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{u},
		ExtraExtensions: []pkix.Extension{
			{Id: oidIssuerV2, Value: issuerDER},
			{Id: oidSourceRepo, Value: []byte(o.sourceRepo)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// cosign writes base64(PEM); mirror that so the tests exercise the real
	// decoding path rather than a friendlier one.
	b64 := []byte(base64.StdEncoding.EncodeToString(pemBytes))
	return leaf, key, b64
}

func signBlob(t *testing.T, key *ecdsa.PrivateKey, blob []byte) []byte {
	t.Helper()
	d := sha256.Sum256(blob)
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(sig))
}

func (ca *fakeCA) policy(identity string) *Policy {
	return &Policy{
		Identity:         identity,
		OIDCIssuer:       GitHubOIDCIssuer,
		SourceRepository: "myrgic/cogos",
		Roots:            ca.pool,
		Intermediates:    x509.NewCertPool(),
	}
}

const goodIdentity = "https://github.com/myrgic/cogos/.github/workflows/release.yml@refs/tags/v9.9.9"

// Control: the synthetic harness itself produces something that verifies.
func TestSyntheticValidSignaturePasses(t *testing.T) {
	ca := newFakeCA(t)
	blob := []byte("deadbeef  cogos-darwin-arm64\n")
	_, key, certPEM := ca.issueLeaf(t, leafOpts{
		sanURI: goodIdentity, issuer: GitHubOIDCIssuer, sourceRepo: "myrgic/cogos",
	})
	if _, err := ca.policy(goodIdentity).VerifyBlob(blob, certPEM, signBlob(t, key, blob)); err != nil {
		t.Fatalf("synthetic valid signature must pass: %v", err)
	}
}

// A VALID SIGNATURE FROM THE WRONG IDENTITY FAILS.
//
// Each case below is a perfectly well-formed signature: correct key, correct
// chain, signature genuinely over the blob. Only the asserted identity differs.
func TestValidSignatureWrongIdentityFails(t *testing.T) {
	blob := []byte("deadbeef  cogos-darwin-arm64\n")

	cases := []struct {
		name string
		opts leafOpts
	}{
		{
			// A fork. Chains fine, signs fine, wrong repo in the SAN.
			name: "different repository",
			opts: leafOpts{
				sanURI:     "https://github.com/attacker/cogos/.github/workflows/release.yml@refs/tags/v9.9.9",
				issuer:     GitHubOIDCIssuer,
				sourceRepo: "myrgic/cogos",
			},
		},
		{
			// Right repo, but signed by some OTHER workflow in it — e.g. one an
			// attacker added via a PR. This is the case a repo-only pin misses.
			name: "different workflow in the same repository",
			opts: leafOpts{
				sanURI:     "https://github.com/myrgic/cogos/.github/workflows/attacker.yml@refs/tags/v9.9.9",
				issuer:     GitHubOIDCIssuer,
				sourceRepo: "myrgic/cogos",
			},
		},
		{
			// Right repo and workflow, wrong tag: cross-release replay.
			name: "different tag",
			opts: leafOpts{
				sanURI:     "https://github.com/myrgic/cogos/.github/workflows/release.yml@refs/tags/v0.0.1",
				issuer:     GitHubOIDCIssuer,
				sourceRepo: "myrgic/cogos",
			},
		},
		{
			// Correct SAN, but the identity was asserted by an issuer we do not
			// trust — an attacker-run OIDC provider federated into a rogue CA.
			name: "wrong OIDC issuer",
			opts: leafOpts{
				sanURI:     goodIdentity,
				issuer:     "https://accounts.google.com",
				sourceRepo: "myrgic/cogos",
			},
		},
		{
			// SAN says ours; the source-repository extension disagrees. Defence
			// in depth: either pin alone would refuse this.
			name: "source repository extension mismatch",
			opts: leafOpts{
				sanURI:     goodIdentity,
				issuer:     GitHubOIDCIssuer,
				sourceRepo: "attacker/cogos",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := newFakeCA(t)
			_, key, certPEM := ca.issueLeaf(t, tc.opts)
			sig := signBlob(t, key, blob)
			_, err := ca.policy(goodIdentity).VerifyBlob(blob, certPEM, sig)
			if !errors.Is(err, ErrVerification) {
				t.Fatalf("want ErrVerification, got: %v", err)
			}
		})
	}
}

// A signature that verifies under an UNTRUSTED ROOT must fail. This is the
// attacker who stands up their own CA and signs everything consistently.
func TestUntrustedRootFails(t *testing.T) {
	attacker := newFakeCA(t)
	blob := []byte("deadbeef  cogos-darwin-arm64\n")
	_, key, certPEM := attacker.issueLeaf(t, leafOpts{
		sanURI: goodIdentity, issuer: GitHubOIDCIssuer, sourceRepo: "myrgic/cogos",
	})
	sig := signBlob(t, key, blob)

	// Policy trusts the REAL pinned Fulcio roots, not the attacker's CA.
	p, err := PolicyForRelease("myrgic/cogos", "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.VerifyBlob(blob, certPEM, sig); !errors.Is(err, ErrVerification) {
		t.Fatalf("certificate under an untrusted root must fail, got: %v", err)
	}
}

// A signature that does not cover the blob must fail even with a perfect cert.
func TestSignatureOverDifferentContentFails(t *testing.T) {
	ca := newFakeCA(t)
	_, key, certPEM := ca.issueLeaf(t, leafOpts{
		sanURI: goodIdentity, issuer: GitHubOIDCIssuer, sourceRepo: "myrgic/cogos",
	})
	sig := signBlob(t, key, []byte("some other content"))
	_, err := ca.policy(goodIdentity).VerifyBlob([]byte("deadbeef  cogos-darwin-arm64\n"), certPEM, sig)
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("signature over other content must fail, got: %v", err)
	}
}

// Guard 2: a long-lived leaf is not a Fulcio ephemeral certificate.
func TestOverlongCertificateLifetimeRejected(t *testing.T) {
	ca := newFakeCA(t)
	blob := []byte("x")
	_, key, certPEM := ca.issueLeaf(t, leafOpts{
		sanURI: goodIdentity, issuer: GitHubOIDCIssuer, sourceRepo: "myrgic/cogos",
		notBefore: time.Now().Add(-48 * time.Hour), lifetime: 90 * 24 * time.Hour,
	})
	_, err := ca.policy(goodIdentity).VerifyBlob(blob, certPEM, signBlob(t, key, blob))
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("over-long certificate lifetime must be rejected, got: %v", err)
	}
}

// Guard 1: a certificate claiming issuance far in the future is rejected, so a
// skewed or manipulated host clock is not a lever.
func TestFutureNotBeforeRejected(t *testing.T) {
	ca := newFakeCA(t)
	blob := []byte("x")
	_, key, certPEM := ca.issueLeaf(t, leafOpts{
		sanURI: goodIdentity, issuer: GitHubOIDCIssuer, sourceRepo: "myrgic/cogos",
		notBefore: time.Now().Add(72 * time.Hour),
	})
	_, err := ca.policy(goodIdentity).VerifyBlob(blob, certPEM, signBlob(t, key, blob))
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("far-future NotBefore must be rejected, got: %v", err)
	}
}

// Both cosign's base64(PEM) and bare PEM must parse.
func TestParseCertificateAcceptsBothEncodings(t *testing.T) {
	_, certRaw, _ := goldenMaterial(t)
	fromB64, err := ParseCertificate(certRaw)
	if err != nil {
		t.Fatalf("base64(PEM): %v", err)
	}
	plain, err := base64.StdEncoding.DecodeString(string(certRaw))
	if err != nil {
		t.Fatal(err)
	}
	fromPEM, err := ParseCertificate(plain)
	if err != nil {
		t.Fatalf("bare PEM: %v", err)
	}
	if !fromB64.Equal(fromPEM) {
		t.Error("both encodings must yield the same certificate")
	}
	if _, err := ParseCertificate([]byte("not a certificate")); err == nil {
		t.Error("garbage must not parse")
	}
}

// The pattern fallback (no known tag) still pins repo and workflow path.
func TestIdentityPatternFallbackPinsRepoAndWorkflow(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, err := PolicyForRelease("myrgic/cogos", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Identity != "" || p.IdentityPattern == nil {
		t.Fatal("empty tag must select the pattern fallback")
	}
	if _, err := p.VerifyBlob(blob, cert, sig); err != nil {
		t.Fatalf("pattern fallback must accept a genuine release: %v", err)
	}
	// ...but still refuse another repository.
	bad, _ := PolicyForRelease("attacker/cogos", "")
	if _, err := bad.VerifyBlob(blob, cert, sig); !errors.Is(err, ErrVerification) {
		t.Fatalf("pattern fallback must still pin the repository, got: %v", err)
	}
	if !regexp.MustCompile(`myrgic/cogos`).MatchString(p.IdentityPattern.String()) {
		t.Error("pattern must mention the repository")
	}
}

// VerifiedChecksums must not hand back bytes on failure — the coupling that
// keeps a caller from ever parsing a digest out of unverified text.
func TestVerifiedChecksumsWithholdsBytesOnFailure(t *testing.T) {
	blob, cert, sig := goldenMaterial(t)
	p, _ := PolicyForRelease("myrgic/cogos", "v0.0.1") // wrong tag
	text, att, err := p.VerifiedChecksums(blob, cert, sig)
	if err == nil {
		t.Fatal("expected failure")
	}
	if text != "" || att != nil {
		t.Fatal("no checksum text or attestation may be returned on failure")
	}
}
