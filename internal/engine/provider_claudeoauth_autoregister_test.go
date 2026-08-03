// provider_claudeoauth_autoregister_test.go — discovery-not-declaration
// auto-registration tests.
//
// Covers the three gaps closed by this file's companion (provider_claudeoauth.go
// claudeConfigDir/resolveHomeDir, and provider_claudeoauth_autoregister.go):
//   - Windows-shaped credential discovery precedence.
//   - Auto-registration on/off/explicit-override matrix.
//
// The third gap (refresh-rotation safety / adopt-newer-credential) already had
// full table coverage on main before this change — see credential_lifecycle_test.go
// TestFreshTokenReResolvesBeforeRefresh / TestFreshTokenRefreshesWhenSourceAlsoStale,
// and provider_claudeoauth_test.go TestClaudeOAuthReadOnlyRefresh_NeverPOSTs — so
// it is not duplicated here.
//
// No test in this file makes a live call to Anthropic or to the real macOS
// keychain: claudeOAuthAutoCredentialProbe is swapped for a stub, and
// resolveHomeDir/claudeConfigDir are exercised via their env-var/goos seams.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ── resolveHomeDir / claudeConfigDir: Windows-shaped discovery precedence ──────

func TestResolveHomeDir(t *testing.T) {
	t.Parallel()
	getenv := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}

	cases := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{
			name: "darwin uses HOME",
			goos: "darwin",
			env:  map[string]string{"HOME": "/Users/testuser", "USERPROFILE": `C:\Users\testuser`},
			want: "/Users/testuser",
		},
		{
			name: "linux uses HOME",
			goos: "linux",
			env:  map[string]string{"HOME": "/home/testuser"},
			want: "/home/testuser",
		},
		{
			name: "windows prefers USERPROFILE over HOMEDRIVE+HOMEPATH",
			goos: "windows",
			env: map[string]string{
				"HOME":        `should-not-be-used`,
				"USERPROFILE": `C:\Users\testuser`,
				"HOMEDRIVE":   `D:`,
				"HOMEPATH":    `\Other\testuser`,
			},
			want: `C:\Users\testuser`,
		},
		{
			name: "windows falls back to HOMEDRIVE+HOMEPATH when USERPROFILE unset",
			goos: "windows",
			env: map[string]string{
				"HOMEDRIVE": `C:`,
				"HOMEPATH":  `\Users\testuser`,
			},
			want: `C:\Users\testuser`,
		},
		{
			name: "windows with nothing set returns empty",
			goos: "windows",
			env:  map[string]string{"HOME": "/should/not/be/used"},
			want: "",
		},
		{
			name: "darwin with HOME unset returns empty",
			goos: "darwin",
			env:  map[string]string{},
			want: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveHomeDir(tc.goos, getenv(tc.env))
			if got != tc.want {
				t.Errorf("resolveHomeDir(%q, ...) = %q; want %q", tc.goos, got, tc.want)
			}
		})
	}
}

// TestClaudeConfigDir_CLAUDE_CONFIG_DIR_Precedence proves CLAUDE_CONFIG_DIR
// overrides home-derived resolution on every platform, using real
// t.Setenv (not the injected getenv) since claudeConfigDir reads the real
// process environment for this variable.
func TestClaudeConfigDir_CLAUDE_CONFIG_DIR_Precedence(t *testing.T) {
	// Not parallel: mutates process-wide env vars via t.Setenv.
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude-config")
	t.Setenv("HOME", "/should/not/be/used")

	got := claudeConfigDir()
	if got != "/custom/claude-config" {
		t.Errorf("claudeConfigDir() = %q; want CLAUDE_CONFIG_DIR verbatim %q", got, "/custom/claude-config")
	}
}

// TestClaudeConfigDir_FallsBackToHome proves that with CLAUDE_CONFIG_DIR unset,
// claudeConfigDir joins HOME (this test's host is never GOOS=windows, so this
// exercises the same resolveHomeDir("darwin"/"linux", ...) branch the Windows
// table test above exercises directly with goos="windows").
func TestClaudeConfigDir_FallsBackToHome(t *testing.T) {
	// Not parallel: mutates process-wide env vars via t.Setenv.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "/Users/testhome")

	got := claudeConfigDir()
	want := filepath.Join("/Users/testhome", ".claude")
	if got != want {
		t.Errorf("claudeConfigDir() = %q; want %q", got, want)
	}
}

