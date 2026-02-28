// cmd_bus.go — CLI commands for the CogOS bus system.
//
// Commands:
//   cog bus watch [flags]     — Live event watcher with filtering
//   cog bus list              — List registered buses
//   cog bus tail [N]          — Last N events from a bus
//   cog bus help              — Show usage

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
)

// ─── Dispatcher ─────────────────────────────────────────────────────────────────

func cmdBus(args []string) int {
	if len(args) == 0 {
		cmdBusHelp()
		return 0
	}

	switch args[0] {
	case "watch":
		return cmdBusWatch(args[1:])
	case "list":
		return cmdBusList(args[1:])
	case "tail":
		return cmdBusTail(args[1:])
	case "help", "-h", "--help":
		cmdBusHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "Unknown bus command: %s\n", args[0])
		cmdBusHelp()
		return 1
	}
}

// ─── watch ──────────────────────────────────────────────────────────────────────

func cmdBusWatch(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	watcher := &BusWatcher{
		format:     "line",
		kernelAddr: "localhost:5100",
		root:       root,
	}

	var triggerExprs []string

	// Parse flags
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-t", "--type":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.filter.Types = append(watcher.filter.Types, args[i])

		case "-f", "--from":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.filter.From = append(watcher.filter.From, args[i])

		case "--to":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --to requires a value\n")
				return 1
			}
			i++
			watcher.filter.To = append(watcher.filter.To, args[i])

		case "-F", "--field":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			ff, err := ParseFieldFilter(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid field filter %q: %v\n", args[i], err)
				return 1
			}
			watcher.filter.Fields = append(watcher.filter.Fields, ff)

		case "-b", "--bus":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			watcher.busID = args[i]

		case "--format":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --format requires a value\n")
				return 1
			}
			i++
			switch args[i] {
			case "line", "json", "full":
				watcher.format = args[i]
			default:
				fmt.Fprintf(os.Stderr, "Error: unknown format %q (use line, json, or full)\n", args[i])
				return 1
			}

		case "--no-replay":
			watcher.filter.NoReplay = true

		case "-n", "--limit":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				return 1
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "Error: invalid limit %q\n", args[i])
				return 1
			}
			watcher.limit = n

		case "--since":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --since requires a value\n")
				return 1
			}
			i++
			t, err := parseSinceDuration(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return 1
			}
			watcher.filter.Since = t

		case "--trigger":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: --trigger requires a value\n")
				return 1
			}
			i++
			triggerExprs = append(triggerExprs, args[i])

		case "--offline":
			watcher.offline = true

		case "-q", "--quiet":
			watcher.quiet = true

		case "-h", "--help":
			cmdBusWatchHelp()
			return 0

		default:
			// Treat positional arg as bus ID if not set
			if watcher.busID == "" && !strings.HasPrefix(args[i], "-") {
				watcher.busID = args[i]
			} else {
				fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", args[i])
				return 1
			}
		}
		i++
	}

	// Build trigger filter if provided
	if len(triggerExprs) > 0 {
		trigger := &WatchFilter{}
		for _, expr := range triggerExprs {
			ff, err := ParseFieldFilter(expr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid trigger %q: %v\n", expr, err)
				return 1
			}
			trigger.Fields = append(trigger.Fields, ff)
		}
		watcher.trigger = trigger
	}

	// Auto-detect bus if not specified
	if watcher.busID == "" {
		busID, err := autoDetectBus(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n  Use -b <bus_id> to specify a bus, or 'cog bus list' to see available buses.\n", err)
			return 1
		}
		watcher.busID = busID
	}

	// Set up signal handling for clean shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()

	if err := watcher.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

// ─── list ───────────────────────────────────────────────────────────────────────

