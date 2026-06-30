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

	c, err := constellation.Open(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open constellation: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	fmt.Fprintf(os.Stderr, "reindexing workspace %s ...\n", root)
	if err := c.IndexWorkspace(); err != nil {
		fmt.Fprintf(os.Stderr, "error: index workspace: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "reindex complete")
}
