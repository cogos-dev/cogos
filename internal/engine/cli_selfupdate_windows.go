//go:build windows

// cli_selfupdate_windows.go — notify-only updater for Windows.
//
// Auto-apply (atomic in-place swap + launchctl restart + detached Setsid
// updater) is macOS-specific. On Windows runSelfUpdateApply returns
// errSelfUpdateUnsupported; the CLI prints download guidance and exits 0. No
// launchctl/Setsid/Statfs references compile on Windows.
package engine

// runSelfUpdateApply is notify-only on Windows. See cli_selfupdate_unix.go for
// the darwin implementation.
func runSelfUpdateApply(p selfUpdateApplyParams) error {
	return errSelfUpdateUnsupported
}
