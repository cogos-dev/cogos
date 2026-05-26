//go:build !windows

// daemon_stop_unix.go — Unix signal helpers for bare-metal daemon stop.

package engine

import (
	"os"
	"syscall"
)

// stopBareMetalPID sends SIGTERM to the process with the given PID.
func stopBareMetalPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// detachServeProcess re-launches the current executable in a detached
// session (Setsid) with stdio redirected to the workspace log. It returns the
// detached child's PID so the caller can record daemon state immediately.
func detachServeProcess(args []string) (int, error) {
	return detachProcess(args)
}
