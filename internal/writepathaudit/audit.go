// audit.go — static inventory of filesystem-write call sites across the repo.
//
// This exists because the write-path vocabulary in this codebase is
// fragmented: os.WriteFile/os.Create/os.OpenFile/os.MkdirAll/os.Rename are
// called directly from 12+ packages across module boundaries (go.work lists
// 12 separate modules that cannot import each other), on top of at least
// three independently-drifted "AppendEvent" ledger functions and six-plus
// independently-implemented atomic-write helpers with no shared canonical
// implementation. Nothing enumerates where the substrate actually touches
// disk. This package is that enumeration, kept honest by construction:
// every write site it cannot fully resolve is still reported, in its own
// DYNAMIC or UNANCHORED bucket, never silently dropped.
//
// # Why primitive-level scanning, not name-based matching
//
// A name-based scan for "AppendEvent" or "writeFileAtomic" call sites would
// have to disambiguate identically-named functions that do unrelated things
// — internal/engine/bus_session.go's (*BusSessionManager).AppendEvent is an
// in-memory bus operation that ALSO happens to open a real JSONL file a few
// lines into the same method body — while internal/engine/ledger.go's
// package-level AppendEvent (a JSONL ledger writer) and pkg/cogblock's
// independently-drifted redeclaration of the same name are two more,
// unrelated functions. Matching on the stdlib primitive instead of the
// wrapper name sidesteps the disambiguation problem entirely: a wrapper
// only shows up here if it (or something it calls) actually reaches a
// recognized write primitive, and it shows up at that primitive's exact
// file:line with the true enclosing function named — strictly better
// attribution than name matching, at the cost of not being able to say
// "wrapper X never writes" purely from this inventory.
//
// # Parsing strategy
//
// Source files are parsed individually with go/parser (no go/packages, no
// compilation, no type-checking) — the same strategy as the existing
// namespace-drift guard at internal/engine/namespace_sync_test.go, and for
// the same reason: this repo is a 12-module go.work workspace whose modules
// cannot import each other, so any tool that needs cross-module reach has
// to work at the source-text level rather than the package-graph level.
//
// # Path resolution — the heuristic, spelled out
//
// A path argument is resolved by structural recursion over a small,
// deliberately narrow set of expression shapes:
//
//   - string literals                                   -> the literal text
//   - filepath.Join(a, b, ...)                           -> resolve each arg, join with "/"
//   - string concatenation (a + b)                       -> resolve each side, concatenate; an empty-resolved left side glued to a right side that itself starts with "/" reports a DEGENERATE ROOT out-of-band (a separate bool alongside the pattern and ok — see resolveExpr's return shape and its BinaryExpr case) rather than folding a sentinel into the pattern text itself — the same over-claim filepath.Join(emptyBase, name) used to produce, and composable through further Join/Dir/Base wrapping without any marker text ever existing to leak
//   - filepath.Dir(x) / filepath.Base(x)                 -> "dirname(...)" / "basename(...)" wrapping the resolved inner expression
//   - os.UserHomeDir()                                   -> the <Home> anchor
//   - os.TempDir()                                       -> the <TempDir> anchor
//   - an identifier or selector field named (case-insensitively)
//     "workspaceroot", "cogdir", "homedir", or "userhomedir" -> the matching
//     anchor token, regardless of where it came from (parameter, field,
//     local variable)
//   - a local variable whose def is itself a matched write-primitive call
//     (e.g. `f, _ := os.CreateTemp(dir, pat)`) -> resolved through to THAT
//     call's own path argument, so a later os.WriteString(f, ...) or
//     io.Copy(f, ...) attributes back to where f actually lives
//   - a struct-field access on the enclosing method's own receiver, or a
//     bare identifier that is the enclosing (no-receiver) function's own
//     parameter -> chased through the CALLABLE-ORIGIN INDEX (below)
//   - any other identifier assigned exactly once in the same function body
//     (":=" or "=") -> resolved by recursing into that assignment's RHS
//   - any other identifier (an untraceable parameter with no chaseable
//     origin, a loop variable, a struct field with no defining assignment
//     anywhere in the repo) -> an opaque "{name}" placeholder segment — the
//     shape of the path is still known, just not the runtime value, so this
//     does NOT by itself mark the site unresolved (see classify)
//
// Anything else — an arbitrary function or method call other than the
// handful above (fmt.Sprintf included, deliberately: format-string
// substitution is not attempted), an index/slice/type-assertion expression,
// a map lookup — makes the whole sub-expression DYNAMIC (ok=false). This is
// a conservative choice on purpose: a wrong "resolved" verdict is worse
// than an honest "we don't know", because the former hides real write
// paths and the latter only asks a human to look once.
//
// # The callable-origin index (field-root and param-root resolution)
//
// A path argument is very often not built at the write call itself but
// received already-computed — as a struct field (bs.root, p.watchDir,
// l.path) or as a function/method parameter (persistNodeRootGrant(path,
// token string)). The ORIGINAL version of this tool treated every such
// name as an immediately-opaque placeholder and, worse, silently binned it
// into "elsewhere" as if "not under .cog/" had been positively established
// — which is the single defect this rewrite exists to fix (see classify).
//
// Two lightweight, source-text-only indexes close the common cases without
// needing real type information:
//
//   - fieldOrigin(type, field): scans every KEYED composite literal in the
//     repo (`&T{field: expr}` / `T{field: expr}`) for the named struct
//     field, resolves expr in the enclosing function's own scope (chasing
//     that function's own parameters transitively, below), and — ONLY when
//     every composite literal that sets the field agrees on the resulting
//     pattern — reports that pattern. Two literals disagreeing is treated
//     as a genuine dead end, not resolved by guessing at which one applies
//     at runtime.
//   - paramOrigin(dir, func, i): scans every bare-identifier call to a
//     given no-receiver function IN THE SAME DIRECTORY (a source-text proxy
//     for "same package", since this tool carries no import graph) and
//     resolves the i'th call argument in each caller's own scope. Again,
//     only an unambiguous, unanimous answer across all call sites is used.
//
// Both indexes recurse into each other through the same resolver (a field
// origin can itself dead-end into a parameter that needs its own call-site
// chase, and vice versa), bounded by the same max-depth guard as everything
// else. A direct (non-receiver) function call whose body has a single
// resolvable-and-agreeing return value (see the CertDir()-shaped case,
// which returns two DIFFERENT literal roots on two branches that both
// happen to land under .cog/) is resolved the same way, one call at a time,
// using that call's own arguments — never aggregated across call sites,
// so a function called from two genuinely different places is not
// conflated.
//
// Only KEYED composite-literal fields and bare (same-file, same-package)
// function/method calls are indexed. A plain `x.field = expr` assignment
// statement, a positional (unkeyed) composite literal, or a call reached
// only through a package-qualified selector from a DIFFERENT module is not
// chased — declared, not silently implied away: those sites still surface
// wherever their own os/io primitive is, just without the extra hop, so
// they fall to the opaque placeholder and land in UNANCHORED rather than
// being lost.
//
// Two further known, deliberate blind spots, both because closing them
// needs real type information (a go/packages load) this tool intentionally
// does not carry:
//
//   - package-level constants and vars are not folded (a path built from a
//     `const cogDirName = ".cog"` will not be recognized as the <CogDir>
//     anchor unless the literal ".cog" appears directly in the call)
//   - the local-variable resolver is flat per function body: it does not
//     model branches, shadowing, or loop iterations, and if a name is
//     assigned more than once it heuristically takes the last textual
//     assignment. Sites resolved this way still carry their real
//     file:line, so this affects the recorded *pattern*, never the
//     recorded *location*.
//
// # Primitive coverage
//
// os.WriteFile, os.Create, write-flagged os.OpenFile, os.MkdirAll,
// os.Rename (destination arg), os.CreateTemp, os.MkdirTemp, os.Symlink
// (newname arg), and os.WriteString are all recognized unconditionally.
// sql.Open is recognized ONLY when the driver-name literal contains
// "sqlite" (the DSN — arg 1 — is the write target; this is how
// .cog/.state/constellation.db surfaces even though sqlite writes through
// CGO, invisible to a primitive scanner otherwise). io.Copy is recognized
// ONLY when its destination argument is, in the same function, visibly a
// local variable bound to one of the primitives above (`f, _ :=
// os.CreateTemp(...); io.Copy(f, r)`) — an unconditional io.Copy match
// would flag countless non-filesystem io.Writer targets (buffers,
// http.ResponseWriter, network conns) as false positives, which is exactly
// the over-claiming failure mode this tool exists to avoid. A bare
// (*os.File).Write/WriteString METHOD call (as opposed to the package-level
// os.WriteString function) is not matched at all: without type information,
// matching any method literally named Write/WriteString on any receiver
// would have the same false-positive problem as unconditional io.Copy, only
// worse (that method name is common on non-file types) — a declared scope
// boundary, not an oversight. os.Chmod/os.Mkdir/os.Remove/os.RemoveAll are
// likewise out of scope; they mutate or delete rather than write content,
// which is a different question the RFC's own §6.4 lint does not ask here.
//
// # Scope boundary: v1 is filesystem primitives + sqlite, full stop
//
// v1 counts direct filesystem write primitives (the list above) plus
// sqlite (via sql.Open, see above). It does NOT count, classify, or
// silently cap:
//
//   - a bare (*os.File).Write/WriteString METHOD call, or os.Chmod/
//     os.Mkdir/os.Remove/os.RemoveAll (declared immediately above);
//   - SUBPROCESS-mediated writes — exec.Command/exec.CommandContext call
//     sites are enumerated, with their cmd.Dir resolved where this tool's
//     existing machinery can reach it, in a SEPARATE, explicitly-labeled
//     "Subprocess writers (declared out of scope for v1 — uncounted)"
//     appendix (see scanSubprocessSites) that contributes NOTHING to
//     Summary or the cog/home/elsewhere/unanchored/dynamic bins — a
//     spawned process can write anywhere its own logic chooses, which
//     this tool cannot see without executing it, and folding a best-effort
//     guess into a classified bin would itself be an over-claim;
//   - non-Go writers entirely — a shell script or Python script this repo
//     runs (e.g. scripts/e2e-integration.sh, scripts/overnight-ablation.py)
//     is not Go source at all, so go/parser cannot see inside it, and no
//     attempt is made to.
//
// Naming this boundary here, in the appendix heading, AND in the rendered
// markdown (see render.go) is deliberate: an unstated boundary reads as
// "nothing more to see" rather than "not measured", which is the
// over-claim by omission this tool exists to refuse everywhere else.
package writepathaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// maxChaseDepth bounds BOTH the structural recursion within one expression
// AND the interprocedural chase through the callable-origin index (field
// origins and parameter origins can recurse into each other). It is
// deliberately generous — a handful of real multi-hop chains in this repo
// (struct field -> constructor parameter -> constructor's own local
// variable -> that function's own parameter -> ITS call site) run five or
// six levels deep — while still being a hard ceiling against runaway or
// mutually-recursive call graphs.
const maxChaseDepth = 16

// Site is one filesystem-write call site found in the repo.
type Site struct {
	File      string `json:"file"` // repo-relative, forward-slash normalized
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Primitive string `json:"primitive"`
	Func      string `json:"func"` // enclosing function/method/closure label
	Pattern   string `json:"pattern"`
	Category  string `json:"category"` // "cog" | "home" | "elsewhere" | "unanchored" | "dynamic"
	Resolved  bool   `json:"resolved"`
	Subsystem string `json:"subsystem"`
	Raw       string `json:"raw"`
}

// Summary is the deterministic rollup over Sites.
type Summary struct {
	Total       int            `json:"total"`
	Cog         int            `json:"cog"`
	Home        int            `json:"home"`
	Elsewhere   int            `json:"elsewhere"`
	Unanchored  int            `json:"unanchored"`
	Dynamic     int            `json:"dynamic"`
	ByPrimitive map[string]int `json:"by_primitive"`
}

// Report is the full, deterministic output of a scan.
type Report struct {
	Note       string           `json:"note"`
	Sites      []Site           `json:"sites"`
	Summary    Summary          `json:"summary"`
	Subprocess []SubprocessSite `json:"subprocess_sites"`
}

// SubprocessSite is one non-test exec.Command/exec.CommandContext call
// site. These are DECLARED OUT OF SCOPE for the categorized v1 inventory
// (see the package doc's "Primitive coverage" section) — a spawned process
// can write anywhere its own logic chooses, which this tool cannot see
// without executing it, and folding a best-effort guess into cog/home/
// elsewhere/unanchored/dynamic would be exactly the kind of over-claim
// this tool exists to refuse. They are still enumerated — never silently
// dropped — with cmd.Dir resolved by the SAME best-effort machinery as
// every other path in this package, where the call site sets it, so the
// scope boundary is visible in the artifact rather than only in a doc
// comment. None of these counts toward Summary.
type SubprocessSite struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Func      string `json:"func"`
	Subsystem string `json:"subsystem"`
	Raw       string `json:"raw"`       // the exec.Command(...)/exec.CommandContext(...) call, rendered as source
	Dir       string `json:"dir"`       // resolved cmd.Dir pattern; "" when DirKnown is false
	DirKnown  bool   `json:"dir_known"` // whether a `<cmdVar>.Dir = ...` assignment was found for this call
}

const reportNote = "generated by internal/writepathaudit; see scan_test.go for how to regenerate the golden file"

// identifierAnchors maps a lower-cased identifier or selector-field name to
// the symbolic anchor token it represents. Deliberately narrow: broader
// names like "root" or "dir" are common enough elsewhere in the codebase
// that anchoring on them would produce confidently wrong categorizations,
// which is worse than the honest "{name}" placeholder / chase-then-opaque
// fallback.
var identifierAnchors = map[string]string{
	"workspaceroot": "<WorkspaceRoot>",
	"cogdir":        "<CogDir>",
	"homedir":       "<Home>",
	"userhomedir":   "<Home>",
}

