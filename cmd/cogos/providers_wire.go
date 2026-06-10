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
	"github.com/myrgic/cogos/internal/eval"
	"github.com/myrgic/cogos/internal/providers/all"
	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
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
}
