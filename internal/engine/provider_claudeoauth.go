// provider_claudeoauth.go — ClaudeOAuthProvider
//
// Implements Provider against the Anthropic Messages API using the operator's
// managed Claude subscription OAuth bearer (the same credential Claude Code
// stores in ~/.claude/.credentials.json and the macOS keychain).
//
// Three parts:
//
//  1. claudeCodeCredentialSource — a CredentialSource that reads from (in
//     priority order) the macOS keychain, CLAUDE_CODE_OAUTH_TOKEN env var,
//     and ~/.claude/.credentials.json.  WriteBack writes atomically to the
//     JSON file only (the keychain is updated by the official client).
//
//  2. claudeOAuthRefreshFunc — a RefreshFunc that calls the Anthropic OAuth
//     token endpoint with the refresh token and returns a new OAuthCredential.
//
//  3. ClaudeOAuthProvider — the Provider implementation. It composes the
//     AnthropicProvider request builder and SSE parser with a CredentialLifecycle
//     for auth.  Available() does a lightweight GET /v1/models behind the
//     refresh wrapper.
//
// Gray-area notice (per RFC): this provider presents Claude Code's client
// identity to use the managed subscription outside the official CLI.  The
// operator has explicitly accepted this trade-off.  Mitigations: version is
// detected dynamically from the installed `claude` binary; the beta set is a
// single named constant; the subprocess client remains as a fallback.
//
// Reference implementation for exact values:
//   ~/.hermes/hermes-agent/agent/anthropic_adapter.py
//   functions: refresh_anthropic_oauth_pure, _write_claude_code_credentials,
//              _read_claude_code_credentials_from_keychain, read_claude_code_credentials,
//              is_claude_code_token_valid
//   constants: _COMMON_BETAS, _OAUTH_ONLY_BETAS, _CLAUDE_CODE_SYSTEM_PREFIX,
//              client_id ("9d1c250a-e61b-44d9-88ed-5944d1962f5e"),
//              token_endpoints (platform.claude.com → console.anthropic.com)
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── 429 backoff / CLI fallback tunables ───────────────────────────────────────
// The managed subscription rate-shapes usage over a rolling window; bursts
// beyond the instantaneous allowance are rejected with HTTP 429 when
// pay-as-you-go overage is disabled (RCA: project_claude_oauth_429_rca). On 429
// the provider respects retry-after, backs off a bounded number of times, then
// falls back to the CLI provider (same subscription via the official client).
const (
	oauthMax429Retries = 2
	oauthMax429Backoff = 8 * time.Second
)