// writeFlagNames are os.O_* flags that indicate a write-capable open. An
// os.OpenFile call whose flags expression contains none of these (e.g. a
// bare os.O_RDONLY) is a read, not a write, and is not reported.
var writeFlagNames = map[string]bool{
	"O_WRONLY": true,
	"O_RDWR":   true,
	"O_CREATE": true,
	"O_APPEND": true,
	"O_TRUNC":  true,
}

// simplePrimitive is one unconditionally-recognized os.* call shape.
type simplePrimitive struct {
	name   string
	argIdx int
}

var simplePrimitives = map[[2]string]simplePrimitive{
	{"os", "WriteFile"}:   {"os.WriteFile", 0},
	{"os", "Create"}:      {"os.Create", 0},
	{"os", "MkdirAll"}:    {"os.MkdirAll", 0},
	{"os", "Rename"}:      {"os.Rename", 1}, // destination; the source is read, not written
	{"os", "CreateTemp"}:  {"os.CreateTemp", 0},
	{"os", "MkdirTemp"}:   {"os.MkdirTemp", 0},
	{"os", "Symlink"}:     {"os.Symlink", 1}, // newname; oldname (arg 0) is read, not written
	{"os", "WriteString"}: {"os.WriteString", 0},
}

// ─── Scan ─────────────────────────────────────────────────────────────────

// parsedFile is one non-test .go file, parsed once and reused across both
// the indexing pass and the site-scanning pass.
type parsedFile struct {
	relPath string
	dir     string // repo-relative directory, forward-slash normalized: the source-text proxy for "package"
	fset    *token.FileSet
	file    *ast.File
	scopes  []funcScope
}

// Scan walks every non-test .go file under repoRoot (across all go.work
// module boundaries — this is a plain filesystem walk, not a build) and
// returns a deterministically-ordered Report.
func Scan(repoRoot string) (*Report, error) {
	var parsed []parsedFile

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		fset := token.NewFileSet()
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}

		dir := filepath.ToSlash(filepath.Dir(rel))
		parsed = append(parsed, parsedFile{
			relPath: rel,
			dir:     dir,
			fset:    fset,
			file:    file,
			scopes:  collectFuncScopes(file, fset),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Deterministic order before indexing: file-content-derived ambiguity
	// checks (multiple composite literals / call sites disagreeing) must
	// not depend on filesystem walk order.
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].relPath < parsed[j].relPath })

	idx := buildGlobalIndex(parsed)

	var sites []Site
	for _, pf := range parsed {
		sites = append(sites, scanParsedFile(pf, idx)...)
	}
	sites = append(sites, appendEventBucketSites(parsed, idx)...)

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		if sites[i].Line != sites[j].Line {
			return sites[i].Line < sites[j].Line
		}
		return sites[i].Column < sites[j].Column
	})

	subprocess := scanSubprocessSites(parsed, idx)

	return &Report{
		Note:       reportNote,
		Sites:      sites,
		Summary:    summarize(sites),
		Subprocess: subprocess,
	}, nil
}

func summarize(sites []Site) Summary {
	s := Summary{ByPrimitive: map[string]int{}}
	for _, site := range sites {
		s.Total++
		switch site.Category {
		case "cog":
			s.Cog++
		case "home":
			s.Home++
		case "elsewhere":
			s.Elsewhere++
		case "unanchored":
			s.Unanchored++
		case "dynamic":
			s.Dynamic++
		}
		s.ByPrimitive[site.Primitive]++
	}
	return s
}

// ─── Function/method/closure scope tracking ────────────────────────────────

// funcScope is one function-like AST scope: a top-level function, a method,
// or a closure literal, with just enough information to (a) label a Site's
// Func column and (b) build a *resolver context for chasing that scope's
// own parameters or receiver fields.
type funcScope struct {
	start, end token.Pos
	label      string
	kind       string // "func" | "method" | "closure"
	name       string
	recvName   string
	recvType   string
	params     []string
	body       *ast.BlockStmt
}

func collectFuncScopes(file *ast.File, fset *token.FileSet) []funcScope {
	var scopes []funcScope
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body == nil {
				return true
			}
			sc := funcScope{start: fn.Pos(), end: fn.End(), body: fn.Body, name: fn.Name.Name}
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recvField := fn.Recv.List[0]
				recvType := strings.TrimPrefix(exprString(recvField.Type), "*")
				var recvName string
				if len(recvField.Names) > 0 {
					recvName = recvField.Names[0].Name
				}
				sc.kind = "method"
				sc.recvName = recvName
				sc.recvType = recvType
				sc.label = fmt.Sprintf("(%s).%s", exprString(recvField.Type), fn.Name.Name)
			} else {
				sc.kind = "func"
				sc.params = paramNames(fn.Type.Params)
				sc.label = fn.Name.Name
			}
			scopes = append(scopes, sc)
		case *ast.FuncLit:
			scopes = append(scopes, funcScope{
				start: fn.Pos(),
				end:   fn.End(),
				body:  fn.Body,
				kind:  "closure",
				label: fmt.Sprintf("func literal (line %d)", fset.Position(fn.Pos()).Line),
			})
		}
		return true
	})
	return scopes
}

func enclosingFuncScope(scopes []funcScope, pos token.Pos) funcScope {
	best := funcScope{label: "package scope"}
	bestSpan := token.Pos(-1)
	for _, sc := range scopes {
		if pos >= sc.start && pos < sc.end {
			span := sc.end - sc.start
			if bestSpan == -1 || span < bestSpan {
				best = sc
				bestSpan = span
			}
		}
	}
	return best
}

func paramNames(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var names []string
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			names = append(names, "_") // unnamed param; keep positional alignment
			continue
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// localDefsFor is computed lazily per (function scope, position) and cached
// by the caller, since most files have far more scopes than matched call
// sites. pos matters because every scope-introducing construct on the path
// down to pos — FOR/RANGE loop bodies, but ALSO if/else bodies, switch/
// type-switch case bodies, and select comm-clause bodies — gets its own
// defs layered back on top of the whole-function flat map (see scopeChain
// and collectLocalDefs) — the cache key is the innermost such body actually
// reached from pos, or fnBody itself when pos is not inside any of them, so
// two call sites inside the SAME loop iteration or the SAME conditional
// branch correctly share one map while two call sites in DIFFERENT
// (sibling, or unrelated) loops or branches never do.
//
// The conditional half of this (added alongside loops) is what closes two
// round-5-gate defects the flat map's own hadOuter existence-test could not:
// two SIBLING branches (an if/else-if pair, or two switch cases) declaring
// the same name via `:=` were flattening onto whichever branch's `:=` the
// AST walk happened to visit FIRST — see applyAssign's own doc comment —
// which not only mis-resolved a read outside both branches but, worse,
// mis-resolved the SECOND branch's own write, inside its own body, onto the
// first branch's value; and a `:=` that genuinely shadows an OUTER name
// protected the outer read correctly (see poisonConditionalAndLoopRebinds)
// but, as a side effect of never being recorded at all, left the shadow's
// OWN value invisible to a read of the SAME name INSIDE the very branch
// that declared it. Layering each conditional body's own direct defs back
// on top, scoped only to positions actually inside that body, fixes both:
// a use site inside a given branch now always sees THAT branch's own
// bindings (its own `:=`, `=`, and `var` declarations, applied with
// conditional depth restarting at zero for that body's own top-level
// statements — see applyDirectLocalDefs), regardless of what a sibling
// branch or the outer scope recorded for the same name. A use site outside
// every conditional is completely unaffected: scopeChain returns an empty
// chain for it, so it resolves purely from the base flat map exactly as
// before this fix (poisonConditionalAndLoopRebinds's existing rules still
// govern what that map holds for such a read).
func localDefsFor(cache map[*ast.BlockStmt]map[string]ast.Expr, fnBody *ast.BlockStmt, pos token.Pos) map[string]ast.Expr {
	if fnBody == nil {
		return map[string]ast.Expr{}
	}
	chain := scopeChain(fnBody, pos)
	key := fnBody
	if len(chain) > 0 {
		key = chain[len(chain)-1]
	}
	if defs, ok := cache[key]; ok {
		return defs
	}
	defs := collectLocalDefs(fnBody)
	for _, scopeBody := range chain {
		applyDirectLocalDefs(defs, scopeBody, true)
	}
	cache[key] = defs
	return defs
}

// collectLocalDefs builds a flat, last-assignment-wins map of local variable
// name -> defining expression for a single function body. See the package
// doc comment for the deliberate limitations (no branch sensitivity beyond
// the compound-assignment handling below, no shadowing).
//
// FOR- and RANGE-loop BODIES are deliberately excluded from this walk (see
// applyDirectLocalDefs) — each loop body's own local defs are layered on
// top of this map separately, only while resolving a position that
// actually falls inside that specific loop (localDefsFor). Without this
// exclusion, two textually separate loops in the same function that happen
// to declare a same-named loop-local variable (e.g. two
// `for _, x := range ...` blocks each declaring their own `target := ...`)
// flatten into a single map, and the SECOND loop's definition silently
// clobbers the first's for every call site — including ones inside the
// first loop (round-2 gate finding: internal/engine/init.go:68's `target`
// resolved using the SECOND loop's `f.target`, not its own loop's `dir`).
func collectLocalDefs(body *ast.BlockStmt) map[string]ast.Expr {
	defs := map[string]ast.Expr{}
	applyDirectLocalDefs(defs, body, true)
	poisonConditionalAndLoopRebinds(defs, body)
	return defs
}

// poisonConditionalAndLoopRebinds deletes from defs every name that a
// FOR/RANGE loop OR a conditionally-executed subtree (IfStmt — body and
// else chain, SwitchStmt/TypeSwitchStmt case bodies, SelectStmt comm
// clauses) rebinds in a way a use site OUTSIDE that subtree cannot safely
// trust. Neither `:=` nor a `var x = expr` ValueSpec ever poisons — when
// either shadows an existing outer name, it never touches that outer
// binding in the first place (see applyDirectLocalDefs/applyAssign's
// condDepth handling, mirrored identically for ValueSpec), so there is
// nothing here to poison; when either introduces a brand-new name with no
// outer counterpart, it is not a rebind of anything outer at all, so
// poisoning has no name to act on either way. (A use site strictly INSIDE
// the subtree that declared such a shadow sees the shadow's own value
// regardless — see localDefsFor/scopeChain, a separate, position-scoped
// overlay that runs after this poisoning pass and is unaffected by it.)
//
// LOOPS: applyDirectLocalDefs never applies a loop body's assignments to
// the outer map at all (skipLoopBodies=true skips the whole construct,
// Init/Cond/Post/Key/Value included) — poisoning here strips a
// PRE-EXISTING outer binding that the (unseen) loop rebind could
// invalidate, plain `=` or compound (`+=`, ...) alike, because in the
// loop case NEITHER kind of rebind was ever folded onto that outer binding
// to begin with — there is no established anchor being discarded, so
// poisoning both is free. A use site OUTSIDE the loop cannot know whether
// the rebind fired at all (it depends on iteration count and branch
// conditions the resolver does not model) — round-3 gate finding:
// internal/engine/daemon_detach_unix.go's logPath (TempDir outer def,
// workspace/.cog rebind inside a range over args) stamped "elsewhere" on
// the outer branch alone. Use sites INSIDE the loop are unaffected:
// localDefsFor layers the loop body's own assignments back on top.
// FuncLit bodies are deliberately NOT excluded: a plain `=` inside a
// closure rebinds the captured outer variable, and over-poisoning fails
// toward unanchored, the honest side.
//
// CONDITIONALS are a deliberately NARROWER poison than loops, for two
// independent reasons layered on top of each other.
//
// First (unchanged from before this fix): a conditional's direct
// assignments ARE folded into the outer map by the ordinary
// applyDirectLocalDefs walk — it does not skip if/switch/select, unlike a
// loop body — see that function's own doc comment: `memPath += ".md"`
// inside a nested if-block must still resolve to the enclosing
// filepath.Join plus the suffix, at any point in the function, not just
// inside that one if-block (sdk/cogos.go's setMemory, a real .cog/mem/
// writer, pinned down by a named regression test). A COMPOUND assign's
// synthetic `BinaryExpr{X: prev, Op: ADD, Y: rhs}` (applyAssign) is
// PROVABLY still anchored on the same prior binding no matter which
// branch executes — every branch's fold shares the identical root, only
// the literal suffix differs — so its classification is guaranteed
// regardless of path, and this function never poisons a compound assign
// found inside a conditional (poisonLoop below is the only place a
// compound assign gets deleted, and only because the loop case never
// established a prior fold to protect in the first place — see the loop
// section of this comment).
//
// Second (the round-4 gate fix, broadened at round 5 — see staticPathText):
// a PLAIN `=` rebind is poisoned only when this function can PROVE, using
// nothing but syntax already sitting in this one function's own defs map —
// no *globalIndex, no interprocedural call chase, no paramOrigin/
// fieldOrigin — that the rebind assigns a root DIFFERENT from .cog/.
// staticPathText (round 4's literalPathText, widened) recognizes: a
// self-contained string literal or `+`-chain of them (the original round-4
// shape); a filepath.Join/Dir/Base/Clean call composed entirely of
// arguments THIS function can itself resolve; os.UserHomeDir()/
// os.TempDir(); the same anchor names identifierAnchors knows; and a plain
// Ident chased through defs (recursively, bounded — see
// maxPoisonChaseDepth) when this SAME function already has a resolvable
// binding for it. Two real, currently-shipping .cog/ writers pin down why
// the check has to stay this narrow rather than "any plain `=` inside a
// conditional poisons, full stop":
//
//   - internal/engine/log_capture.go's `path := cfg.KernelLogPath; if
//     path == "" { path = DefaultKernelLogPath(cfg.WorkspaceRoot) }` — the
//     OUTER value is an opaque, unchased struct field; the CONDITIONAL
//     rebind is the one call that actually resolves to
//     <WorkspaceRoot>/.cog/run/kernel.log.jsonl. Poisoning this (as an
//     earlier, unconditional version of this fix did) throws away the
//     ONLY known-good classification for a genuine .cog/run/ writer,
//     purely because the rebind sits inside an `if`. staticPathText still
//     cannot evaluate DefaultKernelLogPath(...) — it is a call to another
//     declared function, outside the narrow filepath/os allowlist above —
//     so this case is completely unaffected by the round-5 widening.
//   - internal/engine/memory_sections.go's resolveMemoryDocPath: every
//     branch of its switch (and the if/else nested in its default case)
//     assigns `candidate` via a filepath.Join call whose own arguments
//     (cogRoot, path — both function PARAMETERS, which staticPathText
//     deliberately never chases; see its own doc comment) are themselves
//     unresolvable this way, so every branch still reads as "cannot prove"
//     and none of them get poisoned — same outcome as round 4, for a
//     structurally different reason (an unresolvable argument, not an
//     unrecognized call shape).
//
// The round-4 gate's own reproduction —
// `path := filepath.Join(workspaceRoot, ".cog", "settings.json"); if
// !useCog { path = "/etc/cogos/global-settings.json" }` — was already
// caught by the literal-only check. The round-5 gate's finding is that a
// ONE-STEP paraphrase of the identical shape — `path = filepath.Join("/etc",
// "cogos", "g.json")`, or the same idiom threading through one already-known
// local (`root = filepath.Join(home, "workspaces")` where `home` was itself
// bound to `os.UserHomeDir()` earlier in the SAME function) — was NOT
// caught, purely because the rebind happened to be wrapped in a
// filepath.Join call instead of spelled as one bare literal. This was not a
// hypothetical: internal/testkernel/experiment/manifest.go's RunsRoot is a
// real, shipping instance of exactly the second shape (`root :=
// os.Getenv("COGOS_WORKSPACE_ROOT"); if root == "" { home, _ :=
// os.UserHomeDir(); root = filepath.Join(home, "workspaces") }`, read after
// the `if` closes) — the env-var-unset branch's Join was the ONLY thing
// keeping RunsRoot's two call sites classified "home" instead of the honest
// "unanchored" a genuinely branch-dependent, partly-dynamic (os.Getenv)
// root deserves. staticPathText closes exactly this gap by evaluating the
// same narrow filepath/os shapes the real resolver already knows, without
// needing the real resolver's interprocedural machinery to do it — see
// TestManifestGoRunsRootEnvBranch_NoLongerOverclaimsHome in scan_test.go
// for the pinned-down regression. This is the resolver's own invariant,
// applied as far as pre-resolution syntax alone can prove it: a write site
// must never classify from a binding not guaranteed on its execution path —
// when a branch's own fold GUARANTEES the anchor (compound assign) or this
// function cannot yet tell whether a rebind disagrees (an unresolved call,
// field, or parameter), the existing binding stands; only a rebind this
// function can already read — now via staticPathText's wider, still
// self-contained reach — as a DIFFERENT, non-.cog root forces the site to
// the unanchored margin.
//
// staticPathText's reach is still bounded on purpose, and a further,
// currently-open gap follows the identical shape: a rebind this function
// CAN read as .cog-ROOTED is never poisoned either way (see hasCogSegment
// below) — favoring a false "cog" over a false "elsewhere", the same
// over-claim-toward-.cog-is-the-safe-direction posture classify()'s own doc
// comment states for every other ambiguous case in this package. This is a
// deliberate asymmetry, not an oversight left beside this one: a branch
// rebind to a .cog-bearing value, resolvable or not, is trusted; a branch
// rebind this function can positively read as non-.cog is poisoned. Making
// the two directions symmetric (poisoning a .cog-bearing rebind too, the
// instant it sits beside an unguaranteed sibling) would trade real .cog/
// writers currently caught by exactly this one-resolvable-branch heuristic
// (log_capture.go above is one) for a more theoretically consistent, but
// practically worse, failure mode — so it is left as is here, named
// explicitly rather than silently implied, with the asymmetry pinned down
// by a dedicated regression test (TestConditionalCogBearingRebindStaysTrusted
// in resolve_test.go) so a future change to this balance is a conscious
// decision, not an accidental one.
func poisonConditionalAndLoopRebinds(defs map[string]ast.Expr, body *ast.BlockStmt) {
	if body == nil {
		return
	}
	// poisonLoop deletes defs[name] for EVERY name a loop-body rebind
	// touches, plain `=` or compound alike — see this function's doc
	// comment's LOOPS section for why loops get no plain-vs-compound
	// distinction (applyDirectLocalDefs never folds a loop body's
	// assignments into the outer map at all, so there is no established
	// anchor here to protect — only a PRE-EXISTING outer binding, from
	// before the loop, that the loop's own invisible-to-this-map rebind
	// could invalidate).
	poisonLoop := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(m ast.Node) bool {
			assign, ok := m.(*ast.AssignStmt)
			if !ok || assign.Tok == token.DEFINE {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" {
					delete(defs, ident.Name)
				}
			}
			return true
		})
	}
	// poisonConditional deletes defs[name] for a plain `=` rebind found
	// inside a conditionally-executed subtree, but ONLY when the rebind's
	// own RHS is a staticPathText the auditor can read WITHOUT a resolver
	// (using only THIS function's own already-folded defs, passed in below)
	// and that text does not carry a ".cog" segment — see this function's
	// doc comment for the real .cog/ writers (a struct field, and a
	// filepath.Join over unchaseable parameters) that a broader,
	// unconditional version of this check would silently break, and for
	// the deliberate cog-favoring asymmetry in the hasCogSegment check
	// itself.
	poisonConditional := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(m ast.Node) bool {
			assign, ok := m.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			for i, lhs := range assign.Lhs {
				ident, isIdent := lhs.(*ast.Ident)
				if !isIdent || ident.Name == "_" || i >= len(assign.Rhs) {
					continue
				}
				text, litOK := staticPathText(assign.Rhs[i], defs, 0)
				if !litOK || hasCogSegment(text) {
					continue
				}
				delete(defs, ident.Name)
			}
			return true
		})
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.ForStmt:
			poisonLoop(s.Init)
			poisonLoop(s.Post)
			poisonLoop(s.Body)
		case *ast.RangeStmt:
			if s.Tok == token.ASSIGN {
				for _, e := range []ast.Expr{s.Key, s.Value} {
					if ident, ok := e.(*ast.Ident); ok && ident.Name != "_" {
						delete(defs, ident.Name)
					}
				}
			}
			poisonLoop(s.Body)
		case *ast.IfStmt:
			// s.Else is either a further *ast.IfStmt (an else-if link,
			// which ast.Inspect's own continued traversal — we return true
			// below — will re-visit as its own IfStmt case, poisoning its
			// Body/Else in turn) or a final *ast.BlockStmt, poisoned
			// directly here since it is not itself an IfStmt node.
			poisonConditional(s.Body)
			if blk, ok := s.Else.(*ast.BlockStmt); ok {
				poisonConditional(blk)
			}
		case *ast.SwitchStmt:
			poisonConditional(s.Body)
		case *ast.TypeSwitchStmt:
			poisonConditional(s.Body)
		case *ast.SelectStmt:
			poisonConditional(s.Body)
		}
		return true
	})
}

