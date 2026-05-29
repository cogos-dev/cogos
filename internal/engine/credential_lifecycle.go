// credential_lifecycle.go — OAuth credential lifecycle (vendor-agnostic)
//
// A reusable primitive for OAuth-bearer inference providers:
//
//   - Resolve: obtain credentials from a pluggable CredentialSource.
//   - Validate: fresh iff now < expiresAt − buffer (60 s). No expiry → fresh if
//     token present.
//   - Re-resolve before refresh: when the cached token is stale, re-read the
//     source first. If an external client (e.g. a vendor's official CLI) owns
//     and rotates the credential store, the source already holds a fresh token
//     and no refresh_token round-trip is needed.
//   - Reactive refresh: on HTTP 401, refresh once and retry.
//   - Single-flight: at most one in-flight refresh; concurrent callers wait and
//     reuse the same result (sync.Cond broadcast, no goroutine leaks).
//   - Atomic write-back: source.WriteBack called after every successful refresh;
//     implementations use tmp+rename.
//   - Fail-to-Unavailable: refresh failure returns an error so the caller can
//     report Unavailable to the router instead of presenting a stale bearer.
//
// Nothing in this file is vendor-specific. A provider supplies the concrete
// CredentialSource (keychain / file / env) and RefreshFunc (token-endpoint call).
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ── CredentialSource ──────────────────────────────────────────────────────────

// OAuthCredential holds an OAuth bearer plus the minimal fields needed for
// refresh and freshness checking.
type OAuthCredential struct {
	// AccessToken is the bearer sent in Authorization: Bearer <token>.
	AccessToken string

	// RefreshToken is used to obtain a new access token. May be empty for
	// credential sources that do not support refresh (e.g. env-var-only).
	RefreshToken string

	// ExpiresAtMS is the Unix timestamp in milliseconds at which the access
	// token expires. 0 means "no known expiry" — treated as fresh if the
	// access token is non-empty.
	ExpiresAtMS int64
}

// CredentialSource resolves an OAuthCredential from a vendor-specific store
// (keychain, credential file, environment variable, …). It does NOT refresh;
// the lifecycle component handles that.
type CredentialSource interface {
	// Resolve returns the current credential or an error. Called once per
	// lifecycle instantiation (or via ForceResolve). Implementations should
	// be fast (< 200 ms) since they block the first request.
	Resolve() (OAuthCredential, error)

	// WriteBack persists a refreshed credential back to the shared store,
	// preserving unrelated fields. Implementations MUST use atomic write
	// semantics (tmp+rename) and MUST set 0600 permissions so the file is
	// not world-readable. WriteBack errors are non-fatal: the refreshed
	// token is still used in-memory even if persistence fails.
	WriteBack(cred OAuthCredential) error
}

// RefreshFunc exchanges a refresh token for a new OAuthCredential. It is
// responsible for the HTTP call to the token endpoint and any vendor-specific
// request/response mapping. It MUST NOT call WriteBack — the lifecycle does
// that after a successful refresh.
type RefreshFunc func(ctx context.Context, refreshToken string) (OAuthCredential, error)

// ── CredentialLifecycle ───────────────────────────────────────────────────────

// freshnessBuffer is the minimum remaining validity required before we
// consider a token fresh. Matches the reference implementation (60 s).
const freshnessBuffer = 60 * time.Second

// CredentialLifecycle manages an OAuth credential: resolve, validate, refresh,
// and write back — all behind a single-flight gate.
//
// Concurrency: all state is guarded by mu. The single-flight gate is
// implemented with sync.Cond: when a refresh is in progress, concurrent
// callers wait on the cond and wake when the refresh completes, reusing the
// updated credential from the shared cache. This is strictly safer than a
// channel-based approach (no goroutine leaks, no "one reader wins" bugs).
type CredentialLifecycle struct {
	source  CredentialSource
	refresh RefreshFunc

	mu         sync.Mutex
	cond       *sync.Cond // broadcast when refresh completes
	cred       OAuthCredential
	ready      bool  // true once the first Resolve succeeded
	refreshing bool  // true while a refresh goroutine holds the gate
	lastErr    error // last refresh error, cleared on success
}

// NewCredentialLifecycle creates a lifecycle backed by the given source and
// refresh function. Resolution is deferred until the first FreshToken call.
func NewCredentialLifecycle(src CredentialSource, refreshFn RefreshFunc) *CredentialLifecycle {
	lc := &CredentialLifecycle{
		source:  src,
		refresh: refreshFn,
	}
	lc.cond = sync.NewCond(&lc.mu)
	return lc
}

// isFresh reports whether cred is currently valid and not about to expire.
// A zero ExpiresAtMS is treated as "never expires" (fresh if token present).
func isFresh(cred OAuthCredential) bool {
	if cred.AccessToken == "" {
		return false
	}
	if cred.ExpiresAtMS == 0 {
		return true
	}
	bufferMS := freshnessBuffer.Milliseconds()
	return time.Now().UnixMilli() < (cred.ExpiresAtMS - bufferMS)
}