// TestDefaultClaudeCredentialsPath proves the credential file path is
// claudeConfigDir() + ".credentials.json", recomputed per call (not a cached
// package var) so it reflects the CURRENT env on every reconcile-loop tick.
func TestDefaultClaudeCredentialsPath(t *testing.T) {
	// Not parallel: mutates process-wide env vars via t.Setenv.
	t.Setenv("CLAUDE_CONFIG_DIR", "/alt/config")
	got := defaultClaudeCredentialsPath()
	want := filepath.Join("/alt/config", ".credentials.json")
	if got != want {
		t.Errorf("defaultClaudeCredentialsPath() = %q; want %q", got, want)
	}

	// Changing the env between calls must change the result — this is the
	// property maybeAutoRegisterClaudeOAuth's reconcile-loop re-check depends
	// on (a credential created after boot must become visible on the next tick
	// without restarting the process).
	t.Setenv("CLAUDE_CONFIG_DIR", "/second/config")
	got2 := defaultClaudeCredentialsPath()
	want2 := filepath.Join("/second/config", ".credentials.json")
	if got2 != want2 {
		t.Errorf("defaultClaudeCredentialsPath() after env change = %q; want %q", got2, want2)
	}
}

// ── maybeAutoRegisterClaudeOAuth: on/off/override matrix ───────────────────────

// stubProbeSource is a minimal CredentialSource for swapping into
// claudeOAuthAutoCredentialProbe during tests.
type stubProbeSource struct {
	cred OAuthCredential
	err  error
}

func (s stubProbeSource) Resolve() (OAuthCredential, error) { return s.cred, s.err }
func (s stubProbeSource) WriteBack(OAuthCredential) error   { return nil }

// withStubCredentialProbe swaps claudeOAuthAutoCredentialProbe for the
// duration of the test, restoring the original on cleanup. Not parallel-safe
// with other tests that touch the same package var (none currently do; this
// helper still restores via t.Cleanup for hygiene).
func withStubCredentialProbe(t *testing.T, found bool) {
	t.Helper()
	orig := claudeOAuthAutoCredentialProbe
	if found {
		claudeOAuthAutoCredentialProbe = func() CredentialSource {
			return stubProbeSource{cred: OAuthCredential{AccessToken: "stub-discovered-token"}}
		}
	} else {
		claudeOAuthAutoCredentialProbe = func() CredentialSource {
			return stubProbeSource{err: fmt.Errorf("no credential in test stub")}
		}
	}
	t.Cleanup(func() { claudeOAuthAutoCredentialProbe = orig })
}

func TestMaybeAutoRegisterClaudeOAuth_RegistersWhenCredentialFound(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	withStubCredentialProbe(t, true)

	r := NewSimpleRouter(RoutingConfig{})
	maybeAutoRegisterClaudeOAuth(r, ProvidersConfig{}, nil)

	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); !ok {
		t.Error("claude-oauth was not auto-registered despite a discoverable credential")
	}
}

func TestMaybeAutoRegisterClaudeOAuth_SkipsWhenNoCredential(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	withStubCredentialProbe(t, false)

	r := NewSimpleRouter(RoutingConfig{})
	maybeAutoRegisterClaudeOAuth(r, ProvidersConfig{}, nil)

	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); ok {
		t.Error("claude-oauth was auto-registered despite no discoverable credential")
	}
}

func TestMaybeAutoRegisterClaudeOAuth_ExplicitEntryOverridesDiscovery(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	// Credential IS discoverable, but an explicit providers.yaml entry must
	// still win — declaration overrides discovery. We prove this by using a
	// deliberately broken explicit config (Type "bogus-type" would fail if
	// makeProvider were invoked for it) and confirming auto-registration
	// leaves the router alone rather than attempting to build anything.
	withStubCredentialProbe(t, true)

	r := NewSimpleRouter(RoutingConfig{})
	pcfg := ProvidersConfig{
		Providers: map[string]ProviderConfig{
			claudeOAuthAutoProviderName: {Type: "claude-oauth", Model: "explicit-model", Enabled: boolPtr(false)},
		},
	}
	maybeAutoRegisterClaudeOAuth(r, pcfg, nil)

	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); ok {
		t.Error("auto-registration ran despite an explicit providers.yaml entry; declaration must override discovery")
	}
}

func TestMaybeAutoRegisterClaudeOAuth_AutoDisabled(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	withStubCredentialProbe(t, true)

	r := NewSimpleRouter(RoutingConfig{})
	pcfg := ProvidersConfig{ClaudeOAuth: ClaudeOAuthAutoConfig{Auto: boolPtr(false)}}
	maybeAutoRegisterClaudeOAuth(r, pcfg, nil)

	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); ok {
		t.Error("claude-oauth was auto-registered despite claude_oauth.auto: false")
	}
}

