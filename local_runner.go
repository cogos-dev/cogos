// local_runner.go — Bare-metal process supervisor for services with spec.local.
//
// The ServiceProvider delegates to this runner when a CRD declares a local
// execution block and the container runtime is unavailable (or no image is
// declared). Each supervised process is tracked via a PID file under
// .cog/run/services/<name>.pid; stdout/stderr are appended to <name>.log.
//
// Supervision model: there are no watcher goroutines. The reconcile loop
// re-checks liveness on each cycle (default 5 min) and re-creates crashed
// processes. This keeps the kernel simple and inherits the same cadence as
// every other managed resource.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LocalProcess captures the live state of a supervised bare-metal service.
//
// Cmd and Args record the *resolved* argv that LocalStart actually invoked.
// They exist so LocalStop can verify the live process at PID still matches
// what we started before sending signals — without that check, PID reuse
// after a crash or reboot can land an unrelated user process at this PID
// and we'd kill it (and its whole process group, since we use Setsid).
type LocalProcess struct {
	Name      string   `json:"name"`
	PID       int      `json:"pid"`
	StartedAt string   `json:"started_at"`
	CmdHash   string   `json:"cmd_hash"`
	Workdir   string   `json:"workdir"`
	LogPath   string   `json:"log_path"`
	Cmd       string   `json:"cmd,omitempty"`  // resolved executable path (post-venv lookup)
	Args      []string `json:"args,omitempty"` // argv after Cmd
	Running   bool     `json:"running"`        // populated by LocalStatus, not persisted
}

// localServicesDir returns the directory holding PID/log files for local services.
func localServicesDir(root string) string {
	return filepath.Join(root, ".cog", "run", "services")
}

func localPIDPath(root, name string) string {
	return filepath.Join(localServicesDir(root), name+".pid")
}

func localLogPath(root, name string) string {
	return filepath.Join(localServicesDir(root), name+".log")
}

// ─── Command construction ───────────────────────────────────────────────────────