// oauthRetryAfter returns how long to wait before retrying a 429. It honours an
// integer-seconds retry-after header when present, otherwise uses exponential
// backoff (1s, 2s, …) keyed on the attempt index.
func oauthRetryAfter(h http.Header, attempt int) time.Duration {
	if h != nil {
		if ra := strings.TrimSpace(h.Get("retry-after")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

// ── Gray-area constants (mirrored from the reference implementation) ──────────
// Source: ~/.hermes/hermes-agent/agent/anthropic_adapter.py
// These values are binding-specific and must not be moved into the generic
// CredentialLifecycle.

// claudeOAuthClientID is the OAuth client_id used by Claude Code.
// Source: refresh_anthropic_oauth_pure, local var client_id.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeOAuthTokenEndpoints is the ordered list of token endpoints to try.
// Source: refresh_anthropic_oauth_pure, local var token_endpoints.
var claudeOAuthTokenEndpoints = []string{
	"https://platform.claude.com/v1/oauth/token",
	"https://console.anthropic.com/v1/oauth/token",
}

// claudeCodeCommonBetas are beta flags sent on all OAuth requests.
// Source: _COMMON_BETAS.
// Note: "context-1m-2025-08-07" is intentionally excluded from the common set
// because some subscriptions reject it with HTTP 400. It is applied reactively
// if the provider detects a 1M-context request.
var claudeCodeCommonBetas = []string{
	"interleaved-thinking-2025-05-14",
	"fine-grained-tool-streaming-2025-05-14",
}

// claudeCodeOAuthOnlyBetas are additional beta flags required for OAuth auth.
// Source: _OAUTH_ONLY_BETAS.
var claudeCodeOAuthOnlyBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
}

// claudeCodeContext1MBeta is the long-context beta. Not included in the
// default beta set; added reactively on HTTP 400 "long context beta" errors.
// Source: _CONTEXT_1M_BETA.
const claudeCodeContext1MBeta = "context-1m-2025-08-07"

// claudeCodeSystemPrefix is prepended to every system prompt sent via OAuth.
// Source: _CLAUDE_CODE_SYSTEM_PREFIX.
const claudeCodeSystemPrefix = "You are Claude Code, Anthropic's official CLI for Claude."

// claudeCodeVersionFallback is used when the installed claude binary cannot
// be detected.  Source: _CLAUDE_CODE_VERSION_FALLBACK.
const claudeCodeVersionFallback = "2.1.74"

// ── Credential JSON shape ─────────────────────────────────────────────────────

// claudeCredentialsFile is the on-disk credential file path.
// Source: _write_claude_code_credentials.
var claudeCredentialsFile = filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")

// claudeAiOauthJSON is the nested structure stored under "claudeAiOauth" in
// ~/.claude/.credentials.json and in the macOS keychain entry
// "Claude Code-credentials". Field names match the reference implementation.
type claudeAiOauthJSON struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken,omitempty"`
	ExpiresAt    int64    `json:"expiresAt,omitempty"` // milliseconds since epoch
	Scopes       []string `json:"scopes,omitempty"`
}

type claudeCredentialsJSON struct {
	ClaudeAiOauth *claudeAiOauthJSON `json:"claudeAiOauth,omitempty"`
	// other top-level fields are preserved on write-back
}

// ── Version detection ─────────────────────────────────────────────────────────

var (
	claudeCodeVersionOnce  sync.Once
	claudeCodeVersionCache string
)

// detectClaudeCodeVersion returns the installed Claude Code version string,
// falling back to claudeCodeVersionFallback on any error.
// Source: _detect_claude_code_version.
func detectClaudeCodeVersion() string {
	for _, cmd := range []string{"claude", "claude-code"} {
		out, err := exec.Command(cmd, "--version").Output()
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(out))
		if s == "" {
			continue
		}
		// Output formats: "2.1.74 (Claude Code)" or just "2.1.74"
		parts := strings.Fields(s)
		if len(parts) > 0 && len(parts[0]) > 0 && parts[0][0] >= '0' && parts[0][0] <= '9' {
			return parts[0]
		}
	}
	return claudeCodeVersionFallback
}

func getClaudeCodeVersion() string {
	claudeCodeVersionOnce.Do(func() {
		claudeCodeVersionCache = detectClaudeCodeVersion()
	})
	return claudeCodeVersionCache
}

// ── claudeCodeCredentialSource ────────────────────────────────────────────────

// claudeCodeCredentialSource reads Claude Code OAuth credentials from the
// macOS keychain ("Claude Code-credentials"), CLAUDE_CODE_OAUTH_TOKEN env var,
// and ~/.claude/.credentials.json (in that priority order).
// WriteBack writes to ~/.claude/.credentials.json only (the keychain is managed
// by the official Claude Code client).
type claudeCodeCredentialSource struct {
	credPath string // defaults to claudeCredentialsFile; overridable in tests
}

func newClaudeCodeCredentialSource() *claudeCodeCredentialSource {
	return &claudeCodeCredentialSource{credPath: claudeCredentialsFile}
}

