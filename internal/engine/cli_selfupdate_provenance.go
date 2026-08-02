//go:build darwin

// cli_selfupdate_provenance.go — GATE L0, the provenance gate.
//
// This is the step that turns "the bytes arrived intact" into "the bytes came
// from our release pipeline". It runs between fetching checksums.txt and
// trusting anything inside it, which is the only placement that works: the
// write-ahead ledger records the expected digest BEFORE the binary is
// downloaded (issue #442), so verifying afterwards would mean kernel.toml had
// already been written from attacker-controlled text.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
	"github.com/myrgic/cogos/internal/providers/selfupdate/provenance"
)

// gateProvenance verifies the Sigstore signature over checksums.txt and returns
// the checksum text the caller may trust.
//
// The return value — not the input — is what the caller parses the expected
// digest out of. That is deliberate: it makes "parse before verify" a
// compile-visible mistake rather than a silent one.
//
// Posture (u.requireSig):
//
//	enforce — any failure aborts. Nothing is downloaded, kernel.toml is not
//	          written, the running binary is untouched.
//	warn    — verification runs and the outcome is logged loudly; failure does
//	          not block. Transitional only; see the SignaturePolicy migration
//	          note in internal/providers/selfupdate/config.go.
//	off     — skipped entirely, logged every time.
func (u *selfUpdater) gateProvenance(ctx context.Context, target *assetURLs, sums string) (string, error) {
	if u.requireSig == selfupdate.SignatureOff {
		u.logf("WARNING: signature verification DISABLED (require_signature: off) — " +
			"this update's provenance will not be checked")
		return sums, nil
	}

	att, err := u.verifyProvenance(ctx, target, sums)
	if err == nil {
		u.logf("provenance OK: %s", att)
		return sums, nil
	}

	// An unsigned release is a distinct condition from a bad signature: the
	// former is expected for tags older than FirstSignedRelease, the latter is
	// never benign. They get different messages and, on the manual path,
	// different escape hatches.
	unsigned := errors.Is(err, provenance.ErrNoSignature)

	switch {
	case unsigned && u.allowUnsigned:
		u.logf("WARNING: release %s carries no Sigstore signature; proceeding because --allow-unsigned was given", u.toTag)
		return sums, nil

	case u.requireSig == selfupdate.SignatureWarn:
		if unsigned {
			u.logf("WARNING: release %s carries no Sigstore signature (releases before %s are unsigned). "+
				"Applying anyway because require_signature is %q; this will become an error.",
				u.toTag, provenance.FirstSignedRelease, selfupdate.SignatureWarn)
		} else {
			u.logf("WARNING: PROVENANCE VERIFICATION FAILED for %s: %v. "+
				"Applying anyway because require_signature is %q — set it to %q in "+
				".cog/config/self-update.yaml to refuse unverifiable updates.",
				u.toTag, err, selfupdate.SignatureWarn, selfupdate.SignatureEnforce)
		}
		return sums, nil

	case unsigned:
		return "", fmt.Errorf("release %s carries no Sigstore signature (%s and later are signed); "+
			"refusing to apply under require_signature: %s — pin a signed release, or run "+
			"`cogos self-update --to %s --allow-unsigned` to override deliberately: %w",
			u.toTag, provenance.FirstSignedRelease, selfupdate.SignatureEnforce, u.toTag, err)

	default:
		// A present-but-invalid signature. --allow-unsigned deliberately does
		// NOT cover this: "the pipeline predates signing" and "someone handed
		// us a bad signature" are not the same risk, and no flag on the manual
		// path should wave the second one through.
		return "", fmt.Errorf("refusing to apply %s: %w", u.toTag, err)
	}
}

// provenancePolicyFn builds the verification policy. It is the seam that lets
// tests substitute a synthetic CA for the pinned Fulcio roots, so that
// "correctly signed by the wrong identity" is constructible; production always
// uses the embedded pins. Mirrors the resolveAssetURLsFn seam.
var provenancePolicyFn = provenance.PolicyForRelease

// verifyProvenance fetches checksums.txt.sig and checksums.txt.pem for the
// target release and verifies them against the pinned policy.
func (u *selfUpdater) verifyProvenance(ctx context.Context, target *assetURLs, sums string) (*provenance.Attestation, error) {
	if target.SignatureURL == "" || target.CertificateURL == "" {
		return nil, provenance.ErrNoSignature
	}
	if u.fetchOptional == nil {
		// A wiring bug, not a policy outcome. Surface it as a verification
		// failure so the enforce path refuses rather than silently proceeding.
		return nil, fmt.Errorf("%w: signature fetch seam not configured", provenance.ErrVerification)
	}

	sig, sigFound, err := u.fetchOptional(ctx, target.SignatureURL)
	if err != nil {
		return nil, fmt.Errorf("fetching signature: %w", err)
	}
	cert, certFound, err := u.fetchOptional(ctx, target.CertificateURL)
	if err != nil {
		return nil, fmt.Errorf("fetching certificate: %w", err)
	}
	if !sigFound || !certFound {
		return nil, provenance.ErrNoSignature
	}

	// The policy pins the EXACT tag being installed, so a genuine signature
	// belonging to a different release cannot be replayed onto this one.
	policy, err := provenancePolicyFn(u.repo, u.toTag)
	if err != nil {
		return nil, fmt.Errorf("building verification policy: %w", err)
	}
	return policy.VerifyBlob([]byte(sums), []byte(cert), []byte(sig))
}

// fetchOptionalText fetches url, reporting found=false on 404 rather than an
// error. A release published before signing was added genuinely has no .sig or
// .pem asset, and that must not be conflated with a network failure — the two
// have opposite safe responses (one may proceed under policy, the other must
// be retried).
func fetchOptionalText(ctx context.Context, url string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}
