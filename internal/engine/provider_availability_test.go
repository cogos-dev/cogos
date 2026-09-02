// provider_availability_test.go — tests for honest Available() on the
// claude-code and codex providers: binary presence alone must not report
// available; the auth probe must pass too.
package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── codex ────────────────────────────────────────────────────────────────────

func codexOnPath(t *testing.T) {
	t.Helper()
	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) { return "/usr/local/bin/codex", nil }
}

func TestCodexAvailableBinaryPresentAuthGood(t *testing.T) {
	resetCodexResolverForTest(t)
	codexOnPath(t)
	codexAuthProbe = func(ctx context.Context, binary string) error { return nil }

	p := NewCodexProvider("codex", ProviderConfig{})
	if !p.Available(context.Background()) {
		t.Fatal("Available should be true with binary present and auth good")
	}
}

func TestCodexAvailableBinaryPresentAuthBad(t *testing.T) {
	resetCodexResolverForTest(t)
	codexOnPath(t)
	codexAuthProbe = func(ctx context.Context, binary string) error {
		return errors.New("not logged in")
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when the binary exists but auth fails")
	}
}

func TestCodexAvailableBinaryAbsent(t *testing.T) {
	resetCodexResolverForTest(t)
	codexGOOS = "linux"
	codexLookPath = func(file string) (string, error) { return "", errors.New("not found") }
	codexAuthProbe = func(ctx context.Context, binary string) error {
		t.Fatal("auth probe must not run when the binary is absent")
		return nil
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when the binary is absent")
	}
}

func TestCodexAvailableCachesResult(t *testing.T) {
	resetCodexResolverForTest(t)
	codexOnPath(t)
	calls := 0
	codexAuthProbe = func(ctx context.Context, binary string) error {
		calls++
		return nil
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	for i := 0; i < 5; i++ {
		if !p.Available(context.Background()) {
			t.Fatal("Available should be true")
		}
	}
	if calls != 1 {
		t.Fatalf("auth probe ran %d times within TTL, want 1", calls)
	}

	// Expire the cache; the probe must run again.
	p.availAt = time.Now().Add(-availCacheTTL - time.Second)
	p.Available(context.Background())
	if calls != 2 {
		t.Fatalf("auth probe ran %d times after TTL expiry, want 2", calls)
	}
}

// ── claude-code ──────────────────────────────────────────────────────────────

func stubClaudeAuthProbe(t *testing.T, out []byte, err error) *int {
	t.Helper()
	old := claudeCodeAuthProbe
	t.Cleanup(func() { claudeCodeAuthProbe = old })
	calls := new(int)
	claudeCodeAuthProbe = func(ctx context.Context, binary string) ([]byte, error) {
		*calls++
		return out, err
	}
	return calls
}

// claudeCodeTestProvider returns a provider whose cliBinary resolves on any
// host ("sh" is on PATH everywhere the tests run) so LookPath passes and the
// auth probe decides the outcome.
func claudeCodeTestProvider() *ClaudeCodeProvider {
	return NewClaudeCodeProvider("claude-code", ProviderConfig{Endpoint: "sh"}, nil)
}

func TestClaudeCodeAvailableBinaryPresentAuthGood(t *testing.T) {
	stubClaudeAuthProbe(t, []byte(`{"loggedIn":true}`), nil)
	p := claudeCodeTestProvider()
	if !p.Available(context.Background()) {
		t.Fatal("Available should be true with binary present and loggedIn=true")
	}
}

func TestClaudeCodeAvailableBinaryPresentAuthLoggedOut(t *testing.T) {
	stubClaudeAuthProbe(t, []byte(`{"loggedIn":false}`), nil)
	p := claudeCodeTestProvider()
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when auth status reports loggedIn=false")
	}
}

func TestClaudeCodeAvailableBinaryPresentProbeError(t *testing.T) {
	stubClaudeAuthProbe(t, nil, errors.New("exit status 1"))
	p := claudeCodeTestProvider()
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when the auth probe errors")
	}
}

func TestClaudeCodeAvailableBinaryPresentProbeGarbage(t *testing.T) {
	stubClaudeAuthProbe(t, []byte("not json"), nil)
	p := claudeCodeTestProvider()
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when the auth probe output is unparseable")
	}
}

func TestClaudeCodeAvailableBinaryAbsent(t *testing.T) {
	calls := stubClaudeAuthProbe(t, []byte(`{"loggedIn":true}`), nil)
	p := NewClaudeCodeProvider("claude-code", ProviderConfig{Endpoint: "definitely-not-a-real-binary-xyz"}, nil)
	if p.Available(context.Background()) {
		t.Fatal("Available must be false when the binary is absent")
	}
	if *calls != 0 {
		t.Fatal("auth probe must not run when the binary is absent")
	}
}

func TestClaudeCodeAvailableCachesResult(t *testing.T) {
	calls := stubClaudeAuthProbe(t, []byte(`{"loggedIn":true}`), nil)
	p := claudeCodeTestProvider()
	for i := 0; i < 5; i++ {
		if !p.Available(context.Background()) {
			t.Fatal("Available should be true")
		}
	}
	if *calls != 1 {
		t.Fatalf("auth probe ran %d times within TTL, want 1", *calls)
	}

	p.availAt = time.Now().Add(-availCacheTTL - time.Second)
	p.Available(context.Background())
	if *calls != 2 {
		t.Fatalf("auth probe ran %d times after TTL expiry, want 2", *calls)
	}
}
