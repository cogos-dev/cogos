// cli_doctor.go — "cogos doctor" subcommand.
//
// Usage:
//
//	cogos doctor [--workspace PATH] [--json] [--stale-days N]
//	             [--scan-dir DIR ...] [--config FILE ...] [--skip-network]
//
// doctor compares the local install and workspace against what a healthy
// install should look like. It exists because every failure mode it checks
// for fails *silently*: a stale binary still prints a version string, an
// empty search result still exits 0, a dead SQLite store still answers
// queries. See myrgic/cogos#568 for the incident that motivated it — a
// working session spent diagnosing a phantom FTS bug that was actually a
// 79-day-old binary shadowing the real one on PATH.
//
// # Output contract
//
// Every check reports exactly one of:
//
//	OK       - checked, and the property holds.
//	WARN     - checked, holds with a caveat (drift, staleness, an unindexed
//	           tree) that a human should look at but that is not itself
//	           evidence of a broken system.
//	FAIL     - checked, and the property does NOT hold. A FAIL means the
//	           system is misbehaving, not merely untidy (e.g. the negative-
//	           control search returned zero hits for a term known to be
//	           indexed).
//	UNKNOWN  - the check could not be performed (missing file, unreadable
//	           database, unreachable network, no daemon running). UNKNOWN is
//	           never reported as OK — a check that cannot run has learned
//	           nothing, and silently defaulting to "OK" is exactly the
//	           failure class this command exists to close off.
//
// Exit code: 0 unless at least one check reports FAIL, in which case doctor
// exits 1. WARN and UNKNOWN do not affect the exit code — they are surfaced
// in the report for a human to read, not treated as build-breaking.
//
// cli_*.go files may import sdk/constellation; see the package-boundary note
// in cli_reindex.go.
package engine

import (
	"database/sql"
	"debug/buildinfo"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Report types
// ---------------------------------------------------------------------------

// DoctorStatus is the verdict of a single doctor check.
type DoctorStatus string

const (
	StatusOK      DoctorStatus = "OK"
	StatusWarn    DoctorStatus = "WARN"
	StatusFail    DoctorStatus = "FAIL"
	StatusUnknown DoctorStatus = "UNKNOWN"
)

// DoctorCheck is one reported assertion within a group.
type DoctorCheck struct {
	Name   string       `json:"name"`
	Status DoctorStatus `json:"status"`
	Detail string       `json:"detail"`
}

// DoctorGroup is a named cluster of related checks (install integrity,
// config coherence, index health, store liveness).
type DoctorGroup struct {
	Name   string        `json:"name"`
	Checks []DoctorCheck `json:"checks"`
}

// DoctorReport is the full result of a `cogos doctor` run.
type DoctorReport struct {
	Workspace   string        `json:"workspace"`
	GeneratedAt time.Time     `json:"generated_at"`
	Groups      []DoctorGroup `json:"groups"`
}

// ExitCode implements the output contract: non-zero iff any check FAILed.
func (r *DoctorReport) ExitCode() int {
	if r.hasStatus(StatusFail) {
		return 1
	}
	return 0
}

func (r *DoctorReport) hasStatus(s DoctorStatus) bool {
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if c.Status == s {
				return true
			}
		}
	}
	return false
}

func (r *DoctorReport) addGroup(name string) *DoctorGroup {
	r.Groups = append(r.Groups, DoctorGroup{Name: name})
	return &r.Groups[len(r.Groups)-1]
}

