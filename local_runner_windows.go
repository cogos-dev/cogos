//go:build windows

// local_runner_windows.go — Windows process-group primitives for the
// bare-metal service supervisor.
//
// Windows has no Setsid / negative-PID signaling. The analog is to start the
// child in a new process group (CREATE_NEW_PROCESS_GROUP) and tear down the
// whole subtree with `taskkill /T` (tree). Graceful stop uses taskkill without
// /F (posts a close request the child can handle); force-kill adds /F and, as
// a last resort, falls back to the process handle's Kill(). See
// local_runner_unix.go for the Unix analog (Setsid + Kill(-pid)).

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configureProcessGroup starts the child in its own process group so kernel
// shutdown does not cascade to supervised services, and so the whole subtree
// can be torn down later via `taskkill /T`.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateProcessGroup requests a graceful stop of the process tree via
// `taskkill /PID <pid> /T` (no /F). Best-effort; the caller polls for exit and
// escalates to killProcessGroup if the process survives the grace period.
func terminateProcessGroup(_ *os.Process, pid int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T").Run()
}

// killProcessGroup force-kills the process tree via `taskkill /F /PID <pid> /T`
// and falls back to the process handle's Kill() if taskkill is unavailable.
func killProcessGroup(proc *os.Process, pid int) {
	if err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid), "/T").Run(); err == nil {
		return
	}
	if proc != nil {
		_ = proc.Kill()
	}
}
