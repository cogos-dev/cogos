package testkernel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/testkernel"
)

// TestTestKernel_BootsAndStops is the ADR-101 Phase 1 smoke test.
// It proves that:
//  1. Boot returns a Kernel handle without error.
//  2. The kernel's HTTP server accepts connections and returns a valid
//     /health response with status "ok".
//  3. Stop shuts down cleanly without error.
//
// Note: this test runs against whatever Reconcilable providers are globally
// registered at call time — none in the test binary (RegisterProviders is nil).
// The reconcile daemon operates on an empty provider set, which is correct for
// Phase 1. Phase 2 (WithIsolatedRegistry) is the mechanism for injecting real
// providers in parallel-safe tests.
func TestTestKernel_BootsAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t)
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	// Verify Endpoint is non-empty and looks like an HTTP URL.
	endpoint := k.Endpoint()
	if endpoint == "" {
		t.Fatal("Endpoint() returned empty string")
	}
	if len(endpoint) < 7 || endpoint[:7] != "http://" {
		t.Fatalf("Endpoint() = %q; want http://... prefix", endpoint)
	}

	// Probe /health.
	healthURL := endpoint + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		t.Fatalf("build health request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d; want 200", resp.StatusCode)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("/health status = %q; want \"ok\"", body.Status)
	}

	// WorkspaceRoot must be non-empty (it's a temp dir allocated by Boot).
	if k.WorkspaceRoot() == "" {
		t.Error("WorkspaceRoot() returned empty string")
	}

	// Stop is called by t.Cleanup above; if it errors the test will report it.
}