func (g *DoctorGroup) add(name string, status DoctorStatus, detail string) {
	g.Checks = append(g.Checks, DoctorCheck{Name: name, Status: status, Detail: detail})
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// DoctorOptions configures a doctor run. Zero value is a sane default.
type DoctorOptions struct {
	// ExtraScanDirs are additional directories to search for stray
	// cogos/cog binaries, beyond the built-in defaults ($PATH entries,
	// ~/.cog*, ~/go/bin, the workspace root and its .cog dir).
	ExtraScanDirs []string
	// ExtraConfigFiles are additional config files to check for coherence,
	// beyond the built-in defaults (~/.claude/settings*.json, discovered
	// .mcp.json files, ~/.hermes/profiles/*/config.yaml).
	ExtraConfigFiles []string
	// StaleDays is the age threshold, in days, beyond which a SQLite
	// store's last write flags it DEAD (WARN) rather than merely queryable.
	StaleDays int
	// SkipNetwork disables the published-tag lookup (install integrity
	// check "version vs published tag"), which otherwise makes one HTTP
	// call to GitHub. Useful offline; the check reports UNKNOWN instead of
	// silently passing.
	SkipNetwork bool
	// ReleaseRepo is the "owner/repo" GitHub slug used for the
	// published-tag check. Defaults to "myrgic/cogos".
	ReleaseRepo string
}

func (o *DoctorOptions) normalize() {
	if o.StaleDays <= 0 {
		o.StaleDays = 30
	}
	if o.ReleaseRepo == "" {
		o.ReleaseRepo = "myrgic/cogos"
	}
}

// ---------------------------------------------------------------------------
// CLI entry point
// ---------------------------------------------------------------------------

func runDoctorCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path (auto-detected from cwd if empty)")
	jsonOut := fs.Bool("json", false, "Output the report as JSON")
	staleDays := fs.Int("stale-days", 30, "Days since last write before a SQLite store is flagged DEAD")
	skipNetwork := fs.Bool("skip-network", false, "Skip the published-tag network lookup (report UNKNOWN instead)")
	releaseRepo := fs.String("release-repo", "myrgic/cogos", "owner/repo GitHub slug used for the published-tag check")
	var scanDirs stringListFlag
	fs.Var(&scanDirs, "scan-dir", "Extra directory to scan for stray cogos/cog binaries (repeatable)")
	var configFiles stringListFlag
	fs.Var(&configFiles, "config", "Extra config file to check for coherence (repeatable)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cogos doctor [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Compares the local install and workspace against what a healthy install\n")
		fmt.Fprintf(os.Stderr, "should look like: install integrity, config coherence, index health, and\n")
		fmt.Fprintf(os.Stderr, "SQLite store liveness.\n\n")
		fmt.Fprintf(os.Stderr, "Output contract:\n")
		fmt.Fprintf(os.Stderr, "  OK       checked, property holds\n")
		fmt.Fprintf(os.Stderr, "  WARN     checked, holds with a caveat worth a human's attention\n")
		fmt.Fprintf(os.Stderr, "  FAIL     checked, property does NOT hold\n")
		fmt.Fprintf(os.Stderr, "  UNKNOWN  could not be checked (never reported as OK)\n\n")
		fmt.Fprintf(os.Stderr, "Exit code: 0 unless any check reports FAIL (then 1). WARN/UNKNOWN do not\n")
		fmt.Fprintf(os.Stderr, "affect the exit code.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

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

	opts := DoctorOptions{
		ExtraScanDirs:    []string(scanDirs),
		ExtraConfigFiles: []string(configFiles),
		StaleDays:        *staleDays,
		SkipNetwork:      *skipNetwork,
		ReleaseRepo:      *releaseRepo,
	}

	report := RunDoctor(root, opts)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printDoctorReport(os.Stdout, report)
	}

	os.Exit(report.ExitCode())
}

// stringListFlag implements flag.Value for a repeatable string flag.
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func printDoctorReport(w *os.File, r *DoctorReport) {
	fmt.Fprintf(w, "cogos doctor — workspace %s\n\n", r.Workspace)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	counts := map[DoctorStatus]int{}
	for _, g := range r.Groups {
		fmt.Fprintf(tw, "-- %s --\t\t\n", g.Name)
		for _, c := range g.Checks {
			counts[c.Status]++
			fmt.Fprintf(tw, "  [%s]\t%s\t%s\n", c.Status, c.Name, doctorFirstLine(c.Detail))
			for _, extra := range doctorExtraLines(c.Detail) {
				fmt.Fprintf(tw, "\t\t  %s\n", extra)
			}
		}
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d OK, %d WARN, %d FAIL, %d UNKNOWN\n",
		counts[StatusOK], counts[StatusWarn], counts[StatusFail], counts[StatusUnknown])
	if counts[StatusFail] > 0 {
		fmt.Fprintf(w, "exit 1 (FAIL present)\n")
	}
}

func doctorFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func doctorExtraLines(s string) []string {
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return nil
	}
	return strings.Split(s[i+1:], "\n")
}

// ---------------------------------------------------------------------------
// RunDoctor — orchestration (no os.Exit; testable directly)
// ---------------------------------------------------------------------------

// RunDoctor runs every check group against the workspace at root and returns
// the assembled report. It never calls os.Exit and never panics on a missing
// or unreadable workspace — a nonexistent root simply drives every group's
// checks to UNKNOWN or FAIL as appropriate, per the output contract.
func RunDoctor(root string, opts DoctorOptions) *DoctorReport {
	opts.normalize()

	report := &DoctorReport{
		Workspace:   root,
		GeneratedAt: time.Now().UTC(),
	}

	doctorInstallIntegrity(report, root, opts)
	doctorConfigCoherence(report, root, opts)
	doctorIndexHealth(report, root, opts)
	doctorStoreLiveness(report, root, opts)

	return report
}

// ---------------------------------------------------------------------------
// Group 1: Install integrity
// ---------------------------------------------------------------------------