// Resolve reads the credential from the highest-priority available source.
// Source: read_claude_code_credentials (Python reference).
func (s *claudeCodeCredentialSource) Resolve() (OAuthCredential, error) {
	// 1. macOS keychain (Darwin only)
	if runtime.GOOS == "darwin" {
		if cred, ok := s.readKeychain(); ok {
			return cred, nil
		}
	}

	// 2. CLAUDE_CODE_OAUTH_TOKEN env var
	if tok := strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")); tok != "" {
		// Env var tokens have no known refresh path from the env alone.
		return OAuthCredential{AccessToken: tok}, nil
	}

	// 3. ~/.claude/.credentials.json
	if cred, ok := s.readCredFile(); ok {
		return cred, nil
	}

	return OAuthCredential{}, fmt.Errorf("claude-oauth: no credential found in keychain, env, or %s", s.credPath)
}

// readKeychain reads the "Claude Code-credentials" keychain entry on macOS.
// Source: _read_claude_code_credentials_from_keychain.
func (s *claudeCodeCredentialSource) readKeychain() (OAuthCredential, bool) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials",
		"-w").Output()
	if err != nil {
		return OAuthCredential{}, false
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return OAuthCredential{}, false
	}
	var top struct {
		ClaudeAiOauth *claudeAiOauthJSON `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil || top.ClaudeAiOauth == nil {
		return OAuthCredential{}, false
	}
	o := top.ClaudeAiOauth
	if o.AccessToken == "" {
		return OAuthCredential{}, false
	}
	return OAuthCredential{
		AccessToken:  o.AccessToken,
		RefreshToken: o.RefreshToken,
		ExpiresAtMS:  o.ExpiresAt,
	}, true
}

// readCredFile reads ~/.claude/.credentials.json.
// Source: read_claude_code_credentials (JSON file branch).
func (s *claudeCodeCredentialSource) readCredFile() (OAuthCredential, bool) {
	data, err := os.ReadFile(s.credPath)
	if err != nil {
		return OAuthCredential{}, false
	}
	var doc struct {
		ClaudeAiOauth *claudeAiOauthJSON `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.ClaudeAiOauth == nil {
		return OAuthCredential{}, false
	}
	o := doc.ClaudeAiOauth
	if o.AccessToken == "" {
		return OAuthCredential{}, false
	}
	return OAuthCredential{
		AccessToken:  o.AccessToken,
		RefreshToken: o.RefreshToken,
		ExpiresAtMS:  o.ExpiresAt,
	}, true
}

// WriteBack writes the refreshed credential back to ~/.claude/.credentials.json,
// preserving other top-level fields and the existing scopes field.
// Source: _write_claude_code_credentials (Python reference).
//
// Security: the temp file is created with O_EXCL at 0600, so it is never
// world-readable even briefly. os.Rename is atomic on POSIX.
func (s *claudeCodeCredentialSource) WriteBack(cred OAuthCredential) error {
	// Read existing file to preserve other top-level fields and scopes.
	existing := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(s.credPath); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	// Build the claudeAiOauth sub-object, preserving existing scopes if not
	// returned by the refresh response.
	oauthObj := claudeAiOauthJSON{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		ExpiresAt:    cred.ExpiresAtMS,
	}
	// Preserve scopes from the existing record.
	if raw, ok := existing["claudeAiOauth"]; ok {
		var prev claudeAiOauthJSON
		if err := json.Unmarshal(raw, &prev); err == nil {
			oauthObj.Scopes = prev.Scopes
		}
	}

	oauthRaw, err := json.Marshal(oauthObj)
	if err != nil {
		return fmt.Errorf("claude-oauth: marshal oauth: %w", err)
	}
	existing["claudeAiOauth"] = json.RawMessage(oauthRaw)

	newData, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("claude-oauth: marshal credentials: %w", err)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(s.credPath), 0700); err != nil {
		return fmt.Errorf("claude-oauth: mkdir: %w", err)
	}

	// Write to a temp file at 0600 then rename atomically.
	tmp := s.credPath + fmt.Sprintf(".tmp.%d", os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("claude-oauth: create tmp: %w", err)
	}
	_, writeErr := f.Write(newData)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if writeErr != nil {
			return fmt.Errorf("claude-oauth: write tmp: %w", writeErr)
		}
		if syncErr != nil {
			return fmt.Errorf("claude-oauth: sync tmp: %w", syncErr)
		}
		return fmt.Errorf("claude-oauth: close tmp: %w", closeErr)
	}

	if err := os.Rename(tmp, s.credPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("claude-oauth: rename: %w", err)
	}
	return nil
}

