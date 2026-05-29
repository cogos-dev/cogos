// credential_lifecycle_test.go — CredentialLifecycle unit tests
//
// Tests cover:
//   - Validity buffer (fresh / stale boundary)
//   - Proactive refresh before a request when stale
//   - Reactive refresh on forced=true bypass
//   - Single-flight: N concurrent callers share one refresh call
//   - Write-back atomicity: mock source records write-back calls
//   - Error propagation: no refresh token, refresh HTTP error
//   - ForceResolve updates in-memory cache
package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// mockSource implements CredentialSource for tests.
type mockSource struct {
	mu           sync.Mutex
	cred         OAuthCredential
	resolveErr   error
	writeBackErr error
	resolveCalls int
	writeCalls   int
	written      []OAuthCredential
}

func (m *mockSource) Resolve() (OAuthCredential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolveCalls++
	return m.cred, m.resolveErr
}

func (m *mockSource) WriteBack(cred OAuthCredential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCalls++
	m.written = append(m.written, cred)
	return m.writeBackErr
}

// makeRefreshFunc returns a RefreshFunc that returns newCred after a short
// delay. It increments *calls on each invocation so tests can assert on the
// single-flight count.
func makeRefreshFunc(newCred OAuthCredential, calls *atomic.Int64, delay time.Duration, err error) RefreshFunc {
	return func(ctx context.Context, refreshToken string) (OAuthCredential, error) {
		calls.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return OAuthCredential{}, ctx.Err()
			}
		}
		if err != nil {
			return OAuthCredential{}, err
		}
		return newCred, nil
	}
}

// futureMS returns a Unix millisecond timestamp N seconds in the future.
func futureMS(n time.Duration) int64 {
	return time.Now().Add(n).UnixMilli()
}

// pastMS returns a Unix millisecond timestamp N seconds in the past.
func pastMS(n time.Duration) int64 {
	return time.Now().Add(-n).UnixMilli()
}

// ── isFresh ───────────────────────────────────────────────────────────────────

func TestIsFreshEmpty(t *testing.T) {
	t.Parallel()
	if isFresh(OAuthCredential{}) {
		t.Error("empty credential should not be fresh")
	}
}

func TestIsFreshNoExpiry(t *testing.T) {
	t.Parallel()
	cred := OAuthCredential{AccessToken: "tok", ExpiresAtMS: 0}
	if !isFresh(cred) {
		t.Error("token with no expiry should be fresh if access token is non-empty")
	}
}

func TestIsFreshFuture(t *testing.T) {
	t.Parallel()
	cred := OAuthCredential{
		AccessToken: "tok",
		ExpiresAtMS: futureMS(10 * time.Minute),
	}
	if !isFresh(cred) {
		t.Error("token expiring in 10m should be fresh")
	}
}

func TestIsFreshWithinBuffer(t *testing.T) {
	t.Parallel()
	// Expires in 30s — within the 60s buffer, so NOT fresh.
	cred := OAuthCredential{
		AccessToken: "tok",
		ExpiresAtMS: futureMS(30 * time.Second),
	}
	if isFresh(cred) {
		t.Error("token expiring in 30s (within 60s buffer) should NOT be fresh")
	}
}

func TestIsFreshExpired(t *testing.T) {
	t.Parallel()
	cred := OAuthCredential{
		AccessToken: "tok",
		ExpiresAtMS: pastMS(5 * time.Minute),
	}
	if isFresh(cred) {
		t.Error("expired token should not be fresh")
	}
}

func TestIsFreshBoundaryFresh(t *testing.T) {
	t.Parallel()
	// Exactly 61s in future → just outside buffer → fresh.
	cred := OAuthCredential{
		AccessToken: "tok",
		ExpiresAtMS: futureMS(61 * time.Second),
	}
	if !isFresh(cred) {
		t.Error("token expiring in 61s should be fresh (>60s buffer)")
	}
}

func TestIsFreshBoundaryStale(t *testing.T) {
	t.Parallel()
	// Exactly 59s in future → within buffer → stale.
	cred := OAuthCredential{
		AccessToken: "tok",
		ExpiresAtMS: futureMS(59 * time.Second),
	}
	if isFresh(cred) {
		t.Error("token expiring in 59s should be stale (within 60s buffer)")
	}
}

