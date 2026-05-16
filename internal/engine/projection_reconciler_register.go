// projection_reconciler_register.go
// init-time registration of the six canonical lineage-observatory
// ProjectionReconciler instances with the global reconcile registry.
//
// D5 — "Register the 6 projection instances + their generation rules."
// ADR-094 §5: each projection kind maps to one reconcile.Reconcilable.

package engine

func init() {
	RegisterProjectionProviders()
}
