// projection_reconciler_convergence_test.go — regression test for the lineage
// projection convergence fix. The reconciler must treat two projections that
// differ ONLY in their "_Last generated:" timestamp line as equal, otherwise it
// rewrites the 6 lineage projections on every reconcile cycle forever.
package engine

import "testing"

func TestConvergenceIgnoresTimestampLine(t *testing.T) {
	a := []byte("# Lineage\n\n_Last generated: 2026-06-05T10:00:00Z_\n\nnode-a -> node-b\n")
	b := []byte("# Lineage\n\n_Last generated: 2026-06-05T10:00:30Z_\n\nnode-a -> node-b\n")

	if !equalIgnoringTimestamp(a, b) {
		t.Fatalf("expected projections differing only in the _Last generated: line to compare EQUAL, but they did not — reconciler would never converge")
	}
}

func TestConvergenceDetectsRealContentDifference(t *testing.T) {
	a := []byte("# Lineage\n\n_Last generated: 2026-06-05T10:00:00Z_\n\nnode-a -> node-b\n")
	b := []byte("# Lineage\n\n_Last generated: 2026-06-05T10:00:00Z_\n\nnode-a -> node-c\n")

	if equalIgnoringTimestamp(a, b) {
		t.Fatalf("expected projections with a real content difference to compare UNEQUAL, but they compared equal — a genuine change would never be projected")
	}
}
