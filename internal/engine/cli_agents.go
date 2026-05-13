// cli_agents.go — `cogos agents` subcommand.
//
// Displays a unified view of running agents on this node:
//
//	SESSIONS         — hook-registered Claude Code sessions (from kernel registry)
//	SUB-AGENTS       — dispatched Claude Code sub-agents (requires #186)
//
// Data source: GET /v1/sessions on the running kernel daemon. Prints a
// human-readable table with ID, role, workspace, model, status, and
// time-since-last-heartbeat. If the daemon is not reachable the command
// exits with a clear error rather than silently printing nothing.
package engine

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func runAgentsCmd(args []string, defaultWorkspace string, defaultPort int) {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected if empty)")
	port := fs.Int("port", defaultPort, "Daemon port")
	all := fs.Bool("all", false, "Include ended sessions")
	_ = fs.Parse(args)

	baseURL := resolveClientEndpoint(*workspace, *port)
	url := baseURL + "/v1/sessions/presence"
	if *all {
		url += "?include_ended=true"
	}

	ctx0, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx0, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agents: build request: %v\n", err)
		os.Exit(1)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agents: daemon not reachable at %s: %v\n", baseURL, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agents: read response: %v\n", err)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "agents: daemon returned %d: %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	// The /v1/sessions/presence response embeds SessionState with an extra
	// Active bool field per entry. We decode to SessionState directly since
	// the session fields are what we need for the table.
	var payload struct {
		Sessions []struct {
			SessionState
			Active bool `json:"active"`
		} `json:"sessions"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		fmt.Fprintf(os.Stderr, "agents: parse response: %v\n", err)
		os.Exit(1)
	}

	sessions := make([]SessionState, 0, len(payload.Sessions))
	for _, e := range payload.Sessions {
		sessions = append(sessions, e.SessionState)
	}

	now := time.Now().UTC()
	printAgentsTable(sessions, now)
}

// printAgentsTable renders the agents view to stdout.
func printAgentsTable(sessions []SessionState, now time.Time) {
	if len(sessions) == 0 {
		fmt.Println("No sessions registered.")
		return
	}

	// Separate by role for grouped output.
	var active, ended []SessionState
	for _, s := range sessions {
		if s.Ended {
			ended = append(ended, s)
		} else {
			active = append(active, s)
		}
	}

	fmt.Println("SESSIONS")
	if len(active) == 0 {
		fmt.Println("  (none)")
	} else {
		fmt.Printf("  %-32s  %-20s  %-16s  %-10s  %s\n",
			"SESSION-ID", "ROLE", "MODEL", "STATUS", "LAST-SEEN")
		fmt.Printf("  %-32s  %-20s  %-16s  %-10s  %s\n",
			"----------", "----", "-----", "------", "---------")
		for _, s := range active {
			model := s.Model
			if model == "" {
				model = "-"
			}
			status := s.Status
			if status == "" {
				status = "active"
			}
			lastSeen := "-"
			if !s.LastSeen.IsZero() {
				ago := now.Sub(s.LastSeen).Round(time.Second)
				lastSeen = ago.String() + " ago"
			}
			fmt.Printf("  %-32s  %-20s  %-16s  %-10s  %s\n",
				s.SessionID, s.Role, model, status, lastSeen)
		}
	}

	if len(ended) > 0 {
		fmt.Println("\nENDED SESSIONS")
		fmt.Printf("  %-32s  %-20s  %-16s  %s\n",
			"SESSION-ID", "ROLE", "ENDED-AT", "REASON")
		fmt.Printf("  %-32s  %-20s  %-16s  %s\n",
			"----------", "----", "--------", "------")
		for _, s := range ended {
			endedAt := "-"
			if !s.EndedAt.IsZero() {
				endedAt = s.EndedAt.Format(time.RFC3339)
			}
			reason := s.EndReason
			if reason == "" {
				reason = "-"
			}
			fmt.Printf("  %-32s  %-20s  %-16s  %s\n",
				s.SessionID, s.Role, endedAt, reason)
		}
	}

	// Note about sub-agents: available once #186 lands.
	fmt.Println("\nSUB-AGENTS")
	fmt.Println("  (sub-agent registration not yet wired — see issue #186)")
}
