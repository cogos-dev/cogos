// bep_provider_test.go
// Comprehensive test suite for the BEP agent sync system:
//   - BEPProvider lifecycle, file watching, polling fallback
//   - BEPReceiver atomic writes, path traversal rejection, ring buffer
//   - Configuration loading, status reporting, peer copy semantics

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Helpers ────────────────────────────────────────────────────────────────────

// validAgentCRDYAML returns a minimal valid AgentCRD YAML document.
func validAgentCRDYAML(name string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: %s
spec:
  type: interactive
`, name))
}

// setupBEPWatchDir creates the watch directory structure inside a temp root
// and returns the root path.
func setupBEPWatchDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	watchDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("mkdir watchDir: %v", err)
	}
	return root
}

// writeClusterConfig writes a cluster.yaml to the test workspace.
func writeClusterConfig(t *testing.T, root string, content string) {
	t.Helper()
	cfgDir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "cluster.yaml"), []byte(content), 0644); err != nil {
		t.Fatalf("write cluster.yaml: %v", err)
	}
}

// ─── 1. Provider Lifecycle ──────────────────────────────────────────────────────

func TestBEPProviderStartStop(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	if err := p.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer p.Stop()

	// Verify running.
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if !running {
		t.Error("expected running=true after Start()")
	}

	p.Stop()

	p.mu.Lock()
	running = p.running
	p.mu.Unlock()
	if running {
		t.Error("expected running=false after Stop()")
	}
}

func TestBEPProviderDoubleStartReturnsError(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	if err := p.Start(); err != nil {
		t.Fatalf("first Start() failed: %v", err)
	}
	defer p.Stop()

	err := p.Start()
	if err == nil {
		t.Fatal("expected error on double Start(), got nil")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("expected 'already running' in error, got: %v", err)
	}
}

func TestBEPProviderStopWhenNotRunningIsNoOp(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Stop on a never-started provider should not panic.
	p.Stop()

	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if running {
		t.Error("expected running=false on never-started provider")
	}
}

// ─── 2. File Watching Callback ──────────────────────────────────────────────────

func TestBEPProviderFSNotifyCallback(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	var mu sync.Mutex
	var detected []string

	p.OnFileChange(func(filename string) {
		mu.Lock()
		detected = append(detected, filename)
		mu.Unlock()
	})

	if err := p.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer p.Stop()

	// The provider should be using fsnotify (not polling) since we have a real dir.
	p.mu.Lock()
	hasWatcher := p.watcher != nil
	p.mu.Unlock()
	if !hasWatcher {
		t.Skip("fsnotify not available on this platform; skipping fsnotify-specific test")
	}

	// Write an agent CRD file into the watch directory.
	crdPath := filepath.Join(p.watchDir, "whirl.agent.yaml")
	if err := os.WriteFile(crdPath, validAgentCRDYAML("whirl"), 0644); err != nil {
		t.Fatalf("write CRD file: %v", err)
	}

	// Wait for the callback to fire (debounce is 500ms, allow up to 2s).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(detected)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(detected) == 0 {
		t.Error("expected onChange callback to fire for whirl.agent.yaml, but it never did")
	} else {
		found := false
		for _, d := range detected {
			if d == "whirl.agent.yaml" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'whirl.agent.yaml' in detected files, got %v", detected)
		}
	}
}

// ─── 3. Polling Fallback ────────────────────────────────────────────────────────

func TestBEPProviderPollingFallback(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	var mu sync.Mutex
	var detected []string

	p.OnFileChange(func(filename string) {
		mu.Lock()
		detected = append(detected, filename)
		mu.Unlock()
	})

	// Force polling mode: start the provider, then close the watcher and
	// launch the polling goroutine manually.
	// We cannot easily force Start() to skip fsnotify, so we set up manually.
	p.mu.Lock()
	if err := os.MkdirAll(p.watchDir, 0755); err != nil {
		p.mu.Unlock()
		t.Fatalf("create watchDir: %v", err)
	}
	p.running = true
	p.stopCh = make(chan struct{})
	p.watcher = nil // nil watcher = polling mode
	p.mu.Unlock()

	go p.runPolling()
	defer p.Stop()

	// Wait briefly for the initial snapshot to be taken.
	time.Sleep(200 * time.Millisecond)

	// Write a file — the poller should detect it within its 5s interval.
	crdPath := filepath.Join(p.watchDir, "poll-test.agent.yaml")
	if err := os.WriteFile(crdPath, validAgentCRDYAML("poll-test"), 0644); err != nil {
		t.Fatalf("write CRD file: %v", err)
	}

	// Wait up to 10 seconds for the poller to detect the change.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(detected)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(detected) == 0 {
		t.Error("polling fallback did not detect poll-test.agent.yaml within 10s")
	}
}

// ─── 4. isAgentCRDFile Filter ───────────────────────────────────────────────────

func TestBEPIsAgentCRDFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"foo.agent.yaml", true},
		{"foo.yaml", false},
		{".agent.yaml", true},
		{"readme.md", false},
		{"agent.yaml", false},
		{"something.agent.yml", false},
		{"multi.part.agent.yaml", true},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentCRDFile(tt.name)
			if got != tt.want {
				t.Errorf("isAgentCRDFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// ─── 5. scanModTimes ────────────────────────────────────────────────────────────

func TestBEPScanModTimes(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	watchDir := p.watchDir

	// Create a mix of files.
	files := map[string]bool{
		"alpha.agent.yaml": true,
		"beta.agent.yaml":  true,
		"gamma.yaml":       false, // not a CRD file
		"readme.md":        false,
		"delta.agent.yaml": true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(watchDir, name), []byte("test"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Also create a subdirectory — should be ignored.
	if err := os.Mkdir(filepath.Join(watchDir, "subdir.agent.yaml"), 0755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	result := p.scanModTimes()

	for name, expectIncluded := range files {
		_, found := result[name]
		if expectIncluded && !found {
			t.Errorf("expected %q in scan result, not found", name)
		}
		if !expectIncluded && found {
			t.Errorf("did not expect %q in scan result, but found it", name)
		}
	}

	// Subdirectory must not appear.
	if _, found := result["subdir.agent.yaml"]; found {
		t.Error("subdirectory should not appear in scanModTimes")
	}
}

// ─── 6. diffAndNotify ───────────────────────────────────────────────────────────

func TestBEPDiffAndNotify(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	var mu sync.Mutex
	var notified []string

	p.OnFileChange(func(filename string) {
		mu.Lock()
		notified = append(notified, filename)
		mu.Unlock()
	})

	now := time.Now()
	old := map[string]time.Time{
		"existing.agent.yaml": now,
		"deleted.agent.yaml":  now,
		"modified.agent.yaml": now,
	}

	current := map[string]time.Time{
		"existing.agent.yaml": now,                      // unchanged
		"modified.agent.yaml": now.Add(1 * time.Second), // modified
		"created.agent.yaml":  now,                       // new
		// deleted.agent.yaml is absent → deletion
	}

	p.diffAndNotify(old, current)

	mu.Lock()
	defer mu.Unlock()

	// We expect callbacks for: modified, created, deleted. NOT for existing.
	expected := map[string]bool{
		"modified.agent.yaml": false,
		"created.agent.yaml":  false,
		"deleted.agent.yaml":  false,
	}

	for _, name := range notified {
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
		if name == "existing.agent.yaml" {
			t.Error("diffAndNotify should not fire for unchanged files")
		}
	}

	for name, seen := range expected {
		if !seen {
			t.Errorf("expected notification for %q, but it was not fired", name)
		}
	}
}

func TestBEPDiffAndNotifyNilCallback(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)
	// No callback set — should not panic.
	old := map[string]time.Time{"a.agent.yaml": time.Now()}
	current := map[string]time.Time{}
	p.diffAndNotify(old, current)
}

// ─── 7. Receiver: ReceiveAgentCRD ───────────────────────────────────────────────

func TestBEPReceiveAgentCRD(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	data := validAgentCRDYAML("whirl")
	err := p.ReceiveAgentCRD("peer-1", "whirl.agent.yaml", data)
	if err != nil {
		t.Fatalf("ReceiveAgentCRD failed: %v", err)
	}

	// Verify the file was written.
	written, err := os.ReadFile(filepath.Join(p.watchDir, "whirl.agent.yaml"))
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if string(written) != string(data) {
		t.Error("written data does not match input data")
	}

	// Verify the CRD can be loaded back via LoadAgentCRD.
	crd, err := LoadAgentCRD(root, "whirl")
	if err != nil {
		t.Fatalf("LoadAgentCRD failed after ReceiveAgentCRD: %v", err)
	}
	if crd.Metadata.Name != "whirl" {
		t.Errorf("loaded CRD name = %q, want %q", crd.Metadata.Name, "whirl")
	}
	if crd.APIVersion != "cog.os/v1alpha1" {
		t.Errorf("loaded CRD apiVersion = %q, want %q", crd.APIVersion, "cog.os/v1alpha1")
	}
}

func TestBEPReceiveAgentCRDUpdateAction(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// First write → create.
	data := validAgentCRDYAML("whirl")
	if err := p.ReceiveAgentCRD("peer-1", "whirl.agent.yaml", data); err != nil {
		t.Fatalf("first ReceiveAgentCRD: %v", err)
	}

	// Second write → update.
	if err := p.ReceiveAgentCRD("peer-1", "whirl.agent.yaml", data); err != nil {
		t.Fatalf("second ReceiveAgentCRD: %v", err)
	}

	history := p.History()
	if len(history) < 2 {
		t.Fatalf("expected at least 2 history events, got %d", len(history))
	}
	if history[0].Action != "create" {
		t.Errorf("first event action = %q, want 'create'", history[0].Action)
	}
	if history[1].Action != "update" {
		t.Errorf("second event action = %q, want 'update'", history[1].Action)
	}
}

func TestBEPReceiveAgentCRDInvalidYAML(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	err := p.ReceiveAgentCRD("peer-1", "bad.agent.yaml", []byte("not: valid: yaml: ["))
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestBEPReceiveAgentCRDWrongAPIVersion(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	data := []byte(`apiVersion: wrong/v1
kind: Agent
metadata:
  name: test
spec:
  type: interactive
`)
	err := p.ReceiveAgentCRD("peer-1", "test.agent.yaml", data)
	if err == nil {
		t.Error("expected error for wrong apiVersion, got nil")
	}
	if !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("error should mention apiVersion: %v", err)
	}
}

func TestBEPReceiveAgentCRDWrongKind(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	data := []byte(`apiVersion: cog.os/v1alpha1
kind: NotAgent
metadata:
  name: test
spec:
  type: interactive
`)
	err := p.ReceiveAgentCRD("peer-1", "test.agent.yaml", data)
	if err == nil {
		t.Error("expected error for wrong kind, got nil")
	}
}

func TestBEPReceiveAgentCRDMissingName(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	data := []byte(`apiVersion: cog.os/v1alpha1
kind: Agent
metadata: {}
spec:
  type: interactive
`)
	err := p.ReceiveAgentCRD("peer-1", "test.agent.yaml", data)
	if err == nil {
		t.Error("expected error for missing metadata.name, got nil")
	}
	if !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("error should mention metadata.name: %v", err)
	}
}

func TestBEPReceiveAgentCRDBadFilename(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	err := p.ReceiveAgentCRD("peer-1", "notcrd.yaml", validAgentCRDYAML("x"))
	if err == nil {
		t.Error("expected error for non-.agent.yaml filename")
	}
}

// ─── 8. Receiver: Path Traversal Rejection ──────────────────────────────────────

func TestBEPReceiveAgentCRDPathTraversal(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	traversals := []string{
		"../evil.agent.yaml",
		"../../etc/passwd.agent.yaml",
		"subdir/evil.agent.yaml",
		"sub\\evil.agent.yaml",
	}

	for _, name := range traversals {
		t.Run(name, func(t *testing.T) {
			err := p.ReceiveAgentCRD("peer-evil", name, validAgentCRDYAML("evil"))
			if err == nil {
				t.Errorf("expected path traversal rejection for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "plain filename") && !strings.Contains(err.Error(), "must end in") {
				t.Errorf("error should mention filename constraint: %v", err)
			}
		})
	}
}

// ─── 9. Receiver: RemoveAgentCRD ────────────────────────────────────────────────

func TestBEPRemoveAgentCRD(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Write a file first.
	crdPath := filepath.Join(p.watchDir, "doomed.agent.yaml")
	if err := os.WriteFile(crdPath, validAgentCRDYAML("doomed"), 0644); err != nil {
		t.Fatalf("write CRD: %v", err)
	}

	// Verify it exists.
	if _, err := os.Stat(crdPath); err != nil {
		t.Fatalf("file should exist before removal: %v", err)
	}

	// Remove it.
	if err := p.RemoveAgentCRD("peer-1", "doomed.agent.yaml"); err != nil {
		t.Fatalf("RemoveAgentCRD failed: %v", err)
	}

	// Verify it is gone.
	if _, err := os.Stat(crdPath); !os.IsNotExist(err) {
		t.Error("file should not exist after RemoveAgentCRD")
	}
}

func TestBEPRemoveAgentCRDAlreadyAbsent(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Removing a file that does not exist should not error.
	err := p.RemoveAgentCRD("peer-1", "nonexistent.agent.yaml")
	if err != nil {
		t.Errorf("RemoveAgentCRD for absent file should not error, got: %v", err)
	}
}

func TestBEPRemoveAgentCRDPathTraversal(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	err := p.RemoveAgentCRD("peer-evil", "../evil.agent.yaml")
	if err == nil {
		t.Error("expected path traversal rejection for RemoveAgentCRD")
	}
}

func TestBEPRemoveAgentCRDBadFilename(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	err := p.RemoveAgentCRD("peer-1", "notcrd.yaml")
	if err == nil {
		t.Error("expected error for non-.agent.yaml filename")
	}
}

// ─── 10. Receiver: History Ring Buffer ──────────────────────────────────────────

func TestBEPHistoryOrder(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Receive multiple CRDs with different names.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("agent%d", i)
		filename := name + ".agent.yaml"
		data := validAgentCRDYAML(name)
		if err := p.ReceiveAgentCRD("peer-1", filename, data); err != nil {
			t.Fatalf("ReceiveAgentCRD(%s): %v", name, err)
		}
	}

	history := p.History()
	if len(history) != 5 {
		t.Fatalf("expected 5 history events, got %d", len(history))
	}

	// Events should be in chronological order (oldest first).
	for i := 0; i < 5; i++ {
		expected := fmt.Sprintf("agent%d.agent.yaml", i)
		if history[i].Filename != expected {
			t.Errorf("history[%d].Filename = %q, want %q", i, history[i].Filename, expected)
		}
		if history[i].PeerID != "peer-1" {
			t.Errorf("history[%d].PeerID = %q, want 'peer-1'", i, history[i].PeerID)
		}
	}
}

func TestBEPHistoryRingBufferCap(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Fill beyond the 100-event cap.
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("agent%03d", i)
		filename := name + ".agent.yaml"
		data := validAgentCRDYAML(name)
		if err := p.ReceiveAgentCRD("peer-1", filename, data); err != nil {
			t.Fatalf("ReceiveAgentCRD(%s): %v", name, err)
		}
	}

	history := p.History()
	if len(history) != receiverMaxHistory {
		t.Fatalf("expected history capped at %d, got %d", receiverMaxHistory, len(history))
	}

	// The oldest 20 events (agent000..agent019) should have been evicted.
	// The first event in the buffer should be agent020.
	if history[0].Filename != "agent020.agent.yaml" {
		t.Errorf("oldest event should be agent020, got %q", history[0].Filename)
	}

	// The newest event should be agent119.
	if history[len(history)-1].Filename != "agent119.agent.yaml" {
		t.Errorf("newest event should be agent119, got %q", history[len(history)-1].Filename)
	}
}

func TestBEPHistoryReturnsCopy(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	if err := p.ReceiveAgentCRD("peer-1", "test.agent.yaml", validAgentCRDYAML("test")); err != nil {
		t.Fatalf("ReceiveAgentCRD: %v", err)
	}

	h1 := p.History()
	h1[0].PeerID = "MUTATED"

	h2 := p.History()
	if h2[0].PeerID == "MUTATED" {
		t.Error("History() should return a copy; mutation leaked to internal state")
	}
}

// ─── 11. LoadConfig ─────────────────────────────────────────────────────────────

func TestBEPLoadConfigMissingFile(t *testing.T) {
	root := t.TempDir()
	p := NewBEPProvider(root)

	cfg, err := p.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig on missing file should not error: %v", err)
	}
	if cfg.Enabled {
		t.Error("default config should have Enabled=false")
	}
	if cfg.Discovery != "static" {
		t.Errorf("default config Discovery = %q, want 'static'", cfg.Discovery)
	}
}

func TestBEPLoadConfigValidYAML(t *testing.T) {
	root := t.TempDir()
	writeClusterConfig(t, root, `
enabled: true
deviceId: node-alpha
peers:
  - deviceId: node-beta
    address: 10.0.0.2:22000
    name: beta
    trusted: true
syncDirs:
  - .cog/bin/agents/definitions
discovery: tailscale
`)

	p := NewBEPProvider(root)
	cfg, err := p.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.DeviceID != "node-alpha" {
		t.Errorf("DeviceID = %q, want 'node-alpha'", cfg.DeviceID)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("Peers count = %d, want 1", len(cfg.Peers))
	}
	if cfg.Peers[0].Name != "beta" {
		t.Errorf("Peers[0].Name = %q, want 'beta'", cfg.Peers[0].Name)
	}
	if !cfg.Peers[0].Trusted {
		t.Error("Peers[0].Trusted should be true")
	}
	if cfg.Discovery != "tailscale" {
		t.Errorf("Discovery = %q, want 'tailscale'", cfg.Discovery)
	}
	if len(cfg.SyncDirs) != 1 {
		t.Errorf("SyncDirs count = %d, want 1", len(cfg.SyncDirs))
	}
}

func TestBEPLoadConfigInvalidYAML(t *testing.T) {
	root := t.TempDir()
	writeClusterConfig(t, root, `enabled: [invalid yaml structure`)

	p := NewBEPProvider(root)
	_, err := p.LoadConfig()
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse cluster config") {
		t.Errorf("error should mention parsing: %v", err)
	}
}

// ─── 12. Status ─────────────────────────────────────────────────────────────────

func TestBEPStatus(t *testing.T) {
	root := t.TempDir()
	writeClusterConfig(t, root, `
enabled: true
deviceId: node-gamma
peers:
  - deviceId: peer-1
    address: 10.0.0.3:22000
    name: peer-one
    trusted: true
`)

	p := NewBEPProvider(root)

	// Load peers as Start() would.
	cfg, err := p.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	p.mu.Lock()
	p.peers = cfg.Peers
	p.mu.Unlock()

	status := p.Status()
	if !status.Enabled {
		t.Error("Status.Enabled should be true")
	}
	if status.DeviceID != "node-gamma" {
		t.Errorf("Status.DeviceID = %q, want 'node-gamma'", status.DeviceID)
	}
	if status.PeerCount != 1 {
		t.Errorf("Status.PeerCount = %d, want 1", status.PeerCount)
	}
	if status.WatchDir == "" {
		t.Error("Status.WatchDir should not be empty")
	}
}

func TestBEPStatusNoConfig(t *testing.T) {
	root := t.TempDir()
	p := NewBEPProvider(root)

	status := p.Status()
	if status.Enabled {
		t.Error("Status.Enabled should be false with no config")
	}
	if status.DeviceID != "" {
		t.Errorf("Status.DeviceID should be empty, got %q", status.DeviceID)
	}
	if status.PeerCount != 0 {
		t.Errorf("Status.PeerCount should be 0, got %d", status.PeerCount)
	}
}

// ─── 13. ListPeers Copy Semantics ───────────────────────────────────────────────

func TestBEPListPeersCopySemantics(t *testing.T) {
	root := t.TempDir()
	p := NewBEPProvider(root)

	// Set peers directly.
	p.mu.Lock()
	p.peers = []BEPPeer{
		{DeviceID: "node-1", Name: "alpha", Address: "10.0.0.1:22000", Trusted: true},
		{DeviceID: "node-2", Name: "beta", Address: "10.0.0.2:22000", Trusted: false},
	}
	p.mu.Unlock()

	peers1 := p.ListPeers()
	if len(peers1) != 2 {
		t.Fatalf("ListPeers() returned %d peers, want 2", len(peers1))
	}

	// Mutate the returned slice.
	peers1[0].Name = "MUTATED"
	peers1 = append(peers1, BEPPeer{DeviceID: "node-3", Name: "gamma"})

	// Fetch again — internal state should be unaffected.
	peers2 := p.ListPeers()
	if len(peers2) != 2 {
		t.Fatalf("ListPeers() returned %d after mutation, want 2", len(peers2))
	}
	if peers2[0].Name != "alpha" {
		t.Errorf("ListPeers()[0].Name = %q after mutation, want 'alpha'", peers2[0].Name)
	}
}

func TestBEPListPeersEmpty(t *testing.T) {
	root := t.TempDir()
	p := NewBEPProvider(root)

	peers := p.ListPeers()
	if peers == nil {
		t.Error("ListPeers() should return non-nil empty slice")
	}
	if len(peers) != 0 {
		t.Errorf("ListPeers() should be empty, got %d", len(peers))
	}
}

// ─── Additional edge cases ──────────────────────────────────────────────────────

func TestBEPNewBEPProviderDefaultWatchDir(t *testing.T) {
	root := "/tmp/test-workspace"
	p := NewBEPProvider(root)

	expected := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if p.watchDir != expected {
		t.Errorf("watchDir = %q, want %q", p.watchDir, expected)
	}
	if p.root != root {
		t.Errorf("root = %q, want %q", p.root, root)
	}
}

func TestBEPScanModTimesEmptyDir(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	result := p.scanModTimes()
	if len(result) != 0 {
		t.Errorf("expected empty scan for empty dir, got %d entries", len(result))
	}
}

func TestBEPScanModTimesNonexistentDir(t *testing.T) {
	p := NewBEPProvider("/nonexistent/path")
	result := p.scanModTimes()
	if len(result) != 0 {
		t.Errorf("expected empty scan for nonexistent dir, got %d entries", len(result))
	}
}

func TestBEPReceiverAtomicWriteNoTmpLeftBehind(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	data := validAgentCRDYAML("clean")
	if err := p.ReceiveAgentCRD("peer-1", "clean.agent.yaml", data); err != nil {
		t.Fatalf("ReceiveAgentCRD: %v", err)
	}

	// Verify no .tmp file remains.
	entries, err := os.ReadDir(p.watchDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover .tmp file found: %s", e.Name())
		}
	}
}

func TestBEPRemoveAgentCRDRecordsEvent(t *testing.T) {
	root := setupBEPWatchDir(t)
	p := NewBEPProvider(root)

	// Write and remove.
	crdPath := filepath.Join(p.watchDir, "ephemeral.agent.yaml")
	if err := os.WriteFile(crdPath, validAgentCRDYAML("ephemeral"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := p.RemoveAgentCRD("peer-1", "ephemeral.agent.yaml"); err != nil {
		t.Fatalf("RemoveAgentCRD: %v", err)
	}

	history := p.History()
	if len(history) != 1 {
		t.Fatalf("expected 1 history event, got %d", len(history))
	}
	if history[0].Action != "delete" {
		t.Errorf("event action = %q, want 'delete'", history[0].Action)
	}
	if history[0].Filename != "ephemeral.agent.yaml" {
		t.Errorf("event filename = %q, want 'ephemeral.agent.yaml'", history[0].Filename)
	}
	if history[0].PeerID != "peer-1" {
		t.Errorf("event peerID = %q, want 'peer-1'", history[0].PeerID)
	}
}
