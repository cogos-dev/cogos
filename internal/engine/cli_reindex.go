// cli_reindex.go — "cogos reindex" subcommand.
//
// Usage:
//
//	cogos reindex [--workspace PATH]
//
// Rebuilds the FTS5 constellation index from scratch by walking the workspace
// .cog directory and upserting every *.cog.md file.  This is the manual escape
// hatch for widespread FTS drift that exceeds the lazy per-search repair
// threshold (>10 drifted documents).
//
// IndexWorkspace always performs a full rebuild; there is no partial/incremental
// mode exposed here.  The --full flag is accepted for forwards-compatibility but
// is a no-op (IndexWorkspace is always a full rebuild).
//
// cli_*.go files may import sdk/constellation.  Only the long-lived daemon path
// (Boot → MCPServer) must avoid that import to keep the boundary guard in
// cogdoc_service.go intact.  CLI commands exit after their work is done and
// never enter the daemon event loop, so the import is safe here.
package engine

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/myrgic/cogos/sdk/constellation"
)

// runReindexCmd implements "cogos reindex [flags]".
// args is everything after "reindex" (i.e. os.Args[2:]).
func runReindexCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected from cwd if empty)")
	_ = fs.Bool("full", false, "No-op: IndexWorkspace always performs a full rebuild")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cogos reindex [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Rebuilds the FTS5 constellation index by walking the workspace .cog directory\n")
		fmt.Fprintf(os.Stderr, "and upserting every *.cog.md file.  Run this after widespread FTS drift is\n")
		fmt.Fprintf(os.Stderr, "detected (the daemon logs 'run `cogos reindex` to repair').\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	// Resolve workspace root: use flag, then git-root detection, then cwd.
	root := *workspace
	if root == "" {
		var err error
		root, err = reconcileResolveWorkspace()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve workspace: %v\n", err)
			os.Exit(1)
		}
	}

	if err := runReindex(root, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runReindex is the testable core of runReindexCmd: everything after
// workspace-root resolution, taking an io.Writer for progress/warning
// output instead of writing straight to os.Stderr and calling os.Exit. This
// lets tests exercise the corrupt-store-preservation path (and its logging)
// without forking a subprocess or bringing down the test binary on error.
//
// SPEC (issue #571 item 2): nothing in the kernel may destroy a SQLite store
// it could not read. Before Open is allowed to create a fresh constellation
// store in place of an existing one, PreserveCorruptStore checks the
// existing file's integrity (PRAGMA quick_check, bounded) and — only if that
// check fails or the file cannot be opened as a database — renames it (and
// any WAL/SHM sidecars) aside instead of letting Open silently overwrite it.
// A healthy existing file is left untouched; Open's current
// open-or-create-and-migrate behavior is unchanged for that case.
func runReindex(root string, out io.Writer) error {
	dbPath := constellation.StorePath(root)

	preserved, reason, err := constellation.PreserveCorruptStore(dbPath)
	if err != nil {
		return fmt.Errorf("preserve corrupt store: %w", err)
	}
	if preserved != "" {
		fmt.Fprintf(out, "WARNING: existing constellation store at %s failed integrity check: %s\n", dbPath, reason)
		fmt.Fprintf(out, "WARNING: preserved corrupt store at %s -- building a fresh index in its place\n", preserved)
	}

	c, err := constellation.Open(root)
	if err != nil {
		return fmt.Errorf("open constellation: %w", err)
	}
	defer c.Close()

	fmt.Fprintf(out, "reindexing workspace %s ...\n", root)
	if err := c.IndexWorkspace(); err != nil {
		return fmt.Errorf("index workspace: %w", err)
	}
	fmt.Fprintln(out, "reindex complete")
	return nil
}
