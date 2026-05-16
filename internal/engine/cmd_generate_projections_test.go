//go:build generate_projections

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateProjections runs the reconciler against the real workspace.
// Invoked with: go test ./internal/engine/ -run TestGenerateProjections -tags generate_projections
func TestGenerateProjections(t *testing.T) {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, "workspaces", "cog")

	ctx := context.Background()

	for _, kind := range AllProjectionKinds {
		rec := NewProjectionReconciler(kind)

		cfgAny, err := rec.LoadConfig(root)
		if err != nil {
			t.Logf("LoadConfig %s: %v", kind, err)
			continue
		}

		liveAny, err := rec.FetchLive(ctx, cfgAny)
		if err != nil {
			t.Logf("FetchLive %s: %v", kind, err)
			continue
		}

		plan, err := rec.ComputePlan(cfgAny, liveAny, nil)
		if err != nil {
			t.Logf("ComputePlan %s: %v", kind, err)
			continue
		}

		_, err = rec.ApplyPlan(ctx, plan)
		if err != nil {
			t.Errorf("ApplyPlan %s: %v", kind, err)
			continue
		}

		fmt.Printf("Generated projection: %s\n", kind)
	}
}
