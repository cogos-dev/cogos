package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func cmdLoop(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog loop start|status|stop [OPTIONS]")
	}

	// Get workspace root
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	// Path to Python script
	scriptPath := filepath.Join(root, ".cog", "scripts", "autonomous-loop.py")

	// Build command
	cmd := exec.Command("python3", scriptPath, args[0])
	if len(args) > 1 {
		cmd.Args = append(cmd.Args, args[1:]...)
	}

	cmd.Env = append(os.Environ(), fmt.Sprintf("WORKSPACE_ROOT=%s", root))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}
