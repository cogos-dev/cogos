//go:build darwin

// cli_selfupdate_unix.go — the safety-critical updater (darwin auto-apply).
//
// This file runs ONLY in the detached `cogos self-update --to <tag>` subprocess
// spawned by the reconcile provider's ApplyPlan (or invoked manually). The
// running daemon never reaches this code in-process. The full sequence:
//
//	lock → cleanup orphans → resolve → download → verify(sha256) → verify(version)
//	     → backup(copy) → atomic swap(rename) → kickstart → health poll
//	     → on failure: rollback(restore .bak) → kickstart → re-poll → loud log
//
// Every destructive step has an all-or-nothing recovery. The .bak file is the
// rollback point; the running binary is never left absent (backup is a COPY, so
// the only window is the atomic rename of the new binary over cogos).
//
// launchctl is darwin-only, so this file is build-tagged `darwin`. Other
// platforms get the notify-only stubs in cli_selfupdate_other.go (linux/bsd) and
// cli_selfupdate_windows.go, where runSelfUpdateApply returns
// errSelfUpdateUnsupported — those daemons are supervised differently.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// kernelLaunchdLabel is the launchd job label for the CogOS kernel.
// Verified against service_supervisor_launchctl.go (com.cogos.kernel).
const kernelLaunchdLabel = "com.cogos.kernel"

// Timeouts for the health gates.
const (
	healthDeadline      = 30 * time.Second
	healthPollInterval  = 2 * time.Second
	rollbackHealthLimit = 15 * time.Second
	orphanMaxAge        = 24 * time.Hour
)

// selfUpdater carries the inputs plus the injectable seams the integration tests
// replace to exercise the swap/rollback branches without touching launchd or the
// live daemon.
type selfUpdater struct {
	repo   string
	toTag  string
	root   string
	port   int
	force  bool
	manual bool

	// Seams (defaulted in newSelfUpdater; overridden in tests).
	binDir         string                                               // directory holding the cogos binary
	runDirOverride string                                               // when set, runDir() returns this (tests; avoids touching real ~/.cog/run)
	kickstart      func() error                                         // restart the kernel
	healthPoll     func(deadline time.Duration) error                   // poll /health until target version or deadline
	rollbackPoll   func(deadline time.Duration, expectTag string) error // re-poll after rollback
	download       func(ctx context.Context, url, dst string) error     // fetch url → dst
	fetchText      func(ctx context.Context, url string) (string, error)
	smokeTest      func(binPath string) (version string, err error) // run `<bin> version`
	logf           func(format string, args ...any)
}

// resolveAssetURLsFn is the network release-resolution seam, overridable in
// tests so the swap core can be exercised without GitHub.
var resolveAssetURLsFn = resolveAssetURLs

// newSelfUpdater builds a selfUpdater with production seams.
func newSelfUpdater(p selfUpdateApplyParams) *selfUpdater {
	u := &selfUpdater{
		repo:   p.Repo,
		toTag:  p.ToTag,
		root:   p.Workspace,
		port:   p.Port,
		force:  p.Force,
		manual: p.Manual,
		binDir: cogBinDir(),
	}
	if u.port == 0 {
		u.port = 6931
	}
	u.kickstart = u.kickstartKernel
	u.healthPoll = u.pollHealth
	u.rollbackPoll = u.healthPollExpect
	u.download = downloadFile
	u.fetchText = fetchText
	u.smokeTest = smokeTestVersion
	u.logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "self-update: "+format+"\n", args...)
	}
	return u
}

// runSelfUpdateApply is the darwin updater entry point (this file is
// darwin-only; non-darwin Unix uses the notify-only stub in
// cli_selfupdate_other.go).
func runSelfUpdateApply(p selfUpdateApplyParams) error {
	return newSelfUpdater(p).run()
}

// run executes the full sequence with single-flight locking.
func (u *selfUpdater) run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// GATE J — single-flight lockfile.
	lockPath := u.lockPath()
	release, held, err := acquireUpdaterLock(lockPath)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if !held {
		u.logf("another updater already in progress (lock %s); exiting", lockPath)
		return nil
	}
	defer release()

	// Step 6 — clean orphan temp files from prior failed runs.
	u.cleanupOrphans()

	// Manual-path downgrade guard (the auto path enforced this in ComputePlan).
	// A downgrade requires an explicit --force on the interactive `cogos
	// self-update` path; the updater path (--to set by ApplyPlan) is not "manual"
	// and a pinned config already authorised the move.
	if u.manual && !u.force && isDowngrade(u.toTag, Version) {
		return fmt.Errorf("refusing downgrade %s → %s without --force", Version, u.toTag)
	}

	return u.runApply(ctx)
}