func doctorInstallIntegrity(report *DoctorReport, root string, opts DoctorOptions) {
	g := report.addGroup("install integrity")

	// -- PATH resolution -----------------------------------------------
	pathBins := map[string]string{} // name -> resolved path
	for _, name := range []string{"cogos", "cog"} {
		p, err := lookPathAll(name)
		if err != nil || len(p) == 0 {
			g.add("PATH resolution: "+name, StatusWarn, name+" not found on PATH")
			continue
		}
		pathBins[name] = p[0]
		detail := fmt.Sprintf("%s -> %s", name, p[0])
		if len(p) > 1 {
			detail += fmt.Sprintf("\nshadowed by %d more PATH entries: %s", len(p)-1, strings.Join(p[1:], ", "))
		}
		g.add("PATH resolution: "+name, StatusOK, detail)
	}

	// -- Version + build info of the resolved cogos ---------------------
	var resolvedInfo *buildinfo.BuildInfo
	if p, ok := pathBins["cogos"]; ok {
		bi, err := buildinfo.ReadFile(p)
		if err != nil {
			g.add("resolved binary build info", StatusUnknown, fmt.Sprintf("could not read build info from %s: %v", p, err))
		} else {
			resolvedInfo = bi
			g.add("resolved binary build info", StatusOK, fmt.Sprintf(
				"%s\ngo=%s module=%s version=%s",
				p, bi.GoVersion, bi.Main.Path, bi.Main.Version))
		}
	} else {
		g.add("resolved binary build info", StatusUnknown, "no cogos resolved on PATH to inspect")
	}

	// -- Version vs published tag ---------------------------------------
	doctorVersionVsPublished(g, resolvedInfo, opts)

	// -- Enumerate every cogos/cog binary on disk, dev-artifact flagged --
	doctorBinarySprawl(g, root, opts)
}

// lookPathAll resolves name against every directory on PATH, returning every
// match (not just the first) so shadowing can be reported. Mirrors
// exec.LookPath's executable-bit check but does not stop at the first hit.
func lookPathAll(name string) ([]string, error) {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return nil, fmt.Errorf("PATH is empty")
	}
	var matches []string
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, name)
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
			continue // not executable
		}
		matches = append(matches, cand)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s: not found on PATH", name)
	}
	return matches, nil
}

// ghTagRelease is the subset of the GitHub release JSON doctor needs.
type ghTagRelease struct {
	TagName string `json:"tag_name"`
}

func doctorVersionVsPublished(g *DoctorGroup, bi *buildinfo.BuildInfo, opts DoctorOptions) {
	const name = "version vs published tag"
	if opts.SkipNetwork {
		g.add(name, StatusUnknown, "network check skipped (--skip-network)")
		return
	}
	if bi == nil {
		g.add(name, StatusUnknown, "no resolved binary build info to compare")
		return
	}
	running := bi.Main.Version
	if running == "" || running == "(devel)" {
		g.add(name, StatusWarn, fmt.Sprintf("resolved binary reports version %q — a local `go build` without -ldflags, not a tagged release", running))
		return
	}

	url := "https://api.github.com/repos/" + opts.ReleaseRepo + "/releases/latest"
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		g.add(name, StatusUnknown, fmt.Sprintf("build request: %v", err))
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		g.add(name, StatusUnknown, fmt.Sprintf("GitHub unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.add(name, StatusUnknown, fmt.Sprintf("GitHub returned status %d", resp.StatusCode))
		return
	}
	var rel ghTagRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		g.add(name, StatusUnknown, fmt.Sprintf("decode GitHub response: %v", err))
		return
	}
	if rel.TagName == "" {
		g.add(name, StatusUnknown, "GitHub response had no tag_name")
		return
	}

	if running == rel.TagName || "v"+strings.TrimPrefix(running, "v") == rel.TagName {
		g.add(name, StatusOK, fmt.Sprintf("running %s matches latest published tag %s", running, rel.TagName))
		return
	}
	g.add(name, StatusWarn, fmt.Sprintf("running %s, latest published tag is %s", running, rel.TagName))
}

// binInfo is one located cogos/cog binary on disk.
type binInfo struct {
	Path    string
	Size    int64
	Age     time.Duration
	ModTime time.Time
	DevOnly bool // in-repo build artifact, not a real install location
}

// cogosBinaryNameRe matches "cogos" or "cog" themselves plus the naming
// variants a manual install/rollback workflow actually produces on this
// class of tooling: "cogos.prev", "cogos.prev2", "cogos-build", "cog.bak",
// etc. Anchored so it does not match unrelated files that merely start with
// "cog" (e.g. "cogfield", "cogn8-notes.md").
var cogosBinaryNameRe = regexp.MustCompile(`^cog(?:os)?(?:[.\-_].*)?$`)

