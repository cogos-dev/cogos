//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func setSysProcAttrImpl(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
