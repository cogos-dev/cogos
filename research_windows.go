//go:build windows

package main

import "os/exec"

func setSysProcAttrImpl(cmd *exec.Cmd) {
	// No Setsid on Windows — process inherits parent group
}