func doctorBinarySprawl(g *DoctorGroup, root string, opts DoctorOptions) {
	home, _ := os.UserHomeDir()

	dirs := map[string]bool{} // dedupe
	addDir := func(d string) {
		if d != "" {
			dirs[d] = true
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		addDir(d)
	}
	if home != "" {
		addDir(filepath.Join(home, ".cog"))
		addDir(filepath.Join(home, ".cog", "bin"))
		addDir(filepath.Join(home, ".cog", "flight-ops"))
		addDir(filepath.Join(home, "go", "bin"))
	}
	if exe, err := os.Executable(); err == nil {
		addDir(filepath.Dir(exe))
	}
	// The doctor target workspace itself: a workspace-local wrapper script
	// (e.g. scripts/cog) commonly resolves a binary from the workspace root
	// or its .cog dir, and a build sitting there silently shadows whatever
	// the wrapper *thinks* it is running (cogos#568 finding 1).
	if root != "" {
		addDir(root)
		addDir(filepath.Join(root, ".cog"))
	}
	for _, d := range opts.ExtraScanDirs {
		addDir(d)
	}

	var found []binInfo
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !cogosBinaryNameRe.MatchString(e.Name()) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
				continue // not executable — a stray config/log file, not a binary
			}
			found = append(found, binInfo{
				Path:    p,
				Size:    info.Size(),
				Age:     time.Since(info.ModTime()),
				ModTime: info.ModTime(),
				DevOnly: looksLikeDevArtifact(p, opts),
			})
		}
	}

	if len(found) == 0 {
		g.add("binary sprawl", StatusUnknown, "no scan directories yielded a stat'able cogos/cog binary")
		return
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	var lines []string
	for _, b := range found {
		tag := ""
		if b.DevOnly {
			tag = " [dev-only, in-repo build artifact]"
		}
		lines = append(lines, fmt.Sprintf("%s  %.1fMB  age=%s%s",
			b.Path, float64(b.Size)/1e6, roundDays(b.Age), tag))
	}
	detail := fmt.Sprintf("%d binaries found:\n%s", len(found), strings.Join(lines, "\n"))

	status := StatusOK
	nonDev := 0
	for _, b := range found {
		if !b.DevOnly {
			nonDev++
		}
	}
	if nonDev > 1 {
		status = StatusWarn
	}
	g.add("binary sprawl", status, detail)
}

