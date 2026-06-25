//go:build darwin

// spawn_unix.go — detached updater spawn for macOS.
//
// spawnDetachedUpdater re-execs the current binary as
// `cogos self-update --to <tag> ...` in a NEW session (Setsid) and Releases the
// child so it survives the daemon restart it will trigger. It replicates the
// Setsid+Release pattern from engine.detachProcess but writes to a SEPARATE log
// (<root>/.cog/run/selfupdate.log) so the updater's output never interleaves
// with the daemon's own cogos.log.
//
// The running daemon must NEVER swap its own binary in-process; the swap and
// restart happen only in this detached subprocess.
package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// spawnDetachedUpdater launches `cogos self-update` detached. root is the
// workspace root (used for the log path and passed through to the updater);
// toTag is the target release tag; repo is the GitHub repo; port is the health
// endpoint port the updater will poll after restart.
func spawnDetachedUpdater(root, toTag, repo string, port int) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("self-update: locate executable: %w", err)
	}

	logPath := filepath.Join(os.TempDir(), "cogos-selfupdate.log")
	if root != "" {
		logPath = filepath.Join(root, ".cog", "run", "selfupdate.log")
		if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o755); mkErr != nil {
			return fmt.Errorf("self-update: mkdir log dir: %w", mkErr)
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("self-update: open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("self-update: open /dev/null: %w", err)
	}
	defer devnull.Close()

	args := []string{
		"self-update",
		"--to", toTag,
		"--repo", repo,
		"--port", strconv.Itoa(port),
	}
	if root != "" {
		args = append(args, "--workspace", root)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("self-update: start detached updater: %w", err)
	}
	// Release the child so it runs independently of the daemon we are about to
	// restart out from under it.
	return cmd.Process.Release()
}
