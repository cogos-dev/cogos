//go:build !windows

// cli_install_unix.go — PATH installation for Unix/macOS.

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cogBinDir returns the absolute path to ~/.cog/bin.
func cogBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".cog", "bin")
}

// addCogBinToPath appends an export line for ~/.cog/bin to the user's shell
// rc file. It detects the current shell from $SHELL; defaults to ~/.bashrc
// when the shell is unknown.
func addCogBinToPath() error {
	binDir := cogBinDir()
	exportLine := fmt.Sprintf(`export PATH="%s:$PATH"`, binDir)
	marker := "# added by cogos install --add-path"

	rcFile := detectShellRC()

	// Read existing content; skip if the line is already present.
	existing, err := os.ReadFile(rcFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", rcFile, err)
	}
	if strings.Contains(string(existing), binDir) {
		fmt.Fprintf(os.Stderr, "%s is already in PATH config (%s); nothing to do.\n", binDir, rcFile)
		return nil
	}

	// Ensure the rc file parent directory exists.
	if err := os.MkdirAll(filepath.Dir(rcFile), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(rcFile), err)
	}

	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", rcFile, err)
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "\n%s\n%s\n", marker, exportLine); err != nil {
		return fmt.Errorf("write %s: %w", rcFile, err)
	}

	fmt.Fprintf(os.Stderr, "Added %s to PATH in %s\n", binDir, rcFile)
	fmt.Fprintf(os.Stderr, "Restart your shell or run: source %s\n", rcFile)
	return nil
}

// detectShellRC returns the rc file path for the current shell.
func detectShellRC() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish")
	case "bash":
		// bash sources different files per platform: macOS Terminal opens a
		// login shell that reads .bash_profile, while most Linux interactive
		// shells read .bashrc. Prefer an existing .bash_profile (covers the
		// macOS login-shell case); otherwise fall back to .bashrc.
		if _, err := os.Stat(filepath.Join(home, ".bash_profile")); err == nil {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	default:
		return filepath.Join(home, ".bashrc")
	}
}