// looksLikeDevArtifact flags a binary path that sits inside a git worktree of
// the cogos repo itself (a `go build` output next to go.mod) rather than a
// real install location. It walks upward from the binary looking for a
// sibling go.mod that declares the cogos module — cheap, and correct
// regardless of where the repo happens to be checked out.
func looksLikeDevArtifact(binPath string, opts DoctorOptions) bool {
	dir := filepath.Dir(binPath)
	for i := 0; i < 4; i++ { // repo root is at most a few levels up from a build artifact
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/myrgic/cogos") {
				return true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return false
}

func roundDays(d time.Duration) string {
	days := d.Hours() / 24
	if days < 1 {
		return fmt.Sprintf("%.0fh", d.Hours())
	}
	return fmt.Sprintf("%.0fd", days)
}

// ---------------------------------------------------------------------------
// Group 2: Config coherence
// ---------------------------------------------------------------------------

// pathLikeRe extracts filesystem-path-shaped substrings from a larger string
// (e.g. the path embedded in a `Bash(find /Users/x/y ...)` permission
// string). It is deliberately anchored to conventional absolute-path roots
// (~/, /Users/, /home/, /opt/, /usr/, /var/, /tmp/, /etc/) rather than any
// "/segment/segment" shape — Hermes/MCP configs are full of superficially
// path-shaped strings that are not paths at all (HuggingFace model IDs like
// "lmstudio-eclipse/google/gemma-4-e4b", API base URLs like
// "https://api.anthropic.com/v1", docker image refs, MCP route strings like
// "/v1/synthesize"), and an unanchored pattern flags all of them as
// "nonexistent path" false positives.
var pathLikeRe = regexp.MustCompile(`~/[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)*|/(?:Users|home|opt|usr|var|tmp|etc)/[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)*`)
var mcpServerNameRe = regexp.MustCompile(`mcp__([A-Za-z0-9_-]+)__`)

func doctorConfigCoherence(report *DoctorReport, root string, opts DoctorOptions) {
	g := report.addGroup("config coherence")
	home, _ := os.UserHomeDir()

	var files []string
	if home != "" {
		files = append(files,
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "settings.local.json"),
		)
		files = append(files, globQuiet(filepath.Join(home, ".claude", "*.mcp.json"))...)
		files = append(files, globQuiet(filepath.Join(home, ".claude", "**", ".mcp.json"))...)
		files = append(files, globQuiet(filepath.Join(home, ".hermes", "profiles", "*", "config.yaml"))...)
	}
	files = append(files, filepath.Join(root, ".mcp.json"))
	files = append(files, opts.ExtraConfigFiles...)

	type foundConfig struct {
		path string
		data any
	}
	var loaded []foundConfig
	checkedAny := false
	for _, f := range dedupeExisting(files) {
		checkedAny = true
		data, err := loadConfigFile(f)
		if err != nil {
			g.add("parse "+f, StatusWarn, fmt.Sprintf("could not parse: %v", err))
			continue
		}
		loaded = append(loaded, foundConfig{path: f, data: data})
	}

	if !checkedAny {
		g.add("config discovery", StatusUnknown, "no MCP/Claude/Hermes config files found at the known locations")
		return
	}
	g.add("config discovery", StatusOK, fmt.Sprintf("%d config file(s) found", len(loaded)))

	// -- Nonexistent path references -------------------------------------
	var badRefs []string
	serverNames := map[string]bool{}
	cogosCommands := map[string]bool{} // distinct "command" values whose basename is cogos/cog
	for _, fc := range loaded {
		raw, _ := json.Marshal(fc.data) // normalize YAML→map through JSON for uniform string-walking
		var generic any
		if err := json.Unmarshal(raw, &generic); err == nil {
			walkStrings(generic, func(s string) {
				for _, m := range mcpServerNameRe.FindAllStringSubmatch(s, -1) {
					serverNames[m[1]] = true
				}
				for _, m := range pathLikeRe.FindAllString(s, -1) {
					p := expandHome(m, os.Getenv("HOME"))
					if looksLikeRealPath(p) {
						if _, err := os.Stat(p); err != nil {
							badRefs = append(badRefs, fmt.Sprintf("%s: %s (in %s)", "nonexistent path", p, fc.path))
						}
					}
				}
			})
			// Structured "command" field walk: precise binary-path extraction
			// (as opposed to the freeform path-substring scan above), so a
			// docker image tag or model ID that happens to be path-shaped
			// never gets counted as a binary reference.
			walkCommandFields(generic, func(cmd string) {
				base := filepath.Base(cmd)
				if base == "cogos" || base == "cog" {
					cogosCommands[cmd] = true
				}
			})
		}
	}
	badRefs = dedupeStrings(badRefs)
	if len(badRefs) == 0 {
		g.add("nonexistent path references", StatusOK, "no config path reference points at a missing directory")
	} else {
		sort.Strings(badRefs)
		g.add("nonexistent path references", StatusWarn, strings.Join(badRefs, "\n"))
	}

	// -- MCP configs agree on one binary -----------------------------------
	if len(cogosCommands) == 0 {
		g.add("MCP configs point at one binary", StatusUnknown, "no MCP config declares a cogos/cog \"command\" field to compare")
	} else if len(cogosCommands) == 1 {
		g.add("MCP configs point at one binary", StatusOK, "all reference(s) resolve to the same binary: "+doctorSortedBoolKeys(cogosCommands)[0])
	} else {
		g.add("MCP configs point at one binary", StatusWarn, "distinct binaries referenced:\n"+strings.Join(doctorSortedBoolKeys(cogosCommands), "\n"))
	}

	// -- MCP server-name generation drift ---------------------------------
	groups := groupByPrefix(mapKeys(serverNames))
	var driftLines []string
	for _, grp := range groups {
		if len(grp) > 1 {
			sort.Strings(grp)
			driftLines = append(driftLines, strings.Join(grp, ", "))
		}
	}
	if len(driftLines) == 0 {
		g.add("MCP server-name generations", StatusOK, "no server-name family has more than one generation referenced")
	} else {
		g.add("MCP server-name generations", StatusWarn, "multiple generations referenced:\n"+strings.Join(driftLines, "\n"))
	}
}

// walkCommandFields visits every object in a JSON-shaped any value that has
// a string "command" key (the MCP server-config shape: {"command": "...",
// "args": [...]}) and calls fn with that command string.
func walkCommandFields(v any, fn func(string)) {
	switch t := v.(type) {
	case map[string]any:
		if cmd, ok := t["command"].(string); ok {
			fn(cmd)
		}
		for _, e := range t {
			walkCommandFields(e, fn)
		}
	case []any:
		for _, e := range t {
			walkCommandFields(e, fn)
		}
	}
}

func doctorSortedBoolKeys(m map[string]bool) []string {
	out := mapKeys(m)
	sort.Strings(out)
	return out
}

