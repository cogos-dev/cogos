// types_shared.go — Cross-cutting type aliases for the root package.
//
// These aliases were historically scattered across feature-specific files
// (cogfield_buses.go, serve_*.go, bus_api.go). When the root serveServer web
// was deleted in Track 5, the aliases were consolidated here so the
// remaining library packages (bus_session, bus_watch, reactor, etc.) still
// compile.
//
// Canonical types live in pkg/cogfield; this file only re-exports them
// under the legacy short names still used across the root package.

package main

import (
	"github.com/myrgic/cogos/pkg/substrate/cogfield"
)

// CogBlock is the Messages API content block — the canonical wire format
// for bus events. Defined in pkg/cogfield.
type CogBlock = cogfield.Block

// BusEventData is a legacy alias for cogfield.Block kept for tests that
// historically returned the bus-event shape under this name.
type BusEventData = cogfield.Block

// busRegistryEntry is the registry.json row for the bus directory. It is
// aliased from pkg/cogfield so the root-package bus plumbing
// (bus_session, bus_watch, cmd_bus) can keep its legacy names.
type busRegistryEntry = cogfield.BusRegistryEntry

// busSendRequest is the JSON body for POST /v1/bus/send. Kept at the
// root so the CLI bus-send client (cmd_bus.go), its tests, and the HTTP
// contract test (bus_api_contract_test.go) stay in lockstep with the
// engine's /v1/bus/send handler.
type busSendRequest struct {
	BusID   string `json:"bus_id"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"` // event type, defaults to "message"
}

// busSendResponse is the JSON returned by POST /v1/bus/send. The type is
// kept at the root because CLI helpers (cmd_bus.go) decode /v1/bus/send
// responses into it, and the HTTP contract test (bus_api_contract_test.go)
// pins its field set.
type busSendResponse struct {
	OK   bool   `json:"ok"`
	Seq  int    `json:"seq"`
	Hash string `json:"hash"`
}

// busEventEnvelope is the SSE envelope format expected by the CogBus event
// monitor. bus_watch.go still decodes this shape when the local kernel is
// asked to proxy a remote SSE feed.
type busEventEnvelope struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp string    `json:"timestamp"`
	Data      *CogBlock `json:"data"`
}

// === SERVE CONSTANTS ===
//
// Kept at the root so cog.go (cmdClaude) can still point ANTHROPIC_BASE_URL
// at the kernel, and inference.go can still probe the installed-subagent
// CLIs. The binary that actually serves these ports now lives in
// cmd/cogos (engine-backed) — these are just the conventional values.

const (
	// defaultServePort is the kernel HTTP port. Registered at
	// cog://conf/ports#kernel.
	defaultServePort = 6931

	// claudeCommand is the CLI name probed by cmdClaude/cmdInfer.
	claudeCommand = "claude"

	// codexCommand is the CLI name probed by cmdInfer for the Codex provider.
	codexCommand = "codex"
)
