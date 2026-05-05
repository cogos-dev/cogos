//go:build unix

// process_group_unix.go — Unix process-group helpers for ClaudeCodeProvider.
//
// claude(1) commonly spawns dash-style shells that fork helper processes
// (mcp servers, tool sandboxes). Killing only the direct child orphans
// those grandchildren, which keeps stdout open and blocks drainStreamJSON
// until they exit naturally. Setpgid + negative-PID kill signals the whole
// process tree atomically.

package engine

import (
	"os/exec"
	"syscall"
)

// setProcessGroup configures cmd to start in its own process group so the
// whole subprocess tree can be signalled together via killProcessGroup.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGTERM to the entire process group rooted at
// cmd's process. Returns nil if the process has not been started yet.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