// ── FreshToken — basic paths ──────────────────────────────────────────────────

func TestFreshTokenFreshCached(t *testing.T) {
	t.Parallel()
	src := &mockSource{
		cred: OAuthCredential{
			AccessToken: "tok-fresh",
			ExpiresAtMS: futureMS(10 * time.Minute),
		},
	}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("FreshToken: %v", err)
	}
	if tok != "tok-fresh" {
		t.Errorf("token = %q; want tok-fresh", tok)
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d; want 1", src.resolveCalls)
	}
	if calls.Load() != 0 {
		t.Error("refresh should not be called when token is fresh")
	}
}

func TestFreshTokenCachedSecondCall(t *testing.T) {
	t.Parallel()
	src := &mockSource{
		cred: OAuthCredential{
			AccessToken: "tok",
			ExpiresAtMS: futureMS(10 * time.Minute),
		},
	}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 0, nil))

	// First call resolves from source.
	_, _ = lc.FreshToken(context.Background())
	// Second call should use the in-memory cache, not resolve again.
	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("second FreshToken: %v", err)
	}
	if tok != "tok" {
		t.Errorf("token = %q; want tok", tok)
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d; want 1 (cached on second call)", src.resolveCalls)
	}
}

func TestFreshTokenProactiveRefreshOnStale(t *testing.T) {
	t.Parallel()
	staleCred := OAuthCredential{
		AccessToken:  "old-tok",
		RefreshToken: "ref-tok",
		ExpiresAtMS:  pastMS(5 * time.Minute), // definitely expired
	}
	newCred := OAuthCredential{
		AccessToken: "new-tok",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("FreshToken: %v", err)
	}
	if tok != "new-tok" {
		t.Errorf("token = %q; want new-tok (proactively refreshed)", tok)
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want 1", calls.Load())
	}
}

func TestFreshTokenResolveError(t *testing.T) {
	t.Parallel()
	src := &mockSource{resolveErr: errors.New("keychain unavailable")}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 0, nil))

	_, err := lc.FreshToken(context.Background())
	if err == nil {
		t.Error("expected error when Resolve fails")
	}
	if !credContains(err.Error(), "resolve") {
		t.Errorf("error should mention 'resolve', got: %v", err)
	}
}

func TestFreshTokenNoRefreshToken(t *testing.T) {
	t.Parallel()
	staleCred := OAuthCredential{
		AccessToken:  "old-tok",
		RefreshToken: "", // no refresh token
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 0, nil))

	_, err := lc.FreshToken(context.Background())
	if err == nil {
		t.Error("expected error when no refresh token is available")
	}
}

func TestFreshTokenRefreshError(t *testing.T) {
	t.Parallel()
	staleCred := OAuthCredential{
		AccessToken:  "old-tok",
		RefreshToken: "ref-tok",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src,
		makeRefreshFunc(OAuthCredential{}, &calls, 0, errors.New("token endpoint unreachable")))

	_, err := lc.FreshToken(context.Background())
	if err == nil {
		t.Error("expected error when refresh fails")
	}
	if !credContains(err.Error(), "refresh") {
		t.Errorf("error should mention 'refresh', got: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want 1", calls.Load())
	}
}

// ── ReactiveRefreshAndRetry ───────────────────────────────────────────────────

func TestReactiveRefreshAndRetry(t *testing.T) {
	t.Parallel()
	// Token is fresh but we force a reactive refresh (simulating a 401).
	freshCred := OAuthCredential{
		AccessToken:  "old-tok",
		RefreshToken: "ref-tok",
		ExpiresAtMS:  futureMS(30 * time.Minute),
	}
	newCred := OAuthCredential{
		AccessToken: "reactive-tok",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	src := &mockSource{cred: freshCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 0, nil))

	// Load the fresh cred first.
	tok, err := lc.FreshToken(context.Background())
	if err != nil || tok != "old-tok" {
		t.Fatalf("initial FreshToken: tok=%q err=%v", tok, err)
	}

	// Reactive refresh (bypasses freshness check).
	tok, err = lc.ReactiveRefreshAndRetry(context.Background())
	if err != nil {
		t.Fatalf("ReactiveRefreshAndRetry: %v", err)
	}
	if tok != "reactive-tok" {
		t.Errorf("token = %q; want reactive-tok", tok)
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want 1", calls.Load())
	}

	// After reactive refresh, FreshToken should return the new token without
	// another network call.
	tok, err = lc.FreshToken(context.Background())
	if err != nil || tok != "reactive-tok" {
		t.Errorf("FreshToken after reactive refresh: tok=%q err=%v; want reactive-tok", tok, err)
	}
	if calls.Load() != 1 {
		t.Error("no additional refresh expected")
	}
}