// maxPoisonChaseDepth bounds staticPathText's own recursion — a separate,
// smaller ceiling than maxChaseDepth because this evaluator never leaves
// the current function body (no *globalIndex, no paramOrigin/fieldOrigin,
// no callChase), so a handful of levels covers every shape it needs to
// reach: a `+`-chain, a filepath.Join/Dir/Base/Clean wrapping one, and an
// Ident chased through this same function's own defs.
const maxPoisonChaseDepth = 8

// staticPathText evaluates expr as a self-contained path pattern using
// nothing but syntax and the SAME FUNCTION's own already-folded defs — no
// *globalIndex, no interprocedural call chase, no anchor beyond the same
// four identifierAnchors names the real resolver knows. This exists so
// poisonConditionalAndLoopRebinds can tell a conditional rebind's root
// apart from an outer binding's for more than a bare string literal (round
// 4's literalPathText, which this supersedes): a filepath.Join/Dir/Base/
// Clean call composed entirely of arguments THIS function can itself
// resolve — including a plain Ident naming an earlier local this same
// function already defines — is exactly as provably a root as a bare
// literal is, and treating it as unprovable (round 4's version did) is what
// let internal/testkernel/experiment/manifest.go's RunsRoot ship a false
// "home" classification for a root that is genuinely branch-dependent (see
// poisonConditionalAndLoopRebinds's own doc comment for the full story).
//
// Anything this function can't read this way — a call to another declared
// function, a struct field, an unchaseable parameter, os.Getenv and any
// other stdlib call outside the narrow filepath/os allowlist below —
// returns ok=false, which poisonConditionalAndLoopRebinds treats as
// "cannot prove a conflict", not as "no conflict": see that function's doc
// comment for why guessing either way here would be wrong. This is a
// STRICT SUBSET of what the real resolver (resolveExpr/resolveCall) can
// do — deliberately: it runs during collectLocalDefs, before any
// *globalIndex exists to chase a parameter or field through, so it only
// ever needs to prove a rebind's root using information already sitting in
// this one function's own defs map, never information that lives outside
// it.
func staticPathText(expr ast.Expr, defs map[string]ast.Expr, depth int) (string, bool) {
	if depth > maxPoisonChaseDepth {
		return "", false
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return v, true

	case *ast.ParenExpr:
		return staticPathText(e.X, defs, depth+1)

	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := staticPathText(e.X, defs, depth+1)
		if !ok {
			return "", false
		}
		right, ok := staticPathText(e.Y, defs, depth+1)
		if !ok {
			return "", false
		}
		return left + right, true

	case *ast.Ident:
		if anchor, isAnchor := identifierAnchors[strings.ToLower(e.Name)]; isAnchor {
			return anchor, true
		}
		// Only a name THIS function already has a folded def for — never a
		// parameter, a struct field, or anything requiring the real
		// resolver's interprocedural reach (paramOrigin/fieldOrigin), which
		// is not available at this stage. See this function's own doc
		// comment: this is precisely the line between the manifest.go gap
		// this closes (an Ident bound to os.UserHomeDir() earlier in the
		// SAME function) and the memory_sections.go/log_capture.go cases
		// that must stay unresolvable (an Ident that is itself a function
		// parameter).
		if def, isLocal := defs[e.Name]; isLocal {
			return staticPathText(def, defs, depth+1)
		}
		return "", false

	case *ast.CallExpr:
		sel, isSel := e.Fun.(*ast.SelectorExpr)
		if !isSel {
			return "", false
		}
		pkgIdent, isPkg := sel.X.(*ast.Ident)
		if !isPkg {
			return "", false
		}
		switch {
		case pkgIdent.Name == "filepath" && sel.Sel.Name == "Join":
			// Same empty-component-skipping behavior as the real resolver's
			// filepath.Join handling (resolveCall) — real filepath.Join
			// does the same. Unlike resolveCall, ANY unresolvable argument
			// fails the WHOLE call rather than degrading to a partial,
			// opaque-segment pattern: this evaluator has no downstream
			// classify() pass to hand an opaque segment to, so an
			// unresolvable argument must mean "cannot prove", full stop.
			parts := make([]string, 0, len(e.Args))
			for _, arg := range e.Args {
				p, ok := staticPathText(arg, defs, depth+1)
				if !ok {
					return "", false
				}
				if p != "" {
					parts = append(parts, p)
				}
			}
			return strings.Join(parts, "/"), true

		case pkgIdent.Name == "filepath" && sel.Sel.Name == "Dir":
			if len(e.Args) != 1 {
				return "", false
			}
			inner, ok := staticPathText(e.Args[0], defs, depth+1)
			if !ok {
				return "", false
			}
			return "dirname(" + inner + ")", true

		case pkgIdent.Name == "filepath" && sel.Sel.Name == "Base":
			if len(e.Args) != 1 {
				return "", false
			}
			inner, ok := staticPathText(e.Args[0], defs, depth+1)
			if !ok {
				return "", false
			}
			return "basename(" + inner + ")", true

		case pkgIdent.Name == "filepath" && sel.Sel.Name == "Clean":
			if len(e.Args) != 1 {
				return "", false
			}
			return staticPathText(e.Args[0], defs, depth+1)

		case pkgIdent.Name == "os" && sel.Sel.Name == "UserHomeDir":
			return "<Home>", true

		case pkgIdent.Name == "os" && sel.Sel.Name == "TempDir":
			return "<TempDir>", true
		}
		return "", false
	}
	return "", false
}

