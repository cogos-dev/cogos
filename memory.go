// memory.go — CogOS v3 memory system interface
//
// Thin interface over the CogDocs memory layout (.cog/mem/).
// Delegates search to the cog CLI wrapper (scripts/cog memory search).
// In stage 5, this will be replaced with local embedding-based retrieval.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MemorySearch runs `cog memory search <query>` and returns matching paths.
// Falls back to a simple filepath.Walk grep if the cog binary is not available.
func MemorySearch(workspaceRoot, query string) ([]string, error) {
	cogScript := filepath.Join(workspaceRoot, "scripts", "cog")
	if _, err := os.Stat(cogScript); err == nil {
		out, err := exec.Command(cogScript, "memory", "search", query).Output()
		if err == nil {
			return parseSearchOutput(string(out)), nil
		}
	}

	// Fallback: walk .cog/mem/ and return files whose names contain the query.
	memDir := filepath.Join(workspaceRoot, ".cog", "mem")
	var results []string
	lq := strings.ToLower(query)

	_ = filepath.Walk(memDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(strings.ToLower(filepath.Base(path)), lq) {
			results = append(results, path)
		}
		return nil
	})

	return results, nil
}

// MemoryRead returns the text contents of a memory file.
// path may be either an absolute path or a memory-relative path (e.g. "semantic/foo.md").
func MemoryRead(workspaceRoot, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspaceRoot, ".cog", "mem", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read memory %s: %w", path, err)
	}
	return string(data), nil
}

// parseSearchOutput extracts file paths from `cog memory search` stdout.
// The cog CLI outputs one path per line (possibly prefixed with scores).
func parseSearchOutput(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip score prefix "0.87 path/to/file.md" if present.
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			paths = append(paths, parts[len(parts)-1])
		} else {
			paths = append(paths, line)
		}
	}
	return paths
}