func cmdBusList(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	registryPath := filepath.Join(root, ".cog", ".state", "buses", "registry.json")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No buses registered.")
			return 0
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	var entries []busRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		fmt.Fprintf(os.Stderr, "Error: parse registry: %v\n", err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println("No buses registered.")
		return 0
	}

	// Check for JSON output flag
	for _, a := range args {
		if a == "--json" {
			out, _ := json.MarshalIndent(entries, "", "  ")
			fmt.Println(string(out))
			return 0
		}
	}

	// Table output
	fmt.Printf("%-40s  %-8s  %6s  %s\n", "BUS ID", "STATE", "EVENTS", "PARTICIPANTS")
	fmt.Printf("%-40s  %-8s  %6s  %s\n", strings.Repeat("─", 40), strings.Repeat("─", 8), strings.Repeat("─", 6), strings.Repeat("─", 30))
	for _, e := range entries {
		participants := strings.Join(e.Participants, ", ")
		if len(participants) > 50 {
			participants = participants[:47] + "..."
		}
		fmt.Printf("%-40s  %-8s  %6d  %s\n", e.BusID, e.State, e.EventCount, participants)
	}

	return 0
}

// ─── tail ───────────────────────────────────────────────────────────────────────

func cmdBusTail(args []string) int {
	root, _, err := ResolveWorkspace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	n := 10 // default
	busID := ""
	format := "line"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-b", "--bus":
			if i+1 < len(args) {
				i++
				busID = args[i]
			}
		case "--format":
			if i+1 < len(args) {
				i++
				format = args[i]
			}
		case "-h", "--help":
			fmt.Println("Usage: cog bus tail [N] [-b bus_id] [--format line|json|full]")
			fmt.Println("\nShow the last N events from a bus (default: 10).")
			return 0
		default:
			if !strings.HasPrefix(args[i], "-") {
				if parsed, err := strconv.Atoi(args[i]); err == nil {
					n = parsed
				} else if busID == "" {
					busID = args[i]
				}
			}
		}
	}

	// Auto-detect bus
	if busID == "" {
		busID, err = autoDetectBus(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n  Use -b <bus_id> to specify a bus.\n", err)
			return 1
		}
	}

	// Read all events from JSONL
	mgr := newBusSessionManager(root)
	events, err := mgr.readBusEvents(busID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	if len(events) == 0 {
		fmt.Printf("No events in bus %s\n", busID)
		return 0
	}

	// Take last N
	start := 0
	if len(events) > n {
		start = len(events) - n
	}
	tail := events[start:]

	// Format output
	w := &BusWatcher{format: format}
	for i := range tail {
		fmt.Println(w.formatEvent(&tail[i]))
	}

	return 0
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdBusHelp() {
	fmt.Println(`Usage: cog bus <command> [flags]

Commands:
  watch    Live event watcher with filtering
  list     List registered buses
  tail     Show last N events from a bus
  help     Show this help

Run 'cog bus <command> --help' for command-specific help.`)
}

func cmdBusWatchHelp() {
	fmt.Println(`Usage: cog bus watch [bus_id] [flags]

Live event watcher with schema-based filtering. Connects to the kernel's
SSE endpoint for real-time events, or reads from JSONL files (--offline).

Flags:
  -t, --type <glob>      Event type filter (e.g. "chat.*", "*.message")
  -f, --from <glob>      Source filter (e.g. "user:*", "openclaw@*")
      --to <glob>        Target filter
  -F, --field <expr>     Payload field filter (e.g. "model=claude*", "tokens_used>100")
  -b, --bus <id>         Bus ID (default: auto-detect active bus)
      --format <fmt>     Output format: line (default), json, full
      --no-replay        Skip replaying historical events
  -n, --limit <N>        Stop after N matching events
      --since <dur>      Only events after timestamp/duration (e.g. "5m", RFC3339)
      --trigger <expr>   Break condition (same syntax as --field)
      --offline          Read from JSONL files instead of SSE
  -q, --quiet            Suppress status messages

Filter composition:
  Same flag type: OR'd    cog bus watch -t "chat.*" -t "tool.*"
  Different flags: AND'd  cog bus watch -t "chat.*" -f "user:*"

Field filter operators:
  key              Field exists (any value)
  key=val*         Glob match
  key!=val         Not equal
  key>N            Greater than (numeric)
  key<N            Less than (numeric)
  key>=N           Greater than or equal
  key<=N           Less than or equal

Examples:
  cog bus watch                                    # all events on auto-detected bus
  cog bus watch -t "chat.*"                        # chat lifecycle events
  cog bus watch -t "*.message"                     # channel messages
  cog bus watch -F "tokens_used>500" --format json # high-token events as JSON
  cog bus watch --trigger "finish_reason=stop" -n 1
  cog bus watch --offline -b bus_chat_http -t "chat.error"`)
}