// runApply performs the download→verify→swap→restart→health→rollback core.
func (u *selfUpdater) runApply(ctx context.Context) error {
	binPath := filepath.Join(u.binDir, "cogos")
	bakPath := binPath + ".bak"
	newPath := filepath.Join(u.binDir, fmt.Sprintf(".cogos.new.%d", os.Getpid()))

	// Step 7 — resolve fresh asset URLs for the target tag.
	target, err := resolveAssetURLsFn(ctx, u.repo, u.toTag)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", u.toTag, err)
	}

	// Pre-flight disk check: require ≥ 2× the running binary size free.
	if err := u.preflightDisk(binPath); err != nil {
		return err
	}

	// GATE K — download the new binary into the SAME dir (atomic rename target).
	u.logf("downloading %s", target.AssetURL)
	if err := u.download(ctx, target.AssetURL, newPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("download binary: %w", err)
	}
	// From here on, always clean up newPath on any abort before the swap.
	cleanupNew := func() { _ = os.Remove(newPath) }

	// GATE L — checksum verify against checksums.txt.
	sums, err := u.fetchText(ctx, target.ChecksumURL)
	if err != nil {
		cleanupNew()
		return fmt.Errorf("download checksums: %w", err)
	}
	if err := verifyChecksum(newPath, target.AssetName, sums); err != nil {
		cleanupNew()
		return fmt.Errorf("checksum verify: %w (running binary untouched)", err)
	}

	// Step 10 — make the new binary executable.
	if err := os.Chmod(newPath, 0o755); err != nil {
		cleanupNew()
		return fmt.Errorf("chmod new binary: %w", err)
	}

	// GATE M — run the new binary's `version` and confirm it reports the target.
	got, err := u.smokeTest(newPath)
	if err != nil {
		cleanupNew()
		return fmt.Errorf("smoke-test new binary: %w (running binary untouched)", err)
	}
	if !versionFieldEqual(got, u.toTag) {
		cleanupNew()
		return fmt.Errorf("downloaded binary reports %q, expected %s (running binary untouched)", got, u.toTag)
	}

	// Step 12 — backup the current binary by COPY (old binary stays in place).
	if err := copyFileMode(binPath, bakPath, 0o755); err != nil {
		cleanupNew()
		return fmt.Errorf("backup current binary: %w (running binary untouched)", err)
	}

	// GATE N — atomic swap: rename new over the current binary.
	if err := os.Rename(newPath, binPath); err != nil {
		cleanupNew() // best-effort: drop any partial temp file
		// Swap failed; restore from .bak to be safe (binPath may be intact, but
		// restore is idempotent and known-good).
		if rerr := os.Rename(bakPath, binPath); rerr != nil {
			u.logf("FATAL: swap failed (%v) AND restore failed (%v); manually run: cp %s %s", err, rerr, bakPath, binPath)
			return fmt.Errorf("swap failed and restore failed: %w", err)
		}
		return fmt.Errorf("atomic swap failed, restored from backup: %w", err)
	}
	u.logf("swapped binary to %s (backup at %s)", u.toTag, bakPath)

	// Step 14 — restart the kernel.
	if err := u.kickstart(); err != nil {
		u.logf("kickstart failed after swap: %v; rolling back", err)
		return u.rollback(binPath, bakPath, fmt.Errorf("kickstart: %w", err))
	}

	// GATE O — health poll: require status ok AND version == target within 30s.
	if err := u.healthPoll(healthDeadline); err != nil {
		u.logf("health did not converge to %s within %s: %v; rolling back", u.toTag, healthDeadline, err)
		return u.rollback(binPath, bakPath, err)
	}

	// Success — remove the backup, release lock (deferred), exit 0.
	if err := os.Remove(bakPath); err != nil && !os.IsNotExist(err) {
		u.logf("warning: could not remove backup %s: %v", bakPath, err)
	}
	u.logf("update to %s complete and healthy", u.toTag)
	return nil
}