// applyDirectLocalDefs folds body's assignments/declarations into defs IN
// PLACE. Folding in place (rather than building a fresh map per block and
// merging afterward) is what lets a compound assignment see and combine
// with a binding from an ENCLOSING scope: memPath := filepath.Join(...)
// followed by memPath += ".md" inside a nested if-block must still resolve
// to the join plus the suffix at any point in the function where memPath
// is later read, not just inside that one if-block.
//
// When skipLoopBodies is true, a ForStmt/RangeStmt's own Body (and its
// Init/Cond/Post/Key/Value) is not descended into at all — see
// collectLocalDefs's doc comment for why.
//
// A `:=` DECLARATION found while this walk is inside a conditionally-
// executed subtree (IfStmt, SwitchStmt/TypeSwitchStmt, SelectStmt — a
// depth counter, condDepth, tracks this using ast.Inspect's own paired
// nil-on-exit callback) is recorded UNLESS it would SHADOW an existing
// outer binding of the same name (applyAssign checks defs for a prior
// entry before applying): unlike a plain `=` or a compound assign, a
// shadowing `:=` declares a NEW variable scoped to that subtree, and this
// map has no block-scoping of its own to keep the shadow from permanently
// clobbering the outer binding otherwise (the `:=`-shadow-control
// regression test pins this down: the outer binding must resolve exactly
// as if the shadow never existed). A `:=` with NO outer counterpart is not
// a shadow of anything — it is simply this conditional subtree's own
// fresh local — and is recorded exactly as it would be anywhere else;
// suppressing it unconditionally (an earlier, over-eager version of this
// fix did) regressed several real, previously-correct resolutions whose
// only declaration site happens to sit inside an if/switch/select (a bare
// `home := os.UserHomeDir()` inside an if-block, among others) to a
// suddenly-unresolvable opaque name. A plain `=` and a compound assign are
// NOT gated on conditional depth at all here — both are still recorded
// (and still fold, for compound) exactly as outside a conditional; it is
// poisonConditionalAndLoopRebinds, run once collectLocalDefs's walk is
// complete, that decides which of THOSE survive for a use site outside the
// subtree. See that function's doc comment for the plain-vs-compound
// split and why a `:=` needs no matching poison step at all.
//
// A `var x = expr` ValueSpec found inside a conditionally-executed subtree
// is gated identically to `:=` above — shadow-protected only when an outer
// binding of the same name already exists, recorded plainly otherwise (see
// the ValueSpec case's own comment) — since a `var` declaration is exactly
// as much a fresh, block-scoped local as a `:=` is; nothing about the two
// declaration forms warrants different treatment here.
func applyDirectLocalDefs(defs map[string]ast.Expr, body *ast.BlockStmt, skipLoopBodies bool) {
	if body == nil {
		return
	}
	condDepth := 0
	var isCondStack []bool
	isCond := func(n ast.Node) bool {
		switch n.(type) {
		case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			return true
		}
		return false
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			// Paired exit for whichever node's entry pushed last (ast.Inspect
			// calls f(nil) once per non-skipped node, immediately after that
			// node's children are fully walked — see ast.Inspect's doc).
			if len(isCondStack) > 0 {
				wasCond := isCondStack[len(isCondStack)-1]
				isCondStack = isCondStack[:len(isCondStack)-1]
				if wasCond {
					condDepth--
				}
			}
			return true
		}
		switch n.(type) {
		case *ast.ForStmt:
			if skipLoopBodies {
				return false
			}
		case *ast.RangeStmt:
			if skipLoopBodies {
				return false
			}
		}
		cond := isCond(n)
		isCondStack = append(isCondStack, cond)
		if cond {
			condDepth++
		}
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			applyAssign(defs, stmt, condDepth > 0)
		case *ast.ValueSpec:
			// A `var x = expr` declaration is the DECLARE case's exact
			// counterpart to `:=` — see applyAssign's own doc comment for
			// the full reasoning, mirrored here rather than restated: a
			// ValueSpec found inside a conditionally-executed subtree is
			// only ever a SHADOW (and therefore left untouched, protecting
			// the outer binding of the same name) when an outer binding
			// already exists; a `var x = expr` with no outer counterpart is
			// simply this subtree's own fresh local and must still be
			// recorded, exactly as it would be outside a conditional. An
			// earlier version of this function gated the whole case on
			// condDepth == 0 — silently dropping EVERY `var x = expr`
			// declared inside any conditional, shadow or not, which is both
			// undocumented (this comment is the fix) and asymmetric with
			// the `:=` case sitting right next to it for no principled
			// reason: nothing distinguishes a `var`-form fresh conditional
			// local from a `:=`-form one.
			for i, name := range stmt.Names {
				if name.Name == "_" || i >= len(stmt.Values) {
					continue
				}
				if condDepth > 0 {
					if _, hadOuter := defs[name.Name]; hadOuter {
						continue
					}
				}
				defs[name.Name] = stmt.Values[i]
			}
		}
		return true
	})
}

// applyAssign records one AssignStmt's bindings into defs, honoring
// stmt.Tok: a plain `=`/`:=` rebinds the name outright (the existing,
// documented last-assignment-wins heuristic), but a COMPOUND assignment
// (+=, -=, ...) must not — `memPath += ".md"` is not a fresh definition of
// memPath, it is memPath's EXISTING value plus a suffix, and discarding
// the existing value (sdk/cogos.go:392) collapsed a real
// `filepath.Join(p.kernel.MemoryDir(), ...)` join down to a bare ".md"
// literal, which is exactly the shape that let a real .cog/mem/ writer
// misclassify as "elsewhere". When defs already has a binding for the
// name, a compound assignment folds onto it via a synthetic `X + Y`
// BinaryExpr (the resolver only special-cases token.ADD, which is the
// correct semantics for the string-concatenation compound assigns this
// codebase actually performs on path-shaped values, and a harmless
// over-approximation for any other compound operator, none of which occur
// on a path value here). When there is no prior binding to fold onto
// (e.g. the name is a parameter, never itself a local def), the RHS alone
// is recorded — still not a "rebind to nothing": the resolver's opaque
// placeholder fallback for the parameter case keeps this honest either
// way.
//
// insideConditional (true when applyDirectLocalDefs's condDepth is > 0 at
// this statement) affects ONLY the `:=` case: a declaration found inside a
// conditionally-executed subtree is a shadow local to it and must not
// overwrite an outer binding of the same name in this flat, scope-blind
// map — see applyDirectLocalDefs's own doc comment. A plain `=` still
// records outright (poisonConditionalAndLoopRebinds decides afterward
// whether a conditional's plain rebind survives for a use site outside
// it); a compound assign still folds exactly as it does anywhere else,
// conditional or not — folding never discards the prior binding, so there
// is nothing here that needs gating on conditional depth.
func applyAssign(defs map[string]ast.Expr, stmt *ast.AssignStmt, insideConditional bool) {
	for i, lhs := range stmt.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok || ident.Name == "_" || i >= len(stmt.Rhs) {
			continue
		}
		rhs := stmt.Rhs[i]
		if stmt.Tok != token.ASSIGN && stmt.Tok != token.DEFINE {
			if prev, had := defs[ident.Name]; had {
				rhs = &ast.BinaryExpr{X: prev, Op: token.ADD, Y: rhs}
			}
			defs[ident.Name] = rhs
			continue
		}
		if stmt.Tok == token.DEFINE && insideConditional {
			// Only a genuine SHADOW — a `:=` whose name already has an
			// outer binding from before this conditional subtree — is
			// protected here. A `:=` that introduces a brand-new name with
			// no outer counterpart has nothing to protect and must still
			// be recorded, exactly as it was before conditionals got any
			// special treatment: this is the one difference between "shadow"
			// and "fresh conditional-local", and conflating them regressed
			// several real, previously-correct resolutions (a bare `home :=
			// os.UserHomeDir()` declared only inside an if-block, a `ws :=`
			// used only inside a conditional branch, and similar) to a
			// suddenly-unresolvable name the moment this fix started
			// suppressing EVERY conditional `:=` rather than just shadows.
			if _, hadOuter := defs[ident.Name]; hadOuter {
				continue
			}
		}
		defs[ident.Name] = rhs
	}
}

// nestedScopeBody returns the nested block scope inside stmt that contains
// pos, one level down, or nil when stmt introduces no such nested scope (or
// pos falls outside all of its nested bodies — e.g. it's in an if/for/
// switch HEADER, not its body). Handles the constructs that introduce a new
// block scope in Go: a bare block, if/else (including else-if chains via
// recursion), for, range, switch/case, type-switch/case, and select/
// comm-clause. Consumed by scopeChain (via directChildBlockContaining)
// below, which walks every level this function knows about — a loop nested
// inside an if/switch/select, or vice versa, is found correctly either way,
// and each level along the path gets its own defs overlay (see
// localDefsFor).
func nestedScopeBody(stmt ast.Stmt, pos token.Pos) *ast.BlockStmt {
	switch s := stmt.(type) {
	case *ast.BlockStmt:
		if pos >= s.Pos() && pos < s.End() {
			return s
		}
	case *ast.IfStmt:
		if s.Body != nil && pos >= s.Body.Pos() && pos < s.Body.End() {
			return s.Body
		}
		if s.Else != nil && pos >= s.Else.Pos() && pos < s.Else.End() {
			return nestedScopeBody(s.Else, pos)
		}
	case *ast.ForStmt:
		if s.Body != nil && pos >= s.Body.Pos() && pos < s.Body.End() {
			return s.Body
		}
	case *ast.RangeStmt:
		if s.Body != nil && pos >= s.Body.Pos() && pos < s.Body.End() {
			return s.Body
		}
	case *ast.SwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok && pos >= cc.Pos() && pos < cc.End() {
				return &ast.BlockStmt{Lbrace: cc.Colon + 1, List: cc.Body, Rbrace: cc.End()}
			}
		}
	case *ast.TypeSwitchStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok && pos >= cc.Pos() && pos < cc.End() {
				return &ast.BlockStmt{Lbrace: cc.Colon + 1, List: cc.Body, Rbrace: cc.End()}
			}
		}
	case *ast.SelectStmt:
		for _, c := range s.Body.List {
			if cc, ok := c.(*ast.CommClause); ok && pos >= cc.Pos() && pos < cc.End() {
				return &ast.BlockStmt{Lbrace: cc.Colon + 1, List: cc.Body, Rbrace: cc.End()}
			}
		}
	}
	return nil
}

// directChildBlockContaining finds the ONE statement directly in block.List
// that contains pos, and returns whatever nested scope body that statement
// introduces (nil if none, or if pos is in that statement's header rather
// than its body).
func directChildBlockContaining(block *ast.BlockStmt, pos token.Pos) *ast.BlockStmt {
	for _, stmt := range block.List {
		if pos < stmt.Pos() || pos >= stmt.End() {
			continue
		}
		return nestedScopeBody(stmt, pos)
	}
	return nil
}

// scopeChain returns, from outermost to innermost, every nested block scope
// — a loop body, an if/else body, a switch/type-switch case body, a select
// comm-clause body, or a bare `{ }` block — that lexically contains pos
// within fnBody, by walking directChildBlockContaining one level at a time
// from fnBody down. This replaces the earlier, loop-only loopBodyChain:
// generalizing to every scope-introducing construct is what lets
// localDefsFor layer a CONDITIONAL branch's own local defs back on top of
// the flat map for a position inside that specific branch (see
// localDefsFor's doc comment for the two round-5-gate defects this closes
// — sibling branches colliding, and a shadow's own write being invisible to
// reads inside its own branch), not just a loop iteration's.
//
// Loop bodies keep behaving exactly as loopBodyChain always had them:
// nothing here changes what a position OUTSIDE every conditional resolves
// to (collectLocalDefs's own flat fold, plus poisonConditionalAndLoopRebinds,
// is unaffected — this only adds overlays for positions strictly INSIDE a
// tracked body), and a plain nested `{ }` block with no conditional or loop
// around it is a genuine, if previously untracked, Go lexical scope of its
// own — layering it in only makes a same-named local declared solely inside
// such a block resolve correctly for reads inside that block, never for
// reads outside it.
func scopeChain(fnBody *ast.BlockStmt, pos token.Pos) []*ast.BlockStmt {
	var chain []*ast.BlockStmt
	if fnBody == nil || pos < fnBody.Pos() || pos >= fnBody.End() {
		return chain
	}
	cur := fnBody
	for {
		next := directChildBlockContaining(cur, pos)
		if next == nil {
			return chain
		}
		chain = append(chain, next)
		cur = next
	}
}

// topLevelReturns collects every *ast.ReturnStmt directly in body, WITHOUT
// descending into nested func literals (a closure's own return belongs to
// the closure, not to the enclosing function's result).
func topLevelReturns(body *ast.BlockStmt) []*ast.ReturnStmt {
	var rets []*ast.ReturnStmt
	if body == nil {
		return rets
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, isFuncLit := n.(*ast.FuncLit); isFuncLit {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok {
			rets = append(rets, ret)
		}
		return true
	})
	return rets
}

// ─── The callable-origin index ─────────────────────────────────────────────

type funcKey struct {
	dir, name string
}

// callSite is one bare-identifier call to a known no-receiver function,
// recorded with everything needed to resolve its arguments in the CALLER's
// own scope.
type callSite struct {
	args     []ast.Expr
	ellipsis bool
	defs     map[string]ast.Expr
	ctx      *resolver
}

// fieldCandidate is one KEYED composite-literal field assignment, recorded
// with everything needed to resolve its value expression in the
// CONSTRUCTOR's own scope.
type fieldCandidate struct {
	field     string
	expr      ast.Expr
	localDefs map[string]ast.Expr
	ctx       *resolver
}

type originResult struct {
	pattern        string
	ok             bool
	degenerateRoot bool // see resolveExpr's return shape; OR'd across agreeing candidates
}

// globalIndex is built once per Scan and shared read-only across the
// site-scanning pass (aside from the two small memoization maps, which are
// populated lazily but never invalidated within one Scan run).
// methodKey identifies a receiver method: (dir, recvType, methodName). Same
// "same-directory" source-text proxy for "same package" as funcKey.
type methodKey struct {
	dir, recvType, method string
}

type globalIndex struct {
	funcDecls   map[funcKey]*ast.FuncDecl
	funcCalls   map[funcKey][]callSite
	fieldCand   map[string][]fieldCandidate // key: type name
	constDecl   map[funcKey]string          // key: (dir, name) -> literal string value
	methodDecls map[methodKey]*ast.FuncDecl
	fieldTypes  map[[2]string]string // key: (typeName, fieldName) -> declared field type, "*" stripped

	fieldMemo map[string]originResult // key: type+"\x00"+field
	paramMemo map[string]originResult // key: dir+"\x00"+func+"\x00"+paramIdx
}

