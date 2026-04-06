// testhelper_test.go — shared test utilities for cogos-v3
//
// All helpers follow these conventions:
//   - Accept *testing.T as first argument
//   - Call t.Helper() immediately (so failure lines point at the caller)
//   - Use t.TempDir() for any filesystem work (auto-cleaned after test)
//   - Never sleep — use channels or context cancellation for synchronisation
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeWorkspace creates a minimal .cog/ workspace under a temp directory.
// It installs:
//   - .cog/config/identity.yaml (pointing at the test identity card)
//   - .cog/mem/ (empty, but walkable)
//   - .cog/ledger/ (empty)
//   - projects/cog_lab_package/identities/identity_test.md (from testdata/)
//
// Returns the workspace root path.
func makeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dirs := []string{
		filepath.Join(root, ".cog", "config"),
		filepath.Join(root, ".cog", "mem", "semantic"),
		filepath.Join(root, ".cog", "ledger"),
		filepath.Join(root, "projects", "cog_lab_package", "identities"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("makeWorkspace: mkdir %s: %v", d, err)
		}
	}

	writeTestFile(t, filepath.Join(root, ".cog", "config", "identity.yaml"),
		"default_identity: test\nidentity_directory: projects/cog_lab_package/identities\n")

	// Copy testdata/identity_test.md into the workspace.
	card, err := os.ReadFile("testdata/identity_test.md")
	if err != nil {
		t.Fatalf("makeWorkspace: read testdata/identity_test.md: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "projects", "cog_lab_package", "identities", "identity_test.md"), string(card))

	return root
}

// makeConfig returns a Config pointing at root with safe test defaults.
// Ticker intervals are set very long so they don't fire during short tests.
// Port is set to 0 (assign dynamically where needed).
func makeConfig(t *testing.T, root string) *Config {
	t.Helper()
	return &Config{
		WorkspaceRoot:         root,
		CogDir:                filepath.Join(root, ".cog"),
		Port:                  0,
		ConsolidationInterval: 99999,
		HeartbeatInterval:     99999,
		SalienceDaysWindow:    90,
	}
}

// makeNucleus returns a fully initialised Nucleus without touching the filesystem.
func makeNucleus(name, role string) *Nucleus {
	return &Nucleus{
		Name:          name,
		Role:          role,
		Card:          fmt.Sprintf("# %s\nRole: %s\n", name, role),
		WorkspaceRoot: "/tmp/test-workspace",
		LoadedAt:      time.Now(),
	}
}

// writeTestFile writes content to path, failing the test on any error.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeTestFile %s: %v", path, err)
	}
}

// mustLoadNucleus calls LoadNucleus and fatals the test on error.
func mustLoadNucleus(t *testing.T, cfg *Config) *Nucleus {
	t.Helper()
	n, err := LoadNucleus(cfg)
	if err != nil {
		t.Fatalf("LoadNucleus: %v", err)
	}
	return n
}

// waitForState polls process.State() until it equals want or the deadline expires.
// Uses a channel-based ticker (no raw sleep) to avoid flakiness.
func waitForState(t *testing.T, p *Process, want ProcessState, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("process state: got %s after %s; want %s", p.State(), timeout, want)
		case <-tick.C:
			if p.State() == want {
				return
			}
		}
	}
}