// ── Single-flight ─────────────────────────────────────────────────────────────

func TestSingleFlightConcurrentRefresh(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	newCred := OAuthCredential{
		AccessToken: "new",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	// Use a 50 ms delay so goroutines pile up.
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 50*time.Millisecond, nil))

	const n = 20
	errs := make([]error, n)
	toks := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			toks[i], errs[i] = lc.FreshToken(context.Background())
		}()
	}
	wg.Wait()

	// All goroutines must succeed.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: FreshToken error: %v", i, err)
		}
	}
	// All must return the new token.
	for i, tok := range toks {
		if tok != "new" {
			t.Errorf("goroutine %d: token = %q; want new", i, tok)
		}
	}
	// Exactly one refresh call must have been made.
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want exactly 1 (single-flight)", calls.Load())
	}
}

// ── Write-back atomicity ──────────────────────────────────────────────────────

func TestWriteBackCalledAfterRefresh(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	newCred := OAuthCredential{
		AccessToken: "new",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil || tok != "new" {
		t.Fatalf("FreshToken: tok=%q err=%v", tok, err)
	}

	// Give the async write-back goroutine a moment to complete.
	time.Sleep(20 * time.Millisecond)

	src.mu.Lock()
	wc := src.writeCalls
	var written OAuthCredential
	if len(src.written) > 0 {
		written = src.written[0]
	}
	src.mu.Unlock()

	if wc != 1 {
		t.Errorf("WriteBack calls = %d; want 1", wc)
	}
	if written.AccessToken != "new" {
		t.Errorf("written access token = %q; want new", written.AccessToken)
	}
}

func TestWriteBackNotCalledOnRefreshError(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src,
		makeRefreshFunc(OAuthCredential{}, &calls, 0, errors.New("endpoint down")))

	_, err := lc.FreshToken(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	time.Sleep(20 * time.Millisecond)

	src.mu.Lock()
	wc := src.writeCalls
	src.mu.Unlock()
	if wc != 0 {
		t.Errorf("WriteBack calls = %d; want 0 on refresh error", wc)
	}
}

func TestWriteBackErrorDoesNotAbortRequest(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	newCred := OAuthCredential{
		AccessToken: "new",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	// WriteBack returns an error but the token should still be returned.
	src := &mockSource{
		cred:         staleCred,
		writeBackErr: errors.New("disk full"),
	}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("FreshToken should succeed even when WriteBack errors: %v", err)
	}
	if tok != "new" {
		t.Errorf("token = %q; want new", tok)
	}
}

// ── ForceResolve ──────────────────────────────────────────────────────────────

func TestForceResolveUpdatesCache(t *testing.T) {
	t.Parallel()

	src := &mockSource{
		cred: OAuthCredential{
			AccessToken: "old-tok",
			ExpiresAtMS: futureMS(10 * time.Minute),
		},
	}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 0, nil))

	tok, _ := lc.FreshToken(context.Background())
	if tok != "old-tok" {
		t.Fatalf("initial token = %q; want old-tok", tok)
	}

	// Simulate an external update of the credential store.
	src.mu.Lock()
	src.cred = OAuthCredential{
		AccessToken: "externally-updated",
		ExpiresAtMS: futureMS(10 * time.Minute),
	}
	src.mu.Unlock()

	if err := lc.ForceResolve(); err != nil {
		t.Fatalf("ForceResolve: %v", err)
	}

	tok, err := lc.FreshToken(context.Background())
	if err != nil || tok != "externally-updated" {
		t.Errorf("after ForceResolve: tok=%q err=%v; want externally-updated", tok, err)
	}
}

// ── Context cancellation ──────────────────────────────────────────────────────