func buildGlobalIndex(files []parsedFile) *globalIndex {
	idx := &globalIndex{
		funcDecls:   map[funcKey]*ast.FuncDecl{},
		funcCalls:   map[funcKey][]callSite{},
		fieldCand:   map[string][]fieldCandidate{},
		constDecl:   map[funcKey]string{},
		methodDecls: map[methodKey]*ast.FuncDecl{},
		fieldTypes:  map[[2]string]string{},
		fieldMemo:   map[string]originResult{},
		paramMemo:   map[string]originResult{},
	}

	// Pass 1: every no-receiver top-level function, keyed by (dir, name);
	// every receiver method, keyed by (dir, recvType, methodName); every
	// package-level `const NAME = "literal"` declaration, keyed by
	// (dir, name); and every struct field's DECLARED type, keyed by
	// (typeName, fieldName). The const and field-type lookups close two
	// specific blind spots that need no real type information at all —
	// unlike the general "fold arbitrary consts" / "resolve any value's
	// dynamic type" cases the package doc disclaims, these are plain
	// textual lookups over declarations already sitting in the AST. The
	// const lookup is what lets a bucket argument like
	// `identityGrantsLedgerSession` resolve to "identity-grants" instead
	// of an opaque placeholder; the method+field-type combination is what
	// lets a receiver-field method call like `p.kernel.MemoryDir()`
	// resolve through to `<root>/.cog/mem` instead of being an unmatched,
	// unresolvable selector call (see resolveCall's receiver.field.Method()
	// case).
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				if d.Recv == nil {
					idx.funcDecls[funcKey{pf.dir, d.Name.Name}] = d
					continue
				}
				if len(d.Recv.List) == 0 {
					continue
				}
				recvType := strings.TrimPrefix(exprString(d.Recv.List[0].Type), "*")
				idx.methodDecls[methodKey{pf.dir, recvType, d.Name.Name}] = d

			case *ast.GenDecl:
				switch d.Tok {
				case token.CONST:
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							if name.Name == "_" || i >= len(vs.Values) {
								continue
							}
							lit, ok := vs.Values[i].(*ast.BasicLit)
							if !ok || lit.Kind != token.STRING {
								continue
							}
							idx.constDecl[funcKey{pf.dir, name.Name}] = unquote(lit.Value)
						}
					}
				case token.TYPE:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok || st.Fields == nil {
							continue
						}
						for _, field := range st.Fields.List {
							fieldType := strings.TrimPrefix(exprString(field.Type), "*")
							for _, name := range field.Names {
								idx.fieldTypes[[2]string{ts.Name.Name, name.Name}] = fieldType
							}
						}
					}
				}
			}
		}
	}

	// Pass 2: call sites of those functions, and keyed composite-literal
	// field assignments for any struct type.
	for _, pf := range files {
		localDefsCache := map[*ast.BlockStmt]map[string]ast.Expr{}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				id, ok := node.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				key := funcKey{pf.dir, id.Name}
				if _, known := idx.funcDecls[key]; !known {
					return true
				}
				sc := enclosingFuncScope(pf.scopes, node.Pos())
				idx.funcCalls[key] = append(idx.funcCalls[key], callSite{
					args:     node.Args,
					ellipsis: node.Ellipsis.IsValid(),
					defs:     localDefsFor(localDefsCache, sc.body, node.Pos()),
					ctx:      resolverContextFor(idx, pf.dir, sc),
				})

			case *ast.CompositeLit:
				typeName, ok := compositeLitTypeName(node.Type)
				if !ok {
					return true
				}
				sc := enclosingFuncScope(pf.scopes, node.Pos())
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue // unkeyed (positional) literal — not chased, see package doc
					}
					fieldIdent, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					idx.fieldCand[typeName] = append(idx.fieldCand[typeName], fieldCandidate{
						field:     fieldIdent.Name,
						expr:      kv.Value,
						localDefs: localDefsFor(localDefsCache, sc.body, kv.Pos()),
						ctx:       resolverContextFor(idx, pf.dir, sc),
					})
				}
			}
			return true
		})
	}

	return idx
}

func compositeLitTypeName(t ast.Expr) (string, bool) {
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.SelectorExpr:
		return v.Sel.Name, true
	}
	return "", false
}

// fieldOrigin resolves the value assigned to typeName's field across every
// keyed composite literal found for it. Only an unambiguous, unanimous
// answer is used — never a guess between disagreeing literals.
//
// Only a SUCCESSFUL resolution is memoized. depth is the caller's own
// chase depth, not an intrinsic property of (typeName, field): a
// negative result computed here can be nothing more than an artifact of
// how much maxChaseDepth budget the FIRST caller to reach this key
// happened to have left — a deeply-nested call site (many levels of
// filepath.Join, or several interprocedural hops already spent) can
// exhaust the budget resolving a candidate that a shallower caller, with
// its own full budget, would resolve completely. Caching that negative
// result would let the deep caller permanently poison every later
// caller's answer for the same (typeName, field), regardless of how much
// headroom THEY have — silently downgrading an otherwise-knowable
// destination to "unanchored"/"dynamic", so a negative is never frozen.
//
// A memoized POSITIVE is not fully depth-independent either, and this is
// a real, currently-open gap rather than a solved one: with two or more
// candidates for the same (typeName, field), a candidate whose own
// resolution fails partway through (the `if !ok { continue }` below) is
// silently dropped from consideration — it neither confirms nor
// contradicts the other candidates, because this loop cannot tell "this
// candidate genuinely has no answer" apart from "this candidate ran out
// of depth budget under THIS caller". A shallower caller arriving later,
// with more budget, might resolve that same dropped candidate and find
// it disagrees with the unanimous answer already cached here — at which
// point the correct result would have been a conflict (a negative), not
// the positive this function already handed back and memoized. Closing
// that gap needs tracking, per candidate, whether "unresolved" meant
// "exhausted budget" or "genuinely opaque" (e.g. an index expression),
// which this function does not do today; until it does, fieldOrigin's
// unanimity claim is only as complete as the candidate set the FIRST
// caller's own budget happened to resolve. The cycle-guard placeholder
// below is still required — and still correct — for the duration of
// THIS call's own recursion; it is simply not left behind afterward when
// the call ends negatively.
func (idx *globalIndex) fieldOrigin(typeName, field string, depth int) (string, bool, bool) {
	key := typeName + "\x00" + field
	if r, ok := idx.fieldMemo[key]; ok {
		return r.pattern, r.ok, r.degenerateRoot
	}
	idx.fieldMemo[key] = originResult{} // cycle guard: treat as unresolved while in progress

	var found string
	foundSet, conflict, foundDegenerate := false, false, false
	for _, cand := range idx.fieldCand[typeName] {
		if cand.field != field {
			continue
		}
		pattern, ok, degenerate := cand.ctx.resolveExpr(cand.expr, cand.localDefs, depth+1)
		if !ok {
			continue
		}
		if !foundSet {
			found, foundSet = pattern, true
			foundDegenerate = degenerate
		} else if pattern != found {
			conflict = true
		} else if degenerate {
			// Same text, but THIS candidate reached it through the
			// degenerate-concat shape — OR it into the memoized answer
			// rather than trusting whichever candidate happened to be
			// resolved first. Erring toward "degenerate" biases classify
			// toward "unanchored" over a false "elsewhere", which is this
			// tool's always-acceptable failure direction (see classify's
			// doc comment) — the same posture as fieldOrigin's own
			// documented positive-memoization gap above.
			foundDegenerate = true
		}
	}

	if !foundSet || conflict {
		delete(idx.fieldMemo, key) // negative: don't freeze a depth-dependent non-answer
		return "", false, false
	}
	result := originResult{pattern: found, ok: true, degenerateRoot: foundDegenerate}
	idx.fieldMemo[key] = result
	return result.pattern, result.ok, result.degenerateRoot
}

// paramOrigin resolves the i'th argument passed to (dir, funcName) across
// every same-directory bare-identifier call site found for it. Only an
// unambiguous, unanimous answer is used.
//
// Same depth-independence rule as fieldOrigin above, and for the identical
// reason: a negative result here can be purely a function of how much
// budget the FIRST caller to reach this (dir, funcName, paramIdx) key had
// left, not a fact about the key itself. Only a successful resolution is
// memoized; the cycle-guard placeholder is removed rather than frozen when
// the call ends negatively. The same open gap on the POSITIVE side applies
// here too — see fieldOrigin's doc comment: a call site whose argument
// fails to resolve under this caller's remaining budget is dropped from
// the unanimity check rather than treated as a potential disagreement, so
// a cached positive answer is only as complete as the call sites this
// caller's own budget could resolve.
func (idx *globalIndex) paramOrigin(dir, funcName string, paramIdx, depth int) (string, bool, bool) {
	key := dir + "\x00" + funcName + "\x00" + strconv.Itoa(paramIdx)
	if r, ok := idx.paramMemo[key]; ok {
		return r.pattern, r.ok, r.degenerateRoot
	}
	idx.paramMemo[key] = originResult{}

	calls := idx.funcCalls[funcKey{dir, funcName}]
	var found string
	foundSet, conflict, foundDegenerate := false, false, false
	for _, cs := range calls {
		if cs.ellipsis || paramIdx >= len(cs.args) {
			continue
		}
		pattern, ok, degenerate := cs.ctx.resolveExpr(cs.args[paramIdx], cs.defs, depth+1)
		if !ok {
			continue
		}
		if !foundSet {
			found, foundSet = pattern, true
			foundDegenerate = degenerate
		} else if pattern != found {
			conflict = true
		} else if degenerate {
			foundDegenerate = true // see fieldOrigin's identical OR-across-candidates note
		}
	}

	if !foundSet || conflict {
		delete(idx.paramMemo, key) // negative: don't freeze a depth-dependent non-answer
		return "", false, false
	}
	result := originResult{pattern: found, ok: true, degenerateRoot: foundDegenerate}
	idx.paramMemo[key] = result
	return result.pattern, result.ok, result.degenerateRoot
}

// callChase resolves ONE specific call site of a known no-receiver function
// by substituting that call's own (already-resolved) arguments for the
// callee's parameters and resolving the callee's return expression(s).
// Unlike fieldOrigin/paramOrigin this is never aggregated across call
// sites — each call is resolved using only its own arguments.
//
// A callee with multiple return statements is handled in two steps, never
// collapsing to "whichever branch happened to resolve":
//
//  1. An ERROR-GUARD branch — `return "", err`-shaped, see
//     isErrorGuardReturn — is excluded from consideration entirely. It is
//     the callee's own early-out, not a second, disagreeing destination;
//     folding it in as a candidate is exactly how containedJoin's real
//     `return filepath.Clean(full), nil` branch was getting discarded in
//     favor of its OWN `return "", err` guard the moment the real branch
//     needed one extra hop to resolve (filepath.Clean — now a recognized
//     transparent wrapper, see resolveCall) — the guard "won" simply
//     because it was structurally trivial to resolve.
//  2. Among the SURVIVING (non-guard) branches: if any one of them fails
//     to resolve at all, or two of them resolve to DIFFERENT categories
//     (see classify), that is a genuine, real disagreement — not a
//     dropped branch and not a guess at which one applies at runtime. The
//     whole call is reported as an opaque "<call:name>" marker with
//     ok=true, which is NOT the same as ok=false: it deliberately lands
//     the resulting Site in the UNANCHORED honesty margin (see classify),
//     never "dynamic" and never a guessed "elsewhere". Under-claiming is
//     always the acceptable failure mode here; collapsing to one branch's
//     value the way the original version of this function did is not.
func (idx *globalIndex) callChase(dir, name string, callee *ast.FuncDecl, args []ast.Expr, ellipsis bool, caller *resolver, callerDefs map[string]ast.Expr, depth int) (string, bool, bool) {
	opaque := "<call:" + name + ">"
	if depth > maxChaseDepth || ellipsis {
		return opaque, true, false
	}
	params := paramNames(callee.Type.Params)
	merged := substituteAndMergeDefs(params, args, callee.Body, caller, callerDefs, depth)
	calleeResolver := &resolver{idx: idx, dir: dir, funcName: name, params: params}
	return chaseReturns(callee, calleeResolver, merged, depth, opaque)
}

// methodChase is callChase's counterpart for a receiver method reached
// through a struct field — `p.kernel.MemoryDir()`, where `kernel`'s
// declared type is known via fieldTypes and MemoryDir is a method on that
// type found via methodDecls (see resolveCall's receiver.field.Method()
// case). The chase mechanics (guard-branch exclusion, category-agreement
// requirement, opaque-on-any-failure) are identical to callChase's — only
// the callee's OWN scope identity differs (a receiver, not a funcName).
func (idx *globalIndex) methodChase(dir, recvType, methodName string, callee *ast.FuncDecl, args []ast.Expr, ellipsis bool, caller *resolver, callerDefs map[string]ast.Expr, depth int) (string, bool, bool) {
	opaque := "<call:" + recvType + "." + methodName + ">"
	if depth > maxChaseDepth || ellipsis {
		return opaque, true, false
	}
	params := paramNames(callee.Type.Params)
	merged := substituteAndMergeDefs(params, args, callee.Body, caller, callerDefs, depth)
	var recvName string
	if callee.Recv != nil && len(callee.Recv.List) > 0 && len(callee.Recv.List[0].Names) > 0 {
		recvName = callee.Recv.List[0].Names[0].Name
	}
	calleeResolver := &resolver{idx: idx, dir: dir, recvName: recvName, recvType: recvType}
	return chaseReturns(callee, calleeResolver, merged, depth, opaque)
}

// substituteAndMergeDefs resolves each of a callee's arguments in the
// CALLER's own scope, binds the results to the callee's parameter names,
// and merges the callee's own local defs on top (a real local assignment
// inside the callee overrides the raw parameter substitution — the same
// rule callChase always applied).
//
// The argument's own degenerateRoot flag (see resolveExpr's return shape)
// cannot ride along on a substitution: merged holds plain ast.Expr values,
// so a degeneracy-tagged resolution would enter the callee disguised as an
// ordinary string literal and be re-resolved there as positive
// information. Rather than thread a side channel through the defs map, a
// degenerate argument is never substituted at all — the callee's parameter
// falls through to its own opaque "{name}" placeholder, which classifies
// as unanchored. That trades pattern detail for the guarantee that a
// degenerate root cannot cross a call boundary and come back "elsewhere".
// Degeneracy that arises INSIDE the callee (from a substituted,
// non-degenerate argument) is unaffected: chaseReturns carries the
// callee's own flag out.
func substituteAndMergeDefs(params []string, args []ast.Expr, calleeBody *ast.BlockStmt, caller *resolver, callerDefs map[string]ast.Expr, depth int) map[string]ast.Expr {
	merged := map[string]ast.Expr{}
	for i, p := range params {
		if i >= len(args) {
			continue
		}
		pattern, ok, degenerate := caller.resolveExpr(args[i], callerDefs, depth+1)
		if ok && !degenerate {
			merged[p] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(pattern)}
		}
	}
	for k, v := range collectLocalDefs(calleeBody) {
		merged[k] = v
	}
	return merged
}