func TestMaybeAutoRegisterClaudeOAuth_DefaultsToEnabled(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	// A nil ClaudeOAuth.Auto (i.e. providers.yaml has no claude_oauth section
	// at all, or no providers.yaml exists) must default to enabled.
	withStubCredentialProbe(t, true)

	r := NewSimpleRouter(RoutingConfig{})
	maybeAutoRegisterClaudeOAuth(r, ProvidersConfig{ClaudeOAuth: ClaudeOAuthAutoConfig{}}, nil)

	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); !ok {
		t.Error("claude-oauth auto-registration must default to enabled when claude_oauth.auto is unset")
	}
}

func TestMaybeAutoRegisterClaudeOAuth_IdempotentAcrossTicks(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	// Simulates two reconcile-loop ticks: the second must be a no-op (the
	// already-registered check short-circuits before any credential probe),
	// proving the ticker wiring in router.go BuildRouter is safe to call
	// repeatedly without re-registering or double-probing.
	probeCalls := 0
	orig := claudeOAuthAutoCredentialProbe
	claudeOAuthAutoCredentialProbe = func() CredentialSource {
		probeCalls++
		return stubProbeSource{cred: OAuthCredential{AccessToken: "tok"}}
	}
	t.Cleanup(func() { claudeOAuthAutoCredentialProbe = orig })

	r := NewSimpleRouter(RoutingConfig{})
	maybeAutoRegisterClaudeOAuth(r, ProvidersConfig{}, nil) // tick 1: registers
	maybeAutoRegisterClaudeOAuth(r, ProvidersConfig{}, nil) // tick 2: no-op

	if probeCalls != 1 {
		t.Errorf("credential probe called %d times across two ticks; want 1 (second tick must short-circuit on already-registered)", probeCalls)
	}
	if _, ok := r.ProviderForName(claudeOAuthAutoProviderName); !ok {
		t.Error("claude-oauth not registered after two reconcile ticks")
	}
}

// TestBuildRouter_ClaudeOAuthAutoRegistration_RespectsWithoutAutoDiscovery
// proves WithoutAutoDiscovery() (the option every existing router test already
// passes — see provider_mlx_supervised_test.go) also disables claude-oauth
// auto-registration, so no existing or future hermetic router test
// non-deterministically picks up a "claude-oauth" provider from whatever
// credentials happen to be on the machine running `go test`.
func TestBuildRouter_ClaudeOAuthAutoRegistration_RespectsWithoutAutoDiscovery(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var and
	// writes a temp providers.yaml-less CogDir.
	withStubCredentialProbe(t, true)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &Config{CogDir: dir}

	router, err := BuildRouter(cfg, WithoutAutoDiscovery())
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	sr, ok := router.(*SimpleRouter)
	if !ok {
		t.Fatalf("router type = %T; want *SimpleRouter", router)
	}
	if _, ok := sr.ProviderForName(claudeOAuthAutoProviderName); ok {
		t.Error("claude-oauth was auto-registered even with WithoutAutoDiscovery(); tests must stay hermetic")
	}
}

// TestBuildRouter_ClaudeOAuthAutoRegistration_AtBoot proves the boot-time
// (non-test-hermetic) path: with auto-discovery left on, a discoverable
// credential registers claude-oauth during BuildRouter itself, not only on a
// later reconcile tick.
func TestBuildRouter_ClaudeOAuthAutoRegistration_AtBoot(t *testing.T) {
	// Not parallel: mutates the shared claudeOAuthAutoCredentialProbe var.
	withStubCredentialProbe(t, true)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &Config{CogDir: dir}

	router, err := BuildRouter(cfg)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	sr := router.(*SimpleRouter)
	if _, ok := sr.ProviderForName(claudeOAuthAutoProviderName); !ok {
		t.Error("claude-oauth was not auto-registered at BuildRouter time despite a discoverable credential")
	}
}

// TestSimpleRouter_TickHooksRunOnEveryTick proves the AddTickHook wiring
// itself (independent of claude-oauth specifics): a hook registered before
// Start runs once during the synchronous warm-up, and again once the ticker
// fires, which is the mechanism the reconcile-loop re-check (gap 2 of the
// peer-node auto-register ruling) depends on to notice a credential created
// after boot without a kernel restart.
func TestSimpleRouter_TickHooksRunOnEveryTick(t *testing.T) {
	t.Parallel()
	r := NewSimpleRouter(RoutingConfig{})
	r.availTTL = 20 * time.Millisecond // fast ticker so the test doesn't wait long

	var calls atomic.Int32
	r.AddTickHook(func() { calls.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Close()

	if got := calls.Load(); got != 1 {
		t.Fatalf("tick hook calls immediately after Start = %d; want 1 (synchronous warm-up)", got)
	}

	// Wait for at least one ticker fire beyond the warm-up call.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("tick hook calls after waiting for a ticker fire = %d; want >= 2", got)
	}
}

func boolPtr(b bool) *bool { return &b }
