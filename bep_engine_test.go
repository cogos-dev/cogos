package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── Helpers ────────────────────────────────────────────────────────────────────

// setupEngineWorkspace creates a temporary workspace with BEP cert and cluster config.
// Returns (root, certDir, deviceID).
func setupEngineWorkspace(t *testing.T) (string, string, DeviceID) {
	t.Helper()
	root := t.TempDir()

	// Create directory structure.
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

	// Generate unique cert in a workspace-specific dir.
	certDir := filepath.Join(root, ".certs")
	if err := GenerateBEPCert(certDir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(certDir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}
	deviceID, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		t.Fatalf("DeviceIDFromTLSCert: %v", err)
	}

	return root, certDir, deviceID
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
		time.Sleep(100 * time.Millisecond)
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
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be removed from %s", filename, dir)
}

// ─── Two-engine loopback E2E ────────────────────────────────────────────────────

func TestBEPEngineTwoNodeSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	// Setup two workspaces with certs.
	rootA, certDirA, idA := setupEngineWorkspace(t)
	rootB, certDirB, idB := setupEngineWorkspace(t)

	defsA := filepath.Join(rootA, ".cog", "bin", "agents", "definitions")
	defsB := filepath.Join(rootB, ".cog", "bin", "agents", "definitions")

	// Configure each to trust the other.
	cfgA := &BEPConfig{
		Enabled:    true,
		DeviceID:   FormatDeviceID(idA),
		ListenPort: 0, // random port
		CertDir:    certDirA,
		Peers: []BEPPeer{{
			DeviceID: FormatDeviceID(idB),
			// Address filled after B starts.
			Trusted: true,
		}},
		SyncDirs:  []string{defsA},
		Discovery: "static",
	}
	cfgB := &BEPConfig{
		Enabled:    true,
		DeviceID:   FormatDeviceID(idB),
		ListenPort: 0,
		CertDir:    certDirB,
		Peers: []BEPPeer{{
			DeviceID: FormatDeviceID(idA),
			Trusted:  true,
		}},
		SyncDirs:  []string{defsB},
		Discovery: "static",
	}

	// Create providers.
	providerA := NewBEPProvider(rootA)
	providerB := NewBEPProvider(rootB)

	// Start engine B first (listener) with a random port.
	engineB, err := NewBEPEngine(rootB, cfgB, providerB)
	if err != nil {
		t.Fatalf("NewBEPEngine B: %v", err)
	}
	if err := engineB.Start(); err != nil {
		t.Fatalf("engine B start: %v", err)
	}
	defer engineB.Stop()

	// Get B's actual listen address and configure A to dial it.
	bAddr := engineB.listener.Addr().String()
	cfgA.Peers[0].Address = bAddr

	// Start engine A (dialer).
	engineA, err := NewBEPEngine(rootA, cfgA, providerA)
	if err != nil {
		t.Fatalf("NewBEPEngine A: %v", err)
	}
	if err := engineA.Start(); err != nil {
		t.Fatalf("engine A start: %v", err)
	}
	defer engineA.Stop()

	// Wait for peer connection to establish.
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
		time.Sleep(100 * time.Millisecond)
	}
	if !connected {
		t.Fatal("engines failed to connect within 10s")
	}

	// ── G4: Create agent CRD on A → should appear on B ──

	writeTestCRD(t, defsA, "sync-test")

	// Notify engine A of the local change.
	engineA.NotifyLocalChange("sync-test.agent.yaml")

	// Wait for the file to appear on B.
	waitForFile(t, defsB, "sync-test.agent.yaml", 10*time.Second)

	// Verify content matches.
	dataA, _ := os.ReadFile(filepath.Join(defsA, "sync-test.agent.yaml"))
	dataB, _ := os.ReadFile(filepath.Join(defsB, "sync-test.agent.yaml"))
	if string(dataA) != string(dataB) {
		t.Errorf("content mismatch:\n  A: %q\n  B: %q", dataA, dataB)
	}

	t.Log("G4 passed: file synced A → B")

	// ── G6: Delete on A → should be removed on B ──

	os.Remove(filepath.Join(defsA, "sync-test.agent.yaml"))
	engineA.NotifyLocalChange("sync-test.agent.yaml")

	waitForFileAbsent(t, defsB, "sync-test.agent.yaml", 10*time.Second)
	t.Log("G6 passed: deletion synced A → B")
}

