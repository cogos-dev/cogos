// bep_engine_test.go — in-process two-engine tests for BEPEngine.
// Phase 2 S1: proves handshake + basic index/file exchange using
// pkg/substrate/bep types, with two BEPEngine instances on loopback.

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Helpers ────────────────────────────────────────────────────────────────────

func setupEngineWorkspace(t *testing.T) (root, certDir string, id bep.DeviceID) {
	t.Helper()
	root = t.TempDir()

	dirs := []string{
		filepath.Join(root, ".cog", "bin", "agents", "definitions"),
		filepath.Join(root, ".cog", "config"),
		filepath.Join(root, ".cog", ".state", "bep"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	certDir = filepath.Join(root, ".certs")
	if err := bep.GenerateBEPCert(certDir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := bep.LoadBEPCert(certDir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}
	id, err = bep.DeviceIDFromTLSCert(&cert)
	if err != nil {
		t.Fatalf("DeviceIDFromTLSCert: %v", err)
	}

	return root, certDir, id
}

func writeTestCRD(t *testing.T, dir, name string) {
	t.Helper()
	data := fmt.Sprintf("apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: %s\nspec:\n  type: interactive\n", name)
	if err := os.WriteFile(filepath.Join(dir, name+".agent.yaml"), []byte(data), 0644); err != nil {
		t.Fatalf("write CRD: %v", err)
	}
}

func waitForFile(t *testing.T, dir, filename string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, filename)); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in %s", filename, dir)
}

func waitForFileAbsent(t *testing.T, dir, filename string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, filename)); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be removed from %s", filename, dir)
}

// ─── Two-engine in-process loopback test ────────────────────────────────────────

