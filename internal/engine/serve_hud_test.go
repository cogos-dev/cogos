package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleHUDState verifies that GET /v1/hud/state returns 200 with the
// expected top-level fields and requires no inference calls.
func TestHandleHUDState(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/hud/state", nil)
	w := httptest.NewRecorder()

	srv.handleHUDState(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var hud HUDState
	if err := json.NewDecoder(resp.Body).Decode(&hud); err != nil {
		t.Fatalf("decode HUDState: %v", err)
	}

	// Identity must include a node_id.
	if hud.Identity.NodeID == "" {
		t.Error("expected non-empty identity.node_id")
	}

	// Sessions and RecentURIs should at least be non-nil slices.
	if hud.Sessions == nil {
		t.Error("expected non-nil sessions slice")
	}
	if hud.RecentURIs == nil {
		t.Error("expected non-nil recent_uris slice")
	}

	// NodeHealth should be a non-nil map.
	if hud.NodeHealth == nil {
		t.Error("expected non-nil node_health map")
	}

	// Kernel state must be non-empty.
	if hud.Kernel.State == "" {
		t.Error("expected non-empty kernel.state")
	}
	if hud.Kernel.Version == "" {
		t.Error("expected non-empty kernel.version")
	}

	// Timestamp must be set.
	if hud.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}

	t.Logf("HUDState: node_id=%s state=%s sessions=%d uris=%d",
		hud.Identity.NodeID, hud.Kernel.State, len(hud.Sessions), len(hud.RecentURIs))
}

// TestHandleHUDStateRegistered confirms the route appears in the httpRoutes manifest.
func TestHandleHUDStateRegistered(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	found := false
	for _, r := range srv.httpRoutes {
		if r.Path == "/v1/hud/state" && r.Method == "GET" {
			found = true
			break
		}
	}
	if !found {
		t.Error("GET /v1/hud/state not found in srv.httpRoutes — route not registered")
	}
}

// TestHandleHUDStateContentType verifies the response sets Content-Type: application/json.
func TestHandleHUDStateContentType(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/hud/state", nil)
	w := httptest.NewRecorder()

	srv.handleHUDState(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}