// chaseReturns resolves a callee's top-level, non-guard return statements
// and accepts the result only when every surviving branch resolves AND
// they all agree on category (see classify). Shared by callChase and
// methodChase — the only difference between the two is calleeResolver's
// own scope identity (funcName+params vs recvName+recvType).
func chaseReturns(callee *ast.FuncDecl, calleeResolver *resolver, merged map[string]ast.Expr, depth int, opaque string) (string, bool, bool) {
	type candidate struct {
		pattern, category string
		degenerate        bool
	}
	var cands []candidate
	for _, ret := range topLevelReturns(callee.Body) {
		if len(ret.Results) == 0 {
			continue
		}
		if isErrorGuardReturn(ret) {
			continue
		}
		// Index 0: the (value, err) idiom's value is always the first
		// result, and collectLocalDefs already only ever binds a
		// multi-value call to its FIRST assigned name for the same reason.
		p, ok, degenerate := calleeResolver.resolveExpr(ret.Results[0], merged, depth+1)
		if !ok {
			return opaque, true, false // a surviving branch didn't resolve at all — under-claim
		}
		cands = append(cands, candidate{p, classify(p, ok, degenerate), degenerate})
	}
	if len(cands) == 0 {
		return opaque, true, false
	}
	cat0 := cands[0].category
	for _, c := range cands[1:] {
		if c.category != cat0 {
			return opaque, true, false // branches disagree on category — a real dead end, not a guess
		}
	}
	return cands[0].pattern, true, cands[0].degenerate
}

// isErrorGuardReturn reports whether ret is shaped like Go's idiomatic
// (zeroValue, err) early-out: a literal empty string paired with anything
// OTHER than a literal `nil`. This is the callee's own error path, not a
// second, disagreeing destination the function can return at runtime —
// see callChase's doc comment for why folding it in as a candidate was
// silently collapsing otherwise-resolvable functions (containedJoin,
// resolveMemoryDocPath, RunDir) down to "".
func isErrorGuardReturn(ret *ast.ReturnStmt) bool {
	if len(ret.Results) != 2 {
		return false
	}
	lit, isString := ret.Results[0].(*ast.BasicLit)
	if !isString || lit.Kind != token.STRING || unquote(lit.Value) != "" {
		return false
	}
	if id, isIdent := ret.Results[1].(*ast.Ident); isIdent && id.Name == "nil" {
		return false // `return "", nil` is a real, deliberate empty-string success value, not a guard
	}
	return true
}

// ─── The resolver ───────────────────────────────────────────────────────────

// resolver carries just enough scope identity to chase THIS scope's own
// receiver fields or parameters through the global index. It is
// intentionally small and cheap to construct fresh per scope.
type resolver struct {
	idx      *globalIndex
	dir      string
	funcName string // set only for a no-receiver "func" scope
	params   []string
	recvName string // set only for a "method" scope
	recvType string
}

func resolverContextFor(idx *globalIndex, dir string, sc funcScope) *resolver {
	r := &resolver{idx: idx, dir: dir}
	switch sc.kind {
	case "func":
		r.funcName = sc.name
		r.params = sc.params
	case "method":
		r.recvName = sc.recvName
		r.recvType = sc.recvType
	}
	return r
}

// resolveExpr recursively renders e as a best-effort path pattern. ok is
// false only when some sub-expression could not be structurally decomposed
// at all (see the package doc comment for the exact rule set).
//
// degenerateRoot is a THIRD, out-of-band signal, independent of pattern's
// text: it reports whether pattern's apparent root is an artifact of
// manual path-building — specifically, a string concatenation whose left
// operand resolved to the literal empty string glued to a right-hand
// literal that itself begins with "/" (see the BinaryExpr case) — rather
// than a positively-established absolute-path root like a genuine
// "/etc/cogos/config.yaml" literal. The two are textually IDENTICAL
// ("/observations.jsonl" either way), which is exactly why this can't be
// recovered later from pattern's text alone the way every other opacity
// signal in this package is (see isOpaqueRoot): it has to be threaded
// through as its own value from the exact point it's known. Within the
// scope that established it, composing a degenerate sub-expression into a
// larger pattern — via filepath.Join, joining resolved parts with "/";
// filepath.Dir/Base wrapping "dirname(...)"/"basename(...)" around an
// inner resolution; or a further string concatenation gluing another
// operand onto an already-degenerate one (including the SYNTHETIC
// BinaryExpr a compound assign like `p += ".tmp"` folds onto an existing
// binding — see applyAssign) — never launders it back to positive
// information, even when the degenerate fragment ends up buried in the
// composed pattern's tail rather than sitting at its literal front.
// filepath.Join's own degenerateRoot is true when ANY kept argument was
// flagged degenerate, regardless of which position it lands in;
// filepath.Dir/Base inherit their single inner expression's flag
// directly, since dirname(x)/basename(x) never changes what x's own root
// was; a further BinaryExpr concatenation ORs both operands' own flags
// together with its own left=="" test (see the BinaryExpr case), since
// EITHER operand can already be carrying a degenerate root from a nested
// sub-expression, not only this level's own left-hand side. Every other
// case in the switch below and in resolveCall falls into one of THREE
// buckets: it produces a positively-known root (an anchor, a literal, a
// package-level const) and is never degenerate; or it produces an opaque
// placeholder ("{name}", "<call:...>") whose text already flags itself,
// so degeneracy is moot; or it is a transparent propagator — ParenExpr,
// an Ident or SelectorExpr chased through local defs / paramOrigin /
// fieldOrigin, filepath.Clean, and callChase/methodChase via chaseReturns
// — that inherits the flag of whatever it delegates to. The one place a
// degenerate resolution could otherwise enter a fresh scope as a plain
// literal — argument substitution in substituteAndMergeDefs — refuses to
// substitute a degenerate argument at all (see that function's comment),
// so the flag never has to survive a call boundary: a degenerate value
// either stays in the scope that knows it is degenerate, or is replaced
// by an opaque placeholder on the far side.
func (r *resolver) resolveExpr(e ast.Expr, defs map[string]ast.Expr, depth int) (pattern string, ok bool, degenerateRoot bool) {
	if depth > maxChaseDepth {
		return "<max-depth>", false, false
	}

	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return unquote(v.Value), true, false
		}
		return v.Value, true, false

	case *ast.ParenExpr:
		return r.resolveExpr(v.X, defs, depth+1)

	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "<expr>", false, false
		}
		left, leftOK, leftDeg := r.resolveExpr(v.X, defs, depth+1)
		right, rightOK, rightDeg := r.resolveExpr(v.Y, defs, depth+1)
		combined := left + right
		// Concatenation is the manual-join idiom's own primitive
		// (`base + "/observations.jsonl"` instead of filepath.Join(base,
		// "observations.jsonl")) — see the package doc comment's "string
		// concatenation" resolution rule. When the left operand resolves to
		// the literal empty string, the "/" baked into the right-hand
		// literal is doing separator duty, not spelling a genuine absolute
		// path — the exact same degenerate shape the Join handler above
		// normalizes by skipping the empty part. Concatenation has no
		// separator-skipping step to hook that fix into (the "/" already
		// lives inside the literal, not inserted by the join), so the
		// degeneracy is reported out-of-band instead, via this function's
		// own degenerateRoot return value — never folded into combined's
		// text — so isOpaqueRoot can route it to unanchored rather than
		// classify reading its accidental leading slash as the same
		// positive evidence a genuine literal like
		// "/etc/cogos/config.yaml" carries.
		//
		// Either operand's OWN degenerateRoot must also be propagated, not
		// just this level's own left=="" shape — the same any-operand-
		// degenerate posture filepath.Join already takes (see resolveCall's
		// Join case). Without this, a concat composing an ALREADY-degenerate
		// sub-expression silently launders it back to positive information
		// the moment this level's own left operand is no longer literally
		// "" — which is exactly what happens one level up from the
		// degenerate concat itself: `p := base + "/observations.jsonl"`
		// (degenerate, left=="") followed by `p += ".tmp"` folds into a
		// SYNTHETIC outer BinaryExpr(BinaryExpr(base, "/observations.jsonl"),
		// ".tmp") — see applyAssign's compound-assignment fold — whose own
		// left operand resolves to "/observations.jsonl", not "", so the
		// left=="" test alone finds nothing degenerate at this level even
		// though its left operand's OWN root was never positively
		// established. The identical shape recurs for a second concat
		// chained directly onto a degenerate one (`p + ".gz"`) and for a
		// concat appending onto a degenerate filepath.Dir/Base wrapping
		// (dirname(base+"/x") is itself degenerate per the Dir case below,
		// and appending ".log" onto that must not clear it either).
		degenerate := leftDeg || rightDeg || (left == "" && strings.HasPrefix(right, "/"))
		return combined, leftOK && rightOK, degenerate

	case *ast.Ident:
		if anchor, isAnchor := identifierAnchors[strings.ToLower(v.Name)]; isAnchor {
			return anchor, true, false
		}
		if def, isLocal := defs[v.Name]; isLocal {
			if call, isCall := def.(*ast.CallExpr); isCall {
				if primitive, argIdx, matched := matchWriterCall(call, defs); matched && argIdx < len(call.Args) {
					// def is itself a matched write-primitive call (e.g.
					// f, _ := os.CreateTemp(dir, pat)) — chase through to
					// ITS path argument rather than treating the call as
					// an ordinary opaque expression.
					return resolveWriterPathArg(r, primitive, call.Args[argIdx], defs, depth+1)
				}
			}
			return r.resolveExpr(def, defs, depth+1)
		}
		if r.funcName != "" {
			for i, p := range r.params {
				if p == v.Name {
					if pattern, ok, degenerate := r.idx.paramOrigin(r.dir, r.funcName, i, depth+1); ok {
						return pattern, true, degenerate
					}
					break
				}
			}
		}
		if lit, ok := r.idx.constDecl[funcKey{r.dir, v.Name}]; ok {
			// A package-level `const NAME = "literal"` in this same
			// directory, and nothing narrower (a local def, a chased
			// parameter) already claimed the name — see buildGlobalIndex's
			// Pass 1 doc comment for why this specific blind spot needs no
			// type information to close.
			return lit, true, false
		}
		return "{" + v.Name + "}", true, false

	case *ast.SelectorExpr:
		if anchor, isAnchor := identifierAnchors[strings.ToLower(v.Sel.Name)]; isAnchor {
			return anchor, true, false
		}
		if ident, isIdent := v.X.(*ast.Ident); isIdent && r.recvType != "" && ident.Name == r.recvName {
			if pattern, ok, degenerate := r.idx.fieldOrigin(r.recvType, v.Sel.Name, depth+1); ok {
				return pattern, true, degenerate
			}
		}
		return "{" + exprString(v) + "}", true, false

	case *ast.CallExpr:
		return r.resolveCall(v, defs, depth)
	}

	return "<expr>", false, false
}

// resolveCall handles: the narrow filepath/os allowlist (Join/Dir/Base,
// UserHomeDir/TempDir), and — new — a bare call to any other no-receiver
// function this repo declares, chased through callChase.
func (r *resolver) resolveCall(call *ast.CallExpr, defs map[string]ast.Expr, depth int) (string, bool, bool) {
	if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
		if pkgIdent, isPkg := sel.X.(*ast.Ident); isPkg {
			switch {
			case pkgIdent.Name == "filepath" && sel.Sel.Name == "Join":
				// Real filepath.Join skips empty components rather than
				// separator-joining them literally: Join("", "state.json")
				// is "state.json", not "/state.json". Reproduce that here
				// by dropping any part that resolved to the literal empty
				// string before joining with "/" — a resolved-but-opaque
				// part (e.g. "<expr>", "{name}") is never itself "", so
				// this can't accidentally swallow a real pattern marker.
				//
				// The joined result's own degenerateRoot is true when ANY
				// kept argument was itself flagged degenerate, regardless
				// of which position it lands in — not only when it's the
				// argument that happens to become the joined text's own
				// first segment. A degenerate fragment buried in a LATER
				// argument (filepath.Join(workspaceRoot, base+"/x") with an
				// empty base) is still a resolution artifact this tool
				// cannot fully vouch for, the same way a same-shaped opaque
				// segment anywhere in a <WorkspaceRoot>-rooted pattern's
				// TAIL already keeps classify from confirming "elsewhere"
				// (see hasOpaqueSegment) — under-claiming here is the
				// tool's always-acceptable failure direction, not a
				// narrower "only the root position counts" reading.
				parts := make([]string, 0, len(call.Args))
				allOK, anyDegenerate := true, false
				for _, arg := range call.Args {
					p, ok, degenerate := r.resolveExpr(arg, defs, depth+1)
					if p != "" {
						parts = append(parts, p)
						anyDegenerate = anyDegenerate || degenerate
					}
					allOK = allOK && ok
				}
				return strings.Join(parts, "/"), allOK, anyDegenerate

			case pkgIdent.Name == "filepath" && sel.Sel.Name == "Dir":
				if len(call.Args) != 1 {
					return "<call>", false, false
				}
				inner, ok, degenerate := r.resolveExpr(call.Args[0], defs, depth+1)
				// dirname(x) never changes what x's own root was, so a
				// degenerate inner expression makes the wrapped result
				// degenerate too — see resolveExpr's doc comment.
				return "dirname(" + inner + ")", ok, degenerate

			case pkgIdent.Name == "filepath" && sel.Sel.Name == "Base":
				if len(call.Args) != 1 {
					return "<call>", false, false
				}
				inner, ok, degenerate := r.resolveExpr(call.Args[0], defs, depth+1)
				return "basename(" + inner + ")", ok, degenerate

			case pkgIdent.Name == "filepath" && sel.Sel.Name == "Clean":
				// filepath.Clean is a TRANSPARENT wrapper for this tool's
				// purposes: it never changes which anchor or literal a
				// path is rooted at, only its surface spelling
				// (redundant separators, "." elements). containedJoin
				// (path_safety.go) returns filepath.Clean(full) on its
				// real branch specifically so callers can't be confused
				// by an unclean join — treating Clean as opaque here was
				// silently discarding that branch's real value.
				if len(call.Args) != 1 {
					return "<call>", false, false
				}
				return r.resolveExpr(call.Args[0], defs, depth+1)

			case pkgIdent.Name == "os" && sel.Sel.Name == "UserHomeDir":
				return "<Home>", true, false

			case pkgIdent.Name == "os" && sel.Sel.Name == "TempDir":
				return "<TempDir>", true, false
			}
		}
		// receiver.field.Method(...): a method call on a struct field of
		// the ENCLOSING receiver, where the field's declared type is known
		// (fieldTypes) and that type declares the method (methodDecls) —
		// e.g. `p.kernel.MemoryDir()` where `kernel *Kernel` is a field of
		// p's own type. Chased the same way a bare same-directory function
		// call is, just with a receiver-shaped callee scope instead of a
		// funcName-shaped one.
		if innerSel, isInnerSel := sel.X.(*ast.SelectorExpr); isInnerSel && r.recvType != "" {
			if recvIdent, isIdent := innerSel.X.(*ast.Ident); isIdent && recvIdent.Name == r.recvName {
				if fieldType, known := r.idx.fieldTypes[[2]string{r.recvType, innerSel.Sel.Name}]; known {
					if callee, known := r.idx.methodDecls[methodKey{r.dir, fieldType, sel.Sel.Name}]; known {
						pattern, ok, degenerate := r.idx.methodChase(r.dir, fieldType, sel.Sel.Name, callee, call.Args, call.Ellipsis.IsValid(), r, defs, depth)
						if ok {
							return pattern, true, degenerate
						}
					}
				}
			}
		}
		return "<call:" + exprString(call.Fun) + ">", false, false
	}

	if id, isIdent := call.Fun.(*ast.Ident); isIdent {
		if callee, known := r.idx.funcDecls[funcKey{r.dir, id.Name}]; known {
			if pattern, ok, degenerate := r.idx.callChase(r.dir, id.Name, callee, call.Args, call.Ellipsis.IsValid(), r, defs, depth); ok {
				return pattern, true, degenerate
			}
		}
		return "<call:" + id.Name + ">", false, false
	}

	return "<call>", false, false
}

