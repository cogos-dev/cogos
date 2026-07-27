// cli_reconcile.go — "cogos reconcile <type>" subcommand.
//
// Usage:
//
//	cogos reconcile <type> [--dry-run] [--json] [--resource <name>] [--snapshot] [--token <token>]
//
// Runs the plan (and optionally apply) cycle for a single registered
// provider type. The provider must already be registered with pkg/reconcile
// (via an init() import in cmd/cogos/providers_wire.go).
//
// --snapshot inverts the direction (live → spec): it regenerates the
// declared config from live state via reconcile.ConfigExporter, if the
// provider implements it, and returns before the normal plan/apply cycle.
//
// --token supplies an auth token for providers implementing
// reconcile.Tokenable (e.g. discord); falling back to {TYPE}_TOKEN /
// {TYPE}_BOT_TOKEN / {TYPE}_API_TOKEN environment variables via
// reconcile.ConfigureProvider, mirroring the doc comment on
// reconcile.Tokenable itself.
//
// Workspace root is resolved from the --workspace global flag or via git-root
// detection on the cwd. The command exits 0 on success (synced or dry-run),
// 2 if dry-run reveals drift, 1 on any error.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// runReconcileCmd dispatches "cogos reconcile <type> [flags]".
// args is everything after "reconcile" (i.e. os.Args[2:]).
func runReconcileCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected from cwd if empty)")
	dryRun := fs.Bool("dry-run", false, "Plan only; do not apply changes")
	jsonOut := fs.Bool("json", false, "Output plan as JSON")
	snapshot := fs.Bool("snapshot", false, "Snapshot live state into the declared config (live → spec); requires ConfigExporter support")
	token := fs.String("token", "", "Auth token for Tokenable providers (falls back to {TYPE}_TOKEN-style env vars)")
	_ = fs.String("resource", "", "Reserved: reconcile only this named resource (not yet implemented)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cogos reconcile <type> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Registered providers: %s\n\n", strings.Join(reconcile.ListProviders(), ", "))
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	// Pull out the provider type (first non-flag argument) before parsing flags.
	// This lets the user write either:
	//   cogos reconcile site --dry-run --json   (type before flags)
	//   cogos reconcile --dry-run --json site   (flags before type)
	providerType := ""
	flagArgs := make([]string, 0, len(args))
	for _, a := range args {
		if providerType == "" && !strings.HasPrefix(a, "-") {
			providerType = a
		} else {
			flagArgs = append(flagArgs, a)
		}
	}

	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(1)
	}
	if providerType == "" {
		fmt.Fprintf(os.Stderr, "error: provider type required (e.g. cogos reconcile site)\n\n")
		fs.Usage()
		os.Exit(1)
	}

	// Resolve workspace root.
	root := *workspace
	if root == "" {
		var err error
		root, err = reconcileResolveWorkspace()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve workspace: %v\n", err)
			os.Exit(1)
		}
	}

	// Register providers that are NOT self-registering via package init(), and
	// inject the workspace root into the ones that need it. runServe does both
	// (cli.go's RegisterProviders call + LoadConfig's SetProvidersWorkspace);
	// this path did neither, so any provider registered inside the
	// RegisterProviders hook rather than by a package init() was unreachable
	// from the CLI entirely. ProjectionCompiler is the current instance:
	// `cogos reconcile projection-compiler` could only ever report an unknown
	// resource type, because nothing on this path had registered it.
	//
	// The 10 daemon providers are unaffected either way — they register in
	// internal/providers/daemon's init(), which runs in any process that
	// imports it. This restores parity for the rest.
	if RegisterProviders != nil {
		RegisterProviders()
	}
	if SetProvidersWorkspace != nil {
		SetProvidersWorkspace(root)
	}

	// Retrieve the provider.
	provider, err := reconcile.GetProvider(providerType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\nRegistered providers: %s\n",
			err, strings.Join(reconcile.ListProviders(), ", "))
		os.Exit(1)
	}

	// Wire an auth token into the provider if it implements reconcile.Tokenable
	// (flag takes precedence, then {TYPE}_TOKEN-style env vars). A no-op for
	// providers that don't need one. Without this, Tokenable providers (e.g.
	// discord) never receive a token through this CLI path at all — FetchLive/
	// ApplyPlan would always see an empty token even with a valid env var set.
	reconcile.ConfigureProvider(provider, providerType, *token)

	ctx := context.Background()

	// Snapshot (live → spec): if requested, regenerate the declared config
	// from live state and return before the normal spec → live cycle. This is
	// the inverse of reconcile and must not also run plan/apply. Requires the
	// provider to implement reconcile.ConfigExporter.
	if *snapshot {
		exporter, ok := provider.(reconcile.ConfigExporter)
		if !ok {
			fmt.Fprintf(os.Stderr, "error: provider %q does not support snapshot (no ConfigExporter)\n", providerType)
			os.Exit(1)
		}
		// Acquire the same state lock used by the reconcile cycle since we are
		// mutating provider-owned config files.
		snapLock, lockErr := reconcile.AcquireStateLock(root, providerType)
		if lockErr != nil {
			fmt.Fprintf(os.Stderr, "error: acquire state lock for %s: %v\n", providerType, lockErr)
			os.Exit(1)
		}
		defer snapLock.Release()

		if err := exporter.ExportConfig(root); err != nil {
			fmt.Fprintf(os.Stderr, "error: snapshot %s: %v\n", providerType, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\nsnapshot written\n",
			filepath.Join(root, ".cog", "config", providerType))
		return
	}

	// Load config.
	config, err := provider.LoadConfig(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	// Fetch live state.
	live, err := provider.FetchLive(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fetch live state: %v\n", err)
		os.Exit(1)
	}

	// Acquire the cross-process state lock for the full
	// LoadState → ComputePlan → ApplyPlan → BuildState → WriteState cycle so
	// this CLI invocation can't race the daemon's own reconcile-loop cycle
	// for the same providerType (same bug class as issue #449's _meta.json
	// race; see pkg/substrate/reconcile/state.go doc comment). Held across
	// the whole cycle, including the dry-run/read-only path, for simplicity —
	// a stale read under contention is harmless, a racing write is not.
	stateLock, lockErr := reconcile.AcquireStateLock(root, providerType)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: acquire state lock for %s: %v\n", providerType, lockErr)
		os.Exit(1)
	}
	defer stateLock.Release()

	// Load persisted state.
	state, _ := reconcile.LoadState(root, providerType)

	// Compute plan.
	plan, err := provider.ComputePlan(config, live, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: compute plan: %v\n", err)
		os.Exit(1)
	}

	// Render plan.
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode plan: %v\n", err)
			os.Exit(1)
		}
	} else {
		reconcilePrintTable(plan)
	}

	if *dryRun {
		if plan.Summary.HasChanges() {
			// Exit 2 signals drift to callers without being a process error.
			os.Exit(2)
		}
		return
	}

	// Apply.
	if !plan.Summary.HasChanges() {
		fmt.Fprintln(os.Stderr, "no changes — already in sync")
		return
	}
	results, err := provider.ApplyPlan(ctx, plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: apply plan: %v\n", err)
		os.Exit(1)
	}

	// Persist updated state.
	newState, err := provider.BuildState(config, live, state)
	if err == nil && newState != nil {
		if writeErr := reconcile.WriteState(root, providerType, newState); writeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: write state: %v\n", writeErr)
		}
	}

	// Report apply results.
	failed := 0
	for _, r := range results {
		if r.Status == reconcile.ApplyFailed {
			failed++
			fmt.Fprintf(os.Stderr, "  FAILED  %s: %s\n", r.Name, r.Error)
		} else {
			fmt.Fprintf(os.Stderr, "  OK      %s\n", r.Name)
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d action(s) failed\n", failed)
		os.Exit(1)
	}
}

// reconcilePrintTable renders the plan as a human-readable table to stdout.
func reconcilePrintTable(plan *reconcile.Plan) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "ACTION\tNAME\tDETAILS\n")
	for _, a := range plan.Actions {
		details := ""
		if s, ok := a.Details["strategy"].(string); ok {
			details = "strategy=" + s
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", strings.ToUpper(string(a.Action)), a.Name, details)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(os.Stdout, "Summary: %d create, %d update, %d delete, %d skip\n",
		plan.Summary.Creates, plan.Summary.Updates, plan.Summary.Deletes, plan.Summary.Skipped)
}

// reconcileResolveWorkspace detects the workspace root from cwd via git.
// Falls back to cwd if not in a git repository.
func reconcileResolveWorkspace() (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		if root := strings.TrimSpace(out.String()); root != "" {
			return root, nil
		}
	}
	return os.Getwd()
}
