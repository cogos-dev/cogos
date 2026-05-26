//go:build windows

// cli_install_windows.go — PATH installation for Windows.
//
// Full registry/setx write is deferred to follow-up work (tracked in #320).
// For now, this prints a PowerShell snippet the user can run in an elevated
// prompt, which is the recommended approach for per-user PATH edits on Windows.

package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// cogBinDir returns the absolute path to %USERPROFILE%\.cog\bin.
func cogBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("USERPROFILE")
	}
	return filepath.Join(home, ".cog", "bin")
}

// addCogBinToPath prints instructions for adding ~/.cog/bin to the Windows
// user PATH. Automated setx / registry write is tracked in issue #320.
func addCogBinToPath() error {
	binDir := cogBinDir()

	fmt.Fprintf(os.Stderr, "Automated PATH update is not yet implemented on Windows.\n\n")
	fmt.Fprintf(os.Stderr, "Run ONE of the following (restart your terminal after):\n\n")
	fmt.Fprintf(os.Stderr, "  PowerShell (per-user, persistent):\n")
	fmt.Fprintf(os.Stderr, "    $p = [System.Environment]::GetEnvironmentVariable('PATH','User')\n")
	fmt.Fprintf(os.Stderr, "    [System.Environment]::SetEnvironmentVariable('PATH', \"%s;$p\", 'User')\n\n", binDir)
	fmt.Fprintf(os.Stderr, "  Command Prompt (per-user, persistent):\n")
	fmt.Fprintf(os.Stderr, "    setx PATH \"%%%s%%;%%PATH%%\"\n\n", binDir)
	fmt.Fprintf(os.Stderr, "  Current session only:\n")
	fmt.Fprintf(os.Stderr, "    $env:PATH = \"%s;$env:PATH\"\n\n", binDir)
	fmt.Fprintf(os.Stderr, "Automated write will be added in a follow-up (see #320).\n")
	return nil
}
