// reconcile_state.go
// Thin re-export layer: state management delegates to pkg/reconcile.

package main

import "github.com/cogos-dev/cogos/pkg/reconcile"

// reconcileStatePath returns the path to a provider's state file.
func reconcileStatePath(root, resourceType string) string {
	return reconcile.StatePath(root, resourceType)
}

// LoadReconcileState loads the state file for a given resource type.
func LoadReconcileState(root, resourceType string) (*ReconcileState, error) {
	return reconcile.LoadState(root, resourceType)
}

// WriteReconcileState atomically writes the state file for a resource type.
func WriteReconcileState(root, resourceType string, state *ReconcileState) error {
	return reconcile.WriteState(root, resourceType, state)
}

// NewReconcileState creates a fresh state with a new lineage.
func NewReconcileState(resourceType string) *ReconcileState {
	return reconcile.NewState(resourceType)
}

// GenerateLineage creates a random hex string for state lineage tracking.
func GenerateLineage() string {
	return reconcile.GenerateLineage()
}
