//go:build windows

// spawn_windows.go — notify-only updater spawn stub for Windows.
//
// Auto-apply (launchctl restart, atomic in-place swap, detached Setsid updater)
// is macOS-specific. On Windows the provider is notify-only: ComputePlan Gate H
// already short-circuits non-darwin to a Skip, so this stub should not be reached
// on the apply path. It returns nil so the provider package compiles without any
// Setsid/syscall references on Windows.
package selfupdate

// spawnDetachedUpdater is a no-op on Windows (notify-only). See spawn_unix.go for
// the real implementation.
func spawnDetachedUpdater(root, toTag, repo string, port int) error {
	return nil
}
