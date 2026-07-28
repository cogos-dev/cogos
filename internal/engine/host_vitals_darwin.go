//go:build darwin

// host_vitals_darwin.go — RFC-037 S0 gauge samplers for macOS.
//
// disk_free_bytes is a plain Statfs syscall, same as Linux. mem_free_bytes
// and load1 are NOT implemented here: both would need APIs this codebase
// has no path to without adding cgo (see per-function comments below), and
// this is a minimal-seam PR — no new build-mode dependency. Per RFC-037's
// soft-degrade contract, both samplers return an error so the corresponding
// HostVitals field is simply omitted on this platform, never fabricated.
package engine

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// errHostVitalUnsupported is returned by a sampler with no non-cgo
// implementation on this platform.
var errHostVitalUnsupported = errors.New("host_vitals: not supported on darwin without cgo")

// diskFreeBytes returns free space (available to an unprivileged process,
// i.e. Statfs_t.Bavail — the same figure `df` reports) on the filesystem
// containing path.
func diskFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("host_vitals: statfs %q: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// memFreeBytes: macOS has no free-memory figure available via
// golang.org/x/sys/unix. The real reading requires the Mach
// host_statistics64 API (mach ports are not POSIX, so x/sys/unix's
// unix-syscall surface doesn't cover them) — that's a cgo dependency this
// minimal seam does not take on. Shelling out to `vm_stat` was considered
// and rejected: spawning a subprocess every autonomic tick (default 1m
// interval) to parse human-readable text is not the "cheap, existing
// in-process structure" bar this stage sets.
func memFreeBytes() (uint64, error) {
	return 0, errHostVitalUnsupported
}

// loadAvg1: macOS has no /proc/loadavg, and golang.org/x/sys/unix ships no
// Getloadavg wrapper for darwin. The raw `vm.loadavg` sysctl returns a C
// `struct loadavg { fixpt_t ldavg[3]; long fscale; }` whose field widths and
// padding are platform/ABI-dependent; hand-decoding it via SysctlRaw risks a
// silently-wrong value on an ABI change, which is worse than a clean
// omission. Left unimplemented pending either a vetted wrapper or a
// justified cgo dependency.
func loadAvg1() (float64, error) {
	return 0, errHostVitalUnsupported
}
