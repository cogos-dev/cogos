//go:build !darwin && !windows

// spawn_other.go — notify-only updater spawn stub for non-darwin, non-windows
// hosts (Linux, BSD, etc.).
//
// Auto-apply (launchctl restart, atomic in-place swap, detached Setsid updater)
// is macOS-specific. On these platforms the provider is notify-only:
// ApplyPlan's Gate H (runtime.GOOS != "darwin") short-circuits to a Skip, so
// this stub should not be reached on the apply path. It returns nil so the
// provider package compiles without any Setsid/syscall references, keeping the
// build constraints consistent with the runtime GOOS guard in ApplyPlan (the
// real spawn is darwin-only, in spawn_unix.go).
package selfupdate

// spawnDetachedUpdater is a no-op on non-darwin/non-windows hosts (notify-only).
// See spawn_unix.go for the real (darwin) implementation.
func spawnDetachedUpdater(root, toTag, repo string, port int) error {
	return nil
}
