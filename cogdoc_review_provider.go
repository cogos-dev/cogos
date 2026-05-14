// cogdoc_review_provider.go
// T05: Wire CogdocReviewReconciler into the reconcile registry.
//
// The CogdocReviewReconciler registers as resource type "cogdoc_review".
// It is an always-on Reconcilable in the kernel's reconcile loop.
//
// The reconciler requires the workspace root. It reads it from the kernel's
// workspace path resolution (same pattern as other providers that need root).
// If COGOS_WORKSPACE is set, that wins; otherwise falls back to the current
// working directory's git root (best-effort).

package main

import (
	"os"
	"path/filepath"

	"github.com/myrgic/cogos/pkg/cogdoc_review"
)

func init() {
	root := cogdocReviewWorkspaceRoot()
	RegisterProvider("cogdoc_review", cogdoc_review.NewCogdocReviewReconciler(root))
}

// cogdocReviewWorkspaceRoot returns the workspace root for the cogdoc review
// Reconciler. Preference order:
//  1. COGOS_WORKSPACE env var
//  2. Current working directory
func cogdocReviewWorkspaceRoot() string {
	if ws := os.Getenv("COGOS_WORKSPACE"); ws != "" {
		return filepath.Clean(ws)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}
