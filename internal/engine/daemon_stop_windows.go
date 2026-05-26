//go:build windows

// daemon_stop_windows.go — Windows process-stop helpers for bare-metal daemon.
//
// Windows lacks SIGTERM. The graceful equivalent here is `taskkill /PID` (no
// /F), which posts WM_CLOSE / CTRL-style close to the target so it can shut
// down cleanly. We then poll for the process to actually exit (up to a bounded
// interval) and only escalate to `taskkill /F /PID` (forceful) if it is still
// alive. proc.Kill() is the last-resort fallback if taskkill is unavailable.
//
// A CTRL_BREAK_EVENT approach via golang.org/x/sys/windows is deferred to
// follow-up work; it requires the child to have been started in its own
// console group, which the current --detach path does not yet arrange.

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const windowsStopTimeout = 5 * time.Second

// stopBareMetalPID stops the process with the given PID using a
// graceful-then-force strategy:
//
//  1. `taskkill /PID <pid>` (graceful — lets the process handle the close).
//  2. Poll up to windowsStopTimeout for the process to exit.
//  3. If still alive, `taskkill /F /PID <pid>` (forceful).
//  4. If taskkill is entirely unavailable, fall back to proc.Kill().
func stopBareMetalPID(pid int) error {
	// Step 1: graceful taskkill (no /F).
	graceful := exec.Command("taskkill", "/PID", strconv.Itoa(pid))
	gracefulErr := graceful.Run()

	// Step 2: poll for exit regardless of taskkill's exit code — a non-zero
	// code can simply mean "no window to close", and the process may still
	// terminate on its own.
	deadline := time.Now().Add(windowsStopTimeout)
	for time.Now().Before(deadline) {
		if !windowsPIDAlive(pid) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !windowsPIDAlive(pid) {
		return nil
	}

	// Step 3: forceful taskkill.
	force := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
	if err := force.Run(); err == nil {
		return nil
	}

	// Step 4: last-resort kill via the process handle. If even the graceful
	// taskkill failed to launch (taskkill missing), surface that context.
	proc, err := os.FindProcess(pid)
	if err != nil {
		if gracefulErr != nil {
			return fmt.Errorf("taskkill unavailable (%v) and FindProcess failed: %w", gracefulErr, err)
		}
		return err
	}
	return proc.Kill()
}

// windowsPIDAlive reports whether a process with the given PID is currently
// running, using `tasklist /FI "PID eq <pid>"`. tasklist prints a header and
// the matching row when found, or "INFO: No tasks..." when absent.
func windowsPIDAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		// If tasklist itself fails, assume the process may still be alive so we
		// don't prematurely report a clean stop.
		return true
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}

// detachServeProcess on Windows prints guidance and exits, because true
// daemonization (detached process + no console) requires CreateProcess with
// DETACHED_PROCESS which is not yet implemented. Use the PowerShell template
// documented in `cogos serve --help` instead.
func detachServeProcess(args []string) (int, error) {
	return 0, fmt.Errorf(
		"--detach is not yet implemented on Windows.\n" +
			"Background-run template (PowerShell):\n" +
			"  Start-Process cogos -ArgumentList 'serve' -WindowStyle Hidden -RedirectStandardOutput cogos.log -RedirectStandardError cogos.err\n" +
			"Or with nohup in Git Bash / WSL:\n" +
			"  nohup cogos serve &",
	)
}