// TestBEPEngineTwoNodeHandshakeAndSync proves:
// 1. Two BEPEngine instances can complete a TLS handshake on loopback.
// 2. A file written by one engine is received by the other (index + request/response).
// 3. A deletion propagates from one engine to the other.
func TestBEPEngineTwoNodeHandshakeAndSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	// Setup two workspaces with independent certs.
	rootA, certDirA, idA := setupEngineWorkspace(t)
	rootB, certDirB, idB := setupEngineWorkspace(t)

	defsA := filepath.Join(rootA, ".cog", "bin", "agents", "definitions")
	defsB := filepath.Join(rootB, ".cog", "bin", "agents", "definitions")

	cfgA := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(idA),
		ListenPort: 0, // OS-assigned
		CertDir:    certDirA,
		Peers: []bep.Peer{{
			DeviceID: bep.FormatDeviceID(idB),
			// Address filled in after B starts.
			Trusted: true,
		}},
		Discovery: "static",
	}
	cfgB := &bep.Config{
		Enabled:    true,
		DeviceID:   bep.FormatDeviceID(idB),
		ListenPort: 0,
		CertDir:    certDirB,
		Peers: []bep.Peer{{
			DeviceID: bep.FormatDeviceID(idA),
			Trusted:  true,
		}},
		Discovery: "static",
	}

	providerA := NewBEPProvider(rootA)
	providerB := NewBEPProvider(rootB)

	// Start B first (listener).
	engineB, err := NewBEPEngine(rootB, cfgB, providerB)
	if err != nil {
		t.Fatalf("NewBEPEngine B: %v", err)
	}
	if err := engineB.Start(); err != nil {
		t.Fatalf("engine B start: %v", err)
	}
	defer engineB.Stop()

	// Capture events from B via callback.
	var bEvents []bep.SyncEvent
	engineB.SetEventCallback(func(evt bep.SyncEvent) {
		bEvents = append(bEvents, evt)
	})

	// Configure A to dial B's actual address.
	bAddr := engineB.listener.Addr().String()
	cfgA.Peers[0].Address = bAddr

	// Start A (dialer).
	engineA, err := NewBEPEngine(rootA, cfgA, providerA)
	if err != nil {
		t.Fatalf("NewBEPEngine A: %v", err)
	}
	if err := engineA.Start(); err != nil {
		t.Fatalf("engine A start: %v", err)
	}
	defer engineA.Stop()

	// ── Wait for peer connection ──────────────────────────────────────────────────

	deadline := time.Now().Add(10 * time.Second)
	connected := false
	for time.Now().Before(deadline) {
		engineA.peersMu.RLock()
		n := len(engineA.peers)
		engineA.peersMu.RUnlock()
		if n > 0 {
			connected = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !connected {
		t.Fatal("engines failed to connect within 10s")
	}
	t.Log("handshake complete: A connected to B")

	// ── File sync A → B ───────────────────────────────────────────────────────────

	writeTestCRD(t, defsA, "sync-test")
	engineA.NotifyLocalChange("sync-test.agent.yaml")

	waitForFile(t, defsB, "sync-test.agent.yaml", 10*time.Second)

	dataA, _ := os.ReadFile(filepath.Join(defsA, "sync-test.agent.yaml"))
	dataB, _ := os.ReadFile(filepath.Join(defsB, "sync-test.agent.yaml"))
	if string(dataA) != string(dataB) {
		t.Errorf("content mismatch A→B:\n  A: %q\n  B: %q", dataA, dataB)
	}
	t.Log("file synced A → B")

	// ── Deletion sync A → B ───────────────────────────────────────────────────────

	os.Remove(filepath.Join(defsA, "sync-test.agent.yaml"))
	engineA.NotifyLocalChange("sync-test.agent.yaml")

	waitForFileAbsent(t, defsB, "sync-test.agent.yaml", 10*time.Second)
	t.Log("deletion synced A → B")

	// ── Engine status ─────────────────────────────────────────────────────────────

	status := engineA.Status()
	if !status.Running {
		t.Error("engine A: expected Running=true")
	}
	if status.DeviceID != bep.FormatDeviceID(idA) {
		t.Errorf("engine A DeviceID = %q, want %q", status.DeviceID, bep.FormatDeviceID(idA))
	}
}

// ─── Handshake-only test (no file exchange) ───────────────────────────────────

// TestBEPEngineHandshakeOnly verifies that two engines can connect and
// exchange ClusterConfig without any files needing to sync.
func TestBEPEngineHandshakeOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping loopback test in short mode")
	}

	rootA, certDirA, idA := setupEngineWorkspace(t)
	rootB, certDirB, idB := setupEngineWorkspace(t)

	cfgA := &bep.Config{
		Enabled: true, DeviceID: bep.FormatDeviceID(idA),
		ListenPort: 0, CertDir: certDirA,
		Peers: []bep.Peer{{DeviceID: bep.FormatDeviceID(idB), Trusted: true}},
	}
	cfgB := &bep.Config{
		Enabled: true, DeviceID: bep.FormatDeviceID(idB),
		ListenPort: 0, CertDir: certDirB,
		Peers: []bep.Peer{{DeviceID: bep.FormatDeviceID(idA), Trusted: true}},
	}

	engineB, err := NewBEPEngine(rootB, cfgB, NewBEPProvider(rootB))
	if err != nil {
		t.Fatalf("NewBEPEngine B: %v", err)
	}
	if err := engineB.Start(); err != nil {
		t.Fatalf("engine B start: %v", err)
	}
	defer engineB.Stop()

	cfgA.Peers[0].Address = engineB.listener.Addr().String()

	engineA, err := NewBEPEngine(rootA, cfgA, NewBEPProvider(rootA))
	if err != nil {
		t.Fatalf("NewBEPEngine A: %v", err)
	}
	if err := engineA.Start(); err != nil {
		t.Fatalf("engine A start: %v", err)
	}
	defer engineA.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		engineA.peersMu.RLock()
		n := len(engineA.peers)
		engineA.peersMu.RUnlock()
		if n > 0 {
			t.Logf("handshake complete, A peers=%d", n)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("engines failed to connect within 10s")
}

// ─── SetEventCallback wiring ─────────────────────────────────────────────────────