// ── claudeOAuthRefreshFunc ────────────────────────────────────────────────────

// claudeOAuthRefresh is the RefreshFunc for Claude Code OAuth.
// It tries claudeOAuthTokenEndpoints in order, using form-encoded body.
// Source: refresh_anthropic_oauth_pure (Python reference).
func claudeOAuthRefresh(ctx context.Context, refreshToken string) (OAuthCredential, error) {
	if refreshToken == "" {
		return OAuthCredential{}, fmt.Errorf("claude-oauth: refresh: empty refresh token")
	}

	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {claudeOAuthClientID},
	}
	body := formData.Encode()
	ua := fmt.Sprintf("claude-cli/%s (external, cli)", getClaudeCodeVersion())

	var lastErr error
	for _, endpoint := range claudeOAuthTokenEndpoints {
		reqBody := strings.NewReader(body)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reqBody)
		if err != nil {
			lastErr = err
			continue
		}
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		httpReq.Header.Set("User-Agent", ua)

		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			lastErr = err
			slog.Debug("claude-oauth: token refresh failed", "endpoint", endpoint, "err", err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("claude-oauth: token endpoint %s returned %d: %s",
				endpoint, resp.StatusCode, string(respBody))
			slog.Debug("claude-oauth: token refresh non-200", "endpoint", endpoint,
				"status", resp.StatusCode)
			continue
		}

		var result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"` // seconds
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			lastErr = fmt.Errorf("claude-oauth: parse token response: %w", err)
			continue
		}
		if result.AccessToken == "" {
			lastErr = fmt.Errorf("claude-oauth: token response missing access_token")
			continue
		}

		nextRefresh := result.RefreshToken
		if nextRefresh == "" {
			nextRefresh = refreshToken // keep the old one if not rotated
		}
		expiresIn := result.ExpiresIn
		if expiresIn == 0 {
			expiresIn = 3600
		}
		expiresAtMS := time.Now().UnixMilli() + expiresIn*1000

		return OAuthCredential{
			AccessToken:  result.AccessToken,
			RefreshToken: nextRefresh,
			ExpiresAtMS:  expiresAtMS,
		}, nil
	}

	if lastErr != nil {
		return OAuthCredential{}, fmt.Errorf("claude-oauth: all token endpoints failed: %w", lastErr)
	}
	return OAuthCredential{}, fmt.Errorf("claude-oauth: all token endpoints failed")
}

// ── ClaudeOAuthProvider ───────────────────────────────────────────────────────

// ClaudeOAuthProvider implements Provider against the Anthropic Messages API
// using an OAuth bearer credential (Claude Code managed subscription).
//
// It composes the existing AnthropicProvider's request builder and SSE parser,
// replacing only the auth layer with a CredentialLifecycle.
type ClaudeOAuthProvider struct {
	name      string
	model     string
	endpoint  string
	maxTokens int
	timeout   time.Duration
	client    *http.Client
	lc        *CredentialLifecycle

	// fallback is invoked on a persistent 429 (subscription burst rate-limit
	// with overage disabled). Typically a claude-code CLI provider that reaches
	// the same Max subscription via the official client. nil disables fallback.
	fallback Provider
}

// NewClaudeOAuthProvider creates a ClaudeOAuthProvider from a ProviderConfig.
// The credential source is initialised from the default credential store;
// no network calls are made until the first inference request. fallback (may be
// nil) is delegated to on a persistent 429.
func NewClaudeOAuthProvider(name string, cfg ProviderConfig, fallback Provider) *ClaudeOAuthProvider {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = anthropicDefaultEndpoint
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = anthropicDefaultMaxToks
	}

	src := newClaudeCodeCredentialSource()
	lc := NewCredentialLifecycle(src, claudeOAuthRefresh)

	return &ClaudeOAuthProvider{
		name:      name,
		model:     cfg.Model,
		endpoint:  strings.TrimRight(endpoint, "/"),
		maxTokens: maxTokens,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},
		lc:        lc,
		fallback:  fallback,
	}
}

