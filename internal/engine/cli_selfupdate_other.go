//go:build !darwin && !windows

// cli_selfupdate_other.go — notify-only updater for non-darwin Unix (Linux/BSD).
//
// Auto-apply drives the kernel restart via launchctl, which is macOS-only. On
// Linux/BSD the daemon is supervised differently (systemd, etc.), so
// runSelfUpdateApply returns errSelfUpdateUnsupported and the CLI prints
// download guidance. This keeps the launchctl/Statfs/currentUID references
// (which are darwin-only) out of the Linux build entirely.
package engine

// runSelfUpdateApply is notify-only on non-darwin Unix. See cli_selfupdate_unix.go
// for the darwin implementation.
func runSelfUpdateApply(p selfUpdateApplyParams) error {
	return errSelfUpdateUnsupported
}
