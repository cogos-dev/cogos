// complete.go — Shell completion support
//
// Provides `cog __complete <args...>` which outputs completion candidates,
// one per line. Shell completion functions call this instead of reimplementing
// path/section logic in shell script.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cmdComplete handles `cog __complete <args...>`
// args[0] is the subcommand being completed, args[1:] are the words so far.
// Output: one completion candidate per line.
func cmdComplete(args []string) error {
	if len(args) == 0 {
		// Complete top-level commands
		cmds := []string{
			"memory", "workspace", "health", "coherence", "serve", "infer",
			"read", "list", "query", "constellation", "components", "salience",
			"coord", "frontmatter", "ledger", "events", "session", "plan",
			"apply", "status", "snapshot", "refresh", "reconcile", "version",
			"help", "hfs", "artifact", "watch", "validate",
		}
		for _, c := range cmds {
			fmt.Println(c)
		}
		return nil
	}

	switch args[0] {
	case "memory":
		return completeMemory(args[1:])
	default:
		return nil
	}
}

// completeMemory handles `cog __complete memory <args...>`
func completeMemory(args []string) error {
	if len(args) == 0 {
		// Complete memory subcommands
		for _, cmd := range []string{"search", "read", "write", "list", "toc", "index", "append", "stats"} {
			fmt.Println(cmd)
		}
		return nil
	}

	subcmd := args[0]
	rest := args[1:]

	switch subcmd {
	case "list":
		// Complete sectors
		if len(rest) == 0 {
			for _, s := range []string{"semantic", "episodic", "procedural", "reflective"} {
				fmt.Println(s)
			}
		}
		return nil

	case "read", "toc", "index", "write", "append":
		return completeMemoryArgs(subcmd, rest)

	default:
		return nil
	}
}

// completeMemoryArgs completes paths, flags, and section slugs for memory commands
func completeMemoryArgs(subcmd string, args []string) error {
	root, _, _ := ResolveWorkspace()
	if root == "" {
		return nil
	}
	memDir := filepath.Join(root, ".cog", "mem")

	// Determine what we're completing based on context
	// Last arg is the partial word being completed
	// Previous args tell us the context

	// Check if we should complete a section name (previous arg was --section)
	if len(args) >= 1 && args[len(args)-1] == "--section" {
		// Find the path arg and complete section slugs
		docPath := findPathArg(args[:len(args)-1])
		if docPath != "" {
			return completeSections(memDir, docPath)
		}
		return nil
	}

	// Check if the arg before the partial is --section
	if len(args) >= 2 && args[len(args)-2] == "--section" {
		docPath := findPathArg(args[:len(args)-2])
		if docPath != "" {
			return completeSections(memDir, docPath)
		}
		return nil
	}

	// Check if completing flags
	partial := ""
	if len(args) > 0 {
		partial = args[len(args)-1]
	}

	if strings.HasPrefix(partial, "-") {
		if subcmd == "read" {
			fmt.Println("--section")
			fmt.Println("--frontmatter")
		}
		if subcmd == "index" {
			fmt.Println("--dry-run")
			fmt.Println("--force")
		}
		return nil
	}

	// Default: complete memory paths
	return completeMemoryPaths(memDir, partial)
}

// findPathArg finds the first non-flag argument (the document path)
func findPathArg(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--section" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

// completeMemoryPaths outputs memory-relative paths with extensions stripped
func completeMemoryPaths(memDir, partial string) error {
	if _, err := os.Stat(memDir); err != nil {
		return nil
	}

	// If partial has a directory component, scope the search
	searchDir := memDir
	prefix := ""
	if idx := strings.LastIndex(partial, "/"); idx >= 0 {
		prefix = partial[:idx+1]
		searchDir = filepath.Join(memDir, prefix)
	}

	var paths []string
	filepath.WalkDir(searchDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, _ := filepath.Rel(memDir, path)
		if rel == "." {
			return nil
		}

		// Include directories for intermediate completion
		if d.IsDir() {
			paths = append(paths, rel+"/")
			return nil
		}

		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		// Strip extensions
		clean := rel
		clean = strings.TrimSuffix(clean, ".cog.md")
		clean = strings.TrimSuffix(clean, ".md")

		if partial == "" || strings.HasPrefix(clean, partial) {
			paths = append(paths, clean)
		}

		return nil
	})

	sort.Strings(paths)
	for _, p := range paths {
		fmt.Println(p)
	}
	return nil
}

// completeSections outputs section slugs for a given document
func completeSections(memDir, docPath string) error {
	// Resolve the file with extension probing
	fullPath := resolveMemoryPath(memDir, docPath)

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return nil
	}

	body := string(content)
	if doc, err := ExtractFrontmatter(body); err == nil {
		body = doc.Body
	}

	sections := ParseSections(body)
	for _, s := range sections {
		if s.Level < 2 {
			continue
		}
		slug := s.Anchor
		if slug == "" {
			slug = titleToAnchor(s.Title)
		}
		fmt.Println(slug)
	}
	return nil
}