func TestFreshTokenContextCancelled(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	// Refresh takes 200ms — we cancel after 10ms.
	lc := NewCredentialLifecycle(src, makeRefreshFunc(OAuthCredential{}, &calls, 200*time.Millisecond, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := lc.FreshToken(ctx)
	if err == nil {
		t.Error("expected error on context cancellation")
	}
}

// ── Single-flight: N waiters all get error when refresh fails ─────────────────

func TestSingleFlightAllWaitersGetError(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	refreshErr := errors.New("network down")
	lc := NewCredentialLifecycle(src,
		makeRefreshFunc(OAuthCredential{}, &calls, 30*time.Millisecond, refreshErr))

	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = lc.FreshToken(context.Background())
		}()
	}
	wg.Wait()

	errCount := 0
	for _, err := range errs {
		if err != nil {
			errCount++
		}
	}
	if errCount == 0 {
		t.Error("all goroutines should have received an error")
	}
	// Only one refresh call should have been attempted.
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want exactly 1 even on error", calls.Load())
	}
}

// ── Fresh-after-refresh does not re-refresh ───────────────────────────────────

func TestNoDoubleRefreshAfterSuccessfulRefresh(t *testing.T) {
	t.Parallel()

	staleCred := OAuthCredential{
		AccessToken:  "old",
		RefreshToken: "ref",
		ExpiresAtMS:  pastMS(5 * time.Minute),
	}
	newCred := OAuthCredential{
		AccessToken: "new",
		ExpiresAtMS: futureMS(60 * time.Minute),
	}
	src := &mockSource{cred: staleCred}
	var calls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(newCred, &calls, 0, nil))

	// Call FreshToken 5 times sequentially after the first refresh.
	for i := range 5 {
		tok, err := lc.FreshToken(context.Background())
		if err != nil || tok != "new" {
			t.Fatalf("call %d: tok=%q err=%v; want new", i, tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d; want 1 (no re-refresh)", calls.Load())
	}
}

// ── re-resolve before refresh ──────────────────────────────────────────────────

// sequencedSource returns a different credential on each Resolve call, so a test
// can simulate an external client rotating the credential store between reads.
type sequencedSource struct {
	mu      sync.Mutex
	creds   []OAuthCredential
	i       int
	calls   int
	written []OAuthCredential
}

func (s *sequencedSource) Resolve() (OAuthCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	c := s.creds[s.i]
	if s.i < len(s.creds)-1 {
		s.i++
	}
	return c, nil
}

func (s *sequencedSource) WriteBack(cred OAuthCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, cred)
	return nil
}

// When the cached token is stale but the source has since been refreshed by its
// external owner, FreshToken must re-resolve and return the source's fresh token
// WITHOUT calling the refresh_token flow.
func TestFreshTokenReResolvesBeforeRefresh(t *testing.T) {
	t.Parallel()
	stale := OAuthCredential{AccessToken: "stale", RefreshToken: "rt", ExpiresAtMS: pastMS(time.Minute)}
	fresh := OAuthCredential{AccessToken: "fresh-from-source", ExpiresAtMS: futureMS(10 * time.Minute)}
	src := &sequencedSource{creds: []OAuthCredential{stale, fresh}}

	var refreshCalls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(
		OAuthCredential{AccessToken: "refreshed", ExpiresAtMS: futureMS(time.Hour)}, &refreshCalls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "fresh-from-source" {
		t.Errorf("token = %q; want the re-resolved source token", tok)
	}
	if refreshCalls.Load() != 0 {
		t.Errorf("refresh_token flow called %d times; want 0 (re-resolve should satisfy)", refreshCalls.Load())
	}
}

// When BOTH the cached token and a re-resolve are stale, FreshToken falls through
// to the refresh_token flow.
func TestFreshTokenRefreshesWhenSourceAlsoStale(t *testing.T) {
	t.Parallel()
	stale := OAuthCredential{AccessToken: "stale", RefreshToken: "rt", ExpiresAtMS: pastMS(time.Minute)}
	src := &sequencedSource{creds: []OAuthCredential{stale}} // always stale

	var refreshCalls atomic.Int64
	lc := NewCredentialLifecycle(src, makeRefreshFunc(
		OAuthCredential{AccessToken: "refreshed", ExpiresAtMS: futureMS(time.Hour)}, &refreshCalls, 0, nil))

	tok, err := lc.FreshToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "refreshed" {
		t.Errorf("token = %q; want the refresh_token result when source has nothing fresh", tok)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("refresh_token flow called %d times; want 1", refreshCalls.Load())
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func credContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