// ─── Engine status ──────────────────────────────────────────────────────────────

func TestBEPEngineStatus(t *testing.T) {
	root, certDir, id := setupEngineWorkspace(t)

	cfg := &BEPConfig{
		Enabled:    true,
		DeviceID:   FormatDeviceID(id),
		ListenPort: 0,
		CertDir:    certDir,
		Discovery:  "static",
	}

	provider := NewBEPProvider(root)
	engine, err := NewBEPEngine(root, cfg, provider)
	if err != nil {
		t.Fatalf("NewBEPEngine: %v", err)
	}
	if err := engine.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	status := engine.Status()
	if !status.Running {
		t.Error("expected Running=true")
	}
	if status.DeviceID != FormatDeviceID(id) {
		t.Errorf("DeviceID = %q", status.DeviceID)
	}
	if status.ListenAddr == "" {
		t.Error("ListenAddr should not be empty")
	}
}

// ─── Engine with bad cert ───────────────────────────────────────────────────────

func TestBEPEngineNoCert(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".cog", "bin", "agents", "definitions"), 0755)

	cfg := &BEPConfig{
		Enabled:  true,
		CertDir:  filepath.Join(root, "nonexistent"),
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
	// Should not panic on double close.
	pc.Close()
	pc.Close()
}

// ─── Model HandleRequest for non-existent file ─────────────────────────────────

func TestModelHandleRequestMissingFile(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	os.MkdirAll(defsDir, 0755)

	model := NewAgentSyncModel(nil, defsDir, filepath.Join(root, ".state"), 1)

	req := &BEPRequest{ID: 1, Name: "nonexistent.agent.yaml", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != ErrorCodeNoSuchFile {
		t.Errorf("Code = %d, want NoSuchFile (%d)", resp.Code, ErrorCodeNoSuchFile)
	}
}

func TestModelHandleRequestInvalidFilename(t *testing.T) {
	model := NewAgentSyncModel(nil, "/tmp", "/tmp/state", 1)

	req := &BEPRequest{ID: 1, Name: "not-a-crd.txt", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != ErrorCodeInvalidFile {
		t.Errorf("Code = %d, want InvalidFile (%d)", resp.Code, ErrorCodeInvalidFile)
	}
}

func TestModelHandleRequestExistingFile(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	os.MkdirAll(defsDir, 0755)

	content := "apiVersion: cog.os/v1alpha1\nkind: Agent\nmetadata:\n  name: test\n"
	os.WriteFile(filepath.Join(defsDir, "test.agent.yaml"), []byte(content), 0644)

	model := NewAgentSyncModel(nil, defsDir, filepath.Join(root, ".state"), 1)

	req := &BEPRequest{ID: 1, Name: "test.agent.yaml", Folder: "test"}
	resp := model.HandleRequest(req)

	if resp.Code != ErrorCodeNoError {
		t.Errorf("Code = %d, want NoError", resp.Code)
	}
	if string(resp.Data) != content {
		t.Errorf("Data = %q", resp.Data)
	}
}

// ─── Model LoadAndScanIndex ─────────────────────────────────────────────────────

func TestModelLoadAndScanIndex(t *testing.T) {
	root := t.TempDir()
	defsDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	stateDir := filepath.Join(root, ".cog", ".state", "bep")
	os.MkdirAll(defsDir, 0755)
	os.MkdirAll(stateDir, 0755)

	// Write an agent CRD.
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

	// Index should be persisted.
	if _, err := os.Stat(filepath.Join(stateDir, "index.json")); err != nil {
		t.Errorf("index.json not persisted: %v", err)
	}
}

// ─── Cluster CLI command tests ──────────────────────────────────────────────────

func TestCmdClusterHelp(t *testing.T) {
	err := cmdClusterHelp()
	if err != nil {
		t.Errorf("cmdClusterHelp: %v", err)
	}
}

func TestCmdClusterDispatchUnknown(t *testing.T) {
	err := cmdCluster([]string{"nonexistent"})
	if err == nil {
		t.Error("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want 'unknown'", err.Error())
	}
}

func TestCmdClusterDispatchEmpty(t *testing.T) {
	// Empty args should show help (no error).
	err := cmdCluster([]string{})
	if err != nil {
		t.Errorf("empty args: %v", err)
	}
}
