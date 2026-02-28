// bep_provider.go
// BEP sync provider for agent definition distribution across nodes.
//
// Phase 1: file watching + local change detection + reconciliation triggering.
// Phase 2 (future): actual BEP protocol integration via syncthing/lib/protocol.
//
// The provider watches .cog/bin/agents/definitions/ for CRD file changes and
// invokes the onChange callback when files are created, modified, or deleted.
// This forms the local half of the sync loop — the BEP transport layer will
// be wired in later to propagate changes to peer nodes.
//
// Architecture reference: cog://mem/semantic/architecture/bep-agent-sync-spec

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// BEPProvider manages agent definition distribution across nodes.
// Phase 1: file watching + local change detection.
// Phase 2 (future): actual BEP protocol integration.
type BEPProvider struct {
	root     string // workspace root
	watchDir string // .cog/bin/agents/definitions/
	peers    []BEPPeer
	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	onChange         func(filename string)   // primary callback when a CRD file changes
	onChangeHandlers []func(filename string) // additional change handlers (e.g., BEPEngine)
	lastSync time.Time
	watcher  *fsnotify.Watcher    // nil if fsnotify unavailable; falls back to polling
	receiver *receiverState       // ring buffer for received CRD events (see bep_receiver.go)
}

// BEPPeer represents a known peer node.
type BEPPeer struct {
	DeviceID string    `json:"deviceId" yaml:"deviceId"`
	Address  string    `json:"address" yaml:"address"`     // host:port or tailscale address
	Name     string    `json:"name" yaml:"name"`
	Trusted  bool      `json:"trusted" yaml:"trusted"`
	LastSeen time.Time `json:"lastSeen,omitempty" yaml:"lastSeen,omitempty"`
}

// BEPConfig holds cluster configuration loaded from .cog/config/cluster.yaml.
type BEPConfig struct {
	Enabled    bool      `yaml:"enabled"`
	DeviceID   string    `yaml:"deviceId,omitempty"`   // this node's ID
	NodeName   string    `yaml:"nodeName,omitempty"`   // human-readable node name
	ListenPort int       `yaml:"listenPort,omitempty"` // BEP listen port (default 22000)
	CertDir    string    `yaml:"certDir,omitempty"`    // TLS cert directory (default ~/.cog/etc)
	Peers      []BEPPeer `yaml:"peers,omitempty"`
	SyncDirs   []string  `yaml:"syncDirs,omitempty"`   // directories to sync
	Discovery  string    `yaml:"discovery,omitempty"`   // "static", "tailscale", "mdns"
}

// BEPSyncStatus returns current sync state for the BEP provider.
type BEPSyncStatus struct {
	Enabled   bool      `json:"enabled"`
	DeviceID  string    `json:"deviceId"`
	PeerCount int       `json:"peerCount"`
	WatchDir  string    `json:"watchDir"`
	LastSync  time.Time `json:"lastSync,omitempty"`
}

// ─── Constructor ────────────────────────────────────────────────────────────────

// NewBEPProvider creates a new provider for the given workspace root.
// The watch directory defaults to {root}/.cog/bin/agents/definitions/.
func NewBEPProvider(root string) *BEPProvider {
	return &BEPProvider{
		root:     root,
		watchDir: filepath.Join(root, ".cog", "bin", "agents", "definitions"),
		stopCh:   make(chan struct{}),
		receiver: &receiverState{
			history:    make([]ReceivedEvent, 0, receiverMaxHistory),
			maxHistory: receiverMaxHistory,
		},
	}
}

// ─── Configuration ──────────────────────────────────────────────────────────────

// LoadConfig reads cluster config from .cog/config/cluster.yaml.
// Returns a default (disabled) config if the file does not exist.
func (p *BEPProvider) LoadConfig() (*BEPConfig, error) {
	cfgPath := filepath.Join(p.root, ".cog", "config", "cluster.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &BEPConfig{Enabled: false, Discovery: "static"}, nil
		}
		return nil, fmt.Errorf("read cluster config: %w", err)
	}

	var cfg BEPConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cluster config: %w", err)
	}

	return &cfg, nil
}

// ─── Lifecycle ──────────────────────────────────────────────────────────────────

// Start begins watching for CRD file changes in the definitions directory.
// When a change is detected on an *.agent.yaml file, calls the onChange callback.
// Uses fsnotify for event-driven watching with a polling fallback for robustness.
func (p *BEPProvider) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("bep provider already running")
	}

	// Ensure the watch directory exists.
	if err := os.MkdirAll(p.watchDir, 0755); err != nil {
		return fmt.Errorf("create watch dir: %w", err)
	}

	// Try fsnotify first; fall back to polling if it fails.
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[bep] fsnotify unavailable (%v), using polling fallback", err)
		fsWatcher = nil
	} else {
		if err := fsWatcher.Add(p.watchDir); err != nil {
			log.Printf("[bep] cannot watch %s (%v), using polling fallback", p.watchDir, err)
			fsWatcher.Close()
			fsWatcher = nil
		}
	}
	p.watcher = fsWatcher

	// Load peers from config.
	cfg, err := p.LoadConfig()
	if err != nil {
		log.Printf("[bep] warning: could not load cluster config: %v", err)
	} else {
		p.peers = cfg.Peers
	}

	p.running = true
	p.stopCh = make(chan struct{})

	if fsWatcher != nil {
		go p.runFSNotify(fsWatcher)
	} else {
		go p.runPolling()
	}

	log.Printf("[bep] provider started, watching %s", p.watchDir)
	return nil
}

