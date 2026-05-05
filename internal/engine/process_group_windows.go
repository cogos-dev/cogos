//go:build windows

// process_group_windows.go — Windows fallbacks for process-group helpers.
//
// Windows lacks Unix process groups; subprocess-tree termination would
// require a Job Object via golang.org/x/sys/windows. ClaudeCodeProvider
// builds and runs on Windows but may orphan grandchildren on context
// cancellation; the direct child is still terminated.

package engine

import "os/exec"

// setProcessGroup is a no-op on Windows.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup terminates the direct child process. Grandchildren are
// not signalled.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