// ── Provider interface ────────────────────────────────────────────────────────

func (p *ClaudeOAuthProvider) Name() string { return p.name }
func (p *ClaudeOAuthProvider) Model() string { return p.model }

// Available reports whether the provider can serve requests. It does a
// lightweight GET /v1/models behind the credential lifecycle so the token is
// refreshed if needed. Returns false on any error (credential missing, network
// unreachable, or non-2xx response).
func (p *ClaudeOAuthProvider) Available(ctx context.Context) bool {
	availCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	token, err := p.lc.FreshToken(availCtx)
	if err != nil {
		slog.Debug("claude-oauth: Available: credential unavailable", "err", err)
		return false
	}

	req, err := http.NewRequestWithContext(availCtx, http.MethodGet, p.endpoint+"/v1/models", nil)
	if err != nil {
		return false
	}
	p.setOAuthHeaders(req, token, false)

	resp, err := p.client.Do(req)
	if err != nil {
		slog.Debug("claude-oauth: Available: /v1/models probe failed", "err", err)
		return false
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Try a reactive refresh once.
		token, err = p.lc.ReactiveRefreshAndRetry(availCtx)
		if err != nil {
			return false
		}
		req2, _ := http.NewRequestWithContext(availCtx, http.MethodGet, p.endpoint+"/v1/models", nil)
		p.setOAuthHeaders(req2, token, false)
		resp2, err := p.client.Do(req2)
		if err != nil {
			return false
		}
		_ = resp2.Body.Close()
		return resp2.StatusCode >= 200 && resp2.StatusCode < 300
	}

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Capabilities returns what this provider supports.
func (p *ClaudeOAuthProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Capabilities: []Capability{
			CapStreaming, CapToolUse, CapVision, CapLongContext, CapJSON, CapCaching,
		},
		MaxContextTokens:   1_000_000, // 1M with context beta on eligible subscriptions
		MaxOutputTokens:    p.maxTokens,
		ModelsAvailable:    []string{p.model},
		IsLocal:            false,
		AgenticHarness:     false,
		CostPerInputToken:  0, // subscription pricing: no per-token cost signal
		CostPerOutputToken: 0,
	}
}

// Ping probes the Anthropic API and returns measured round-trip latency.
func (p *ClaudeOAuthProvider) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	token, err := p.lc.FreshToken(ctx)
	if err != nil {
		return 0, fmt.Errorf("claude-oauth: ping: credential: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+"/v1/models", nil)
	if err != nil {
		return 0, fmt.Errorf("claude-oauth: ping: build request: %w", err)
	}
	p.setOAuthHeaders(req, token, false)

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("claude-oauth: ping: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return 0, fmt.Errorf("claude-oauth: ping: 401 — credential rejected")
	}
	return time.Since(start), nil
}

// ── setOAuthHeaders ───────────────────────────────────────────────────────────

// setOAuthHeaders applies the Claude Code OAuth identity headers to req.
// with1MBeta adds the long-context beta header (not on by default because some
// subscriptions reject it with HTTP 400).
// Source: build_anthropic_client (OAuth branch), models.py:2342-2360.
func (p *ClaudeOAuthProvider) setOAuthHeaders(req *http.Request, token string, with1MBeta bool) {
	betas := append(append([]string{}, claudeCodeCommonBetas...), claudeCodeOAuthOnlyBetas...)
	if with1MBeta {
		betas = append(betas, claudeCodeContext1MBeta)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("anthropic-beta", strings.Join(betas, ","))
	req.Header.Set("User-Agent", fmt.Sprintf("claude-cli/%s (external, cli)", getClaudeCodeVersion()))
	req.Header.Set("x-app", "cli")
	req.Header.Set("Content-Type", "application/json")
}

// ── effectiveModel ────────────────────────────────────────────────────────────

func (p *ClaudeOAuthProvider) effectiveModel(req *CompletionRequest) string {
	if req.ModelOverride != "" {
		return req.ModelOverride
	}
	return p.model
}

// ── buildOAuthSystem ──────────────────────────────────────────────────────────

// buildOAuthSystem prepends the Claude Code system prefix to every request,
// then appends context items and the caller's system prompt.
// Source: _CLAUDE_CODE_SYSTEM_PREFIX usage in the reference implementation.
func buildOAuthSystem(req *CompletionRequest) string {
	var sb strings.Builder
	sb.WriteString(claudeCodeSystemPrefix)

	// Context items follow the prefix (labelled comment blocks).
	for _, item := range req.Context {
		if item.Content == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n\n<!-- context id=%q zone=%s salience=%.2f -->\n%s",
			item.ID, item.Zone, item.Salience, item.Content)
	}

	// Caller's system prompt appended last.
	if req.SystemPrompt != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(req.SystemPrompt)
	}
	return sb.String()
}

