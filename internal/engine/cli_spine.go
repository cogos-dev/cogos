// cli_spine.go — "cogos spine" subcommand.
//
// Surfaces the decision manifold (the spine) computed by spine.go over the
// decision corpus. Read-only.
//
// Usage:
//
//	cogos spine                  overview: centre of mass, basins, accretion eras
//	cogos spine <id-or-slug>     one decision's mass / inertia / basin / edges / orbit
//	cogos spine --orbits         full basin membership
//	cogos spine --accretion      the accretion timeline
//	cogos spine --json [<id>]    machine-readable manifold (or one vertebra)
//
// The id argument matches by exact id, case-insensitive id, suffix (e.g. "84"
// → "adr-084"/"084"), or title substring.
package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// runSpineCmd dispatches "cogos spine [id] [flags]". args is everything after
// "spine" (i.e. os.Args[2:]).
func runSpineCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("spine", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected from cwd if empty)")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	orbits := fs.Bool("orbits", false, "Show full orbital-basin membership")
	accretion := fs.Bool("accretion", false, "Show the accretion timeline")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cogos spine [<id-or-slug>] [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Surfaces the decision manifold (the spine): the gravity/inertia field\n")
		fmt.Fprintf(os.Stderr, "over the architecture's own decisions (ADRs, RFCs, decision-insights).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	// Support both orders: `cogos spine <id> --json` and `cogos spine --json <id>`.
	// Go's flag package stops parsing at the first non-flag token, so we parse,
	// peel off the first leftover positional as the query, and re-parse any
	// flags that followed it. This correctly keeps "--workspace <path>" together
	// (the value is consumed by the flag, never mistaken for the query).
	query := ""
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			os.Exit(1)
		}
		leftovers := fs.Args()
		if len(leftovers) == 0 {
			break
		}
		if query == "" {
			query = leftovers[0]
		}
		rest = leftovers[1:]
		if len(rest) == 0 {
			break
		}
	}

	root := *workspace
	if root == "" {
		var err error
		root, err = reconcileResolveWorkspace()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve workspace: %v\n", err)
			os.Exit(1)
		}
	}

	decisions, err := LoadDecisionCorpus(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load decision corpus: %v\n", err)
		os.Exit(1)
	}
	if len(decisions) == 0 {
		fmt.Fprintf(os.Stderr, "no decision corpus found under %s\n", root)
		fmt.Fprintf(os.Stderr, "(looked in %s)\n", strings.Join(DecisionCorpusDirs(root), ", "))
		os.Exit(1)
	}

	// NOTE: the manifold's computed_at (surfaced in --json) is always fresh
	// wall-clock at invocation. It is NOT derived from the corpus, so two
	// --json snapshots of an unchanged corpus differ only in that field;
	// scripts diffing manifolds should ignore computed_at.
	manifold := ComputeManifold(decisions, time.Now().UTC())

	// Single-decision view.
	if query != "" {
		v := manifold.Lookup(query)
		if v == nil {
			fmt.Fprintf(os.Stderr, "no decision matching %q (corpus has %d decisions)\n", query, len(decisions))
			os.Exit(1)
		}
		if *jsonOut {
			emitJSON(v)
			return
		}
		fmt.Print(renderSpineDecision(manifold, v))
		return
	}

	// Whole-manifold views.
	if *jsonOut {
		emitJSON(manifold)
		return
	}
	switch {
	case *orbits:
		fmt.Print(renderSpineOrbits(manifold))
	case *accretion:
		fmt.Print(renderSpineAccretion(manifold))
	default:
		fmt.Print(renderSpineOverview(manifold))
	}
}

// emitJSON writes v as indented JSON to stdout.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "error: encode json: %v\n", err)
		os.Exit(1)
	}
}