// FreshToken returns a fresh access token, blocking if a refresh is needed.
//
// Algorithm:
//  1. Fast path: if cached credential is fresh, return it immediately.
//  2. If not yet resolved, call Resolve.
//  3. If resolved but stale and a refresh is already in flight, wait (via
//     sync.Cond) until the in-flight refresh completes, then return the
//     updated cached token.
//  4. If resolved but stale and no refresh is in flight, become the refresh
//     goroutine: call RefreshFunc, update the cache, write back, broadcast.
//  5. On refresh failure, return an error so the caller can signal Unavailable.
func (lc *CredentialLifecycle) FreshToken(ctx context.Context) (string, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	return lc.freshTokenLocked(ctx, false)
}

// freshTokenLocked implements FreshToken. MUST be called with lc.mu held.
// forceRefresh=true skips the freshness check (used by ReactiveRefreshAndRetry).
func (lc *CredentialLifecycle) freshTokenLocked(ctx context.Context, forceRefresh bool) (string, error) {
	// Ensure we've resolved at least once.
	if !lc.ready {
		lc.mu.Unlock()
		cred, err := lc.source.Resolve()
		lc.mu.Lock()
		if err != nil {
			return "", fmt.Errorf("credential lifecycle: resolve: %w", err)
		}
		lc.cred = cred
		lc.ready = true
	}

	// Fast path: fresh and no forced refresh needed.
	if !forceRefresh && isFresh(lc.cred) {
		return lc.cred.AccessToken, nil
	}

	// Re-resolve from source BEFORE attempting a token refresh. When an external
	// client (e.g. a vendor's official CLI) owns and rotates the credential store
	// on its own schedule, re-reading the source picks up that freshly-rotated
	// token without us touching the rotating refresh_token flow — which, with
	// single-use refresh tokens, (a) fails if the owner already consumed the
	// token, and (b) would itself rotate a credential we do not own, breaking the
	// owner. Prefer being a *reader* of an externally-maintained credential over
	// competing to refresh it.
	if !forceRefresh {
		lc.mu.Unlock()
		reCred, reErr := lc.source.Resolve()
		lc.mu.Lock()
		if reErr == nil && isFresh(reCred) {
			lc.cred = reCred
			slog.Debug("credential lifecycle: re-resolved fresh token from source (client rotated)",
				"expires_in", time.Until(time.UnixMilli(reCred.ExpiresAtMS)).Round(time.Second).String())
			return reCred.AccessToken, nil
		}
		slog.Debug("credential lifecycle: source has no fresher token; falling back to refresh_token flow",
			"resolve_err", reErr)
	}

	// Stale (or forced) and the source had nothing fresher. Wait if another
	// goroutine is already refreshing.
	for lc.refreshing {
		// Release mu and sleep until broadcast wakes us.
		// Check ctx to avoid blocking forever if the caller cancels.
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				lc.cond.Broadcast() // wake waiters so they can check ctx
			case <-done:
			}
		}()
		lc.cond.Wait()
		close(done)

		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// After waking, check if the refresh succeeded.
		if lc.lastErr == nil && (forceRefresh || isFresh(lc.cred)) {
			return lc.cred.AccessToken, nil
		}
		if lc.lastErr != nil {
			return "", fmt.Errorf("credential lifecycle: refresh: %w", lc.lastErr)
		}
	}

	// We are the designated refresh goroutine.
	if lc.cred.RefreshToken == "" {
		return "", fmt.Errorf("credential lifecycle: no refresh token available; re-authenticate")
	}

	lc.refreshing = true
	refreshToken := lc.cred.RefreshToken
	lc.mu.Unlock()

	newCred, err := lc.refresh(ctx, refreshToken)

	lc.mu.Lock()
	lc.refreshing = false
	lc.lastErr = err
	if err == nil {
		lc.cred = newCred
	}
	lc.cond.Broadcast()

	if err != nil {
		return "", fmt.Errorf("credential lifecycle: refresh: %w", err)
	}

	// Write back asynchronously — do not block the request.
	go func() { _ = lc.source.WriteBack(newCred) }()

	return newCred.AccessToken, nil
}

// ReactiveRefreshAndRetry is called by providers on HTTP 401. Forces one
// refresh attempt regardless of the current freshness state. If a refresh is
// already in flight (e.g. another goroutine racing on 401), joins that flight.
func (lc *CredentialLifecycle) ReactiveRefreshAndRetry(ctx context.Context) (string, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.freshTokenLocked(ctx, true /* forceRefresh */)
}

// ForceResolve re-reads the credential from the source. Useful after an
// external process updates the credential store, and in tests.
func (lc *CredentialLifecycle) ForceResolve() error {
	cred, err := lc.source.Resolve()
	if err != nil {
		return err
	}
	lc.mu.Lock()
	lc.cred = cred
	lc.ready = true
	lc.mu.Unlock()
	return nil
}