// looksLikeRealPath filters pathLikeRe matches down to plausible filesystem
// paths: at least two path segments and not obviously a URL fragment.
func looksLikeRealPath(p string) bool {
	if strings.Contains(p, "://") {
		return false
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	return len(segs) >= 2
}

func expandHome(p, home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") && home != "" {
		return filepath.Join(home, p[2:])
	}
	return p
}

// groupByPrefix groups names where the shortest name in a cluster is a
// literal prefix of the others (e.g. "cogos" is a prefix of "cogos-http" and
// "cogos-v3"), which is exactly the naming-generation drift pattern the
// config-coherence check looks for. Names that share no such relation stay
// in their own singleton group.
func groupByPrefix(names []string) [][]string {
	sort.Strings(names) // shortest-first within equal length is not guaranteed,
	// but sorting lexicographically clusters common-prefix names adjacently
	// enough for this heuristic; exhaustive pairwise check below is authoritative.
	used := make([]bool, len(names))
	var groups [][]string
	for i, base := range names {
		if used[i] {
			continue
		}
		group := []string{base}
		used[i] = true
		for j := i + 1; j < len(names); j++ {
			if used[j] {
				continue
			}
			if strings.HasPrefix(names[j], base+"-") || strings.HasPrefix(base, names[j]+"-") {
				group = append(group, names[j])
				used[j] = true
			}
		}
		groups = append(groups, group)
	}
	return groups
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func dedupeExisting(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func globQuiet(pattern string) []string {
	// filepath.Glob does not support "**"; expand it into a bounded manual
	// walk (depth <= 4) when present, otherwise defer to filepath.Glob.
	if strings.Contains(pattern, "**") {
		parts := strings.SplitN(pattern, "**", 2)
		base := strings.TrimSuffix(parts[0], string(filepath.Separator))
		suffix := strings.TrimPrefix(parts[1], string(filepath.Separator))
		var out []string
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				depth := strings.Count(strings.TrimPrefix(p, base), string(filepath.Separator))
				if depth > 4 {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, suffix) {
				out = append(out, p)
			}
			return nil
		})
		return out
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	return matches
}

func loadConfigFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out any
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		if err := yaml.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkStrings visits every string leaf in a JSON-shaped any value (as
// produced by encoding/json or converted from YAML).
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case map[string]any:
		for k, e := range t {
			fn(k)
			walkStrings(e, fn)
		}
	}
}

// ---------------------------------------------------------------------------
// Group 3: Index health
// ---------------------------------------------------------------------------

func doctorIndexHealth(report *DoctorReport, root string, opts DoctorOptions) {
	g := report.addGroup("index health")

	dbPath := filepath.Join(root, ".cog", ".state", "constellation.db")
	if _, err := os.Stat(dbPath); err != nil {
		g.add("constellation.db present", StatusUnknown, fmt.Sprintf("not found at %s: %v", dbPath, err))
		g.add("documents vs files on disk", StatusUnknown, "no database to compare against")
		g.add("index freshness", StatusUnknown, "no database to compare against")
		g.add("negative control", StatusUnknown, "no database to query")
		return
	}
	g.add("constellation.db present", StatusOK, dbPath)

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=3000")
	if err != nil {
		g.add("constellation.db open", StatusUnknown, fmt.Sprintf("%v", err))
		g.add("documents vs files on disk", StatusUnknown, "database unreadable")
		g.add("index freshness", StatusUnknown, "database unreadable")
		g.add("negative control", StatusUnknown, "database unreadable")
		return
	}
	defer db.Close()

	doctorDocsVsFiles(g, db, root)
	doctorIndexFreshness(g, db, root)
	doctorNegativeControl(g, db, dbPath, root)
}

func doctorDocsVsFiles(g *DoctorGroup, db *sql.DB, root string) {
	cogDir := filepath.Join(root, ".cog")
	entries, err := os.ReadDir(cogDir)
	if err != nil {
		g.add("documents vs files on disk", StatusUnknown, fmt.Sprintf("read %s: %v", cogDir, err))
		return
	}

	var lines []string
	unindexed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := e.Name()
		subPath := filepath.Join(cogDir, sub)
		fileCount := countMarkdownFiles(subPath)
		if fileCount == 0 {
			continue // nothing on disk in this tree; no signal either way
		}
		rowCount, err := countDocsUnderPrefix(db, subPath)
		if err != nil {
			continue
		}
		status := "ok"
		if rowCount == 0 {
			unindexed++
			status = "UNINDEXED"
		} else if rowCount < fileCount/2 {
			status = "partial"
		}
		lines = append(lines, fmt.Sprintf(".cog/%s: %d files on disk, %d indexed [%s]", sub, fileCount, rowCount, status))
	}

	sort.Strings(lines)
	detail := strings.Join(lines, "\n")
	if detail == "" {
		g.add("documents vs files on disk", StatusUnknown, "no markdown-bearing .cog subdirectories found")
		return
	}
	if unindexed > 0 {
		g.add("documents vs files on disk", StatusWarn, detail)
	} else {
		g.add("documents vs files on disk", StatusOK, detail)
	}
}

func countMarkdownFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			n++
		}
		return nil
	})
	return n
}

func countDocsUnderPrefix(db *sql.DB, prefix string) (int, error) {
	var n int
	// Escape LIKE metacharacters in the literal prefix and require the
	// wildcard to start after a path separator, not immediately after the
	// prefix text. Without both of these, a bare `prefix+"%"` pattern (a) lets
	// any literal `_`/`%` in the real path act as an unintended wildcard, and
	// (b) matches sibling subtrees that merely share a name prefix -- e.g.
	// ".cog/adr" would also match every row under ".cog/adr-legacy" -- which
	// silently merges two subtrees' document counts. Same hazard the project
	// already called out and avoided once in sdk/constellation/indexer.go's
	// removed prefix-DELETE.
	pattern := escapeLikePattern(prefix) + string(filepath.Separator) + "%"
	err := db.QueryRow(`SELECT COUNT(*) FROM documents WHERE path LIKE ? ESCAPE '\'`, pattern).Scan(&n)
	return n, err
}

// escapeLikePattern escapes SQL LIKE metacharacters (%, _, and the escape
// character itself) in a literal string so it can be used as a LIKE prefix
// without a literal underscore or percent sign in a real path acting as an
// unintended wildcard. Pair with `ESCAPE '\'` in the query.
func escapeLikePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func doctorIndexFreshness(g *DoctorGroup, db *sql.DB, root string) {
	var newestIndexed sql.NullString
	if err := db.QueryRow(`SELECT MAX(indexed_at) FROM documents`).Scan(&newestIndexed); err != nil || !newestIndexed.Valid {
		g.add("index freshness", StatusUnknown, "could not read newest indexed_at from documents table")
		return
	}
	indexedAt, err := time.Parse(time.RFC3339, newestIndexed.String)
	if err != nil {
		g.add("index freshness", StatusUnknown, fmt.Sprintf("unparseable indexed_at %q: %v", newestIndexed.String, err))
		return
	}

	memDir := filepath.Join(root, ".cog", "mem")
	var newestMtime time.Time
	found := false
	_ = filepath.WalkDir(memDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if info.ModTime().After(newestMtime) {
			newestMtime = info.ModTime()
			found = true
		}
		return nil
	})
	if !found {
		g.add("index freshness", StatusUnknown, fmt.Sprintf("no markdown files found under %s", memDir))
		return
	}

	gap := newestMtime.Sub(indexedAt)
	detail := fmt.Sprintf("newest indexed_at=%s, newest file mtime=%s, gap=%s",
		indexedAt.Format(time.RFC3339), newestMtime.Format(time.RFC3339), gap.Round(time.Minute))
	if gap > time.Hour {
		g.add("index freshness", StatusWarn, detail)
	} else {
		g.add("index freshness", StatusOK, detail)
	}
}

// doctorNegativeControl is the single most important check: it derives a
// sentinel phrase from a document already known to be indexed, then runs
// that phrase through the REAL search path (searchMemoryFTS — the same
// function the MCP search tool calls). A live, healthy index MUST return at
// least one hit for a term drawn from its own contents; if it does not, the
// index is broken, not empty, and this check reports FAIL rather than
// letting an empty result read as "no matches" the way a caller normally
// would.
func doctorNegativeControl(g *DoctorGroup, db *sql.DB, dbPath, root string) {
	const name = "negative control (sentinel query)"

	rows, err := db.Query(`
		SELECT title FROM documents
		WHERE (status IS NULL OR status != 'deprecated') AND title != ''
		ORDER BY indexed_at DESC LIMIT 200
	`)
	if err != nil {
		g.add(name, StatusUnknown, fmt.Sprintf("could not sample documents: %v", err))
		return
	}
	defer rows.Close()

	sentinel := ""
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			continue
		}
		if w := longestWord(title); w != "" {
			sentinel = w
			break
		}
	}
	_ = rows.Close()

	if sentinel == "" {
		g.add(name, StatusUnknown, "could not derive a sentinel term from any indexed document title (empty or unqueryable table)")
		return
	}

	result, err := searchMemoryFTS(dbPath, root, sentinel, 5, "")
	if err != nil {
		g.add(name, StatusFail, fmt.Sprintf("BROKEN: sentinel %q derived from an indexed title, but searchMemoryFTS errored: %v", sentinel, err))
		return
	}
	count, _ := result["count"].(int)
	if count < 1 {
		g.add(name, StatusFail, fmt.Sprintf("BROKEN: sentinel %q was drawn from an indexed document title but the real search path (searchMemoryFTS) returned 0 hits — treat ALL empty search results from this index as untrustworthy until this is fixed", sentinel))
		return
	}
	g.add(name, StatusOK, fmt.Sprintf("sentinel %q (drawn from an indexed title) returned %d hit(s) through searchMemoryFTS", sentinel, count))
}

