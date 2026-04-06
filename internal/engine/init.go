// init.go — cogos init command
//
// Scaffolds a new CogOS workspace with the minimum structure needed for
// the daemon to start: config files, memory directories, a default
// identity card, and an empty ledger.
//
// Idempotent: does not overwrite existing files. Safe to run on an
// existing workspace to fill in missing structure.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// initDirs are the directories created by cogos init.
var initDirs = []string{
	".cog/config",
	".cog/agents/identities",
	".cog/mem/semantic",
	".cog/mem/episodic",
	".cog/mem/procedural",
	".cog/mem/reflective",
	".cog/mem/working",
	".cog/ledger",
	".cog/run",
	".cog/blobs",
}

// initFile maps a target path (relative to workspace root) to its
// embedded default source path.
type initFile struct {
	target string // e.g. ".cog/config/kernel.yaml"
	source string // e.g. "defaults/kernel.yaml"
}

var initFiles = []initFile{
	{".cog/config/kernel.yaml", "defaults/kernel.yaml"},
	{".cog/config/identity.yaml", "defaults/identity.yaml"},
	{".cog/config/providers.yaml", "defaults/providers.yaml"},
	{".cog/agents/identities/identity_cogos.md", "defaults/identity.md"},
}

// RunInit scaffolds a CogOS workspace at the given root directory.
// It creates directories and writes default config files, skipping
// any that already exist.
func RunInit(workspaceRoot string) error {
	if workspaceRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		workspaceRoot = wd
	}

	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	workspaceRoot = abs

	fmt.Fprintf(os.Stderr, "Initializing CogOS workspace at %s\n", workspaceRoot)

	// Create directories.
	for _, dir := range initDirs {
		target := filepath.Join(workspaceRoot, dir)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	fmt.Fprintf(os.Stderr, "  directories: %d created\n", len(initDirs))

	// Write default files (skip existing).
	written := 0
	skipped := 0
	for _, f := range initFiles {
		target := filepath.Join(workspaceRoot, f.target)

		// Don't overwrite existing files.
		if _, err := os.Stat(target); err == nil {
			skipped++
			continue
		}

		data, err := defaultsFS.ReadFile(f.source)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", f.source, err)
		}

		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.target, err)
		}
		written++
	}
	fmt.Fprintf(os.Stderr, "  config files: %d written, %d already existed\n", written, skipped)

	// Write VERSION marker.
	versionPath := filepath.Join(workspaceRoot, ".cog", "VERSION")
	if _, err := os.Stat(versionPath); os.IsNotExist(err) {
		_ = os.WriteFile(versionPath, []byte("3.0.0\n"), 0o644)
	}

	fmt.Fprintf(os.Stderr, "\nWorkspace ready. Start the daemon with:\n")
	fmt.Fprintf(os.Stderr, "  cogos serve --workspace %s\n\n", workspaceRoot)

	return nil
}