// ── Complete ──────────────────────────────────────────────────────────────────

// Complete sends a non-streaming request, handling proactive + reactive token
// refresh.
func (p *ClaudeOAuthProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	model := p.effectiveModel(req)

	payload := buildAnthropicRequest(model, req, false, p.maxTokens)
	// Override the system field to include the Claude Code prefix.
	payload.System = buildOAuthSystem(req)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: marshal request: %w", err)
	}

	// Proactive refresh.
	token, err := p.lc.FreshToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: credential unavailable: %w", err)
	}

	resp, err := p.doComplete(ctx, body, token, false)
	if err != nil {
		return nil, err
	}
	if resp.statusCode == http.StatusUnauthorized {
		// Reactive refresh — one retry.
		newToken, refreshErr := p.lc.ReactiveRefreshAndRetry(ctx)
		if refreshErr != nil {
			return nil, fmt.Errorf("claude-oauth: reactive refresh failed: %w", refreshErr)
		}
		token = newToken
		resp, err = p.doComplete(ctx, body, token, false)
		if err != nil {
			return nil, err
		}
	}

	// 429: respect retry-after, back off a bounded number of times, then fall
	// back to the CLI provider (same subscription via the official client).
	for attempt := 0; resp.statusCode == http.StatusTooManyRequests && attempt < oauthMax429Retries; attempt++ {
		wait := oauthRetryAfter(resp.header, attempt)
		if wait > oauthMax429Backoff {
			break // too long to wait inline — go to fallback
		}
		slog.Debug("claude-oauth: 429, backing off", "attempt", attempt+1, "wait", wait.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		resp, err = p.doComplete(ctx, body, token, false)
		if err != nil {
			return nil, err
		}
	}
	if resp.statusCode == http.StatusTooManyRequests {
		if p.fallback != nil {
			slog.Warn("claude-oauth: rate limited (429) after backoff; falling back to CLI",
				"fallback", p.fallback.Name())
			return p.fallback.Complete(ctx, req)
		}
		return nil, fmt.Errorf("claude-oauth: rate limited (429), no fallback configured: %s", resp.bodyText)
	}

	if resp.statusCode == http.StatusBadRequest {
		// 1M context beta reactive strip: some subscriptions reject it.
		if strings.Contains(resp.bodyText, "long context beta") {
			resp, err = p.doComplete(ctx, body, token, false /* with1MBeta already false */)
			if err != nil {
				return nil, err
			}
		}
	}

	if resp.statusCode != http.StatusOK {
		return nil, fmt.Errorf("claude-oauth: status %d: %s", resp.statusCode, resp.bodyText)
	}

	var ar anthropicResponse
	if err := json.Unmarshal([]byte(resp.bodyText), &ar); err != nil {
		return nil, fmt.Errorf("claude-oauth: decode response: %w", err)
	}
	return parseAnthropicResponse(&ar, model, p.name, time.Since(start)), nil
}

