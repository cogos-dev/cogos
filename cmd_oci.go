// cmd_oci.go — CLI commands for OCI artifact management.
//
// Commands:
//   cog oci push [binary-path]  — push binary to local OCI layout
//   cog oci info                — show latest artifact metadata
//   cog oci help                — show usage

package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func cmdOCI(args []string) int {
	if len(args) == 0 {
		cmdOCIHelp()
		return 0
	}

	switch args[0] {
	case "push":
		return cmdOCIPush(args[1:])
	case "info":
		return cmdOCIInfo(args[1:])
	case "help", "-h", "--help":
		cmdOCIHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown oci command: %s\n", args[0])
		cmdOCIHelp()
		return 1
	}
}

// cmdOCIPush pushes a binary into the local OCI layout.
// Usage: cog oci push [binary-path]
// If no path given, defaults to the current executable.
func cmdOCIPush(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	// Determine binary path
	var binaryPath string
	if len(args) > 0 {
		binaryPath = args[0]
	} else {
		binaryPath, err = os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: cannot determine executable path: %v\n", err)
			return 1
		}
	}

	// Validate binary exists
	info, err := os.Stat(binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	store := NewOCIStore(root)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	digest, err := store.Push(ctx, binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("Pushed %s to OCI layout\n", binaryPath)
	fmt.Printf("  size:   %d bytes\n", info.Size())
	fmt.Printf("  digest: %s\n", digest)
	fmt.Printf("  layout: %s\n", store.layoutDir)

	return 0
}

// cmdOCIInfo shows metadata about the current OCI artifact.
func cmdOCIInfo(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: no workspace found: %v\n", err)
		return 1
	}

	store := NewOCIStore(root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := store.Info(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Printf("OCI Artifact Info\n")
	fmt.Printf("  digest:   %s\n", info.Digest)
	fmt.Printf("  size:     %d bytes\n", info.Size)
	fmt.Printf("  os/arch:  %s/%s\n", info.OS, info.Arch)
	fmt.Printf("  version:  %s\n", info.Version)
	if info.PushedAt != "" {
		fmt.Printf("  pushed:   %s\n", info.PushedAt)
	}
	fmt.Printf("  layout:   %s\n", store.layoutDir)

	return 0
}

func cmdOCIHelp() {
	fmt.Println("Usage: cog oci <command>")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  push [binary]  Push binary to local OCI layout (.cog/oci/)")
	fmt.Println("  info           Show latest artifact metadata")
	fmt.Println("  help           Show this help")
	fmt.Println()
	fmt.Println("The OCI layout enables auto-reload: the running kernel watches")
	fmt.Println("for new artifacts and re-execs when a new digest is detected.")
	fmt.Println()
	fmt.Println("Quick start:")
	fmt.Println("  make push      Build + push (kernel auto-reloads)")
}
