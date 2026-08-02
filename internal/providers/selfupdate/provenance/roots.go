package provenance

import (
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
)

// fulcioRootsPEM is the PINNED TRUST ROOT: Sigstore's public-good Fulcio CA.
//
// Contents (in order): the self-signed root `O=sigstore.dev, CN=sigstore`, then
// the intermediate `O=sigstore.dev, CN=sigstore-intermediate` that actually
// issues the ephemeral signing certificates. Both are valid until
// 2031-10-05. Fetched from https://fulcio.sigstore.dev/api/v1/rootCert and
// checked in deliberately: the trust root must ship INSIDE the binary that uses
// it. Fetching it at update time would defeat the purpose entirely — an
// attacker able to substitute the release could equally substitute a root and
// mint their own chain.
//
// ROTATION.
//
// The pin rotates by shipping a new binary. Because the update path itself is
// what consumes the pin, the sequence matters:
//
//  1. Sigstore announces a new root or intermediate.
//  2. APPEND the new certificate(s) to this file — do not replace. The pool
//     accepts every root it contains, so old and new verify simultaneously.
//  3. Release. Nodes running the previous binary verify that release against
//     the OLD root (still valid), install it, and thereafter accept both.
//  4. Only after the fleet has moved may a retired root be removed, in a later
//     release.
//
// Appending before removing is what keeps the rotation from wedging the very
// channel that delivers it. If a rotation is ever missed and updates stall with
// "does not chain to the pinned Sigstore root", the operator's escape hatch is
// `require_signature: warn` in self-update.yaml, which restores updates while
// keeping the failure loud — see docs/release-signing.md.
//
//go:embed fulcio_roots.pem
var fulcioRootsPEM []byte

var (
	poolsOnce  sync.Once
	poolRoots  *x509.CertPool
	poolInters *x509.CertPool
	poolErr    error
)

// embeddedFulcioPools parses the pinned PEM bundle into a root pool (self-signed
// certificates) and an intermediate pool (everything else), once.
//
// Splitting by self-signedness rather than by file position means the bundle can
// be appended to during a rotation without anyone having to remember an ordering
// convention.
func embeddedFulcioPools() (roots, intermediates *x509.CertPool, err error) {
	poolsOnce.Do(func() {
		poolRoots = x509.NewCertPool()
		poolInters = x509.NewCertPool()
		rest := fulcioRootsPEM
		n, nRoots := 0, 0
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				continue
			}
			c, perr := x509.ParseCertificate(blk.Bytes)
			if perr != nil {
				poolErr = fmt.Errorf("parsing pinned Fulcio bundle: %w", perr)
				return
			}
			if c.CheckSignatureFrom(c) == nil {
				poolRoots.AddCert(c)
				nRoots++
			} else {
				poolInters.AddCert(c)
			}
			n++
		}
		if n == 0 {
			poolErr = errors.New("pinned Fulcio bundle contains no certificates")
			return
		}
		if nRoots == 0 {
			poolErr = errors.New("pinned Fulcio bundle contains no self-signed root")
		}
	})
	return poolRoots, poolInters, poolErr
}
