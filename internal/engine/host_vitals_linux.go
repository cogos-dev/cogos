//go:build linux

// host_vitals_linux.go — RFC-040 S0 gauge samplers for Linux.
//
// All three readings are pure syscall/procfs, no subprocess spawn, no cgo:
// cheap enough to run on every autonomic tick.
package engine

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

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

// memFreeBytes reads MemAvailable from /proc/meminfo — the kernel's own
// estimate of memory available for new workloads without swapping, which is
// the figure operators actually mean by "free memory" (unlike MemFree,
// which excludes reclaimable cache/buffers and reads misleadingly low).
func memFreeBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("host_vitals: open /proc/meminfo: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("host_vitals: unexpected /proc/meminfo line %q", line)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("host_vitals: parse MemAvailable %q: %w", fields[1], err)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("host_vitals: read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("host_vitals: MemAvailable not found in /proc/meminfo")
}

// loadAvg1 reads the 1-minute load average from /proc/loadavg.
func loadAvg1() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, fmt.Errorf("host_vitals: read /proc/loadavg: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("host_vitals: empty /proc/loadavg")
	}
	l1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("host_vitals: parse load1 %q: %w", fields[0], err)
	}
	return l1, nil
}
