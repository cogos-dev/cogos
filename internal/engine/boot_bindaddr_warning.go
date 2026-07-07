// boot_bindaddr_warning.go — non-loopback bind-address warning (L5-HTTP-AUTHZ).
//
// The kernel's HTTP surface has no bearer/API-key auth middleware (see
// serve.go); its access-control boundary is "bind to loopback only" plus a
// handful of opt-in-default-off gates (EnableSkillExec, EnableServiceControl,
// EnableConfigMutation). That boundary only holds if the operator keeps
// Config.BindAddr on loopback. cfg.BindAddr is a supported config surface
// that can be set to "0.0.0.0" (or any non-loopback address) for pod/LAN/
// Tailnet deployments, and the kernel currently has no auth-token config
// field at all — so widening the bind address removes the *only* access
// control that exists, with no compensating control available today.
//
// Refuse-to-start is an explicit later decision (tracked in the v1.0
// alignment ledger, L5). For now: warn loudly at boot so the gap is visible
// in logs rather than silent.
package engine

import (
	"log/slog"
	"net"
)

// hasHTTPAuthToken reports whether the kernel config carries an HTTP
// bearer/API-key auth field. There is currently no such field anywhere in
// Config — this always returns false. It exists as a named seam so that
// when an auth-token field is added, this function (and the warning it
// gates) gets updated in the same change rather than silently going stale.
func hasHTTPAuthToken(cfg *Config) bool {
	_ = cfg
	return false
}

// warnIfUnauthenticatedNonLoopback logs a loud warning at boot when
// cfg.BindAddr is resolved to a non-loopback address and no HTTP auth token
// is configured. It never blocks startup — refuse-to-start is a later
// decision (L5-HTTP-AUTHZ). Logs via slog.Default(); see
// warnIfUnauthenticatedNonLoopbackTo for a variant that takes an explicit
// logger (used by tests to avoid mutating global slog state).
func warnIfUnauthenticatedNonLoopback(cfg *Config) {
	warnIfUnauthenticatedNonLoopbackTo(slog.Default(), cfg)
}

// warnIfUnauthenticatedNonLoopbackTo is the logger-injectable core of
// warnIfUnauthenticatedNonLoopback. Split out so tests can assert on the
// emitted warning without racing on the process-global slog default (see
// serve_config_gate_test.go).
func warnIfUnauthenticatedNonLoopbackTo(logger *slog.Logger, cfg *Config) {
	if cfg == nil || logger == nil {
		return
	}
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	if isLoopbackBindAddr(bindAddr) {
		return
	}
	if hasHTTPAuthToken(cfg) {
		return
	}
	logger.Warn("SECURITY: kernel HTTP API is binding to a non-loopback address with no auth token configured — "+
		"the entire HTTP surface (including config mutation, skill exec, and service control when enabled) "+
		"is reachable by anything that can reach this address/port; loopback-bind is currently the kernel's "+
		"only access-control boundary",
		"bind_addr", bindAddr, "port", cfg.Port)
}

// isLoopbackBindAddr reports whether addr (a Config.BindAddr value — a bare
// host, not host:port) resolves to a loopback address. Handles the literal
// forms the kernel actually accepts: "127.0.0.1", "localhost", "::1", and
// any other address net.ParseIP recognizes as loopback. Unparseable or
// unresolved hostnames are treated as non-loopback (fail toward warning,
// not toward silence).
func isLoopbackBindAddr(addr string) bool {
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
