//go:build !linux && !darwin

// host_vitals_other.go — RFC-040 S0 gauge samplers for every platform other
// than Linux and macOS (Windows, BSD, ...).
//
// No gauge has a non-cgo, non-subprocess implementation wired up for these
// platforms yet. All three samplers report "unsupported" so HostVitals ships
// with every optional field omitted rather than fabricated — the soft-degrade
// contract RFC-040 N5 requires, applied at the platform granularity instead
// of the single-reading granularity.
package engine

import "errors"

// errHostVitalUnsupported is returned by every sampler on this platform.
var errHostVitalUnsupported = errors.New("host_vitals: not supported on this platform")

func diskFreeBytes(path string) (uint64, error) {
	return 0, errHostVitalUnsupported
}

func memFreeBytes() (uint64, error) {
	return 0, errHostVitalUnsupported
}

func loadAvg1() (float64, error) {
	return 0, errHostVitalUnsupported
}