type oauthRespResult struct {
	statusCode int
	bodyText   string
	header     http.Header   // response headers (for retry-after on 429)
	body       io.ReadCloser // set for streaming paths
}

func (p *ClaudeOAuthProvider) doComplete(ctx context.Context, payload []byte, token string, with1MBeta bool) (*oauthRespResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: build request: %w", err)
	}
	p.setOAuthHeaders(httpReq, token, with1MBeta)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: http: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	return &oauthRespResult{
		statusCode: resp.StatusCode,
		bodyText:   string(data),
		header:     resp.Header,
	}, nil
}

// ── Stream ────────────────────────────────────────────────────────────────────

// Stream sends a streaming request, handling proactive + reactive token refresh.
// On HTTP 401, it refreshes once and retries.
// On HTTP 400 with "long context beta" error body, it strips that beta and retries.
func (p *ClaudeOAuthProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	model := p.effectiveModel(req)

	payload := buildAnthropicRequest(model, req, true, p.maxTokens)
	payload.System = buildOAuthSystem(req)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: marshal stream request: %w", err)
	}

	// Proactive refresh before the request.
	token, err := p.lc.FreshToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: credential unavailable: %w", err)
	}

	// Use a no-timeout client for streaming — generation can run long.
	streamClient := &http.Client{}

	resp, err := p.doStreamRequest(ctx, streamClient, body, token, false)
	if err != nil {
		return nil, err
	}

	// Reactive 401: refresh and retry before we start reading the stream.
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		newToken, refreshErr := p.lc.ReactiveRefreshAndRetry(ctx)
		if refreshErr != nil {
			return nil, fmt.Errorf("claude-oauth: reactive refresh failed: %w", refreshErr)
		}
		token = newToken
		resp, err = p.doStreamRequest(ctx, streamClient, body, token, false)
		if err != nil {
			return nil, err
		}
	}

	// 429: respect retry-after, back off a bounded number of times, then fall
	// back to the CLI provider (same subscription via the official client).
	for attempt := 0; resp.StatusCode == http.StatusTooManyRequests; attempt++ {
		wait := oauthRetryAfter(resp.Header, attempt)
		_ = resp.Body.Close()
		if attempt >= oauthMax429Retries || wait > oauthMax429Backoff {
			if p.fallback != nil {
				slog.Warn("claude-oauth: stream rate limited (429) after backoff; falling back to CLI",
					"fallback", p.fallback.Name())
				return p.fallback.Stream(ctx, req)
			}
			return nil, fmt.Errorf("claude-oauth: stream rate limited (429), no fallback configured")
		}
		slog.Debug("claude-oauth: stream 429, backing off", "attempt", attempt+1, "wait", wait.String())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		resp, err = p.doStreamRequest(ctx, streamClient, body, token, false)
		if err != nil {
			return nil, err
		}
	}

	// 400 with "long context beta" error: strip beta and retry.
	if resp.StatusCode == http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(errBody), "long context beta") {
			resp, err = p.doStreamRequest(ctx, streamClient, body, token, false)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("claude-oauth: stream status 400: %s", string(errBody))
		}
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("claude-oauth: stream status %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseAnthropicSSE(ctx, resp.Body, ch, model, p.name)
	}()
	return ch, nil
}

func (p *ClaudeOAuthProvider) doStreamRequest(ctx context.Context, client *http.Client, payload []byte, token string, with1MBeta bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: build stream request: %w", err)
	}
	p.setOAuthHeaders(httpReq, token, with1MBeta)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude-oauth: stream request: %w", err)
	}
	return resp, nil
}

// ── SSE re-use ────────────────────────────────────────────────────────────────
// ClaudeOAuthProvider reuses parseAnthropicSSE from provider_anthropic.go.
// The function is package-internal and accepts model/providerName strings,
// so we simply pass p.name and the effective model.
