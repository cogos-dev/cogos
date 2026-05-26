// cli_install.go — cogos install subcommand.
//
// Currently supports one flag:
//
//	--add-path   Opt-in: add ~/.cog/bin to the user's shell PATH.
//
// Platform notes:
//   - Unix/macOS: appends an export line to the detected shell rc file
//     (~/.bashrc, ~/.zshrc, ~/.config/fish/config.fish). The user still
//     needs to restart their shell or source the file.
//   - Windows: prints a PowerShell snippet and the equivalent setx invocation;
//     actual registry write is deferred to follow-up work (tracked in #320).

package engine

import (
	"flag"
	"fmt"
	"os"
)

func runInstallCmd(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	addPath := fs.Bool("add-path", false, "Add ~/.cog/bin to the user PATH (appends to shell profile on Unix; prints instructions on Windows)")
	_ = fs.Parse(args)

	if !*addPath {
		fmt.Fprintln(os.Stderr, "usage: cogos install --add-path")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
		os.Exit(1)
	}

	if err := addCogBinToPath(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
