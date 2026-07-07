// cli_coherence.go — "cogos coherence check" subcommand.
//
// First Instruments Module B (M1-A). Surfaces the graded git-distance
// coherence score (internal/coherence.CheckCoherence) alongside the existing
// boolean coherent/drift fields. Side-effect-free: CheckCoherence computes
// the tree hash via a throwaway git index (K3 one-way-readout) and performs
// no writes.
//
// Usage:
//
//	cogos coherence check [--json]
package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/myrgic/cogos/internal/coherence"
)

// runCoherenceCmd dispatches "cogos coherence <subcommand> [flags]".
// args is everything after "coherence" (i.e. os.Args[2:]).
func runCoherenceCmd(args []string, defaultWorkspace string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: cogos coherence check [--json]\n")
		os.Exit(1)
	}

	switch args[0] {
	case "check":
		runCoherenceCheckCmd(args[1:], defaultWorkspace)
	default:
		fmt.Fprintf(os.Stderr, "cogos coherence: unknown subcommand %q\n\nUsage: cogos coherence check [--json]\n", args[0])
		os.Exit(1)
	}
}

func runCoherenceCheckCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("coherence check", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected from cwd if empty)")
	jsonOut := fs.Bool("json", false, "Output as JSON")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
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

	state, err := coherence.CheckCoherence(root)
	if err != nil && state == nil {
		fmt.Fprintf(os.Stderr, "error: check coherence: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(state)
		return
	}

	fmt.Printf("Coherent: %v\n", state.Coherent)
	fmt.Printf("Score:    %.4f\n", state.Score)
	if state.CanonicalHash != "" {
		fmt.Printf("Canonical: %s\n", state.CanonicalHash)
	}
	if state.CurrentHash != "" {
		fmt.Printf("Current:   %s\n", state.CurrentHash)
	}
	if len(state.Drift) > 0 {
		fmt.Printf("Drift (%d paths):\n", len(state.Drift))
		for _, d := range state.Drift {
			fmt.Printf("  %s\n", d)
		}
	}
}