// longestWord returns the longest alphabetic word (>=6 chars) in s, lower-
// cased, as a distinctive-enough sentinel term. Empty if none qualifies.
func longestWord(s string) string {
	best := ""
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		if len(w) >= 6 && len(w) > len(best) {
			best = w
		}
	}
	return strings.ToLower(best)
}

// ---------------------------------------------------------------------------
// Group 4: Store liveness
// ---------------------------------------------------------------------------

func doctorStoreLiveness(report *DoctorReport, root string, opts DoctorOptions) {
	g := report.addGroup("store liveness")

	cogDir := filepath.Join(root, ".cog")
	if _, err := os.Stat(cogDir); err != nil {
		g.add("store discovery", StatusUnknown, fmt.Sprintf("%s: %v", cogDir, err))
		return
	}

	var dbFiles []string
	_ = filepath.WalkDir(cogDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".db") {
			dbFiles = append(dbFiles, p)
		}
		return nil
	})

	if len(dbFiles) == 0 {
		g.add("store discovery", StatusUnknown, "no *.db files found under "+cogDir)
		return
	}
	sort.Strings(dbFiles)
	g.add("store discovery", StatusOK, fmt.Sprintf("%d SQLite store(s) found", len(dbFiles)))

	for _, path := range dbFiles {
		doctorOneStore(g, path, opts.StaleDays)
	}
}

func doctorOneStore(g *DoctorGroup, path string, staleDays int) {
	rel := path
	info, statErr := os.Stat(path)
	if statErr != nil {
		g.add("store: "+rel, StatusUnknown, statErr.Error())
		return
	}
	age := time.Since(info.ModTime())

	db, err := sql.Open("sqlite3", path+"?mode=ro&_busy_timeout=3000")
	if err != nil {
		g.add("store: "+rel, StatusUnknown, fmt.Sprintf("open: %v", err))
		return
	}
	defer db.Close()

	rowCount, tableErr := sumRowCounts(db)

	detail := fmt.Sprintf("last-write age=%s (mtime=%s)", roundDays(age), info.ModTime().Format(time.RFC3339))
	if tableErr == nil {
		detail = fmt.Sprintf("%s, ~%d rows across user tables", detail, rowCount)
	} else {
		detail = fmt.Sprintf("%s, row count unavailable: %v", detail, tableErr)
	}

	switch {
	case age > time.Duration(staleDays)*24*time.Hour:
		g.add("store: "+rel, StatusWarn, "DEAD (stale beyond "+fmt.Sprintf("%d", staleDays)+"d): "+detail)
	case tableErr != nil:
		// Row count could not be established (corrupt file, permission
		// denied, etc.). The last-write age above is a genuine measurement
		// (os.Stat, no db open required), but liveness itself is unverified
		// -- report UNKNOWN rather than falling through to OK, per the
		// never-report-unverified-as-OK contract this command advertises.
		g.add("store: "+rel, StatusUnknown, detail)
	case rowCount == 0:
		g.add("store: "+rel, StatusWarn, "empty store: "+detail)
	default:
		g.add("store: "+rel, StatusOK, detail)
	}
}

// sumRowCounts sums row counts across every non-internal, non-FTS-shadow
// table in the database. FTS5 virtual tables spawn shadow tables
// (`<name>_data`, `_idx`, `_docsize`, `_config`) that would double-count
// against the logical row total, so they are skipped.
func sumRowCounts(db *sql.DB) (int64, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if strings.HasSuffix(name, "_data") || strings.HasSuffix(name, "_idx") ||
			strings.HasSuffix(name, "_docsize") || strings.HasSuffix(name, "_config") ||
			strings.HasSuffix(name, "_content") {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(tables) == 0 {
		return 0, fmt.Errorf("no user tables")
	}

	var total int64
	for _, t := range tables {
		var n int64
		// Table names come from sqlite_master, not user input, but are
		// still interpolated into SQL text (COUNT(*) queries can't be
		// parameterized on table name) — quote defensively.
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, strings.ReplaceAll(t, `"`, `""`))
		if err := db.QueryRow(q).Scan(&n); err != nil {
			continue
		}
		total += n
	}
	return total, nil
}