// rollback restores the backup binary and re-validates health. GATE P.
func (u *selfUpdater) rollback(binPath, bakPath string, cause error) error {
	if err := os.Rename(bakPath, binPath); err != nil {
		u.logf("FATAL: rollback restore failed: %v; the operator must manually run: cp %s %s", err, bakPath, binPath)
		return fmt.Errorf("rollback restore failed (original cause: %v): %w", cause, err)
	}
	u.logf("restored previous binary from %s", bakPath)

	if err := u.kickstart(); err != nil {
		u.logf("FATAL: rollback kickstart failed: %v; daemon may be down, binary restored at %s", err, binPath)
		return fmt.Errorf("rollback kickstart failed (original cause: %v): %w", cause, err)
	}

	if err := u.rollbackPoll(rollbackHealthLimit, Version); err != nil {
		u.logf("FATAL: daemon unhealthy after rollback: %v; binary restored at %s", err, binPath)
		return fmt.Errorf("rollback unhealthy (original cause: %v): %w", cause, err)
	}

	u.logf("rollback healthy; daemon on previous version %s", Version)
	// Rollback succeeded: daemon intact on the old version. Surface the cause as
	// a non-fatal error so the caller exits non-zero (update did not apply).
	return fmt.Errorf("update aborted, rolled back to %s: %w", Version, cause)
}

// ─── Health polling ──────────────────────────────────────────────────────────

// pollHealth polls /health until status==ok AND version==toTag, or deadline.
func (u *selfUpdater) pollHealth(deadline time.Duration) error {
	return u.healthPollExpect(deadline, u.toTag)
}

// healthPollExpect polls /health until status==ok AND version==expectTag, or the
// deadline elapses. A healthy-but-wrong-version response keeps polling (handles
// the in-flight old process during restart).
func (u *selfUpdater) healthPollExpect(deadline time.Duration, expectTag string) error {
	// Pass the BASE URL (no /health path): checkDaemonHealth appends "/health"
	// itself (daemon_lifecycle.go), matching every other caller via
	// endpointForPort(). Including /health here would produce /health/health (404).
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", u.port)
	end := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(end) {
		health, err := checkDaemonHealth(endpoint, healthPollInterval)
		if err != nil {
			lastErr = err
		} else if health.Status == "ok" && versionFieldEqual(health.Version, expectTag) {
			return nil
		} else {
			lastErr = fmt.Errorf("status=%q version=%q (want ok / %s)", health.Status, health.Version, expectTag)
		}
		time.Sleep(healthPollInterval)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("deadline exceeded")
	}
	return lastErr
}

// ─── launchctl restart ───────────────────────────────────────────────────────

// kickstartKernel restarts the kernel via launchctl, trying the system domain
// first then the gui/<uid>/ domain, mirroring LaunchctlController.Start.
func (u *selfUpdater) kickstartKernel() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := runLaunchctlKick(ctx, "system/"+kernelLaunchdLabel); err == nil {
		return nil
	}
	// Fall back to the user GUI domain.
	if err := runLaunchctlKick(ctx, "gui/"+currentUID()+"/"+kernelLaunchdLabel); err != nil {
		return fmt.Errorf("kickstart %s: %w", kernelLaunchdLabel, err)
	}
	return nil
}

// runLaunchctlKick runs `launchctl kickstart -k <domainTarget>`.
func runLaunchctlKick(ctx context.Context, domainTarget string) error {
	cmd := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domainTarget)
	return cmd.Run()
}

// ─── Lockfile (single-flight) ────────────────────────────────────────────────

func (u *selfUpdater) lockPath() string {
	dir := u.runDir()
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "selfupdate.lock")
}

// runDir resolves the override (tests), then ~/.cog/run (preferred), then
// <workspace>/.cog/run as a fallback.
func (u *selfUpdater) runDir() string {
	if u.runDirOverride != "" {
		return u.runDirOverride
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cog", "run")
	}
	if u.root != "" {
		return filepath.Join(u.root, ".cog", "run")
	}
	return filepath.Join(os.TempDir(), "cog-run")
}

