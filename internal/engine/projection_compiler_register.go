// projection_compiler_register.go — D5 registration entry point.
//
// RegisterProjectionCompiler installs a single ProjectionCompiler instance
// into the pkg/substrate/reconcile global provider registry under the
// CompilerType key ("projection-compiler"). This is the parity counterpart
// to RegisterProjectionProviders for the lineage observatory.
//
// Daemon binaries that want the Projection Compiler driven by the
// ReconcileDaemon (ADR-095) call this from their providers wiring (e.g.
// cmd/cogos/providers_wire.go via internal/providers/daemon). Test binaries
// and isolated reconcile drivers can omit it.
//
// UpsertProvider semantics mean repeated calls are safe — useful for
// test resets via reconcile.ResetProviders.

package engine

import "github.com/myrgic/cogos/pkg/substrate/reconcile"

// RegisterProjectionCompiler installs the ProjectionCompiler in the global
// reconcile registry. Idempotent.
func RegisterProjectionCompiler() {
	reconcile.UpsertProvider(CompilerType, NewProjectionCompiler())
}