// ─── Write-primitive matching ───────────────────────────────────────────────

// matchWriterCall reports whether call is a recognized filesystem/database
// write primitive, and if so which argument index holds the write target.
// defs is the CALLER's local-def map, needed only to gate io.Copy (see
// destIsLocallyCreatedFile).
func matchWriterCall(call *ast.CallExpr, defs map[string]ast.Expr) (primitive string, pathArgIdx int, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", 0, false
	}
	pkgIdent, isPkg := sel.X.(*ast.Ident)
	if !isPkg {
		return "", 0, false
	}

	if pkgIdent.Name == "os" && sel.Sel.Name == "OpenFile" {
		if len(call.Args) < 2 || !hasWriteFlag(call.Args[1]) {
			return "", 0, false
		}
		return "os.OpenFile", 0, true
	}

	if pm, known := simplePrimitives[[2]string{pkgIdent.Name, sel.Sel.Name}]; known {
		return pm.name, pm.argIdx, true
	}

	if pkgIdent.Name == "sql" && sel.Sel.Name == "Open" {
		if len(call.Args) >= 2 && isSQLiteDriverLiteral(call.Args[0]) {
			return "sql.Open(sqlite3)", 1, true
		}
		return "", 0, false
	}

	if pkgIdent.Name == "io" && sel.Sel.Name == "Copy" {
		// Only recognized "where visible": the destination must be, in
		// THIS function, a local variable bound to one of the primitives
		// above. An unconditional io.Copy match would flag every
		// in-memory io.Writer target (bytes.Buffer, http.ResponseWriter,
		// a network conn) as a false positive.
		if len(call.Args) >= 1 && destIsLocallyCreatedFile(call.Args[0], defs) {
			return "io.Copy", 0, true
		}
		return "", 0, false
	}

	return "", 0, false
}

// resolveWriterPathArg resolves a matched writer call's own path argument,
// with one deliberate override: an EXPLICITLY EMPTY dir argument to
// os.CreateTemp/os.MkdirTemp maps to the <TempDir> anchor, matching what
// those stdlib calls actually do at runtime (fall back to os.TempDir()).
// Without this, the true "writes into os.TempDir()" sites and a resolution
// that merely gave up partway through and left a leftover empty string
// (see classify's empty-root handling) would render identically — the
// exact ambiguity that let genuine .cog/ writers hide among legitimate
// TempDir rows undetected. This is checked on the RAW argument expression,
// not the resolved pattern, so it only ever fires for a literal `""` in
// source, never for some deeper chase that happens to collapse to empty.
func resolveWriterPathArg(r *resolver, primitive string, pathArg ast.Expr, defs map[string]ast.Expr, depth int) (string, bool, bool) {
	if (primitive == "os.CreateTemp" || primitive == "os.MkdirTemp") && isEmptyStringLit(pathArg) {
		return "<TempDir>", true, false
	}
	return r.resolveExpr(pathArg, defs, depth)
}

func isEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && unquote(lit.Value) == ""
}

func isSQLiteDriverLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	return strings.Contains(strings.ToLower(unquote(lit.Value)), "sqlite")
}

func destIsLocallyCreatedFile(e ast.Expr, defs map[string]ast.Expr) bool {
	ident, isIdent := e.(*ast.Ident)
	if !isIdent {
		return false
	}
	def, isLocal := defs[ident.Name]
	if !isLocal {
		return false
	}
	call, isCall := def.(*ast.CallExpr)
	if !isCall {
		return false
	}
	_, _, matched := matchWriterCall(call, defs)
	return matched
}

// hasWriteFlag reports whether the os.OpenFile flags expression contains at
// least one write-indicating os.O_* flag. Flags are typically combined with
// bitwise OR (os.O_CREATE|os.O_WRONLY|os.O_APPEND); this walks that chain.
func hasWriteFlag(e ast.Expr) bool {
	found := false
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BinaryExpr:
			walk(v.X)
			walk(v.Y)
		case *ast.ParenExpr:
			walk(v.X)
		case *ast.SelectorExpr:
			if pkgIdent, ok := v.X.(*ast.Ident); ok && pkgIdent.Name == "os" && writeFlagNames[v.Sel.Name] {
				found = true
			}
		}
	}
	walk(e)
	return found
}

// ─── Per-file site scanning ─────────────────────────────────────────────────

// scanParsedFile finds every write-call site in an already-parsed file.
func scanParsedFile(pf parsedFile, idx *globalIndex) []Site {
	localDefsCache := map[*ast.BlockStmt]map[string]ast.Expr{}
	var sites []Site

	ast.Inspect(pf.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sc := enclosingFuncScope(pf.scopes, call.Pos())
		defs := localDefsFor(localDefsCache, sc.body, call.Pos())

		primitive, argIdx, matched := matchWriterCall(call, defs)
		if !matched || argIdx >= len(call.Args) {
			return true
		}

		r := resolverContextFor(idx, pf.dir, sc)
		pathArg := call.Args[argIdx]
		pattern, resolvedOK, degenerateRoot := resolveWriterPathArg(r, primitive, pathArg, defs, 0)
		category := classify(pattern, resolvedOK, degenerateRoot)
		pos := pf.fset.Position(call.Pos())

		sites = append(sites, Site{
			File:      pf.relPath,
			Line:      pos.Line,
			Column:    pos.Column,
			Primitive: primitive,
			Func:      sc.label,
			Pattern:   pattern,
			Category:  category,
			Resolved:  resolvedOK,
			Subsystem: subsystemOf(pf.relPath),
			Raw:       exprString(pathArg),
		})
		return true
	})

	return sites
}

// ─── AppendEvent bucket-literal rows ────────────────────────────────────────
//
// ledger.go's AppendEvent has exactly ONE os.OpenFile call site — the
// bucket (sessionID) argument it opens under .cog/ledger/<bucket>/ is
// itself a PARAMETER, so paramOrigin's cross-call-site unanimity
// requirement collapses to the generic opaque placeholder the moment two
// callers pass DIFFERENT literal buckets, which they do (identity-grants,
// worktree-reconciler, mcp-client, ...). That is the right, honest answer
// for THAT one row — it is not established that every caller uses the
// same bucket — but it throws away real, individually-known bucket names
// an RFC-driven writer inventory wants to see. This pass walks every call
// site that reaches AppendEvent — directly, or through the
// "appendEvent: AppendEvent" seam this repo uses so a test can substitute
// a fake writer (see IdentityGrantRegistry) — and emits one EXTRA row per
// call site whose own bucket argument resolves to a positive literal.
// Genuinely dynamic bucket arguments are not reported here at all: the
// ledger.go primitive row (the "sanitize-hole" row) already covers them,
// and adding a row here too would double-count the same write.

// findAppendEventAliases finds every (typeName, fieldName) struct field
// whose ONLY composite-literal binding, repo-wide, is the bare identifier
// appendEventName — i.e. a function-VALUED field this repo uses as an
// indirection seam over the real ledger writer, always defaulting to it.
// A field with more than one binding, or any binding to something other
// than that bare identifier, is not an alias: guessing which of several
// bound functions applies at a given call site would be exactly the kind
// of over-claim this tool exists to refuse.
func findAppendEventAliases(idx *globalIndex, appendEventName string) map[[2]string]bool {
	aliases := map[[2]string]bool{}
	byTypeField := map[[2]string][]fieldCandidate{}
	for typeName, cands := range idx.fieldCand {
		for _, c := range cands {
			key := [2]string{typeName, c.field}
			byTypeField[key] = append(byTypeField[key], c)
		}
	}
	for key, cands := range byTypeField {
		if len(cands) == 0 {
			continue
		}
		allMatch := true
		for _, c := range cands {
			id, isIdent := c.expr.(*ast.Ident)
			if !isIdent || id.Name != appendEventName {
				allMatch = false
				break
			}
		}
		if allMatch {
			aliases[key] = true
		}
	}
	return aliases
}

// appendEventBucketSites finds every call site that ultimately dispatches
// to (dir, appendEventName) — a bare identifier call in that same
// directory, or a selector call through a receiver field identified by
// aliases — and reports one Site per call site whose bucket argument
// (index 1: sessionID) resolves to a positive, non-opaque literal.
func appendEventBucketSites(files []parsedFile, idx *globalIndex) []Site {
	const appendEventDir = "internal/engine"
	const appendEventName = "AppendEvent"
	if _, ok := idx.funcDecls[funcKey{appendEventDir, appendEventName}]; !ok {
		return nil // defensive; never assume the shape this pass depends on
	}
	aliases := findAppendEventAliases(idx, appendEventName)

	var sites []Site
	for _, pf := range files {
		localDefsCache := map[*ast.BlockStmt]map[string]ast.Expr{}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sc := enclosingFuncScope(pf.scopes, call.Pos())

			reaches := false
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				reaches = pf.dir == appendEventDir && fn.Name == appendEventName
			case *ast.SelectorExpr:
				if sc.kind == "method" && sc.recvType != "" {
					if recv, isIdent := fn.X.(*ast.Ident); isIdent && recv.Name == sc.recvName {
						reaches = aliases[[2]string{sc.recvType, fn.Sel.Name}]
					}
				}
			}
			if !reaches || len(call.Args) < 2 {
				return true
			}

			defs := localDefsFor(localDefsCache, sc.body, call.Pos())
			r := resolverContextFor(idx, pf.dir, sc)
			bucket, ok, bucketDegenerate := r.resolveExpr(call.Args[1], defs, 0)
			if !ok || bucket == "" || isOpaqueRoot(bucket, bucketDegenerate) {
				return true // genuinely dynamic — the ledger.go primitive row already covers it
			}

			// bucket is guaranteed non-degenerate past the isOpaqueRoot
			// check above, and the constructed pattern's own root is the
			// real <WorkspaceRoot> anchor regardless — bucket only ever
			// lands in the middle of it — so this classify call's own
			// degenerateRoot is unconditionally false.
			pattern := "<WorkspaceRoot>/.cog/ledger/" + bucket + "/events.jsonl"
			pos := pf.fset.Position(call.Pos())
			sites = append(sites, Site{
				File:      pf.relPath,
				Line:      pos.Line,
				Column:    pos.Column,
				Primitive: "engine.AppendEvent(bucket)",
				Func:      sc.label,
				Pattern:   pattern,
				Category:  classify(pattern, true, false),
				Resolved:  true,
				Subsystem: subsystemOf(pf.relPath),
				Raw:       exprString(call.Args[1]),
			})
			return true
		})
	}
	return sites
}

// ─── Subprocess writers (declared out of scope for v1 — uncounted) ─────────
//
// A spawned process can write anywhere its own logic chooses; this tool
// has no way to see that without executing it, and the primitive-level
// scanning strategy the rest of this package relies on (see the package
// doc) simply does not apply once a write crosses a process boundary. v1
// scope is direct filesystem primitives plus sqlite (see "Primitive
// coverage" in the package doc); subprocess-mediated writes are
// ENUMERATED here — never silently capped or omitted — but not
// classified into cog/home/elsewhere/unanchored/dynamic, and none of them
// counts toward Summary.