// Stop halts the file watcher and cleans up resources.
func (p *BEPProvider) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running {
		return
	}

	close(p.stopCh)
	if p.watcher != nil {
		p.watcher.Close()
		p.watcher = nil
	}
	p.running = false
	log.Printf("[bep] provider stopped")
}

// ─── Callbacks ──────────────────────────────────────────────────────────────────

// OnFileChange sets the primary callback for CRD file changes.
// The callback receives the basename of the file that changed (e.g. "whirl.agent.yaml").
func (p *BEPProvider) OnFileChange(fn func(filename string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
}

// AddChangeHandler registers an additional change handler (e.g., BEPEngine.NotifyLocalChange).
// Multiple handlers can be registered; all are called on each change.
func (p *BEPProvider) AddChangeHandler(fn func(filename string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChangeHandlers = append(p.onChangeHandlers, fn)
}

// ─── Queries ────────────────────────────────────────────────────────────────────

// ListPeers returns known peer nodes from the loaded configuration.
func (p *BEPProvider) ListPeers() []BEPPeer {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]BEPPeer, len(p.peers))
	copy(out, p.peers)
	return out
}

// Status returns current sync state.
func (p *BEPProvider) Status() BEPSyncStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	cfg, _ := p.LoadConfig()
	deviceID := ""
	enabled := false
	if cfg != nil {
		deviceID = cfg.DeviceID
		enabled = cfg.Enabled
	}

	return BEPSyncStatus{
		Enabled:   enabled,
		DeviceID:  deviceID,
		PeerCount: len(p.peers),
		WatchDir:  p.watchDir,
		LastSync:  p.lastSync,
	}
}

// ─── Internal: fsnotify event loop ──────────────────────────────────────────────

// runFSNotify watches for filesystem events via fsnotify.
// Coalesces rapid events with a short debounce window to avoid firing
// multiple callbacks for a single logical write (editor save sequences,
// atomic rename patterns, etc.).
func (p *BEPProvider) runFSNotify(w *fsnotify.Watcher) {
	// debounce: collect events for 500ms before firing callback
	const debounce = 500 * time.Millisecond
	pending := make(map[string]struct{})
	var timer *time.Timer

	flushPending := func() {
		p.mu.Lock()
		if !p.running {
			p.mu.Unlock()
			return
		}
		cb := p.onChange
		extraHandlers := make([]func(string), len(p.onChangeHandlers))
		copy(extraHandlers, p.onChangeHandlers)
		p.lastSync = time.Now()
		p.mu.Unlock()

		for name := range pending {
			log.Printf("[bep] change detected: %s", name)
			if cb != nil {
				cb(name)
			}
			for _, h := range extraHandlers {
				h(name)
			}
		}
		pending = make(map[string]struct{})
	}

	for {
		select {
		case <-p.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return

		case event, ok := <-w.Events:
			if !ok {
				return
			}
			// Only care about agent CRD files.
			base := filepath.Base(event.Name)
			if !isAgentCRDFile(base) {
				continue
			}
			// Track relevant operations: create, write, remove, rename.
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			pending[base] = struct{}{}
			// Reset debounce timer.
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, flushPending)

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("[bep] fsnotify error: %v", err)
		}
	}
}

// ─── Internal: polling fallback ─────────────────────────────────────────────────

// runPolling checks file modification times every 5 seconds.
// Used when fsnotify is unavailable (e.g. network filesystems, container mounts).
func (p *BEPProvider) runPolling() {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// snapshot: filename -> modtime
	snapshot := p.scanModTimes()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			current := p.scanModTimes()
			p.diffAndNotify(snapshot, current)
			snapshot = current
		}
	}
}

// scanModTimes returns a map of filename -> modification time for all
// agent CRD files in the watch directory.
func (p *BEPProvider) scanModTimes() map[string]time.Time {
	result := make(map[string]time.Time)
	entries, err := os.ReadDir(p.watchDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() || !isAgentCRDFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result[entry.Name()] = info.ModTime()
	}
	return result
}

// diffAndNotify compares old and new snapshots and fires the onChange callback
// for any files that were created, modified, or deleted.
func (p *BEPProvider) diffAndNotify(old, current map[string]time.Time) {
	p.mu.Lock()
	cb := p.onChange
	extraHandlers := make([]func(string), len(p.onChangeHandlers))
	copy(extraHandlers, p.onChangeHandlers)
	p.mu.Unlock()

	if cb == nil && len(extraHandlers) == 0 {
		return
	}

	notifyAll := func(name string) {
		if cb != nil {
			cb(name)
		}
		for _, h := range extraHandlers {
			h(name)
		}
	}

	changed := false

	// Detect creates and modifications.
	for name, modTime := range current {
		oldTime, existed := old[name]
		if !existed || !modTime.Equal(oldTime) {
			log.Printf("[bep] change detected (poll): %s", name)
			notifyAll(name)
			changed = true
		}
	}

	// Detect deletions.
	for name := range old {
		if _, exists := current[name]; !exists {
			log.Printf("[bep] deletion detected (poll): %s", name)
			notifyAll(name)
			changed = true
		}
	}

	if changed {
		p.mu.Lock()
		p.lastSync = time.Now()
		p.mu.Unlock()
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────────

// isAgentCRDFile returns true if the filename matches the agent CRD naming convention.
func isAgentCRDFile(name string) bool {
	return strings.HasSuffix(name, ".agent.yaml")
}
