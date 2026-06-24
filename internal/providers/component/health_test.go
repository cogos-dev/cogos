package component

import (
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// TestHealthNilLoadRegistryIsHealthy verifies that the component provider reports
// a benign Synced/Healthy status when LoadRegistry is nil.
//
// In the daemon binary LoadRegistry is intentionally nil (it is wired only
// through the cog CLI DI seams), so the provider is inert: LoadConfig returns
// nil and the reconcile cycle reports "in sync" with nothing to observe. Health
// must mirror that state. Previously Health returned Degraded/Unknown here,
// which pinned the autonomic engine on a standing degraded=1 and triggered a
// needless LLM self-heal escalation on every reconcile cycle.
func TestHealthNilLoadRegistryIsHealthy(t *testing.T) {
	orig := LoadRegistry
	LoadRegistry = nil
	t.Cleanup(func() { LoadRegistry = orig })

	c := &ComponentProvider{}
	// LoadConfig sets the provider root; with LoadRegistry nil it returns
	// (nil, nil), the same no-op path the daemon takes.
	if _, err := c.LoadConfig(t.TempDir()); err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	st := c.Health()
	if st.Health != reconcile.HealthHealthy {
		t.Errorf("Health = %v, want Healthy (inert provider in daemon context)", st.Health)
	}
	if st.Sync != reconcile.SyncStatusSynced {
		t.Errorf("Sync = %v, want Synced", st.Sync)
	}
}