// acquireUpdaterLock creates lockPath with O_EXCL and writes the PID. Returns
// (release, held, err). When the lock is held by a dead PID it is reclaimed once.
func acquireUpdaterLock(lockPath string) (release func(), held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return nil, false, err
		}
		// Lock exists — check whether the holder is alive.
		if pidAlive(readPID(lockPath)) {
			return nil, false, nil
		}
		// Stale lock: reclaim once.
		_ = os.Remove(lockPath)
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			// Lost the race to another reclaimer.
			return nil, false, nil
		}
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return func() { _ = os.Remove(lockPath) }, true, nil
}

func readPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// pidAlive reports whether a process with the given PID exists (signal 0).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ─── Orphan cleanup ──────────────────────────────────────────────────────────

// cleanupOrphans removes .cogos.new* temp files older than orphanMaxAge from the
// binary directory (residue from a crashed prior run, before the swap).
func (u *selfUpdater) cleanupOrphans() {
	entries, err := os.ReadDir(u.binDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-orphanMaxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".cogos.new") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(u.binDir, e.Name()))
		}
	}
}

// ─── Pre-flight disk check ───────────────────────────────────────────────────

// preflightDisk requires at least 2× the current binary size of free space in
// the binary directory before the backup-and-swap.
func (u *selfUpdater) preflightDisk(binPath string) error {
	info, err := os.Stat(binPath)
	if err != nil {
		// No current binary to size against — skip the check (fresh install).
		return nil
	}
	need := uint64(info.Size()) * 2
	var stat syscall.Statfs_t
	if err := syscall.Statfs(u.binDir, &stat); err != nil {
		// Cannot determine free space — do not block the update on this.
		return nil
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free < need {
		return fmt.Errorf("insufficient disk: need ~%d bytes free in %s, have %d", need, u.binDir, free)
	}
	return nil
}

// ─── download / checksum / smoke-test ────────────────────────────────────────

// downloadFile streams url into dst.
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// fetchText fetches url and returns its body as a string.
func fetchText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// verifyChecksum computes sha256(path) and matches it against the entry for
// assetName in a sha256sum-format checksums.txt body. Malformed lines are
// tolerated; a missing entry or a mismatch is an error.
func verifyChecksum(path, assetName, checksums string) error {
	want, ok := checksumFor(assetName, checksums)
	if !ok {
		return fmt.Errorf("no checksum entry for %s", assetName)
	}
	got, err := sha256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", assetName, got, want)
	}
	return nil
}

// checksumFor parses sha256sum-format lines ("<hex>  <name>") and returns the
// hex digest for assetName. The filename is taken from the SECOND field (the
// sha256sum spec is exactly "<hex>  <name>"), not the last field — so a line
// with trailing annotations or extra columns still resolves correctly instead
// of matching the trailing token. A "*name" or "./name" prefix still resolves
// via TrimPrefix + filepath.Base.
func checksumFor(assetName, checksums string) (string, bool) {
	for _, line := range strings.Split(checksums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue // malformed line tolerated
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = filepath.Base(name)
		if name == assetName {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// sha256File returns the lowercase hex sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// smokeTestVersion runs `<binPath> version` and returns the parsed version=
// field from the "cogos version=<v> build=<t>" output.
func smokeTestVersion(binPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "version").Output()
	if err != nil {
		return "", err
	}
	return parseVersionField(string(out)), nil
}

// parseVersionField extracts the value of the version= token from a line shaped
// like "cogos version=v0.16.5 build=...".
func parseVersionField(out string) string {
	for _, tok := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(tok, "version="); ok {
			return v
		}
	}
	return ""
}

// ─── version helpers (delegating to the selfupdate package's semver) ─────────

// resolveAssetURLs re-resolves the asset/checksum URLs for an exact tag.
func resolveAssetURLs(ctx context.Context, repo, tag string) (*assetURLs, error) {
	rt, err := selfUpdateResolveTag(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	return &assetURLs{
		AssetName:   rt.AssetName,
		AssetURL:    rt.AssetURL,
		ChecksumURL: rt.ChecksumURL,
	}, nil
}

type assetURLs struct {
	AssetName   string
	AssetURL    string
	ChecksumURL string
}

// copyFileMode copies src to dst with the given mode. The copy is atomic: it
// writes to a sibling temp file and renames into dst on success, so an
// interrupted copy (e.g. disk-full mid-stream) never leaves a partial file at
// dst. The temp file is removed on any failure.
func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp) // drop the partially-written temp file
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
