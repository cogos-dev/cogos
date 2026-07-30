// cli_debug.go — `cogos debug heap [--out FILE]` / `cogos debug goroutines
// [--out FILE]`: one-command capture of a live pprof profile from the
// running daemon onto disk.
//
// Filed against #505 ("Diagnostic gap, same class as #501: no pprof surface
// exists on the daemon ... the kernel cannot be asked about its own memory
// in one command today"). serve_debug.go mounts the loopback-only
// /debug/pprof/* + /debug/vars surface this talks to; this file is the CLI
// side of the same fix, so the operator/seat capturing evidence for a leak
// doesn't have to hand-roll a curl command against a port they had to look
// up first.
package engine

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runDebugCmd dispatches `cogos debug <subcommand> [flags]`.
func runDebugCmd(args []string, defaultWorkspace string, defaultPort int) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "cogos debug: missing subcommand (want: heap, goroutines)")
		os.Exit(1)
	}
	switch args[0] {
	case "heap":
		runDebugProfileCmd(args[1:], defaultWorkspace, defaultPort, "heap", "cogos-heap")
	case "goroutines":
		runDebugProfileCmd(args[1:], defaultWorkspace, defaultPort, "goroutine", "cogos-goroutine")
	default:
		fmt.Fprintf(os.Stderr, "cogos debug: unknown subcommand %q (want: heap, goroutines)\n", args[0])
		os.Exit(1)
	}
}

// runDebugProfileCmd fetches GET /debug/pprof/<profile> from the running
// daemon — resolved the same way `cogos health` resolves its target
// (recorded daemon state first, falling back to --port / the default 6931,
// via resolveClientEndpoint) — and writes the raw response body to --out
// (default "<filePrefix>-<timestamp>.pprof"; timestamped so repeat captures,
// e.g. a before/after differential across a leak's growth window per #505's
// own investigation, don't clobber each other). The body is exactly what
// net/http/pprof's Handler(profile) emits for a bare GET: the binary
// profile.proto format `go tool pprof <file>` reads directly.
func runDebugProfileCmd(args []string, defaultWorkspace string, defaultPort int, profile, filePrefix string) {
	fs := flag.NewFlagSet("debug "+profile, flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (used to resolve the running daemon)")
	port := fs.Int("port", defaultPort, "Daemon port when no runtime state exists")
	out := fs.String("out", "", "Output file path (default: "+filePrefix+"-<timestamp>.pprof)")
	_ = fs.Parse(args)

	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%s.pprof", filePrefix, time.Now().Format("20060102-150405"))
	}

	baseURL := resolveClientEndpoint(*workspace, *port)
	url := baseURL + "/debug/pprof/" + profile

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fetch %s: %v\n(is the daemon running? try `cogos health`)\n", url, err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(os.Stderr, "error: %s returned status %d: %s\n", url, resp.StatusCode, string(body))
		os.Exit(1)
	}

	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create %s: %v\n", outPath, err)
		os.Exit(1)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Printf("wrote %d bytes to %s\n", n, outPath)
	fmt.Printf("inspect with: go tool pprof %s\n", outPath)
}
