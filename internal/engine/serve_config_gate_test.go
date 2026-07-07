// serve_config_gate_test.go — outside-in httptest coverage for the
// EnableConfigMutation gate on GET/PATCH /v1/config and POST
// /v1/config/rollback (L5-HTTP-AUTHZ, ledger L5).
//
// Covers:
//  1. Gate — 403 for all three routes when EnableConfigMutation=false (default).
//  2. Gated-on — 200 for all three routes when EnableConfigMutation=true.
//  3. Boot warning — non-loopback BindAddr with no auth token configured logs
//     a warning; loopback BindAddr does not.
package engine

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newConfigGateTestServer returns an HTTP handler backed by a Server whose
// EnableConfigMutation gate is set as requested. Uses a real temp workspace
// root (no kernel.yaml present) so ReadConfigSnapshot/WriteConfigPatch/
// RollbackConfig have a valid root to operate against.
func newConfigGateTestServer(t *testing.T, enableConfigMutation bool) http.Handler {
	t.Helper()
	root := t.TempDir()
	cfg := makeConfig(t, root)
	cfg.EnableConfigMutation = enableConfigMutation
	nucleus := makeNucleus("Test", "tester")
	proc := NewProcess(cfg, nucleus)

	srv := NewServer(cfg, nucleus, proc)
	t.Cleanup(func() {
		if b := proc.Broker(); b != nil {
			_ = b.Close()
		}
	})
	return srv.Handler()
}

// TestConfigMutation_GateDisabled verifies all three config routes return
// 403 with a "disabled" error when EnableConfigMutation is false (the
// default) — matching the EnableSkillExec / EnableServiceControl convention.
func TestConfigMutation_GateDisabled(t *testing.T) {
	t.Parallel()
	handler := newConfigGateTestServer(t, false)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"get", http.MethodGet, "/v1/config", ""},
		{"patch", http.MethodPatch, "/v1/config", `{"patch":{"port":7000}}`},
		{"rollback", http.MethodPost, "/v1/config/rollback", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s: status=%d body=%q; want 403", tc.method, tc.path, rec.Code, rec.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("%s %s: decode response: %v; body=%q", tc.method, tc.path, err, rec.Body.String())
			}
			if resp["error"] != "disabled" {
				t.Errorf("%s %s: error=%q; want \"disabled\"", tc.method, tc.path, resp["error"])
			}
			if !strings.Contains(resp["detail"], "enable_config_mutation") {
				t.Errorf("%s %s: detail=%q; want mention of enable_config_mutation", tc.method, tc.path, resp["detail"])
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s %s: content-type=%q; want application/json", tc.method, tc.path, ct)
			}
		})
	}
}

// TestConfigMutation_GateEnabled verifies all three config routes are
// reachable (not 403) when EnableConfigMutation is true.
func TestConfigMutation_GateEnabled(t *testing.T) {
	t.Parallel()
	handler := newConfigGateTestServer(t, true)

	t.Run("get", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/config: status=%d body=%q; want 200", rec.Code, rec.Body.String())
		}
		var snap map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Fatalf("decode snapshot: %v; body=%q", err, rec.Body.String())
		}
		if _, ok := snap["effective_config"]; !ok {
			t.Errorf("snapshot missing effective_config field: %v", snap)
		}
	})

	t.Run("patch", func(t *testing.T) {
		t.Parallel()
		body := bytes.NewReader([]byte(`{"patch":{"heartbeat_interval":45},"dry_run":true}`))
		req := httptest.NewRequest(http.MethodPatch, "/v1/config", body)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /v1/config: status=%d body=%q; want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("rollback", func(t *testing.T) {
		t.Parallel()
		body := bytes.NewReader([]byte(`{"list_only":true}`))
		req := httptest.NewRequest(http.MethodPost, "/v1/config/rollback", body)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		// No backups exist in a fresh temp workspace; list_only must still
		// reach the handler (not be gated) and return a structured result
		// rather than a 403.
		if rec.Code == http.StatusForbidden {
			t.Fatalf("POST /v1/config/rollback: got 403 with gate enabled; body=%q", rec.Body.String())
		}
	})
}

// ─── Boot bind-address warning ──────────────────────────────────────────────

// captureSlogTo builds a logger writing to a fresh buffer, calls fn with it,
// and returns everything logged. Unlike mutating slog.SetDefault, this never
// touches process-global state, so it's safe under t.Parallel() alongside
// any other test in the package that might also log.
func captureSlogTo(fn func(*slog.Logger)) string {
	var buf bytes.Buffer
	fn(slog.New(slog.NewTextHandler(&buf, nil)))
	return buf.String()
}

func TestWarnIfUnauthenticatedNonLoopback_WarnsOnNonLoopback(t *testing.T) {
	t.Parallel()
	cfg := &Config{BindAddr: "0.0.0.0", Port: 6931}
	out := captureSlogTo(func(l *slog.Logger) {
		warnIfUnauthenticatedNonLoopbackTo(l, cfg)
	})
	if !strings.Contains(out, "SECURITY") {
		t.Errorf("expected a SECURITY warning for non-loopback bind, got: %q", out)
	}
	if !strings.Contains(out, "0.0.0.0") {
		t.Errorf("expected the bind address in the warning, got: %q", out)
	}
}

func TestWarnIfUnauthenticatedNonLoopback_SilentOnLoopback(t *testing.T) {
	t.Parallel()
	cases := []string{"127.0.0.1", "localhost", "::1", ""}
	for _, addr := range cases {
		addr := addr
		t.Run("addr="+addr, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{BindAddr: addr, Port: 6931}
			out := captureSlogTo(func(l *slog.Logger) {
				warnIfUnauthenticatedNonLoopbackTo(l, cfg)
			})
			if strings.Contains(out, "SECURITY") {
				t.Errorf("expected no warning for loopback bind_addr=%q, got: %q", addr, out)
			}
		})
	}
}

func TestWarnIfUnauthenticatedNonLoopback_LANAddress(t *testing.T) {
	t.Parallel()
	cfg := &Config{BindAddr: "192.168.10.191", Port: 6931}
	out := captureSlogTo(func(l *slog.Logger) {
		warnIfUnauthenticatedNonLoopbackTo(l, cfg)
	})
	if !strings.Contains(out, "SECURITY") {
		t.Errorf("expected a SECURITY warning for LAN bind, got: %q", out)
	}
}

func TestIsLoopbackBindAddr(t *testing.T) {
	t.Parallel()
	loopback := []string{"127.0.0.1", "localhost", "::1", "127.5.5.5"}
	nonLoopback := []string{"0.0.0.0", "192.168.1.10", "10.0.0.5", ""}

	for _, addr := range loopback {
		if !isLoopbackBindAddr(addr) {
			t.Errorf("isLoopbackBindAddr(%q) = false; want true", addr)
		}
	}
	for _, addr := range nonLoopback {
		if isLoopbackBindAddr(addr) {
			t.Errorf("isLoopbackBindAddr(%q) = true; want false", addr)
		}
	}
}
