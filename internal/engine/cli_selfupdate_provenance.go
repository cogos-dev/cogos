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
	"time"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
	"github.com/myrgic/cogos/internal/providers/selfupdate/provenance"
)

// signatureFetchAttempts / signatureFetchBackoff bound the retry of a TRANSPORT
// failure while fetching signature material.
//
// A CDN 5xx, or the VPN condition already known to break github.com access on
// some hosts, is not an attack and must not be handled like one. Retrying a
// couple of times costs seconds and turns most transient failures into an
// ordinary verification; whatever remains is reported in the language of
// connectivity rather than of compromise.
const signatureFetchAttempts = 3

// signatureFetchBackoff is a var only so tests need not sleep through it.
var signatureFetchBackoff = 2 * time.Second

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
//
// Every path records a terminal outcome (internal/providers/selfupdate/outcome.go)
// so a refusal reaches the daemon's status surfaces instead of living only in
// this process's log file.
func (u *selfUpdater) gateProvenance(ctx context.Context, target *assetURLs, sums string) (string, error) {
	if u.requireSig == selfupdate.SignatureOff {
		u.logf("WARNING: signature verification DISABLED (require_signature: off) — " +
			"this update's provenance will not be checked")
		u.recordProvenance(selfupdate.ProvenanceSkipped, false, "verification disabled by require_signature: off")
		return sums, nil
	}

	att, err := u.verifyProvenance(ctx, target, sums)
	if err == nil {
		u.logf("provenance OK: %s", att)
		u.recordProvenance(selfupdate.ProvenanceOK, false, att.String())
		return sums, nil
	}

	warnOnly := u.requireSig == selfupdate.SignatureWarn

	switch {
	// ── Transport: nothing was learned about authenticity. ───────────────────
	case errors.Is(err, provenance.ErrTransport):
		if warnOnly {
			u.logf("WARNING: could not retrieve signature material for %s: %v. "+
				"Applying anyway because require_signature is %q. This is a CONNECTIVITY "+
				"failure, not a failed verification.", u.toTag, err, selfupdate.SignatureWarn)
			u.recordProvenance(selfupdate.ProvenanceTransport, false, err.Error())
			return sums, nil
		}
		u.recordProvenance(selfupdate.ProvenanceTransport, true, err.Error())
		// Deliberately NOT phrased as a verification failure: nothing was
		// disproven, so nothing should read as an attack. The next reconcile
		// cycle retries.
		return "", fmt.Errorf("cannot verify %s because its signature material could not be "+
			"retrieved: %w — this is a network condition, not a bad signature; the update "+
			"will be retried", u.toTag, err)

	// ── Unsigned: expected for tags older than FirstSignedRelease. ───────────
	case errors.Is(err, provenance.ErrNoSignature):
		// --allow-unsigned exists for exactly one situation: installing a
		// release published before the pipeline signed anything. Honouring it
		// for a CURRENT tag would turn it into a blanket provenance bypass — an
		// attacker who simply 404s the .sig and .pem assets would be waved
		// straight through by an operator who typed the flag for an unrelated
		// reason. So the flag is bounded by the same constant its help text
		// advertises.
		switch {
		case u.allowUnsigned && !selfupdate.VersionAfter(u.toTag, provenance.FirstSignedRelease):
			u.logf("WARNING: release %s is at or before %s and carries no Sigstore signature; "+
				"proceeding because --allow-unsigned was given",
				u.toTag, provenance.FirstSignedRelease)
			u.recordProvenance(selfupdate.ProvenanceUnsigned, false,
				"pre-signing release accepted via --allow-unsigned")
			return sums, nil

		case u.allowUnsigned:
			// The flag was given, but for a tag that SHOULD be signed. Missing
			// material here is a red flag, not a legacy condition.
			u.recordProvenance(selfupdate.ProvenanceUnsigned, true,
				fmt.Sprintf("signature absent from %s, which postdates %s", u.toTag, provenance.FirstSignedRelease))
			return "", fmt.Errorf("refusing to apply %s: it carries no Sigstore signature even though "+
				"releases after %s are always signed — --allow-unsigned covers only releases at or "+
				"before %s, because a missing signature on a current release is indistinguishable "+
				"from one that was stripped: %w",
				u.toTag, provenance.FirstSignedRelease, provenance.FirstSignedRelease, err)

		case warnOnly:
			u.logf("WARNING: release %s carries no Sigstore signature (releases before %s are unsigned). "+
				"Applying anyway because require_signature is %q; this will become an error.",
				u.toTag, provenance.FirstSignedRelease, selfupdate.SignatureWarn)
			u.recordProvenance(selfupdate.ProvenanceUnsigned, false,
				"unsigned release accepted under require_signature: warn")
			return sums, nil
		}

		u.recordProvenance(selfupdate.ProvenanceUnsigned, true, "release carries no Sigstore signature")
		return "", fmt.Errorf("release %s carries no Sigstore signature (%s and later are signed); "+
			"refusing to apply under require_signature: %s — pin a signed release, or if this tag "+
			"genuinely predates signing run `cogos self-update --to %s --allow-unsigned`: %w",
			u.toTag, provenance.FirstSignedRelease, selfupdate.SignatureEnforce, u.toTag, err)

	// ── Present and invalid: attack, or a broken pipeline. ───────────────────
	default:
		if warnOnly {
			u.logf("WARNING: PROVENANCE VERIFICATION FAILED for %s: %v. "+
				"Applying anyway because require_signature is %q — set it to %q in "+
				"<workspace>/.cog/config/self-update.yaml to refuse unverifiable updates.",
				u.toTag, err, selfupdate.SignatureWarn, selfupdate.SignatureEnforce)
			u.recordProvenance(selfupdate.ProvenanceInvalid, false, err.Error())
			return sums, nil
		}
		// --allow-unsigned deliberately does NOT cover this: "the pipeline
		// predates signing" and "someone handed us a bad signature" are not the
		// same risk, and no flag on the manual path should wave the second one
		// through.
		u.recordProvenance(selfupdate.ProvenanceInvalid, true, err.Error())
		return "", fmt.Errorf("refusing to apply %s: %w", u.toTag, err)
	}
}

