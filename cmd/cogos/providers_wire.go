// providers_wire.go — thin caller that applies all production provider and
// MCP-extension registrations into the kernel daemon at boot.
//
// The logic formerly in this file has been extracted to
// internal/providers/all so it is importable by test helpers outside this
// package main.  This file keeps the package-level vars (daemonEvalProvider,
// daemonHarnessRegistry) that are referenced by binary-assembly tests in
// this directory, and its init() delegates to all.Register.
//
// See internal/providers/all/all.go for the full wiring description.
package main

import (
	"log/slog"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/internal/eval"
	"github.com/myrgic/cogos/internal/providers/all"
	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
	"github.com/myrgic/cogos/sdk/constellation"
)

// daemonEvalProvider is the daemon-side EvalProvider instance. The daemon does
// not run plan/apply; it only exposes the four read/trigger eval MCP tools
// whose state effects are read by the CLI's reconcile loop.
var daemonEvalProvider = eval.New(nil, nil)

// daemonHarnessRegistry is the in-memory RBAC harness-binding store.
// It satisfies engine.HarnessAttacher so cog_register_session can create
// HarnessBindingCRDs for sessions that supply a "subject" field.
var daemonHarnessRegistry = subidentity.NewHarnessRegistry()

func init() {
	all.Register(daemonEvalProvider, daemonHarnessRegistry)

	// Wire the constellation indexer so CogDocService.WriteAndSync / PatchAndSync
	// perform an eager per-file FTS upsert and the lazy drift-repair path in
	// searchMemoryFTSDriftRepair can call IndexFile without importing
	// sdk/constellation from internal/engine (package-boundary guard).
	//
	// constellation.Open is called once at daemon boot with the resolved workspace
	// root.  The sdk/constellation package may maintain its own internal connection
	// pool; the returned handle is long-lived for the process lifetime.
	engine.WireConstellationIndexer = func(s *engine.Server) {
		c, err := constellation.Open(s.WorkspaceRoot())
		if err != nil {
			slog.Warn("constellation: failed to open indexer for FTS wiring; eager upsert disabled", "err", err)
			return
		}
		s.SetConstellationIndexer(c)
	}
}
