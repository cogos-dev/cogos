//go:build !windows

// daemon_detach_unix.go — detached daemon launch for Unix/macOS.
//
// detachProcess re-executes the current binary with the supplied args in a
// new session (Setsid) so it survives the parent's terminal close. stdout and
// stderr are redirected to <workspace>/.cog/run/cogos.log; stdin is wired to
// /dev/null. The calling process exits with 0 after the child starts.

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// detachProcess re-launches the binary (determined via os.Executable) with
// args in a detached session. It returns the child's PID so the caller can
// record daemon state before the child has finished booting (closing the
// race where `cogos stop` runs before the child writes its own state file).
func detachProcess(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate executable: %w", err)
	}

	// Determine log path from workspace arg if present; fall back to the OS
	// temp dir (never the cwd, which may be read-only or transient).
	logPath := filepath.Join(os.TempDir(), "cogos-daemon.log")
	for i, a := range args {
		if (a == "--workspace" || a == "-workspace") && i+1 < len(args) {
			logPath = filepath.Join(args[i+1], ".cog", "run", "cogos.log")
			if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
				return 0, fmt.Errorf("mkdir log dir: %w", err)
			}
			break
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devnull.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start detached daemon: %w", err)
	}

	pid := cmd.Process.Pid
	fmt.Fprintf(os.Stderr, "cogos daemon started (pid %d), logging to %s\n", pid, logPath)
	// Release the child so it runs independently.
	_ = cmd.Process.Release()
	return pid, nil
}
