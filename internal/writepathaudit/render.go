// render.go — deterministic markdown rendering of a Report.
//
// The markdown is a derived view, not a second source of truth: it is
// generated from the same Report the JSON golden file diffs against, so the
// two can never drift from each other by construction.
package writepathaudit

import (
	"fmt"
	"strings"
)

// RenderMarkdown produces the path-pattern -> writer function -> file:line
// -> subsystem inventory table, plus the honesty-margin section for
// unresolved (DYNAMIC) sites.
func RenderMarkdown(r *Report) string {
	var b strings.Builder

	b.WriteString("# Write-path inventory\n\n")
	b.WriteString(r.Note)
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Total sites: %d  (cog: %d, home: %d, elsewhere: %d, unanchored: %d, dynamic: %d)\n\n",
		r.Summary.Total, r.Summary.Cog, r.Summary.Home, r.Summary.Elsewhere, r.Summary.Unanchored, r.Summary.Dynamic)

	b.WriteString("**Scope**: direct filesystem write primitives (os.WriteFile/Create/OpenFile/MkdirAll/Rename/CreateTemp/MkdirTemp/Symlink/WriteString) plus sqlite (sql.Open). NOT counted here: a bare `(*os.File).Write`/`WriteString` method call, `os.Chmod`/`Mkdir`/`Remove`/`RemoveAll`, subprocess-mediated writes (see the appendix at the bottom of this document), and non-Go writers (shell/Python scripts this repo runs). See the package doc for the full rationale.\n\n")

	b.WriteString("| primitive | count |\n|---|---|\n")
	for _, p := range sortedPrimitiveNames(r.Summary.ByPrimitive) {
		fmt.Fprintf(&b, "| %s | %d |\n", p, r.Summary.ByPrimitive[p])
	}
	b.WriteString("\n")

	writeSection(&b, "Under .cog/", "cog", r)
	writeSection(&b, "Under the user home directory", "home", r)
	writeSection(&b, "Elsewhere (root positively resolved to a literal or known non-.cog anchor)", "elsewhere", r)

	b.WriteString("## UNANCHORED (the honesty margin, part 1: root not positively resolved)\n\n")
	b.WriteString("These sites structurally resolved (the shape of the path is known) but the ROOT — a struct field, a variable, or a call result — could not be traced to a concrete value. This is NOT the same as \"elsewhere\": it is not established that these write outside .cog/, only that this tool could not determine where they write. Never inferred as elsewhere; treat this section as the list a human still needs to eyeball for .cog/ membership.\n\n")
	writeRows(&b, "unanchored", r)

	b.WriteString("## DYNAMIC (the honesty margin, part 2: unresolved entirely)\n\n")
	b.WriteString("These sites write to disk but this tool could not structurally resolve their path expression at all — not even the shape is known. They are never dropped — treat this section as the list of write paths a human still needs to eyeball.\n\n")
	writeRows(&b, "dynamic", r)

	writeSubprocessAppendix(&b, r)

	return b.String()
}

// writeSubprocessAppendix renders the declared-out-of-scope subprocess
// writer enumeration. See the package doc's "Scope boundary" section and
// SubprocessSite's doc comment: these rows are listed for visibility only
// and are never counted toward Summary or any cog/home/elsewhere/
// unanchored/dynamic bin.
func writeSubprocessAppendix(b *strings.Builder, r *Report) {
	b.WriteString("## Subprocess writers (declared out of scope for v1 — uncounted)\n\n")
	b.WriteString("v1 counts direct filesystem primitives + sqlite only (see the package doc). A spawned process can write anywhere its own logic chooses, which this tool cannot see without executing it — these sites are enumerated for visibility, with cmd.Dir resolved where possible, but are NOT classified into any bin above and do not contribute to the totals at the top of this document. Non-Go writers (shell/Python scripts this repo runs) are not enumerable by a Go source scanner at all and are not listed here either — see the package doc.\n\n")
	b.WriteString(fmt.Sprintf("Total subprocess sites: %d\n\n", len(r.Subprocess)))
	b.WriteString("| cmd.Dir | call | file:line | subsystem |\n|---|---|---|---|\n")
	if len(r.Subprocess) == 0 {
		b.WriteString("| _(none found)_ | | | |\n")
	}
	for _, s := range r.Subprocess {
		dir := "_(not set — inherits the process's own working directory)_"
		if s.DirKnown {
			dir = "`" + mdEscape(s.Dir) + "`"
		}
		fmt.Fprintf(b, "| %s | `%s` | %s:%d | %s |\n",
			dir, mdEscape(s.Raw), s.File, s.Line, s.Subsystem)
	}
	b.WriteString("\n")
}

func writeSection(b *strings.Builder, title, category string, r *Report) {
	fmt.Fprintf(b, "## %s\n\n", title)
	writeRows(b, category, r)
}

func writeRows(b *strings.Builder, category string, r *Report) {
	b.WriteString("| pattern | writer func | file:line | subsystem |\n|---|---|---|---|\n")
	any := false
	for _, s := range r.Sites {
		if s.Category != category {
			continue
		}
		any = true
		fmt.Fprintf(b, "| `%s` | %s (`%s`) | %s:%d | %s |\n",
			mdEscape(s.Pattern), s.Func, s.Primitive, s.File, s.Line, s.Subsystem)
	}
	if !any {
		b.WriteString("| _(no site resolved to this anchor — see UNANCHORED/DYNAMIC)_ | | | |\n")
	}
	b.WriteString("\n")
}

func mdEscape(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func sortedPrimitiveNames(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	// Small, fixed set (the five os.* primitives) — plain insertion sort
	// keeps this package free of an extra sort.Strings import decision
	// tied to call-site count; deterministic either way.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
