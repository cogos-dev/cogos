// cmd_alias.go — CLI commands for the workspace alias system (#167).
//
// Commands:
//
//	cog alias list                              — tabular view with stale detection
//	cog alias add <name> <workspace> [flags]    — register an alias
//	cog alias remove <name>                     — remove an alias (idempotent)
//	cog alias resolve <name> [--uri <uri>]      — debug: show resolution chain
//	cog alias help                              — usage
//
// All writes go through pkg/filelock so concurrent invocations serialise
// cleanly.  Stale aliases (target not in global.yaml) are shown with a
// "[stale]" suffix in list output but are not errors.

package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/myrgic/cogos/pkg/alias"
)

// ── Dispatcher ────────────────────────────────────────────────────────────────

func cmdAlias(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		return cmdAliasHelp()
	}

	switch args[0] {
	case "list", "ls":
		return cmdAliasList()
	case "add":
		return cmdAliasAdd(args[1:])
	case "remove", "rm":
		return cmdAliasRemove(args[1:])
	case "resolve":
		return cmdAliasResolve(args[1:])
	default:
		return fmt.Errorf("unknown alias subcommand: %s\nRun 'cog alias help' for usage", args[0])
	}
}

// ── List ──────────────────────────────────────────────────────────────────────

func cmdAliasList() error {
	m, err := loadAliasMap()
	if err != nil {
		return err
	}

	entries := m.List()
	if len(entries) == 0 {
		fmt.Println("No aliases configured.")
		fmt.Println("Run 'cog alias add <name> <workspace>' to create one.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tWORKSPACE\tNODE\tDESCRIPTION")
	for _, e := range entries {
		desc := e.Description
		if e.Stale {
			if desc != "" {
				desc += " [stale]"
			} else {
				desc = "[stale]"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, e.Workspace, e.Node, desc)
	}
	return w.Flush()
}

// ── Add ───────────────────────────────────────────────────────────────────────

func cmdAliasAdd(args []string) error {
	// Parse: cog alias add <name> <workspace> [--node X] [--description "..."]
	if len(args) < 2 {
		return fmt.Errorf("usage: cog alias add <name> <workspace> [--node <node>] [--description <desc>]")
	}

	name := args[0]
	workspace := args[1]
	opts := alias.AliasOpts{}

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--node", "-n":
			if i+1 >= len(args) {
				return fmt.Errorf("--node requires a value")
			}
			i++
			opts.Node = args[i]
		case "--description", "--desc", "-d":
			if i+1 >= len(args) {
				return fmt.Errorf("--description requires a value")
			}
			i++
			opts.Description = args[i]
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	m, err := loadAliasMap()
	if err != nil {
		return err
	}

	if err := m.Add(name, workspace, opts); err != nil {
		return err
	}

	fmt.Printf("Alias %q → %q added.\n", name, workspace)
	return nil
}

// ── Remove ────────────────────────────────────────────────────────────────────

func cmdAliasRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog alias remove <name>")
	}
	name := args[0]

	m, err := loadAliasMap()
	if err != nil {
		return err
	}

	// Check before remove so we can print a useful message.
	_, _, existed := m.Expand(name)

	if err := m.Remove(name); err != nil {
		return err
	}

	if existed {
		fmt.Printf("Alias %q removed.\n", name)
	} else {
		fmt.Printf("Alias %q not found (nothing to remove).\n", name)
	}
	return nil
}

// ── Resolve ───────────────────────────────────────────────────────────────────

func cmdAliasResolve(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: cog alias resolve <name> [--uri <cog://uri>]")
	}

	name := args[0]
	rawURI := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--uri", "-u":
			if i+1 >= len(args) {
				return fmt.Errorf("--uri requires a value")
			}
			i++
			rawURI = args[i]
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	m, err := loadAliasMap()
	if err != nil {
		return err
	}

	fmt.Printf("Alias resolution chain for %q:\n\n", name)

	ws, node, ok := m.Expand(name)
	if !ok {
		fmt.Printf("  1. Alias lookup:      %q — NOT FOUND\n", name)
		return nil
	}
	fmt.Printf("  1. Alias lookup:      %q → workspace=%q node=%q\n", name, ws, node)

	// Check stale status.
	for _, e := range m.List() {
		if e.Name == name && e.Stale {
			fmt.Printf("  2. Registry check:    workspace %q — NOT IN global.yaml (STALE)\n", ws)
			return nil
		}
	}
	fmt.Printf("  2. Registry check:    workspace %q — OK\n", ws)

	if rawURI != "" {
		// Show what the URI would expand to.
		if strings.HasPrefix(rawURI, "cog://"+name) {
			expanded := "cog://" + ws + strings.TrimPrefix(rawURI, "cog://"+name)
			fmt.Printf("  3. URI rewrite:       %q → %q\n", rawURI, expanded)
		} else {
			fmt.Printf("  3. URI rewrite:       (authority in URI doesn't match alias name)\n")
		}
	}

	return nil
}

// ── Help ──────────────────────────────────────────────────────────────────────

func cmdAliasHelp() error {
	fmt.Print(`cog alias — workspace alias management

Usage:
  cog alias list
      Show all aliases. Stale entries (target no longer in global.yaml)
      are marked [stale].

  cog alias add <name> <workspace> [flags]
      Create or update an alias. <name> must match ^[a-z][a-z0-9_-]{0,30}$
      and must not be a reserved projection namespace (mem, adr, etc.).
      Flags:
        --node <node>            Pin alias to a specific node name
        --description <text>     Human-readable note

  cog alias remove <name>
      Remove an alias. Idempotent; removing a non-existent alias is not an error.

  cog alias resolve <name> [--uri <cog://uri>]
      Debug: show the full resolution chain for an alias name.
      Optionally rewrite a full cog://alias/path URI to its canonical form.

  cog alias help
      Show this message.

Schema (~/.cog/node/aliases.yaml):
  version: "1.0"
  aliases:
    cog: cog-workspace            # short form
    m3:                           # long form with metadata
      workspace: cogos-dev/mod3
      description: "mod3 voice server"
      node: darkstar

Writes are serialised via advisory file locking (pkg/filelock).
`)
	return nil
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// loadAliasMap loads the alias map from ~/.cog/node/aliases.yaml.
// nodeDir() is defined in cmd_node.go and returns ~/.cog/node/.
func loadAliasMap() (*alias.AliasMap, error) {
	return alias.Load(nodeDir())
}
