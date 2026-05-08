// cmd_pin.go — CLI commands for inter-workspace pin management.
//
// Commands:
//   cog pin list                              — list all pins for the current workspace
//   cog pin add <target> <ref> [flags]        — declare a new pin
//   cog pin remove <target>                   — delete a pin record
//   cog pin verify [<target>]                 — check live ref and digest drift
//   cog pin bump <target> <new-ref> [flags]   — update pin ref (explicit bump)
//
// Flags for add / bump:
//   --digest sha256:<hex>  — content-addressed pin
//   --branch <name>        — default branch context (default: main)
//   --sync <policy>        — read-only | read-write | mirror (default: read-only)

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/myrgic/cogos/internal/providers/pin"
	"gopkg.in/yaml.v3"
)

// cmdPin dispatches `cog pin <subcommand>`.
func cmdPin(args []string) error {
	if len(args) == 0 {
		return cmdPinHelp()
	}
	switch args[0] {
	case "list":
		return cmdPinList(args[1:])
	case "add":
		return cmdPinAdd(args[1:])
	case "remove", "rm":
		return cmdPinRemove(args[1:])
	case "verify":
		return cmdPinVerify(args[1:])
	case "bump":
		return cmdPinBump(args[1:])
	case "help", "-h", "--help":
		return cmdPinHelp()
	default:
		return fmt.Errorf("pin: unknown subcommand %q — run `cog pin help`", args[0])
	}
}

// cmdPinHelp prints usage.
func cmdPinHelp() error {
	fmt.Print(`Usage: cog pin <subcommand> [args]

Subcommands:
  list                            List all pins for the current workspace
  add <target> <ref> [flags]      Declare a new pin relationship
  remove <target>                 Delete a pin record
  verify [<target>]               Check live ref and digest drift
  bump <target> <new-ref> [flags] Update pin ref to a new value

Flags for add / bump:
  --digest sha256:<hex>           Content-addressed pin digest
  --branch <name>                 Default branch context (default: main)
  --sync <policy>                 read-only | read-write | mirror (default: read-only)

Examples:
  cog pin add cogos-dev/cogos v0.5.0 --digest sha256:abc123 --branch main
  cog pin verify cogos-dev/cogos
  cog pin bump cogos-dev/cogos v0.6.0
  cog pin list
  cog pin remove cogos-dev/cogos
`)
	return nil
}

// ─── list ────────────────────────────────────────────────────────────────────

// cmdPinList prints all pin records for the current workspace.
func cmdPinList(_ []string) error {
	root, err := resolveWorkspaceRoot()
	if err != nil {
		return err
	}

	p := pin.New(nil)
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		return fmt.Errorf("pin list: %w", err)
	}

	// Re-load directly for display; cast via WritePinRecord interface.
	entries, err := listPinRecords(root)
	if err != nil {
		return fmt.Errorf("pin list: %w", err)
	}
	_ = cfgAny // loaded to validate; display uses raw load

	if len(entries) == 0 {
		fmt.Println("No pins declared. Use `cog pin add` to create one.")
		return nil
	}

	fmt.Printf("%-30s  %-20s  %-10s  %s\n", "TARGET", "REF", "SYNC", "BRANCH")
	fmt.Println(strings.Repeat("-", 80))
	for _, rec := range entries {
		branch := rec.Branch
		if branch == "" {
			branch = "(default)"
		}
		sync := string(rec.Sync)
		if sync == "" {
			sync = "read-only"
		}
		fmt.Printf("%-30s  %-20s  %-10s  %s\n", rec.Target, rec.Pin.Ref, sync, branch)
	}
	return nil
}

// ─── add ─────────────────────────────────────────────────────────────────────

// cmdPinAdd creates a new pin record.
// Usage: cog pin add <target> <ref> [--digest sha256:...] [--branch main] [--sync read-only]
func cmdPinAdd(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("pin add: requires <target> and <ref> — run `cog pin help`")
	}
	target := args[0]
	ref := args[1]

	opts, err := parsePinFlags(args[2:])
	if err != nil {
		return fmt.Errorf("pin add: %w", err)
	}

	root, err := resolveWorkspaceRoot()
	if err != nil {
		return err
	}

	rec := &pin.PinRecord{
		Target: target,
		Pin: pin.PinRef{
			Ref:    ref,
			Digest: opts.digest,
		},
		Branch:  opts.branch,
		Sync:    pin.SyncPolicy(opts.sync),
		Updated: time.Now().UTC(),
	}

	if err := pin.WritePinRecord(root, rec); err != nil {
		return fmt.Errorf("pin add: %w", err)
	}
	fmt.Printf("Pin added: %s @ %s (sync: %s)\n", target, ref, rec.Sync)
	return nil
}

// ─── remove ──────────────────────────────────────────────────────────────────

