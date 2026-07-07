// Command lmstudio-observer projects LM Studio local-model conversation
// history into the CogOS Conversations Observatory's normalized ingest
// surface, so LM Studio sessions become searchable via
// cog_search_conversations / cog_list_conversations.
//
// It is a standalone, additive producer: it does not link against or modify
// internal/engine or internal/conversations. It only writes normalized JSONL
// files under <workspace>/.cog/observatory/ingest/lm-studio/, which the
// existing (unmodified) conversations Reconcilable already knows how to
// consume via its ingest_dirs configuration.
//
// Usage:
//
//	lmstudio-observer [--workspace PATH] [--lmstudio-dir PATH] [--force]
//
// --workspace defaults to $COGOS_WORKSPACE, then $HOME/workspaces/cog.
// --lmstudio-dir defaults to $LMSTUDIO_CONVERSATIONS_DIR, then
// $HOME/.lmstudio/conversations.
//
// Intended to run periodically (cron/launchd) or on demand; each run is
// incremental — only conversation files that are new or changed since the
// last run are re-parsed and re-emitted.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/myrgic/cogos/internal/lmstudio"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("lmstudio-observer", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "CogOS workspace root (default: $COGOS_WORKSPACE or $HOME/workspaces/cog)")
	lmstudioDir := fs.String("lmstudio-dir", "", "LM Studio conversations dir (default: $LMSTUDIO_CONVERSATIONS_DIR or $HOME/.lmstudio/conversations)")
	force := fs.Bool("force", false, "re-parse and re-emit every discovered conversation file, ignoring tracked state")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := resolveWorkspace(*workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lmstudio-observer:", err)
		return 1
	}

	res, err := lmstudio.Run(lmstudio.RunOptions{
		LMStudioDir:   *lmstudioDir,
		WorkspaceRoot: root,
		Force:         *force,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "lmstudio-observer:", err)
		return 1
	}

	fmt.Printf("lmstudio-observer: scanned=%d emitted=%d skipped=%d turns=%d errors=%d\n",
		res.Scanned, res.Emitted, res.Skipped, res.TotalTurns, len(res.Errors))
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "  error:", e)
	}
	if len(res.Errors) > 0 {
		return 1
	}
	return 0
}

// resolveWorkspace applies the same fallback convention documented in the
// project's path-convention guidance: an explicit flag wins, then
// $COGOS_WORKSPACE, then $HOME/workspaces/cog. Never hardcodes a specific
// user's absolute path.
func resolveWorkspace(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("COGOS_WORKSPACE"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "workspaces", "cog"), nil
}
