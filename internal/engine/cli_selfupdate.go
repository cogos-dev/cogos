// cli_selfupdate.go — `cogos self-update` subcommand (platform-neutral entry).
//
// Three paths:
//   - --check            : dry-run. Resolve target, print running vs target, exit 0.
//   - bare (no --to)     : interactive. Resolve latest in channel, run the full
//     download→verify→swap→restart→health→rollback apply.
//   - --to <tag>         : updater path spawned by the reconcile provider's
//     ApplyPlan; runs the apply directly against <tag>.
//
// The destructive apply (runSelfUpdateApply) and the rollback/restart machinery
// live in cli_selfupdate_unix.go (darwin/linux) and cli_selfupdate_windows.go
// (notify-only). This file never swaps a binary; it only parses flags, performs
// the read-only --check, and dispatches.
package engine

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
)

// errSelfUpdateUnsupported is returned by runSelfUpdateApply on platforms where
// auto-apply is not supported (non-darwin). It is a notify-only signal, not a
// hard failure: the CLI prints guidance and exits 0.
var errSelfUpdateUnsupported = errors.New("self-update auto-apply unsupported on this platform")

// runSelfUpdateCmd is the dispatch entry called from cli.go's switch.
func runSelfUpdateCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	check := fs.Bool("check", false, "Dry-run: print running version and resolved target; no download, no swap")
	to := fs.String("to", "", "Target release tag (e.g. v0.16.5); empty resolves latest in --channel")
	channel := fs.String("channel", "stable", "Release channel for resolve: stable|prerelease")
	repo := fs.String("repo", "myrgic/cogos", "GitHub repository (owner/name)")
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root (lock + log path)")
	port := fs.Int("port", defaultPort, "Health endpoint port (matches daemon --port)")
	force := fs.Bool("force", false, "Permit a downgrade on the manual path without a pin")
	_ = fs.Parse(args)

	if *port == 0 {
		*port = 6931
	}

	// Resolve the workspace root for lock/log paths. A missing workspace is not
	// fatal for self-update (the updater can still operate on ~/.cog/bin).
	root := *workspace
	if cfg, err := LoadConfig(*workspace, *port); err == nil {
		root = cfg.WorkspaceRoot
	}

	if *check {
		if err := runSelfUpdateCheck(*repo, *channel, *to); err != nil {
			fmt.Fprintf(os.Stderr, "self-update: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Resolve the target tag when not supplied explicitly.
	target := *to
	if target == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rt, err := selfupdate.ResolveTarget(ctx, *repo, *channel, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "self-update: resolve target: %v\n", err)
			os.Exit(1)
		}
		target = rt.Tag
	}

	err := runSelfUpdateApply(selfUpdateApplyParams{
		Repo:      *repo,
		ToTag:     target,
		Workspace: root,
		Port:      *port,
		Force:     *force,
		Manual:    *to == "", // interactive path (no --to) enforces the downgrade guard unless --force
	})
	if errors.Is(err, errSelfUpdateUnsupported) {
		fmt.Fprintf(os.Stderr, "self-update: auto-apply unsupported on %s; download %s from the releases page\n",
			runtime.GOOS, selfupdate.AssetName())
		return // notify, not error
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "self-update: %v\n", err)
		os.Exit(1)
	}
}

// selfUpdateApplyParams bundles the inputs to runSelfUpdateApply so the
// platform-specific implementations share one signature.
type selfUpdateApplyParams struct {
	Repo      string
	ToTag     string
	Workspace string
	Port      int
	Force     bool
	Manual    bool
}

// runSelfUpdateCheck is the dry-run path: resolve the target, compare against the
// running version, print the verdict. Never mutates anything.
func runSelfUpdateCheck(repo, channel, pin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, err := selfupdate.ResolveTarget(ctx, repo, channel, pin)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}

	running := Version
	fmt.Printf("running:  %s\n", running)
	fmt.Printf("target:   %s\n", rt.Tag)

	switch {
	case selfupdate.NormVersion(running) == "":
		fmt.Println("verdict:  running a dev/unknown build; auto-update would skip (use --to to force)")
	case selfupdate.VersionEqual(rt.Tag, running):
		fmt.Println("verdict:  up to date")
	case selfupdate.VersionAfter(rt.Tag, running):
		fmt.Println("verdict:  update available (target is newer)")
	default:
		fmt.Println("verdict:  running ahead of target; auto path would not downgrade")
	}
	return nil
}
