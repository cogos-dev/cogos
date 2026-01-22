// Package git provides git primitives for the SDK.
//
// This package wraps git operations needed by the kernel, primarily
// for coherence tracking (tree hashes) and content-addressable storage.
package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// TreeHash computes the tree hash of a directory using git.
// This is used for coherence checking.
//
// Equivalent to: git write-tree --prefix=<prefix>/
func TreeHash(repoRoot, prefix string) (string, error) {
	// First stage the prefix to ensure unstaged changes are included
	stageCmd := exec.Command("git", "-C", repoRoot, "add", "-A", prefix)
	stageCmd.Run() // Ignore errors - may fail if nothing to stage

	// Compute tree hash
	cmd := exec.Command("git", "-C", repoRoot, "write-tree", "--prefix="+prefix+"/")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git write-tree failed: %w (stderr: %s)", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// BlobHash computes the blob hash of a file.
//
// Equivalent to: git hash-object <path>
func BlobHash(path string) (string, error) {
	cmd := exec.Command("git", "hash-object", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git hash-object failed: %w (stderr: %s)", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// DiffTree returns files that changed between two tree hashes.
//
// Equivalent to: git diff-tree -r --name-only <from> <to>
func DiffTree(repoRoot, from, to string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "diff-tree", "-r", "--name-only", from, to)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff-tree failed: %w (stderr: %s)", err, stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}

	return strings.Split(output, "\n"), nil
}

// IsGitRepo checks if the given path is inside a git repository.
func IsGitRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

// RepoRoot returns the root directory of the git repository.
func RepoRoot(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}

	return strings.TrimSpace(stdout.String()), nil
}