// scanSubprocessSites finds every non-test exec.Command/exec.CommandContext
// call site and, where the call's result is bound to a local variable and
// that variable's .Dir field is assigned somewhere in the same function,
// resolves the assigned expression with the same best-effort machinery as
// everything else in this package.
func scanSubprocessSites(files []parsedFile, idx *globalIndex) []SubprocessSite {
	var out []SubprocessSite
	for _, pf := range files {
		localDefsCache := map[*ast.BlockStmt]map[string]ast.Expr{}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "exec" {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}

			sc := enclosingFuncScope(pf.scopes, call.Pos())
			dirExpr, dirKnown := ast.Expr(nil), false
			if cmdVar, hasVar := enclosingAssignTarget(sc.body, call); hasVar {
				dirExpr, dirKnown = findCmdDirAssignment(sc.body, cmdVar)
			}

			dirPattern := ""
			if dirKnown {
				defs := localDefsFor(localDefsCache, sc.body, call.Pos())
				r := resolverContextFor(idx, pf.dir, sc)
				if p, ok, _ := r.resolveExpr(dirExpr, defs, 0); ok {
					dirPattern = p
				} else {
					dirPattern = "<unresolved>"
				}
			}

			pos := pf.fset.Position(call.Pos())
			out = append(out, SubprocessSite{
				File:      pf.relPath,
				Line:      pos.Line,
				Column:    pos.Column,
				Func:      sc.label,
				Subsystem: subsystemOf(pf.relPath),
				Raw:       exprString(call),
				Dir:       dirPattern,
				DirKnown:  dirKnown,
			})
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// enclosingAssignTarget finds the local variable name that call's result is
// assigned to, if any — i.e. `cmdVar := exec.Command(...)` where call IS
// (by AST identity) that exact CallExpr node.
func enclosingAssignTarget(body *ast.BlockStmt, call *ast.CallExpr) (string, bool) {
	if body == nil {
		return "", false
	}
	var name string
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if rhs == ast.Expr(call) && i < len(as.Lhs) {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					name, found = id.Name, true
					return false
				}
			}
		}
		return true
	})
	return name, found
}

// findCmdDirAssignment finds a `<cmdVarName>.Dir = <expr>` assignment
// anywhere in body (last one wins if there is more than one, matching this
// package's general flat/branch-insensitive convention) and returns its
// RHS expression.
func findCmdDirAssignment(body *ast.BlockStmt, cmdVarName string) (ast.Expr, bool) {
	if body == nil {
		return nil, false
	}
	var found ast.Expr
	foundAny := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Dir" {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != cmdVarName {
			return true
		}
		found, foundAny = as.Rhs[0], true
		return true
	})
	return found, foundAny
}

// ─── Classification ─────────────────────────────────────────────────────────

// classify buckets a pattern into the five categories the auditor reports
// on.
//
// cog / home are checked first and are independent of resolvedOK: a
// filepath.Join can have one opaque segment (e.g. a sanitized session ID
// this tool can't chase through pathsafe.SanitizeComponent) sitting right
// next to a fully legible "<WorkspaceRoot>/.cog/ledger/" prefix, and that
// site unambiguously writes under .cog/ even though the pattern isn't
// FULLY known. Losing that signal just because one segment out of several
// was opaque would throw away exactly the information this tool exists to
// surface.
//
// For everything else, "elsewhere" is reserved for a pattern whose ROOT —
// its first "/"-delimited segment — is a NON-EMPTY, positively resolved
// literal or a known non-cog anchor (<WorkspaceRoot>, <TempDir>, an
// absolute "/..." path). A pattern whose root is an opaque placeholder
// ({name}), an unresolved call marker (<call:...>), or one of the several
// DEGENERATE shapes a partially-failed resolution leaves behind (an empty
// root, a bare extension fragment like ".md", a bare "dirname()"/
// "basename()" with nothing resolved inside it) has NOT established that
// the write lands outside .cog/ — it has established nothing about the
// destination at all — and claiming "elsewhere" for it was this tool's
// worst failure mode: over-claiming a negative for the one question the
// whole inventory exists to answer. Such a site is "unanchored" when it
// was at least structurally decomposed (resolvedOK — we know the SHAPE,
// just not the value), and "dynamic" when resolution failed outright (an
// arbitrary call, an index expression — we don't even know the shape).
//
// A root that IS positively known can still fail to clear the bar: a
// <WorkspaceRoot>-rooted pattern with an OPAQUE segment anywhere in its
// tail (not just the root) has not established non-.cog membership
// either, because .cog/ is itself "<WorkspaceRoot>/.cog" — an untraced
// tail segment could easily BE, or start with, ".cog" (a range-loop
// variable over a slice of ".cog/..." literals this tool has no data-flow
// into is exactly this shape; see internal/engine/init.go). <TempDir> and
// a genuine absolute-path literal don't get this extra scrutiny: unlike
// <WorkspaceRoot>, they are categorically disjoint from any workspace's
// .cog/ regardless of what an untraced tail segment turns out to be.
func classify(pattern string, resolvedOK bool, degenerateRoot bool) string {
	if hasCogSegment(pattern) {
		return "cog"
	}
	if strings.Contains(pattern, "<Home>") {
		return "home"
	}
	// "elsewhere" requires BOTH a structurally-complete resolution AND a
	// positively-known root. Root opacity/degeneracy alone is not a
	// sufficient test on its own: a resolution that gave up partway
	// through (a self-referential local — e.g. a directory-walk-upward
	// loop's `dir = filepath.Dir(dir)` — hitting the recursion ceiling
	// inside a filepath.Join) can still produce a pattern whose leading
	// "/"-delimited segment happens to look non-opaque, even though
	// resolvedOK is false. Gating on resolvedOK first closes that: a
	// failed resolution is never "elsewhere", however its leftover text
	// happens to look.
	//
	// degenerateRoot is the same gate applied to a DIFFERENT failure shape:
	// a pattern whose text looks exactly like a positively-known root (a
	// literal starting with "/") but was actually produced by concatenating
	// an empty-resolved base onto a literal — see resolveExpr's doc
	// comment. Text alone cannot tell the two apart, which is exactly why
	// this is threaded in as its own explicit signal rather than folded
	// into pattern's text as an in-band marker.
	if !resolvedOK || isOpaqueRoot(pattern, degenerateRoot) {
		if resolvedOK {
			return "unanchored"
		}
		return "dynamic"
	}
	if strings.HasPrefix(pattern, "<WorkspaceRoot>") && hasOpaqueSegment(pattern) {
		return "unanchored"
	}
	if isBareUnanchoredLiteral(pattern) {
		return "unanchored"
	}
	return "elsewhere"
}

// isBareUnanchoredLiteral reports whether pattern is a single path segment
// — no "/" anywhere in it — that also isn't one of this tool's own
// single-token anchors (<TempDir>, <WorkspaceRoot>; <Home> and <CogDir>
// never reach this point, both intercepted earlier in classify). Such a
// pattern carries no directory context at all: it is exactly what a
// filepath.Join(emptyOrOpaqueBase, "name") resolves to now that the Join
// handler above correctly skips an empty leading argument instead of
// leaving a stray "/" in front of it (real filepath.Join does the same —
// Join("", "state.json") is "state.json", not "/state.json"), and it is
// equally what a bare relative literal passed straight to a write
// primitive resolves to. Neither has positively established a destination
// outside .cog/ — the tool has no visibility into the process's working
// directory a relative path like this would actually resolve against —
// so "unanchored" is the honest bin, not "elsewhere". This check is
// deliberately NOT folded into isOpaqueRoot: that function is also used by
// appendEventBucketSites to validate a bucket/session-ID VALUE (never a
// filepath), where a bare literal like "main" is exactly the positive,
// non-opaque case it must accept.
func isBareUnanchoredLiteral(pattern string) bool {
	if strings.Contains(pattern, "/") {
		return false
	}
	switch pattern {
	case "<TempDir>", "<WorkspaceRoot>":
		return false
	}
	return true
}

func hasCogSegment(pattern string) bool {
	if strings.Contains(pattern, "<CogDir>") {
		return true
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == ".cog" {
			return true
		}
	}
	return false
}

// isOpaqueRoot reports whether pattern's first "/"-delimited segment
// carries no positive information about the destination. This covers
// genuine text-visible opacity (a "{name}" placeholder, an unresolved
// "<call:...>" marker) and the DEGENERATE shapes a partially-failed
// resolution leaves behind in pattern's TEXT, none of which are positive
// evidence of anything — plus one shape that is NOT visible in pattern's
// text at all: degenerateRoot, threaded in from the caller (see
// resolveExpr's doc comment), reports a concatenation whose left operand
// resolved to "" glued to a right-hand literal starting with "/". That
// leaves a pattern indistinguishable, by text alone, from a genuine
// absolute-path literal like "/etc/cogos/config.yaml" — which is exactly
// why it is carried as an explicit out-of-band bool instead of being
// spelled into pattern itself as some reserved marker text (a marker is
// exactly the shape that leaks: it composes only as long as every
// wrapper that touches the text remembers to preserve or strip it, and
// filepath.Join joining resolved parts with "/", or filepath.Dir/Base
// wrapping "dirname(...)"/"basename(...)" around an inner resolution,
// both do neither).
//
// The text-visible shapes:
//
//   - a bare "/" or a doubled leading separator ("//...") — an empty root
//     with NOTHING real after it, or with another empty component
//     immediately behind it. This is distinct from a genuine absolute-path
//     literal like "/etc/cogos/config.yaml" (a single leading slash with
//     real content right after it), which legitimately IS positive
//     evidence and is deliberately left alone: the two are the same
//     "empty root" shape by IndexByte alone, so the extra check below is
//     keyed on the full pattern, not just root. This shape can only arise
//     from a resolution that dropped, or duplicated, an empty component —
//     e.g. a naive "/"-join of an empty leading argument. The
//     filepath.Join handling above already skips empty arguments (real
//     filepath.Join does the same: Join("", "x") is "x", not "/x"), so
//     this specific artifact shouldn't recur from there; this is defense
//     in depth against any other pattern-building path making the same
//     mistake. (A bare-word root with no leading slash at all — the shape
//     Join("", "x") actually produces once fixed — carries the identical
//     lack of evidence but is handled at the classify() level instead,
//     via isBareUnanchoredLiteral, because isOpaqueRoot is also used on
//     non-path bucket/session-ID values where a bare word is the valid,
//     positive case.)
//   - a bare extension fragment, e.g. ".md" — the leftover of a suffix
//     with nothing real joined in front of it (see the compound-assignment
//     fix in applyAssign: before it, `memPath += ".md"` on an
//     unresolved base rendered exactly this way)
//   - a bare "dirname()" or "basename()" wrapping nothing (an empty inner
//     resolution), as opposed to "dirname(<WorkspaceRoot>)" or similar,
//     which DOES carry positive information and is left alone
func isOpaqueRoot(pattern string, degenerateRoot bool) bool {
	if degenerateRoot {
		return true
	}
	if pattern == "" {
		// The WHOLE pattern resolved to nothing at all — not "starts with
		// a slash" (a genuine absolute-path literal like
		// "/etc/cogos/config.yaml" legitimately does that and IS positive
		// evidence), but truly empty end to end. The one realistic
		// producer of this in practice — os.CreateTemp/os.MkdirTemp's
		// empty dir argument — is intercepted earlier by
		// resolveWriterPathArg and mapped to <TempDir> before it ever
		// reaches here, so a bare "" surviving to this point is itself a
		// signal something failed to resolve, not a real destination.
		return true
	}
	root := pattern
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		root = pattern[:i]
	}
	switch {
	case root == "<expr>" || root == "<max-depth>":
		return true
	case strings.Contains(root, "{") || strings.Contains(root, "<call:"):
		return true
	case root != "" && strings.HasPrefix(root, "."):
		// A bare extension fragment (".md") is not a root at all. An
		// empty root here (pattern begins with "/") is handled by the
		// "pattern == \"\"" case above when there's truly nothing else;
		// when there IS real content after the leading slash, that is a
		// genuine absolute path and is deliberately NOT caught here.
		return true
	case root == "dirname()" || root == "basename()":
		// filepath.Dir/Base wrapping an empty inner resolution: the
		// opaque tail this wraps could easily have absorbed .cog. A
		// non-empty inner value, e.g. "dirname(<WorkspaceRoot>)", is left
		// alone — it carries real information.
		return true
	case pattern == "/" || strings.HasPrefix(pattern, "//"):
		// Defense in depth: a bare "/" or a doubled leading separator, as
		// opposed to a single leading slash with real content after it
		// (a genuine absolute path, left alone — see the function doc
		// comment above). Keyed on the full pattern rather than root
		// because root alone can't tell these two apart.
		return true
	}
	return false
}

// hasOpaqueSegment reports whether ANY "/"-delimited segment of pattern —
// not just the root — carries no positive information about the
// destination. See classify's doc comment for why this matters
// specifically for a <WorkspaceRoot>-rooted pattern.
func hasOpaqueSegment(pattern string) bool {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" {
			continue // an interior empty segment is isOpaqueRoot's concern
			// when it's the ROOT; on its own it is not opacity evidence.
		}
		if seg == "<expr>" || seg == "<max-depth>" {
			return true
		}
		if strings.Contains(seg, "{") || strings.Contains(seg, "<call:") {
			return true
		}
	}
	return false
}

// subsystemOf derives a coarse subsystem label from a repo-relative file
// path, purely from directory structure (no imports are resolved).
func subsystemOf(relPath string) string {
	parts := strings.Split(relPath, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "internal" && parts[1] == "providers":
		return "provider:" + parts[2]
	case len(parts) >= 2 && parts[0] == "internal":
		return "internal:" + parts[1]
	case len(parts) >= 2 && parts[0] == "pkg":
		return "pkg:" + parts[1]
	case parts[0] == "sdk":
		return "sdk"
	case parts[0] == "cmd":
		return "cmd"
	case parts[0] == "harness":
		return "harness"
	default:
		return "other"
	}
}

// ─── Rendering helpers ──────────────────────────────────────────────────────

// exprString renders an AST expression back to Go source text using the
// same printer the stdlib formatter relies on, for a stable, exact
// human-readable "raw" column that never depends on this package's own
// resolution heuristics.
func exprString(e ast.Expr) string {
	var buf strings.Builder
	if err := printExpr(&buf, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return buf.String()
}

func printExpr(w io.Writer, e ast.Expr) error {
	cfg := printer.Config{Mode: printer.RawFormat}
	return cfg.Fprint(w, token.NewFileSet(), e)
}

func unquote(lit string) string {
	if len(lit) >= 2 {
		switch lit[0] {
		case '"', '`':
			return lit[1 : len(lit)-1]
		}
	}
	return lit
}
