// worktree_reconciler_register.go — boot-time registration of WorktreeReconciler.
//
// Per ADR-096 §5: one WorktreeReconciler instance is registered per managed
// repo root. v0 takes a single-repo path; multi-repo registration is a
// follow-up (ADR-096 Open Question 4 territory). RegisterWorktreeReconciler
// upserts a single provider for the given repo root, using filesystem-backed
// ledger adapters and the real-git GitAdapter unless explicit overrides are
// supplied.
//
// Production wiring lives in cmd/cogos/providers_wire.go and is invoked once
// the workspace root is resolved at boot.

package engine

import (
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// RegisterWorktreeReconciler registers a WorktreeReconciler instance for the
// given repo root with the global reconcile registry. Adapters may be nil;
// NewWorktreeReconciler fills them with the filesystem-backed defaults at
// construction (so the fields are never written at runtime — see the data-race
// note there).
//
// Safe to call multiple times for the same repo root (uses UpsertProvider).
func RegisterWorktreeReconciler(repoRoot string, reader LedgerReader, writer LedgerWriter, git GitAdapter) *WorktreeReconciler {
	r := NewWorktreeReconciler(repoRoot, reader, writer, git)
	reconcile.UpsertProvider(r.Type(), r)
	return r
}