// recordProvenance persists the terminal verdict for the daemon to read.
// A failure to record is logged and otherwise ignored — observability must
// never change the outcome it is observing.
func (u *selfUpdater) recordProvenance(result string, blocked bool, msg string) {
	if err := selfupdate.WriteProvenanceOutcome(u.root, selfupdate.ProvenanceOutcome{
		Tag:     u.toTag,
		Result:  result,
		Mode:    u.requireSig,
		Blocked: blocked,
		Message: msg,
	}); err != nil {
		u.logf("could not record provenance outcome: %v", err)
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

	sig, sigFound, err := u.fetchSignatureAsset(ctx, target.SignatureURL)
	if err != nil {
		return nil, err
	}
	cert, certFound, err := u.fetchSignatureAsset(ctx, target.CertificateURL)
	if err != nil {
		return nil, err
	}
	if !sigFound || !certFound {
		return nil, provenance.ErrNoSignature
	}

	// An unset identityRepo is a wiring bug (every production path fills it
	// from SignatureSettingsFor). Fall back to the compile-time canonical repo
	// rather than building a policy with an empty owner, which would pin a
	// nonsense identity that nothing can ever match.
	identityRepo := u.identityRepo
	if identityRepo == "" {
		identityRepo = selfupdate.DefaultRepo()
	}

	// IDENTITY REPO, NOT DOWNLOAD REPO.
	//
	// u.identityRepo is the compile-time canonical repository unless the
	// operator explicitly set signature_repo. It is deliberately independent of
	// u.repo, which says only where bytes are fetched from. Were the pin taken
	// from the download source, redirecting `repo:` to a fork would make that
	// fork's own genuine keyless signature verify — and the updater would print
	// "provenance OK" over an attacker's binary, which is strictly worse than
	// printing nothing. Keeping the two separate means a redirected download
	// fails the identity check instead of blessing it.
	if identityRepo != u.repo {
		u.logf("NOTE: downloading from %s but requiring the signature to be bound to %s",
			u.repo, identityRepo)
	}
	if identityRepo != selfupdate.DefaultRepo() {
		u.logf("WARNING: signature identity is pinned to %s, NOT the canonical %s "+
			"(signature_repo is set in self-update.yaml)",
			identityRepo, selfupdate.DefaultRepo())
	}

	// The policy pins the EXACT tag being installed, so a genuine signature
	// belonging to a different release cannot be replayed onto this one.
	policy, err := provenancePolicyFn(identityRepo, u.toTag)
	if err != nil {
		return nil, fmt.Errorf("%w: building verification policy: %v", provenance.ErrVerification, err)
	}
	return policy.VerifyBlob([]byte(sums), []byte(cert), []byte(sig))
}

// fetchSignatureAsset fetches one signature asset, retrying transport failures
// before giving up. A 404 is reported as found=false rather than an error: a
// release published before signing existed genuinely has no such asset.
func (u *selfUpdater) fetchSignatureAsset(ctx context.Context, url string) (string, bool, error) {
	var lastErr error
	for attempt := 1; attempt <= signatureFetchAttempts; attempt++ {
		body, found, err := u.fetchOptional(ctx, url)
		if err == nil {
			return body, found, nil
		}
		lastErr = err
		if attempt == signatureFetchAttempts {
			break
		}
		u.logf("fetching signature material (attempt %d/%d) failed: %v; retrying",
			attempt, signatureFetchAttempts, err)
		select {
		case <-ctx.Done():
			return "", false, fmt.Errorf("%w: %v", provenance.ErrTransport, ctx.Err())
		case <-time.After(time.Duration(attempt) * signatureFetchBackoff):
		}
	}
	return "", false, fmt.Errorf("%w: %v", provenance.ErrTransport, lastErr)
}

// fetchOptionalText fetches url, reporting found=false on 404 rather than an
// error. A release published before signing was added genuinely has no .sig or
// .pem asset, and that must not be conflated with a network failure — the two
// have opposite safe responses (one may proceed under policy, the other must be
// retried).
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
