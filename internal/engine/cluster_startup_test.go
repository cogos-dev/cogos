// cluster_startup_test.go — Phase 2 S2 boot-wiring tests.
//
// Tests the dark-by-default invariant and the enabled wiring path without
// requiring a full Boot() (which spins up an HTTP server, process loop, etc.).
//
// Strategy:
//   - Disabled path: create a Server with bepEngine=nil and assert
//     /v1/cluster/status returns {"enabled":false} with no goroutines started.
//   - Enabled path: construct a BEPEngine with a temp cert + loopback
//     self-peer config (ListenPort 0), verify Status() is queryable, and
//     confirm the handler returns enabled:true.
//
// CI safety: ListenPort 0 means the OS picks an ephemeral loopback port; we
// Stop() the engine immediately after the wiring check so no port lingers.
// No real dialing happens because the peer list carries no Address field
// (runDialer short-circuits on missing address) or carries a self-address
// that fails fast with a short backoff. All assertions are structural
// (field presence), not timing-dependent — no sleeps.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Disabled-path ─────────────────────────────────────────────────────────────

// TestClusterStatusDisabled verifies that when bepEngine is nil (the shipped
// default — cluster.enabled=false), the endpoint returns {"enabled":false}
// with no engine state accessed. This is the primary dark-default guarantee.
func TestClusterStatusDisabled(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	// bepEngine is nil by default — do NOT set it.

	req := httptest.NewRequest(http.MethodGet, "/v1/cluster/status", nil)
	w := httptest.NewRecorder()
	srv.handleClusterStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	enabled, ok := body["enabled"]
	if !ok {
		t.Fatal("response missing 'enabled' field")
	}
	if enabled != false {
		t.Errorf("enabled = %v; want false", enabled)
	}

	// No extra engine fields should appear in the disabled response.
	for _, key := range []string{"running", "device_id", "listen_addr", "peer_count"} {
		if _, present := body[key]; present {
			t.Errorf("disabled response unexpectedly contains key %q", key)
		}
	}
}

// TestClusterStatusDisabledViaClusterYaml verifies that when a cluster.yaml
// with enabled=false exists on disk, BEPProvider.LoadConfig returns a disabled
// config and the engine handle stays nil — exactly as-if no cluster.yaml existed.
func TestClusterStatusDisabledViaClusterYaml(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)

	// Write cluster.yaml with enabled: false.
	clusterYAML := "enabled: false\nlistenPort: 22000\ndiscovery: static\npeers: []\n"
	cfgPath := filepath.Join(root, ".cog", "config", "cluster.yaml")
	if err := os.WriteFile(cfgPath, []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("write cluster.yaml: %v", err)
	}

	provider := NewBEPProvider(root)
	clusterCfg, err := provider.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if clusterCfg.Enabled {
		t.Error("expected Enabled=false with enabled: false in cluster.yaml")
	}

	// Simulate the boot path: if !clusterCfg.Enabled, no engine is built.
	var engineHandle *BEPEngine
	if clusterCfg.Enabled {
		t.Fatal("boot path would have started engine — should not reach here")
	}
	// engineHandle stays nil.

	// Wire up handler and assert disabled response.
	srv := newTestServer(t)
	srv.bepEngine = engineHandle // nil

	req := httptest.NewRequest(http.MethodGet, "/v1/cluster/status", nil)
	w := httptest.NewRecorder()
	srv.handleClusterStatus(w, req)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v; want false", body["enabled"])
	}
}

// ─── Enabled path (construction + Status) ─────────────────────────────────────

// TestClusterStatusEnabled verifies that when a BEPEngine is constructed with
// a temp cert and started on a loopback ephemeral port, the /v1/cluster/status
// handler returns enabled:true and a non-empty device_id. The engine is stopped
// immediately after the structural checks — no port lingers.
//
// CI safety: ListenPort 0 → OS-assigned ephemeral loopback port. No dialing
// occurs because the peer list is empty (no Address configured), so runDialer
// never fires. The test does not wait for connection establishment.
func TestClusterStatusEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping engine-start test in short mode")
	}

	root, certDir, deviceID := setupEngineWorkspace(t)

	clusterCfg := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(deviceID),
		ListenPort: 0, // ephemeral — OS picks the port
		CertDir:    certDir,
		Discovery:  "static",
		Peers:      nil, // no peers → no dialing goroutines
	}

	provider := NewBEPProvider(root)
	eng, err := NewBEPEngine(root, clusterCfg, provider)
	if err != nil {
		t.Fatalf("NewBEPEngine: %v", err)
	}
	if err := eng.Start(); err != nil {
		t.Fatalf("BEPEngine.Start: %v", err)
	}
	defer eng.Stop()

	// Verify Status() is queryable.
	status := eng.Status()
	if !status.Running {
		t.Error("Status.Running = false; want true")
	}
	if status.DeviceID == "" {
		t.Error("Status.DeviceID is empty")
	}
	if status.DeviceID != bep.FormatDeviceID(deviceID) {
		t.Errorf("Status.DeviceID = %q; want %q", status.DeviceID, bep.FormatDeviceID(deviceID))
	}
	if status.ListenAddr == "" {
		t.Error("Status.ListenAddr is empty after Start")
	}

	// Wire the engine into a test server and hit the HTTP handler.
	srv := newTestServer(t)
	srv.bepEngine = eng

	req := httptest.NewRequest(http.MethodGet, "/v1/cluster/status", nil)
	w := httptest.NewRecorder()
	srv.handleClusterStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["enabled"] != true {
		t.Errorf("enabled = %v; want true", body["enabled"])
	}
	if body["running"] != true {
		t.Errorf("running = %v; want true", body["running"])
	}
	if body["device_id"] == "" || body["device_id"] == nil {
		t.Error("device_id missing or empty in enabled response")
	}
	if body["listen_addr"] == "" || body["listen_addr"] == nil {
		t.Error("listen_addr missing or empty in enabled response")
	}
	// peer_count should be 0 (no peers configured).
	if body["peer_count"] != float64(0) {
		t.Errorf("peer_count = %v; want 0", body["peer_count"])
	}
}

// ─── BEPProvider.LoadConfig absent/disabled ───────────────────────────────────

// TestBEPProviderLoadConfigAbsent verifies that a missing cluster.yaml returns
// a disabled (Enabled=false) config rather than an error. This is the
// no-cluster-yaml path that must leave the engine unstarted.
func TestBEPProviderLoadConfigAbsent(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	provider := NewBEPProvider(root)

	cfg, err := provider.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error with absent cluster.yaml: %v", err)
	}
	if cfg.Enabled {
		t.Error("Enabled = true; want false (absent cluster.yaml → disabled default)")
	}
}
