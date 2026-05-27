//go:build !windows

// local_runner_unix.go — Unix process-group primitives for the bare-metal
// service supervisor.
//
// Supervised services are started in their own session (Setsid) so that
// signals propagated to the kernel's process group do not cascade into the
// children, and so that stopping a service can tear down its whole subtree via
// a negative-PID signal. See local_runner_windows.go for the Windows analog
// (CREATE_NEW_PROCESS_GROUP + taskkill /T).

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup starts the child in its own session/process group so
// kernel shutdown (or any signal we propagate) does not cascade to supervised
// services, and so the whole subtree can be signaled later via -PID.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateProcessGroup sends SIGTERM to the whole process group (negative
// PID) and to the leader process directly. Both are best-effort.
func terminateProcessGroup(proc *os.Process, pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = proc.Signal(syscall.SIGTERM)
}

// killProcessGroup sends SIGKILL to the whole process group and to the leader
// process directly. Both are best-effort.
func killProcessGroup(proc *os.Process, pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = proc.Signal(syscall.SIGKILL)
}