// TestBEPEngineEventCallback verifies that SetEventCallback fires on sync events.
func TestBEPEngineEventCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping loopback test in short mode")
	}

	rootA, certDirA, idA := setupEngineWorkspace(t)
	rootB, certDirB, idB := setupEngineWorkspace(t)

	defsA := filepath.Join(rootA, ".cog", "bin", "agents", "definitions")

	cfgA := &bep.Config{
		Enabled: true, DeviceID: bep.FormatDeviceID(idA),
		ListenPort: 0, CertDir: certDirA,
		Peers: []bep.Peer{{DeviceID: bep.FormatDeviceID(idB), Trusted: true}},
	}
	cfgB := &bep.Config{
		Enabled: true, DeviceID: bep.FormatDeviceID(idB),
		ListenPort: 0, CertDir: certDirB,
		Peers: []bep.Peer{{DeviceID: bep.FormatDeviceID(idA), Trusted: true}},
	}

	engineB, err := NewBEPEngine(rootB, cfgB, NewBEPProvider(rootB))
	if err != nil {
		t.Fatalf("NewBEPEngine B: %v", err)
	}
	if err := engineB.Start(); err != nil {
		t.Fatalf("engine B start: %v", err)
	}
	defer engineB.Stop()

	cfgA.Peers[0].Address = engineB.listener.Addr().String()

	// Track events emitted on A.
	received := make(chan bep.SyncEvent, 10)
	engineA, err := NewBEPEngine(rootA, cfgA, NewBEPProvider(rootA))
	if err != nil {
		t.Fatalf("NewBEPEngine A: %v", err)
	}
	engineA.SetEventCallback(func(evt bep.SyncEvent) {
		select {
		case received <- evt:
		default:
		}
	})
	if err := engineA.Start(); err != nil {
		t.Fatalf("engine A start: %v", err)
	}
	defer engineA.Stop()

	// Wait for connection.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		engineA.peersMu.RLock()
		n := len(engineA.peers)
		engineA.peersMu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Trigger a local change — should produce a SyncEventFileSent event.
	writeTestCRD(t, defsA, "callback-test")
	engineA.NotifyLocalChange("callback-test.agent.yaml")

	// Wait for an event.
	select {
	case evt := <-received:
		t.Logf("received event: type=%s", evt.Type)
	case <-time.After(10 * time.Second):
		t.Fatal("no SyncEvent received within 10s")
	}
}

// ─── Constructor error cases ─────────────────────────────────────────────────────

func TestBEPEngineNoCert(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".cog", "bin", "agents", "definitions"), 0755)

	cfg := &bep.Config{
		Enabled:   true,
		CertDir:   filepath.Join(root, "nonexistent"),
		Discovery: "static",
	}
	provider := NewBEPProvider(root)

	_, err := NewBEPEngine(root, cfg, provider)
	if err == nil {
		t.Error("expected error with missing cert")
	}
}

// ─── PeerConnection.Close idempotent ────────────────────────────────────────────

func TestPeerConnectionCloseIdempotent(t *testing.T) {
	pc := &PeerConnection{
		closeCh: make(chan struct{}),
	}
	pc.Close()
	pc.Close() // must not panic
}

// ─── AgentSyncModel ─────────────────────────────────────────────────────────────

func TestModelHandleRequestMissingFile(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	os.MkdirAll(defsDir, 0755)

	model := NewAgentSyncModel(nil, defsDir, filepath.Join(root, ".state"), 1)

	req := &bep.Request{ID: 1, Name: "nonexistent.agent.yaml", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != bep.ErrorCodeNoSuchFile {
		t.Errorf("Code = %d, want NoSuchFile (%d)", resp.Code, bep.ErrorCodeNoSuchFile)
	}
}

func TestModelHandleRequestInvalidFilename(t *testing.T) {
	model := NewAgentSyncModel(nil, "/tmp", "/tmp/state", 1)

	req := &bep.Request{ID: 1, Name: "not-a-crd.txt", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != bep.ErrorCodeInvalidFile {
		t.Errorf("Code = %d, want InvalidFile (%d)", resp.Code, bep.ErrorCodeInvalidFile)
	}
}

func TestModelHandleRequestExistingFile(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	os.MkdirAll(defsDir, 0755)

	content := "apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: test\n"
	os.WriteFile(filepath.Join(defsDir, "test.agent.yaml"), []byte(content), 0644)

	model := NewAgentSyncModel(nil, defsDir, filepath.Join(root, ".state"), 1)

	req := &bep.Request{ID: 1, Name: "test.agent.yaml", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != bep.ErrorCodeNoError {
		t.Errorf("Code = %d, want NoError", resp.Code)
	}
	if string(resp.Data) != content {
		t.Errorf("Data = %q, want %q", resp.Data, content)
	}
}

func TestModelLoadAndScanIndex(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	stateDir := filepath.Join(root, ".cog", ".state", "bep")
	os.MkdirAll(defsDir, 0755)
	os.MkdirAll(stateDir, 0755)

	content := "apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: test\n"
	os.WriteFile(filepath.Join(defsDir, "test.agent.yaml"), []byte(content), 0644)

	model := NewAgentSyncModel(nil, defsDir, stateDir, 42)
	files := model.LoadAndScanIndex()

	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].Name != "test.agent.yaml" {
		t.Errorf("Name = %q", files[0].Name)
	}
	if files[0].Size == 0 {
		t.Error("Size should not be 0")
	}

	if _, err := os.Stat(filepath.Join(stateDir, "index.json")); err != nil {
		t.Errorf("index.json not persisted: %v", err)
	}
}
