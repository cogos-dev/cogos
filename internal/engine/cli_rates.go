// cli_rates.go — "cogos rates" subcommand (First Instruments Module C, M3).
//
// Usage:
//
//	cogos rates [--json]
//
// Prints the deterministic-echo 15-ratio table over the kernel's six clock
// constants (KernelRates). Side-effect-free: pure config read.
package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// runRatesCmd dispatches "cogos rates [flags]". args is everything after
// "rates" (i.e. os.Args[2:]).
func runRatesCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("rates", flag.ExitOnError)
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

	cfg, err := LoadConfig(root, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}

	// The CLI has no running ReconcileDaemon to ask for its actual
	// PollInterval; report the daemon's own default (0 => 30s, mirroring
	// ReconcileDaemonConfig.withDefaults). A live kernel's GET
	// /v1/kernel/rates reports the daemon's real PollInterval instead
	// (handleKernelRates).
	report := KernelRates(cfg, 0)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}

	fmt.Println("Clock constants:")
	for _, c := range report.Constants {
		knob := "config knob"
		if !c.HasConfigKnob {
			knob = "hardcoded default, no config knob"
		}
		fmt.Printf("  %-24s %10.1fs  (%s)\n", c.Name, c.Seconds, knob)
	}
	fmt.Println()
	fmt.Println("Rate ratios (deterministic-echo, all 15 pairs):")
	for _, r := range report.Ratios {
		cluster := ""
		if r.ClusterID != "" {
			cluster = fmt.Sprintf(" [%s]", r.ClusterID)
		}
		fmt.Printf("  %-24s / %-24s = %10.4f%s\n", r.Numerator, r.Denominator, r.Ratio, cluster)
	}
}