// cmdPinRemove deletes a pin record.
func cmdPinRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("pin remove: requires <target> — run `cog pin help`")
	}
	target := args[0]

	root, err := resolveWorkspaceRoot()
	if err != nil {
		return err
	}

	if err := pin.RemovePinRecord(root, target); err != nil {
		return fmt.Errorf("pin remove: %w", err)
	}
	fmt.Printf("Pin removed: %s\n", target)
	return nil
}

// ─── verify ──────────────────────────────────────────────────────────────────

// cmdPinVerify resolves live HEAD for each pin and reports drift.
// Optional argument: specific target to verify. Omit to verify all.
//
// This is a thin shim over pin.RunVerify so the core logic is testable
// independently of workspace resolution and os.Stdout.
func cmdPinVerify(args []string) error {
	var filterTarget string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		filterTarget = args[0]
	}

	root, err := resolveWorkspaceRoot()
	if err != nil {
		return err
	}

	_, err = pin.RunVerify(context.Background(), root, filterTarget, nil, os.Stdout)
	return err
}

// ─── bump ────────────────────────────────────────────────────────────────────

// cmdPinBump updates the ref (and optional digest) on an existing pin record.
// Usage: cog pin bump <target> <new-ref> [--digest sha256:...] [--branch ...]
func cmdPinBump(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("pin bump: requires <target> and <new-ref> — run `cog pin help`")
	}
	target := args[0]
	newRef := args[1]

	opts, err := parsePinFlags(args[2:])
	if err != nil {
		return fmt.Errorf("pin bump: %w", err)
	}

	root, err := resolveWorkspaceRoot()
	if err != nil {
		return err
	}

	// Load existing record to preserve fields not being changed.
	entries, err := listPinRecords(root)
	if err != nil {
		return fmt.Errorf("pin bump: %w", err)
	}
	var existing *pin.PinRecord
	for _, rec := range entries {
		if rec.Target == target {
			existing = rec
			break
		}
	}
	if existing == nil {
		return fmt.Errorf("pin bump: no pin record found for target %q", target)
	}

	existing.Pin.Ref = newRef
	if opts.digest != "" {
		existing.Pin.Digest = opts.digest
	}
	if opts.branch != "" {
		existing.Branch = opts.branch
	}
	if opts.sync != "" {
		existing.Sync = pin.SyncPolicy(opts.sync)
	}
	existing.Updated = time.Now().UTC()

	if err := pin.WritePinRecord(root, existing); err != nil {
		return fmt.Errorf("pin bump: %w", err)
	}
	fmt.Printf("Pin bumped: %s @ %s\n", target, newRef)
	return nil
}

// ─── Flag parser ─────────────────────────────────────────────────────────────

type pinFlagOpts struct {
	digest string
	branch string
	sync   string
}

func parsePinFlags(args []string) (pinFlagOpts, error) {
	opts := pinFlagOpts{
		branch: "main",
		sync:   "read-only",
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--digest":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--digest requires a value")
			}
			i++
			opts.digest = args[i]
		case "--branch":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--branch requires a value")
			}
			i++
			opts.branch = args[i]
		case "--sync":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--sync requires a value")
			}
			i++
			opts.sync = args[i]
		default:
			return opts, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return opts, nil
}

// ─── Workspace resolution helpers ────────────────────────────────────────────

// resolveWorkspaceRoot returns the workspace root or an error. Wraps the
// kernel's ResolveWorkspace to centralise error handling for pin commands.
func resolveWorkspaceRoot() (string, error) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return "", fmt.Errorf("pin: could not resolve workspace root: %w", err)
	}
	return root, nil
}

// listPinRecords is a thin helper that loads pin records from root for display.
// Used by list, verify, and bump (which need the record data, not the full
// provider lifecycle).
func listPinRecords(root string) ([]*pin.PinRecord, error) {
	dir := pinsDirPath(root)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []*pin.PinRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := dir + "/" + e.Name()
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec pin.PinRecord
		if err := parseYAMLInto(data, &rec); err != nil {
			continue
		}
		if rec.Target == "" {
			continue
		}
		recs = append(recs, &rec)
	}
	return recs, nil
}

// pinsDirPath returns the .cog/pins path for a workspace root.
func pinsDirPath(root string) string {
	return root + "/.cog/pins"
}

// parseYAMLInto is a thin wrapper over yaml.Unmarshal for local use.
func parseYAMLInto(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}

// ─── Provider registration ────────────────────────────────────────────────────

// init registers the pin provider in the CLI binary's reconcile registry.
// The CLI binary uses the full internal/providers/pin implementation, which
// exercises the complete seven-method lifecycle (unlike the daemon-side stub
// which provides Health-only).
//
// The workspace root is injected lazily: LoadConfig is called with the resolved
// workspace root at reconcile time, not here. The provider is constructed with
// the default local-checkout resolver; tests replace the resolver via pin.New().
func init() {
	RegisterProvider("pin", pin.New(nil))
}