// localCmdHash produces a stable hash of the command + args + workdir + venv
// + env so we can detect drift between declared spec and a running process
// without comparing argv verbatim (which is noisy across shell escaping
// variations).
//
// Venv is included because the same Command (e.g. "python3") resolves to a
// different interpreter under a different venv — without it, swapping the
// venv field leaves reconcile reporting "in sync" while running the wrong
// binary.
func localCmdHash(local *ServiceLocal, workdir string) string {
	h := sha256.New()
	h.Write([]byte(local.Command))
	h.Write([]byte{0})
	for _, a := range local.Args {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	h.Write([]byte(workdir))
	h.Write([]byte{0})
	// Venv influences which binary `Command` resolves to (see LocalStart's
	// venv lookup); changing it must invalidate the hash.
	h.Write([]byte(local.Venv))
	h.Write([]byte{0})
	// Sort env keys so hash is stable across map iteration orders.
	keys := make([]string, 0, len(local.Env))
	for k := range local.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(local.Env[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// resolveWorkdir returns the absolute working directory for a local service.
// Relative paths are resolved against the workspace root.
func resolveWorkdir(root string, local *ServiceLocal) string {
	if local.Workdir == "" {
		return root
	}
	if filepath.IsAbs(local.Workdir) {
		return local.Workdir
	}
	return filepath.Join(root, local.Workdir)
}

// resolveLocalCommand returns the executable path LocalStart would actually
// invoke for the given CRD — post-venv lookup. Shared with LocalStatusWithCRD
// so adoption of a legacy PID file compares the *same* argv the supervisor
// would produce on a fresh start. Extracted to keep both call sites in sync.
func resolveLocalCommand(root string, local *ServiceLocal) string {
	command := local.Command
	if venv := resolveVenv(root, local); venv != "" && !filepath.IsAbs(command) {
		candidate := filepath.Join(venv, "bin", command)
		if _, err := os.Stat(candidate); err == nil {
			command = candidate
		}
	}
	return command
}

// resolveVenv returns the absolute venv path (or empty string).
func resolveVenv(root string, local *ServiceLocal) string {
	if local.Venv == "" {
		return ""
	}
	if filepath.IsAbs(local.Venv) {
		return local.Venv
	}
	return filepath.Join(root, local.Venv)
}

// buildLocalEnv merges the parent env with venv adjustments and CRD-declared
// overrides. Venv bin/ is prepended to PATH and VIRTUAL_ENV is set so Python
// tooling behaves as if activated.
func buildLocalEnv(root string, local *ServiceLocal) []string {
	env := os.Environ()

	if venv := resolveVenv(root, local); venv != "" {
		binDir := filepath.Join(venv, "bin")
		out := make([]string, 0, len(env)+2)
		pathSet := false
		for _, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				out = append(out, "PATH="+binDir+string(os.PathListSeparator)+strings.TrimPrefix(e, "PATH="))
				pathSet = true
			} else if strings.HasPrefix(e, "VIRTUAL_ENV=") {
				continue
			} else {
				out = append(out, e)
			}
		}
		if !pathSet {
			out = append(out, "PATH="+binDir)
		}
		out = append(out, "VIRTUAL_ENV="+venv)
		env = out
	}

	for k, v := range local.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// ─── PID file I/O ───────────────────────────────────────────────────────────────

func readLocalProcess(root, name string) (*LocalProcess, error) {
	data, err := os.ReadFile(localPIDPath(root, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pid file for %s: %w", name, err)
	}
	var p LocalProcess
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pid file for %s: %w", name, err)
	}
	return &p, nil
}

func writeLocalProcess(root string, p *LocalProcess) error {
	dir := localServicesDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(localPIDPath(root, p.Name), data, 0o644)
}

func removeLocalPID(root, name string) {
	_ = os.Remove(localPIDPath(root, name))
}

// processAlive returns true if PID is a live process owned by this user.
// signal 0 delivers no signal but performs error checking.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess on Windows doesn't fail for dead PIDs; treat as alive.
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// psLookup is the seam used by verifyOwnership to read a process's argv.
// Tests override it; the production implementation shells out to `ps`.
//
// Returns the verbatim argv string (single line, fields joined by spaces)
// or an error if the process is gone or ps fails.
var psLookup = func(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// verifyOwnership reports whether the live process at pid is the one we
// started for this service, by comparing its argv against the cmd/args we
// recorded at LocalStart time. Used as a guard before sending signals so we
// don't kill an unrelated process that reused this PID after a crash or
// reboot.
//
// Returns:
//   - (true,  nil) when argv matches recorded cmd+args.
//   - (false, nil) when the process is gone, ps refuses to look it up, or
//     argv differs from what we expect (PID reuse, replacement, etc.).
//   - (false, err) only on unexpected internal errors. Callers should treat
//     non-nil err as "do not signal" — same as a false ownership result.
//
// The expectedCmd is the resolved binary path LocalStart used (post-venv
// lookup), and expectedArgs is the argv tail. An empty expectedCmd is
// treated as a legacy / unrecorded PID file; we refuse ownership in that
// case so the caller declines to signal — restart the service to repopulate.
//
// On Windows, ps is unavailable so we fall back to the liveness check.
func verifyOwnership(pid int, expectedCmd string, expectedArgs []string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	if runtime.GOOS == "windows" {
		// No portable ps; fall back to liveness — windows is not a primary
		// platform for the local runner today, but we don't want to leak
		// processes there either.
		return processAlive(pid), nil
	}
	if expectedCmd == "" {
		// Legacy PID file with no recorded argv — we can't verify, and the
		// safe default is to refuse signals. The caller will log + clear.
		return false, nil
	}

	actual, err := psLookup(pid)
	if err != nil {
		// Most common case: process doesn't exist → ps exits non-zero.
		// Treat as "not ours" so the caller stops without signaling.
		return false, nil
	}
	if actual == "" {
		return false, nil
	}

	expected := expectedCmd
	for _, a := range expectedArgs {
		expected += " " + a
	}
	return actual == strings.TrimSpace(expected), nil
}

// ─── Public API ─────────────────────────────────────────────────────────────────

// LocalStatus returns the tracked process for the named service, or nil if
// no PID file exists. The Running field is populated by verifying the PID
// is alive AND that its argv matches what we recorded — protecting against
// PID reuse misreporting an unrelated process as our service.
//
// Stale PID files (dead process or argv mismatch) are cleaned up
// automatically so the next reconcile treats this as a create.
//
// This form has no CRD context, so legacy PID files (written before Cmd/Args
// were recorded) cannot be adopted — they get cleared. Callers that *do* have
// the CRD in hand should use LocalStatusWithCRD instead: it falls back to
// re-deriving the expected argv from the CRD and adopting the live process
// when it matches. Used by FetchLive during reconcile so in-place kernel
// upgrades don't start a duplicate alongside an already-running service.
func LocalStatus(root, name string) (*LocalProcess, error) {
	return LocalStatusWithCRD(root, name, nil)
}

// LocalStatusWithCRD returns the tracked process for the named service, with
// an optional CRD used to recover from legacy PID files. Preserves all the
// strict-match guarantees of LocalStatus; only the "empty recorded cmd" case
// is changed: when a CRD is supplied and the live process's argv matches what
// the CRD would resolve to, the runner adopts the process (writes a fresh PID
// file with the recorded cmd/args) so subsequent calls use the fast path.
//
// Callers without the CRD (e.g. raw `cog service status` dumps) should keep
// using LocalStatus — adoption is reconciliation-time behavior, not a general
// status-read concern.
func LocalStatusWithCRD(root, name string, crd *ServiceCRD) (*LocalProcess, error) {
	p, err := readLocalProcess(root, name)
	if err != nil || p == nil {
		return p, err
	}
	if !processAlive(p.PID) {
		removeLocalPID(root, name)
		return p, nil
	}

	// Fast path: PID file has a recorded argv → verify against live argv.
	if p.Cmd != "" {
		owns, _ := verifyOwnership(p.PID, p.Cmd, p.Args)
		if !owns {
			log.Printf("[local-runner] warn: PID %d for service %s does not match recorded argv (cmd=%q); clearing stale PID file",
				p.PID, name, p.Cmd)
			removeLocalPID(root, name)
			return p, nil
		}
		p.Running = true
		return p, nil
	}

	// Legacy PID file (empty Cmd) — written before LocalProcess tracked argv.
	// Without a CRD we can't verify ownership, so fall back to the old strict
	// refusal: clear it and let the reconciler recreate the service.
	if crd == nil || crd.Spec.Local == nil {
		log.Printf("[local-runner] warn: PID %d for service %s has no recorded argv and no CRD available for adoption; clearing stale PID file",
			p.PID, name)
		removeLocalPID(root, name)
		return p, nil
	}

	// Adoption path: re-derive the expected argv from the CRD and compare
	// against the live process. If it matches, the PID really is ours — we
	// just didn't know it because the PID file predates argv tracking. Write
	// a fresh PID file with the resolved cmd so future calls take the fast
	// path above (no CRD needed).
	expectedCmd := resolveLocalCommand(root, crd.Spec.Local)
	expectedArgs := crd.Spec.Local.Args
	owns, _ := verifyOwnership(p.PID, expectedCmd, expectedArgs)
	if !owns {
		log.Printf("[local-runner] warn: PID %d for service %s has legacy PID file and live argv does not match CRD (expected cmd=%q); clearing stale PID file",
			p.PID, name, expectedCmd)
		removeLocalPID(root, name)
		return p, nil
	}

	log.Printf("[local-runner] adopting legacy PID %d for service %s (argv matches CRD cmd=%q)",
		p.PID, name, expectedCmd)
	p.Cmd = expectedCmd
	p.Args = append([]string(nil), expectedArgs...)
	p.Running = true
	if err := writeLocalProcess(root, p); err != nil {
		// Non-fatal — next reconcile will try again. Return Running=true
		// regardless since the live process is genuinely ours.
		log.Printf("[local-runner] warn: adoption succeeded but PID file rewrite failed for %s: %v", name, err)
	}
	return p, nil
}

// LocalStart launches the service in a detached process group, writes the
// PID file, and returns immediately. Logs stream to
// .cog/run/services/<name>.log. The caller must ensure no other process is
// already running under this service's PID file.
func LocalStart(root string, crd *ServiceCRD) (*LocalProcess, error) {
	if crd.Spec.Local == nil {
		return nil, fmt.Errorf("service %s: no local spec", crd.Metadata.Name)
	}
	local := crd.Spec.Local

	workdir := resolveWorkdir(root, local)
	if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("service %s: workdir %s not a directory", crd.Metadata.Name, workdir)
	}

	if err := os.MkdirAll(localServicesDir(root), 0o755); err != nil {
		return nil, fmt.Errorf("service %s: create run dir: %w", crd.Metadata.Name, err)
	}

	logFile, err := os.OpenFile(localLogPath(root, crd.Metadata.Name),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("service %s: open log: %w", crd.Metadata.Name, err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n=== %s started at %s ===\n", crd.Metadata.Name, nowISO())

	// Resolve command against the venv's bin/ when non-absolute. Shared with
	// LocalStatusWithCRD so adoption of legacy PID files compares argv the
	// exact same way.
	command := resolveLocalCommand(root, local)

	cmd := exec.Command(command, local.Args...)
	cmd.Dir = workdir
	cmd.Env = buildLocalEnv(root, local)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach stdin. Some Python MCP servers inspect stdin and exit if it's
	// not a pipe/TTY; we give them /dev/null so nothing blocks and nothing
	// triggers stdio-mode logic in the child.
	if devnull, err := os.Open(os.DevNull); err == nil {
		cmd.Stdin = devnull
		defer devnull.Close()
	}
	// Detach from the kernel's session/process group so kernel shutdown
	// (or any signal we propagate) doesn't cascade to supervised services.
	// Platform-specific: Setsid on Unix, CREATE_NEW_PROCESS_GROUP on Windows
	// (see local_runner_unix.go / local_runner_windows.go).
	configureProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("service %s: start: %w", crd.Metadata.Name, err)
	}

	// Release the child so we don't accumulate zombie state in the kernel.
	go func(p *os.Process) { _, _ = p.Wait() }(cmd.Process)

	proc := &LocalProcess{
		Name:      crd.Metadata.Name,
		PID:       cmd.Process.Pid,
		StartedAt: nowISO(),
		CmdHash:   localCmdHash(local, workdir),
		Workdir:   workdir,
		LogPath:   localLogPath(root, crd.Metadata.Name),
		Cmd:       command,
		Args:      append([]string(nil), local.Args...),
		Running:   true,
	}
	if err := writeLocalProcess(root, proc); err != nil {
		// Best-effort: kill the orphan we can't track.
		_ = cmd.Process.Kill()
		return nil, err
	}
	return proc, nil
}

// LocalStop sends SIGTERM, waits up to 10s, then SIGKILL. The PID file is
// removed regardless of outcome — either the process is gone or we've lost
// confidence that it is ours.
//
// Before signaling, the live process's argv is verified against the recorded
// cmd+args. If they don't match (PID reuse after a crash/reboot, manual
// replacement, etc.), we log a warning and clear the stale PID file *without
// sending any signal* — otherwise we'd kill an unrelated user process and,
// worse, its whole process group, since we start it in its own group.
func LocalStop(root, name string) error {
	p, err := readLocalProcess(root, name)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	defer removeLocalPID(root, name)

	if !processAlive(p.PID) {
		return nil
	}

	owns, _ := verifyOwnership(p.PID, p.Cmd, p.Args)
	if !owns {
		log.Printf("[local-runner] warn: refusing to signal PID %d for service %s — argv does not match recorded cmd=%q (likely PID reuse). Clearing stale PID file without signaling.",
			p.PID, name, p.Cmd)
		return nil
	}

	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return nil
	}

	// Graceful stop of the whole process group (platform-specific:
	// Kill(-pid, SIGTERM) on Unix, taskkill /T on Windows).
	terminateProcessGroup(proc, p.PID)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(p.PID) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Still alive after the grace period — force-kill the group.
	killProcessGroup(proc, p.PID)
	return nil
}

// ListLocalProcesses scans .cog/run/services/ for tracked services and
// returns their live status. Useful for reconcile orphan detection.
//
// No CRD map is consulted, so legacy PID files without recorded argv are
// cleared rather than adopted. Call sites that have CRDs in hand (reconcile's
// FetchLive) should use ListLocalProcessesWithCRDs instead to preserve live
// services across in-place kernel upgrades.
func ListLocalProcesses(root string) (map[string]*LocalProcess, error) {
	return ListLocalProcessesWithCRDs(root, nil)
}

// ListLocalProcessesWithCRDs scans the PID directory and reports live status
// per service, consulting the provided CRD map for adoption of legacy PID
// files. The map is keyed by service name; entries absent from the map use
// the strict (non-adopting) code path.
//
// nil map is equivalent to ListLocalProcesses: no adoption attempts.
func ListLocalProcessesWithCRDs(root string, crds map[string]*ServiceCRD) (map[string]*LocalProcess, error) {
	entries, err := os.ReadDir(localServicesDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*LocalProcess{}, nil
		}
		return nil, err
	}

	out := make(map[string]*LocalProcess)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".pid")
		var crd *ServiceCRD
		if crds != nil {
			crd = crds[name]
		}
		p, err := LocalStatusWithCRD(root, name, crd)
		if err != nil || p == nil {
			continue
		}
		out[name] = p
	}
	return out, nil
}
